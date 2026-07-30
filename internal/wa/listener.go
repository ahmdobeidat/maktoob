package wa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
)

// queueDepth bounds the handoff between the event handler and the worker.
//
// The send blocks when it is full rather than spawning a goroutine. Spawning
// would be unbounded: one busy group could put N simultaneous downloads in
// flight, each buffering a whole file in memory. Blocking pushes backpressure
// into whatsmeow's own bounded queue, which is built to absorb it.
const queueDepth = 256

// drainGrace bounds how long shutdown spends recording jobs that never made it
// to the worker. They are metadata structs whose downloads have not started, so
// a single insert each, and this bounds the whole drain loop rather than one
// note. It exists to stop Ctrl-C hanging, so it is deliberately short.
const drainGrace = 5 * time.Second

// deliveryTimeout bounds one ordinary delivery: a note that has its audio and
// is on its way into the store.
//
// It is separate from drainGrace, and much larger, because the work behind it
// is not comparable. A delivery writes up to maxMediaBytes to disk, then probes
// the file with ffprobe, which allots itself audio.ConvertTimeout (60s) on its
// own, then performs two database writes. The database is opened with
// busy_timeout(5000), so lock contention is meant to be absorbed by waiting up
// to five seconds per statement.
//
// The previous value here was drainGrace itself, which made the delivery
// deadline exactly equal to the busy timeout: the very contention busy_timeout
// exists to ride out would instead consume the entire delivery window, and
// CreateNote would return a deadline error. A note lost that way is lost for
// good, because whatsmeow acknowledged it to the server on arrival and nothing
// will redeliver it. One writer makes that rare; serve, which runs the web
// layer, the transcription worker and the listener in one process, will not.
const deliveryTimeout = 90 * time.Second

// freshDeliveryContext returns a context bounded by timeout but detached from
// the listener's own lifecycle context.
//
// Some call sites here deliver a note precisely because l.ctx is cancelled or
// about to be: a download that raced a shutdown, a job orphaned by a full
// queue at cancellation, or one still sitting in the channel when the worker
// exits. whatsmeow has already acknowledged each of these to the server, so a
// note lost past this point is unrecoverable. Passing l.ctx through to
// delivery here would hand a context-aware sink a context whose Err() is
// already non-nil, and a sink built to respect cancellation would then refuse
// the write for the exact case where refusing is the one thing it must not do.
func freshDeliveryContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// Reasons recorded against a note whose audio never made it to disk.
//
// They are written into notes.error, which is rendered to the user in place of
// a transcript. The reader is deaf and cannot fall back to playing the audio, so
// this string is the entire explanation they get: it has to be a sentence about
// what happened, not an internal error value.
const (
	reasonNotQueued     = "shut down before the note could be queued"
	reasonNotDownloaded = "shut down before the audio was downloaded"
)

// maxMediaBytes rejects an implausible declared media length before download.
// The field is supplied by the peer, so it is not trustworthy input.
const maxMediaBytes = 64 << 20

// VoiceNote is one incoming voice note, with every WhatsApp identifier already
// reduced to an install-scoped alias. Nothing downstream can recover a phone
// number from this struct.
type VoiceNote struct {
	ChatAlias    string
	ChatName     string
	SenderAlias  string
	SenderName   string
	DedupeKey    string
	ReceivedAt   time.Time
	DurationHint int64
	Ext          string

	// Audio is nil when the download failed. DownloadErr then says why.
	//
	// The note is still delivered, because whatsmeow acknowledges a message to
	// the server as it is decrypted: a note we do not record here is gone for
	// good. A user who cannot hear the audio is precisely the user who needs to
	// be told that something arrived.
	Audio       io.Reader
	DownloadErr string
}

// Sink receives voice notes. It is deliberately the only channel through which
// this package reaches the rest of maktoob.
//
// Nothing in internal/pipeline may implement this interface. The adapter lives
// in cmd/maktoob, so the dependency arrow only ever points out of this package.
// Implementing it downstream would reverse that arrow and make this package
// impossible to delete, which is the one property it exists to have.
//
// Implementations must be safe for concurrent use. Ingest is normally called
// from the single worker goroutine, but a handler that cannot queue a note
// records it directly, so a call from whatsmeow's node handler can overlap one
// already in flight on the worker.
type Sink interface {
	Ingest(ctx context.Context, n VoiceNote) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, n VoiceNote) error

