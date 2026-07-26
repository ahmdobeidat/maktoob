package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type fakeSink struct {
	mu      sync.Mutex
	got     []VoiceNote
	ctxErrs []error
	budgets []time.Duration
	err     error
	seen    chan struct{}
}

func newFakeSink() *fakeSink { return &fakeSink{seen: make(chan struct{}, 64)} }

func (f *fakeSink) Ingest(ctx context.Context, n VoiceNote) error {
	// ctx.Err() is read here, synchronously inside the call, rather than the
	// context itself being stored for the caller to inspect later. process,
	// drain and deliverFailed all defer the fresh context's cancel func right
	// after this call returns, so a context stored and checked after the fact
	// would always read as cancelled regardless of whether it was live at
	// delivery time -- that would make this fake unable to tell a real bug
	// (delivery given an already-cancelled context) from ordinary cleanup.
	ctxErr := ctx.Err()

	// The remaining budget is read here for the same reason: it is only
	// meaningful while the call is in progress.
	var budget time.Duration
	if dl, ok := ctx.Deadline(); ok {
		budget = time.Until(dl)
	}

	f.mu.Lock()
	f.got = append(f.got, n)
	f.ctxErrs = append(f.ctxErrs, ctxErr)
	f.budgets = append(f.budgets, budget)
	err := f.err
	f.mu.Unlock()
	f.seen <- struct{}{}
	return err
}

func (f *fakeSink) notes() []VoiceNote {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]VoiceNote(nil), f.got...)
}

// ctxErrors returns, in delivery order, ctx.Err() as observed at the moment
// each Ingest call was made. A note delivered specifically because the
// listener is shutting down must still arrive with a nil error here: a real,
// context-aware sink checks exactly this before writing, and the note has
// already been acknowledged to WhatsApp by the time it reaches the sink, so a
// sink that honoured a cancelled context would refuse the one write that
// cannot be retried.
func (f *fakeSink) ctxErrors() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.ctxErrs...)
}

// budget returns how much time the i'th delivery was actually given.
func (f *fakeSink) budget(i int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.budgets[i]
}

func (f *fakeSink) wait(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.seen:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for note %d of %d", i+1, n)
		}
	}
}

type fakeDownloader struct {
	data  []byte
	err   error
	delay time.Duration
}

func (f fakeDownloader) Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.data, f.err
}

func newTestListener(t *testing.T, sink Sink, down Downloader) (*Listener, context.CancelFunc) {
	t.Helper()
	l := &Listener{Down: down, Sink: sink, Salt: testSalt(t)}
	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); l.Close() })
	return l, cancel
}

func TestAcceptedNoteReachesTheSink(t *testing.T) {
	sink := newFakeSink()
	l, _ := newTestListener(t, sink, fakeDownloader{data: []byte("audio")})

	dm := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	l.onEvent(message(dm, audioPTT()))
	sink.wait(t, 1)

	got := sink.notes()[0]
	if got.SenderName != "Um Ahmad" {
		t.Fatalf("sender name: %q", got.SenderName)
	}
	if got.Ext != ".ogg" {
		t.Fatalf("ext: %q", got.Ext)
	}
	if got.DurationHint != 7000 {
		t.Fatalf("duration hint: got %d, want 7000", got.DurationHint)
	}
	if got.DownloadErr != "" || got.Audio == nil {
		t.Fatalf("expected audio, got err %q", got.DownloadErr)
	}
	body, err := io.ReadAll(got.Audio)
	if err != nil || string(body) != "audio" {
		t.Fatalf("audio body %q err %v", body, err)
	}
}

// A successful delivery writes up to maxMediaBytes to disk, probes the result
// with ffprobe (which allots itself 60s on its own), then makes two writes
// against a database opened with busy_timeout(5000). A delivery deadline at or
// near that busy timeout means the ordinary lock contention busy_timeout exists
// to absorb instead exhausts the whole delivery window, and the note is lost
// permanently because whatsmeow acknowledged it on arrival.
func TestDeliveryBudgetExceedsTheDatabaseBusyTimeout(t *testing.T) {
	// store.Open's DSN carries _pragma=busy_timeout(5000). internal/wa cannot
	// import internal/store, so the value is restated here rather than shared.
	const busyTimeout = 5 * time.Second

	sink := newFakeSink()
	l, _ := newTestListener(t, sink, fakeDownloader{data: []byte("audio")})

	dm := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	l.onEvent(message(dm, audioPTT()))
	sink.wait(t, 1)

	got := sink.budget(0)
	if got <= busyTimeout {
		t.Fatalf("delivery budget %s does not exceed the database busy timeout %s", got, busyTimeout)
	}
	if got < time.Minute {
		t.Fatalf("delivery budget %s is under the 60s ffprobe allowance on its own", got)
	}
}

