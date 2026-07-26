package wa

import (
	"bytes"
	"context"
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

	jobs   chan job
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// Start registers the event handler and starts the worker. It does not block.
func (l *Listener) Start(ctx context.Context) error {
	if l.Sink == nil {
		return fmt.Errorf("wa: Sink is required")
	}
	if len(l.Salt) == 0 {
		return fmt.Errorf("wa: Salt is required")
	}
	if l.Log == nil {
		l.Log = slog.Default()
	}

	l.ctx, l.cancel = context.WithCancel(ctx)
	l.jobs = make(chan job, queueDepth)

	l.wg.Add(1)
	go l.run()

	if l.Client != nil {
		l.Client.AddEventHandler(l.onEvent)
	}
	return nil
}

// Close stops the worker and waits for in-flight work to finish.
func (l *Listener) Close() error {
	l.once.Do(func() {
		if l.cancel != nil {
			l.cancel()
		}
		l.wg.Wait()
	})
	return nil
}

// onEvent runs on whatsmeow's node handler goroutine, which processes one node
// at a time and tolerates a slow handler for five minutes before continuing it
// in the background. Everything expensive belongs in the worker; this function
// filters, aliases, and hands off.
func (l *Listener) onEvent(raw any) {
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

	select {
	case l.jobs <- j:
	case <-l.ctx.Done():
		// Cancelled while the queue was full. Record it rather than dropping it:
		// whatsmeow already acked this message, so nothing will redeliver it.
		l.deliverFailed(j.note, "shut down before the note could be queued")
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

// drain records everything still queued at shutdown.
//
// Without this, every job in the channel at Ctrl-C is lost silently: whatsmeow
// acked each of them on arrival, so WhatsApp will not send them again, and the
// user has no way to learn they existed.
func (l *Listener) drain() {
	ctx, cancel := freshDeliveryContext(drainGrace)
	defer cancel()

	for {
		select {
		case j := <-l.jobs:
			l.deliverFailedCtx(ctx, j.note, "shut down before the audio was downloaded")
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
		// error is ctx.Err() because the listener is shutting down, ctx is
		// already cancelled, and delivery needs its own uncancelled window. See
		// freshDeliveryContext.
		l.Log.Warn("voice note download failed", "error", err)
		l.deliverFailed(j.note, err.Error())
		return
	}

	j.note.Audio = bytes.NewReader(data)

	dctx, cancel := freshDeliveryContext(deliveryTimeout)
	defer cancel()
	l.deliver(dctx, j.note)
}

func (l *Listener) deliver(ctx context.Context, n VoiceNote) {
	if err := l.Sink.Ingest(ctx, n); err != nil {
		// A sink failure is a disk or database problem. Dropping the WhatsApp
		// connection over it would turn one lost note into all of them.
		l.Log.Error("could not record voice note", "error", err)
	}
}

// deliverFailed records a note whose audio is gone. It gets deliveryTimeout
// rather than drainGrace even on the shutdown path, because it still performs
// the same two database writes against the same busy_timeout(5000) as a
// successful delivery, and losing the row to lock contention would leave the
// user with no trace of a note they can neither read nor hear. drain() is the
// one exception: it passes its own, shorter context, because it is bounding a
// whole loop rather than a single note.
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