func (f SinkFunc) Ingest(ctx context.Context, n VoiceNote) error { return f(ctx, n) }

// State is the connection state reported to the composition root.
type State int

const (
	Disconnected State = iota
	Connected
	// LoggedOut is terminal. The session is dead on WhatsApp's side and only
	// re-pairing recovers it, so the interface must say so rather than going
	// quiet and looking healthy.
	LoggedOut
)

func (s State) String() string {
	switch s {
	case Connected:
		return "connected"
	case LoggedOut:
		return "logged out"
	default:
		return "disconnected"
	}
}

// Downloader is the part of *whatsmeow.Client this package uses. It exists so
// tests can substitute a fake; *whatsmeow.Client satisfies it.
type Downloader interface {
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
}

type job struct {
	note  VoiceNote
	audio *waE2E.AudioMessage
}

// Listener bridges whatsmeow events into a Sink.
type Listener struct {
	Client  *whatsmeow.Client
	Down    Downloader
	Sink    Sink
	OnState func(State)
	Salt    Salt
	Log     *slog.Logger

	jobs      chan job
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	once      sync.Once
	handlerID uint32

	// removeWG tracks the goroutine Close starts to unregister the event handler.
	// Close deliberately does not wait on it — waiting is what would deadlock when
	// Close is called from inside a handler — so this exists to make the removal's
	// completion observable to a test, which otherwise could only poll a
	// destructive check that passes whether or not Close did anything.
	removeWG sync.WaitGroup

	// mu guards closed, and is held across enqueue's send to l.jobs.
	//
	// It is what makes the handoff safe rather than probabilistic. drain() must
	// set closed before it starts emptying the channel, and it can only take mu
	// once every in-flight enqueue has either completed its send or given up.
	// So a job that made it into the channel is guaranteed to be visible to the
	// drain loop, and a job that did not is guaranteed to see closed and record
	// itself as failed. Without that ordering, drain()'s default: arm can exit a
	// microsecond before a send lands and strand the note with no trace.
	mu     sync.Mutex
	closed bool
}

// Start registers the event handler and starts the worker. It does not block.
func (l *Listener) Start(ctx context.Context) error {
	if l.Sink == nil {
		return fmt.Errorf("wa: Sink is required")
	}
	if len(l.Salt) == 0 {
		return fmt.Errorf("wa: Salt is required")
	}
	// Down is defaulted rather than only validated: *whatsmeow.Client is the
	// only real implementation, and a caller that supplied a Client has already
	// said which one to use. The interface exists for tests, not for choice.
	//
	// process() dereferences Down unconditionally on the worker goroutine, where
	// a nil interface panics with nothing to recover it and takes the process
	// down. Catching it here turns what would be a crash on the first voice note
	// into a startup error the caller can read.
	if l.Down == nil {
		if l.Client == nil {
			return fmt.Errorf("wa: Down or Client is required")
		}
		l.Down = l.Client
	}
	if l.Log == nil {
		l.Log = slog.Default()
	}

	l.ctx, l.cancel = context.WithCancel(ctx)
	l.jobs = make(chan job, queueDepth)

	l.wg.Add(1)
	go l.run()

	if l.Client != nil {
		l.handlerID = l.Client.AddEventHandler(l.HandleEvent)
	}
	return nil
}

// Close stops the worker and waits for in-flight work to finish.
//
// The context is cancelled first and the handler unregistered second, which is
// the reverse of what it looks like it should be. An earlier version did it the
// other way, reasoning that leaving the handler registered means whatsmeow keeps
// dispatching notes at a listener with no worker left. That reasoning does not
// hold: whatsmeow acks a message to the server as it decrypts it, so a note
// arriving during teardown is already acked whether or not our handler is
// listening. Unregistering first does not save it — it only makes it arrive
// somewhere nobody is watching, with no row and no log. Cancelling first sends
// it through enqueue's short-circuit instead, which records it as failed. The
// user learns a voice note came in, which is the whole point.
//
// The old order was also a circular wait. RemoveEventHandler takes whatsmeow's
// event-handler write lock, which dispatchEvent holds for the duration of any
// in-flight handler, which may be blocked in enqueue on a full queue, whose only
// escape is the cancellation that had not happened yet. Close could block for a
// whole download plus probe plus delivery. Cancelling first releases that
// immediately.
//
// Removal runs in its own goroutine because whatsmeow documents RemoveEventHandler
// as deadlocking when called from inside an event handler, and the most natural
// wiring for this package — OnState(LoggedOut) calling Close — is exactly that
// case. Close therefore starts the removal rather than completing it. Anything
// dispatched in the gap is handled deterministically by the same short-circuit.
func (l *Listener) Close() error {
	l.once.Do(func() {
		if l.cancel != nil {
			l.cancel()
		}
		if l.Client != nil && l.handlerID != 0 {
			l.removeWG.Add(1)
			go func() {
				defer l.removeWG.Done()
				l.Client.RemoveEventHandler(l.handlerID)
			}()
		}
		l.wg.Wait()
	})
	return nil
}