// The handler runs on whatsmeow's node loop, which processes one node at a time
// and waits up to five minutes for a slow handler. A download on that thread
// stalls every later message.
func TestHandlerDoesNotBlock(t *testing.T) {
	sink := newFakeSink()
	l, _ := newTestListener(t, sink, fakeDownloader{data: []byte("a"), delay: 2 * time.Second})

	dm := types.JID{User: "962790000000", Server: types.DefaultUserServer}

	start := time.Now()
	l.onEvent(message(dm, audioPTT()))
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("handler took %s, want under 10ms", elapsed)
	}
}

func TestFailedDownloadIsRecordedNotDropped(t *testing.T) {
	sink := newFakeSink()
	l, _ := newTestListener(t, sink, fakeDownloader{err: errors.New("410 gone")})

	dm := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	l.onEvent(message(dm, audioPTT()))
	sink.wait(t, 1)

	got := sink.notes()[0]
	if got.Audio != nil {
		t.Fatal("expected no audio")
	}
	if got.DownloadErr == "" {
		t.Fatal("expected a download error to be recorded")
	}
	if got.SenderAlias == "" || got.DedupeKey == "" {
		t.Fatal("metadata must survive a failed download")
	}
}

// notes.error is rendered to the user in place of a transcript, and the user
// cannot play the audio to find out what happened. A cancelled download must
// therefore read as a sentence, not as the Go runtime's error value.
func TestCancelledDownloadIsReportedInWords(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"cancelled":        {context.Canceled, reasonNotDownloaded},
		"deadline":         {context.DeadlineExceeded, reasonNotDownloaded},
		"wrapped cancel":   {fmt.Errorf("download media: %w", context.Canceled), reasonNotDownloaded},
		"real CDN failure": {errors.New("410 gone"), "410 gone"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := downloadFailureReason(tc.err)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "context canceled") {
				t.Fatalf("a Go error value reached the user: %q", got)
			}
		})
	}
}

func TestSinkErrorDoesNotKillListener(t *testing.T) {
	sink := newFakeSink()
	sink.err = errors.New("disk full")
	l, _ := newTestListener(t, sink, fakeDownloader{data: []byte("a")})

	dm := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	l.onEvent(message(dm, audioPTT()))
	sink.wait(t, 1)

	sink.mu.Lock()
	sink.err = nil
	sink.mu.Unlock()

	l.onEvent(message(dm, audioPTT()))
	sink.wait(t, 1)

	if len(sink.notes()) != 2 {
		t.Fatalf("got %d notes, want 2", len(sink.notes()))
	}
}

func TestDroppedMessagesNeverReachTheSink(t *testing.T) {
	sink := newFakeSink()
	l, _ := newTestListener(t, sink, fakeDownloader{data: []byte("a")})

	l.onEvent(message(types.StatusBroadcastJID, audioPTT()))
	time.Sleep(100 * time.Millisecond)

	if n := len(sink.notes()); n != 0 {
		t.Fatalf("a dropped message reached the sink: %d notes", n)
	}
}

