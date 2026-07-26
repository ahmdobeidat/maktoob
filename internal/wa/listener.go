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
// this is generous.
const drainGrace = 5 * time.Second

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
	ctx, cancel := context.WithTimeout(context.Background(), drainGrace)
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
		l.Log.Warn("voice note download failed", "error", err)
		l.deliverFailedCtx(ctx, j.note, err.Error())
		return
	}

	j.note.Audio = bytes.NewReader(data)
	l.deliver(ctx, j.note)
}

func (l *Listener) deliver(ctx context.Context, n VoiceNote) {
	if err := l.Sink.Ingest(ctx, n); err != nil {
		// A sink failure is a disk or database problem. Dropping the WhatsApp
		// connection over it would turn one lost note into all of them.
		l.Log.Error("could not record voice note", "error", err)
	}
}

func (l *Listener) deliverFailed(n VoiceNote, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), drainGrace)
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