// HandleEvent runs on whatsmeow's node handler goroutine, which processes one
// node at a time and tolerates a slow handler for five minutes before
// continuing it in the background. Everything expensive belongs in the worker;
// this function filters, aliases, and hands off.
//
// It is exported because it is this package's entire ingress, and because
// nothing else can drive it. Sink is the only outbound edge and whatsmeow's
// dispatchEvent is unexported, so without this the seam from a WhatsApp event
// through to a stored note — the actual product — can only be tested inside
// this package, with the two halves it joins mocked out. cmd/maktoob is the one
// place allowed to import both sides, and this is what lets it prove they fit.
func (l *Listener) HandleEvent(raw any) {
	switch evt := raw.(type) {
	case *events.Message:
		l.enqueue(evt)
	case *events.Connected:
		l.state(Connected)
	case *events.Disconnected:
		l.state(Disconnected)
	case *events.LoggedOut:
		l.state(LoggedOut)
		l.cancel()
	}
}

func (l *Listener) enqueue(evt *events.Message) {
	audio, ok := voiceNote(evt)
	if !ok {
		return
	}
	if audio.GetFileLength() > maxMediaBytes {
		l.Log.Warn("voice note exceeds the size cap, ignored",
			"bytes", audio.GetFileLength())
		return
	}

	src := evt.Info.MessageSource

	// MessageSource carries SenderAlt but no ChatAlt. Passing SenderAlt as the
	// chat's alternate form is correct rather than lazy: canonical only consults
	// it when the primary JID is on the hidden-user server, which a group JID
	// never is, and in a direct message the chat JID and the sender JID are the
	// same value. If whatsmeow ever adds a ChatAlt field, use it here.
	j := job{
		audio: audio,
		note: VoiceNote{
			ChatAlias:    l.Salt.ChatAlias(src.Chat, src.SenderAlt),
			SenderAlias:  l.Salt.SenderAlias(src.Sender, src.SenderAlt),
			SenderName:   evt.Info.PushName,
			DedupeKey:    l.Salt.MessageAlias(src.Chat, evt.Info.ID),
			ReceivedAt:   evt.Info.Timestamp,
			DurationHint: int64(audio.GetSeconds()) * 1000,
			Ext:          extForMimetype(audio.GetMimetype()),
		},
	}

	// A group's subject is not carried on the message. Fetching it needs a
	// network round trip, which must never happen on this goroutine, so the name
	// is left empty and UpsertChat keeps whatever better name it already has.
	if !src.IsGroup {
		j.note.ChatName = evt.Info.PushName
	}

	// The short-circuit is not an optimisation. Once l.ctx is cancelled and the
	// worker has returned, both arms of the select below are ready — l.jobs still
	// has capacity and Done() is closed — and Go picks between ready arms
	// uniformly at random. Half the notes arriving in that window would be
	// written into a buffer nobody will ever read again: no log, no failed row,
	// nothing the user could ever learn from. Checking first makes the outcome
	// deterministic.
	l.mu.Lock()
	if l.closed || l.ctx.Err() != nil {
		l.mu.Unlock()
		l.deliverFailed(j.note, reasonNotQueued)
		return
	}

	select {
	case l.jobs <- j:
		l.mu.Unlock()
	case <-l.ctx.Done():
		l.mu.Unlock()
		// Cancelled while the queue was full. Record it rather than dropping it:
		// whatsmeow already acked this message, so nothing will redeliver it.
		l.deliverFailed(j.note, reasonNotQueued)
	}
}