func TestLoggedOutReportsStateAndStops(t *testing.T) {
	sink := newFakeSink()
	states := make(chan State, 4)

	l := &Listener{
		Down: fakeDownloader{data: []byte("a")}, Sink: sink, Salt: testSalt(t),
		OnState: func(s State) { states <- s },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.onEvent(&events.LoggedOut{})

	// Checked before the deferred Close(), which cancels the context itself:
	// without this, a LoggedOut handler that forgot to call l.cancel() would
	// still pass, since Close() would cancel it moments later regardless.
	if l.ctx.Err() == nil {
		t.Fatal("LoggedOut did not stop the listener: l.ctx is still live")
	}

	select {
	case got := <-states:
		if got != LoggedOut {
			t.Fatalf("got state %v, want LoggedOut", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no state reported")
	}
}

// TestNoteArrivingAfterCloseIsStillRecorded pins down the window between
// cancellation and the socket actually tearing down.
//
// enqueue's select used to be reached unconditionally. Once l.ctx was cancelled
// and the worker had returned, both arms were ready — l.jobs still had capacity
// and Done() was closed — so Go chose between them uniformly at random and
// roughly half of these notes were written into a buffer with no reader left,
// with no log and no failed row. Twenty notes make a coin-flip regression fail
// with probability 1 - 2^-20 rather than intermittently.
func TestNoteArrivingAfterCloseIsStillRecorded(t *testing.T) {
	const notes = 20

	sink := newFakeSink()
	l := &Listener{Down: fakeDownloader{data: []byte("a")}, Sink: sink, Salt: testSalt(t)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	dm := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	for i := 0; i < notes; i++ {
		m := message(dm, audioPTT())
		m.Info.ID = fmt.Sprintf("3EB0%04d", i)
		l.onEvent(m)
	}
	sink.wait(t, notes)

	got := sink.notes()
	if len(got) != notes {
		t.Fatalf("got %d notes after Close, want %d", len(got), notes)
	}
	for i, n := range got {
		if n.DownloadErr == "" {
			t.Fatalf("note %d has no reason explaining the missing audio", i)
		}
		if n.SenderAlias == "" || n.DedupeKey == "" {
			t.Fatalf("note %d lost its metadata", i)
		}
	}
	for i, err := range sink.ctxErrors() {
		if err != nil {
			t.Fatalf("note %d delivered with an already-cancelled context: %v", i, err)
		}
	}
}

// Close must unregister from whatsmeow, not merely cancel. A handler left
// registered keeps receiving messages for as long as the socket takes to tear
// down, and every one of those arrives at a listener with no worker.
func TestCloseRemovesTheEventHandler(t *testing.T) {
	client := &whatsmeow.Client{}
	l := &Listener{
		Client: client,
		Down:   fakeDownloader{data: []byte("a")},
		Sink:   newFakeSink(),
		Salt:   testSalt(t),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if l.handlerID == 0 {
		t.Fatal("Start discarded the handler id, so Close cannot unregister")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// RemoveEventHandler reports whether it found the id. False here means Close
	// already removed it, which is the whole claim.
	if client.RemoveEventHandler(l.handlerID) {
		t.Fatal("the handler was still registered after Close")
	}
}

func TestShutdownDrainsQueuedJobs(t *testing.T) {
	sink := newFakeSink()
	// A slow download keeps the worker busy so the next job stays queued.
	l := &Listener{
		Down: fakeDownloader{data: []byte("a"), delay: 500 * time.Millisecond},
		Sink: sink, Salt: testSalt(t),
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}

	dm := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	first := message(dm, audioPTT())
	second := message(dm, audioPTT())
	second.Info.ID = "3EB0DEF"

	l.onEvent(first)
	l.onEvent(second)

	cancel()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Both must be accounted for: whatsmeow acked them on arrival, so anything
	// dropped here is lost permanently with no record the user can see.
	if n := len(sink.notes()); n != 2 {
		t.Fatalf("got %d notes after shutdown, want 2", n)
	}

	// The first job's download is in flight with l.ctx already cancelled by the
	// time it returns, which is exactly the case that must not leak l.ctx into
	// delivery: a context-aware sink checking ctx.Err() would otherwise refuse
	// to record a note that whatsmeow will never redeliver.
	for i, err := range sink.ctxErrors() {
		if err != nil {
			t.Fatalf("note %d delivered with an already-cancelled context: %v", i, err)
		}
	}
}

// TestFullQueueCancellationDeliversTheBlockedNote exercises the one send-side
// branch no other test reaches: enqueue's select blocks because l.jobs is at
// queueDepth capacity, and only unblocks when l.ctx is cancelled. That branch
// is the last line of defence against losing a note whatsmeow has already
// acknowledged, so it needs its own coverage rather than trusting the code by
// inspection.
//
// The Listener here is built by hand rather than via Start/newTestListener:
// Start's worker goroutine would drain l.jobs as fast as it is filled, and the
// whole point is to keep the queue genuinely full so the second onEvent call
// has to block on the channel send.
func TestFullQueueCancellationDeliversTheBlockedNote(t *testing.T) {
	sink := newFakeSink()
	ctx, cancel := context.WithCancel(context.Background())

	l := &Listener{
		Down: fakeDownloader{data: []byte("a")},
		Sink: sink,
		Salt: testSalt(t),
		Log:  slog.Default(),
	}
	l.ctx = ctx
	l.cancel = cancel
	l.jobs = make(chan job, queueDepth)

	for i := 0; i < queueDepth; i++ {
		l.jobs <- job{}
	}

	dm := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	blocked := message(dm, audioPTT())

	returned := make(chan struct{})
	go func() {
		l.onEvent(blocked)
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("onEvent returned before the full queue should have blocked it")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()

	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("onEvent never returned after the context was cancelled")
	}

	sink.wait(t, 1)
	got := sink.notes()[0]
	if got.DownloadErr == "" {
		t.Fatal("expected a download error explaining why the note has no audio")
	}
	if got.SenderAlias == "" || got.DedupeKey == "" {
		t.Fatal("metadata must survive a cancelled enqueue")
	}
	for i, err := range sink.ctxErrors() {
		if err != nil {
			t.Fatalf("note %d delivered with an already-cancelled context: %v", i, err)
		}
	}
}
