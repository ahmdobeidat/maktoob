package wa

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type fakeSink struct {
	mu   sync.Mutex
	got  []VoiceNote
	err  error
	seen chan struct{}
}

func newFakeSink() *fakeSink { return &fakeSink{seen: make(chan struct{}, 64)} }

func (f *fakeSink) Ingest(_ context.Context, n VoiceNote) error {
	f.mu.Lock()
	f.got = append(f.got, n)
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

	select {
	case got := <-states:
		if got != LoggedOut {
			t.Fatalf("got state %v, want LoggedOut", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no state reported")
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
}