func (l *Listener) run() {
	defer l.wg.Done()

	for {
		select {
		case j := <-l.jobs:
			l.process(l.ctx, j)
		case <-l.ctx.Done():
			l.drain()
			return
		}
	}
}

// drain records everything still queued at teardown.
//
// Without this, every job in the channel at Ctrl-C is lost silently: whatsmeow
// acked each of them on arrival, so WhatsApp will not send them again, and the
// user has no way to learn they existed.
//
// closed is set under l.mu before the loop starts, which is what makes the
// default: arm below safe to exit on. Taking l.mu means every enqueue that was
// mid-send has finished, so anything it sent is already in the channel and this
// loop will see it; and every enqueue that arrives afterwards will observe
// closed and record its own note instead of sending into a channel with no
// reader. A bare default: without that ordering strands any job whose send
// lands a moment after the loop gives up.
func (l *Listener) drain() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()

	ctx, cancel := freshDeliveryContext(drainGrace)
	defer cancel()

	for {
		select {
		case j := <-l.jobs:
			l.deliverFailedCtx(ctx, j.note, reasonNotDownloaded)
		default:
			return
		}
	}
}

func (l *Listener) process(ctx context.Context, j job) {
	data, err := l.Down.Download(ctx, j.audio)
	if err != nil {
		// whatsmeow retries internally, and returns immediately on 403, 404 and
		// 410, which is expired media on WhatsApp's CDN. There is no second
		// chance to take, so record the arrival and move on.
		//
		// ctx (the download context) is not used for delivery below: if this
		// error is ctx.Err() because the listener is stopping, ctx is already
		// cancelled, and delivery needs its own uncancelled window. See
		// freshDeliveryContext.
		l.Log.Warn("voice note download failed", "error", err)
		l.deliverFailed(j.note, downloadFailureReason(err))
		return
	}

	j.note.Audio = bytes.NewReader(data)

	dctx, cancel := freshDeliveryContext(deliveryTimeout)
	defer cancel()
	l.deliver(dctx, j.note)
}

// downloadFailureReason turns a download error into the sentence stored on the
// note.
//
// run()'s select is random when a job is queued and the context is cancelled at
// the same moment, so the worker can start a download it is about to abandon.
// Download then returns context.Canceled, and the raw error string would be
// written verbatim into notes.error. "context canceled" is the entire
// explanation a deaf user gets next to a note they cannot play — a Go runtime
// value standing in for the product's core surface at the worst possible time.
// It is the same event drain() already describes in words, so it gets the same
// words.
func downloadFailureReason(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return reasonNotDownloaded
	}
	return err.Error()
}

func (l *Listener) deliver(ctx context.Context, n VoiceNote) {
	if err := l.Sink.Ingest(ctx, n); err != nil {
		// A sink failure is a disk or database problem. Dropping the WhatsApp
		// connection over it would turn one lost note into all of them.
		l.Log.Error("could not record voice note", "error", err)
	}
}

// deliverFailed records a note whose audio is gone. It gets deliveryTimeout
// rather than drainGrace even on the teardown path, because it still performs
// the same two database writes against the same busy_timeout(5000) as a
// successful delivery, and losing the row to lock contention would leave the
// user with no trace of a note they can neither read nor hear. drain() is the
// one exception: it passes its own, shorter context, because it is bounding a
// whole loop rather than a single note.
//
// The cost of that choice, since it is not obvious: both calls in enqueue run on
// whatsmeow's node handler goroutine, so a wedged sink stalls node processing for
// up to deliveryTimeout rather than drainGrace — 90 seconds instead of 5. That is
// still inside whatsmeow's own five-minute tolerance, and the alternative is
// discarding the one record of a note that cannot be recovered, so the trade is
// deliberate. It is only reachable when the sink itself is hung; an ordinary
// database write returns in milliseconds.
func (l *Listener) deliverFailed(n VoiceNote, reason string) {
	ctx, cancel := freshDeliveryContext(deliveryTimeout)
	defer cancel()
	l.deliverFailedCtx(ctx, n, reason)
}

func (l *Listener) deliverFailedCtx(ctx context.Context, n VoiceNote, reason string) {
	n.Audio = nil
	n.DownloadErr = reason
	l.deliver(ctx, n)
}

func (l *Listener) state(s State) {
	if l.OnState != nil {
		l.OnState(s)
	}
	l.Log.Info("whatsapp connection state", "state", s.String())
}
