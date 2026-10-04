package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"rsc.io/qr"

	"github.com/ahmdobeidat/maktoob/internal/pipeline"
	"github.com/ahmdobeidat/maktoob/internal/store"
	"github.com/ahmdobeidat/maktoob/internal/wa"
	"github.com/ahmdobeidat/maktoob/internal/web"
)

// transcriber is the slice of the pipeline the worker loop uses. Narrow on
// purpose: the loop's retry and shutdown behaviour is worth testing without
// standing up ffmpeg and a whisper server.
type transcriber interface {
	ProcessNext(context.Context) (bool, error)
}

// idlePoll is how long the transcription worker waits before asking for work
// again after finding the queue empty.
//
// The queue is a table rather than a channel, so there is no signal to wait on
// and this is a poll. Three quarters of a second against a local SQLite file is
// unmeasurable next to the ten-plus seconds a note takes to transcribe, and it
// buys the worker being a plain loop with no wake-up plumbing between the web
// handler, the WhatsApp adapter and the pipeline.
const idlePoll = 750 * time.Millisecond

// shutdownGrace bounds how long a graceful shutdown waits for in-flight
// requests. Long enough to finish serving a note's audio, short enough that
// Ctrl-C feels like Ctrl-C.
const shutdownGrace = 5 * time.Second

// workerGrace is how long stopping waits for a note already being transcribed.
//
// Sized against the measured 13-15s per note, so an ordinary Ctrl-C mid-note
// keeps the transcript instead of throwing it away and burning one of the
// note's two attempts. A note still running after this is stuck rather than
// busy, and waiting longer only makes the process feel hung.
const workerGrace = 25 * time.Second

func cmdServe(ctx context.Context, cfg config, addr string) error {
	log := slog.Default()
	// One locale for the whole process: the pages, the exports and the spoken
	// announcements all have to agree about what a status is called.
	loc := web.English

	st, pl, err := open(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	// A previous run may have died mid-transcription. Recover before accepting
	// anything new, or those notes sit in a transient state forever and the
	// interface shows them as permanently converting.
	requeued, failed, err := st.RequeueStale(ctx)
	if err != nil {
		return err
	}
	if requeued > 0 || failed > 0 {
		log.Info("recovered interrupted notes", "requeued", requeued, "exceeded_retries", failed)
	}

	broker := web.NewBroker()
	defer broker.Close()

	// The pipeline learns nothing about the web layer; it calls a function. This
	// is the same seam the WhatsApp adapter uses, and it is why transcription
	// keeps working when the interface is not running.
	pl.OnNote = func(noteID string) {
		publishNoteUpdate(ctx, st, broker, loc, noteID)
	}

	// Built up front, and unpaired startWhatsApp attaches to it in place rather
	// than returning a fresh one — a runtime pairing from the web button needs
	// the same handle the server already handed its stateFunc to, or a
	// successful pairing would light up nothing the page is looking at.
	waHandle := &whatsAppHandle{}
	if err := startWhatsApp(ctx, cfg, pl, broker, loc, log, waHandle); err != nil {
		// Not fatal. Imported files and stored transcripts do not need WhatsApp,
		// and a server that refuses to start because a phone is unreachable is
		// worse than one that starts and says so.
		log.Warn("WhatsApp is not available; serving stored notes only", "err", err)
	}
	defer waHandle.Close()

	pairing := &pairingController{
		ctx:    context.WithoutCancel(ctx),
		cfg:    cfg,
		pl:     pl,
		broker: broker,
		loc:    loc,
		log:    log,
		handle: waHandle,
	}

	srv, err := web.New(web.Options{
		Store:         st,
		Pipeline:      pl,
		Broker:        broker,
		Logger:        log,
		Locale:        &loc,
		WhatsAppState: waHandle.stateFunc(),
		CanPair:       waHandle.canPair,
		StartPairing:  pairing.Start,
	})
	if err != nil {
		return err
	}

	// The worker's context is deliberately NOT a child of the signal context.
	//
	// A child is cancelled synchronously with its parent, in the same call. So
	// deriving the worker from ctx meant that by the time this function noticed
	// the signal, the in-flight ffmpeg subprocess and the in-flight whisper
	// request had already been aborted — the note was killed by the signal
	// itself, not stopped in an orderly way afterwards. It came back on the
	// next launch as a retry, and a note on its second attempt was marked
	// permanently failed. An earlier version of this file carried a comment
	// claiming the opposite was true; the comment was aspirational and the code
	// did the thing it said it avoided.
	workerCtx, killWorker := context.WithCancel(context.WithoutCancel(ctx))
	defer killWorker()

	// Closing this asks the worker to stop between notes without interrupting
	// the one it is on. Cancelling workerCtx is the harsher fallback.
	stopWorker := make(chan struct{})
	var stopOnce sync.Once
	askWorkerToStop := func() { stopOnce.Do(func() { close(stopWorker) }) }
	defer askWorkerToStop()

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		runWorker(workerCtx, stopWorker, pl, log)
	}()

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout. The event stream is a long-lived response by
		// design, and a write deadline would sever it on a schedule.
		IdleTimeout: 120 * time.Second,
		// Request contexts DO follow the signal, so an open /events stream
		// unblocks on Ctrl-C instead of holding shutdown open for its lifetime.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	fmt.Printf("maktoob is serving on http://%s\n", listener.Addr())
	fmt.Println("Everything stays on this machine. Press Ctrl-C to stop.")

	serveErr := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		askWorkerToStop()
		<-workerDone
		return err

	case <-ctx.Done():
		fmt.Println("\nstopping…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()

		// Stop accepting first, so nothing new is queued while we wait.
		shutErr := httpSrv.Shutdown(shutdownCtx)

		// Then let the worker finish the note it is on, rather than killing a
		// transcription that may be a second from done. This only asks it to
		// stop looping; the note in flight runs to completion.
		askWorkerToStop()

		select {
		case <-workerDone:
		case <-time.After(workerGrace):
			// The note has outlasted its grace. killWorker fires from the
			// deferred call on the way out, which costs this note a retry —
			// the lesser evil against a process that will not exit.
			fmt.Println("a transcription is still running; it will resume on the next launch")
		}
		return shutErr
	}
}

// runWorker drains the transcription queue until asked to stop.
//
// stop and ctx are two different requests and the difference is the whole point.
// Closing stop means "finish the note you are on, then exit" — it is checked
// between notes and never interrupts one. Cancelling ctx means "drop everything
// now", which kills the ffmpeg subprocess and the in-flight whisper request and
// costs the note a retry. Shutdown uses the first and falls back to the second
// only if a note outlasts its grace period.
func runWorker(ctx context.Context, stop <-chan struct{}, pl transcriber, log *slog.Logger) {
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		default:
		}

		did, err := pl.ProcessNext(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			// Torn down mid-note. Not a fault worth reporting.
			return
		case err != nil:
			// ProcessNext already recorded a per-note failure against the row;
			// reaching here means the store itself refused, so backing off is
			// better than spinning on it.
			log.Error("transcription worker", "err", err)
			if sleep(ctx, stop, 2*time.Second) {
				return
			}
		case !did:
			if sleep(ctx, stop, idlePoll) {
				return
			}
		}
	}
}

// sleep waits for d, reporting whether it was asked to stop instead.
func sleep(ctx context.Context, stop <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stop:
		return true
	case <-ctx.Done():
		return true
	case <-timer.C:
		return false
	}
}

// publishNoteUpdate tells connected browsers a note changed.
//
// The status is read back from the store rather than assumed, because the
// caller only knows the note reached *a* terminal state, and "failed" and
// "transcribed" are not interchangeable to the person waiting for it.
//
// Text is a whole sentence taken from the configured locale, not a status word.
// It is spoken aloud by a screen reader, and "transcribed" on its own, with no
// subject, tells the listener nothing.
func publishNoteUpdate(ctx context.Context, st *store.Store, broker *web.Broker, loc web.Locale, noteID string) {
	// Detached from the caller's cancellation. The worker now outlives the
	// signal by design, so a note finishing during stopping must still be able
	// to read its own status back — otherwise the last note of every session
	// reports as an error it did not have.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	note, _, err := st.GetNote(ctx, noteID)
	if err != nil {
		broker.Publish(web.Event{Kind: web.KindUpdated, NoteID: noteID})
		return
	}
	broker.Publish(web.Event{
		Kind:   web.KindUpdated,
		NoteID: noteID,
		Status: note.Status,
		Text:   loc.Announcement(note.Status),
	})
}

// --- WhatsApp ------------------------------------------------------------

// whatsAppHandle owns the pieces that only exist when a device is linked.
//
// It is constructed empty and filled in later — either immediately, by
// startWhatsApp reading an existing session, or minutes into a running
// server, by a pairing attempt started from the web button. mu guards the
// fields a concurrent pairing attempt writes; state is a separate atomic
// because the SSE handler and every page render read it far more often than
// it changes.
type whatsAppHandle struct {
	mu       sync.Mutex
	listener *wa.Listener
	disconn  func()
	linked   atomic.Bool
	state    atomic.Value // string
}

// stateFunc returns a reporter for the interface badge. The badge is absent
// (an empty string) until a device is linked, rather than showing a permanent
// "disconnected", which would read as a fault on a machine that was never
// meant to be linked yet.
func (h *whatsAppHandle) stateFunc() func() string {
	return func() string {
		if !h.linked.Load() {
			return ""
		}
		s, _ := h.state.Load().(string)
		return s
	}
}

// canPair reports whether the "Link WhatsApp" button should be offered: no
// device linked yet.
func (h *whatsAppHandle) canPair() bool {
	return !h.linked.Load()
}

func (h *whatsAppHandle) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listener != nil {
		h.listener.Close()
	}
	if h.disconn != nil {
		h.disconn()
	}
}

// attach wires a freshly connected, linked client into h: the alias salt, the
// transcription sink, and the listener loop. Shared by the two paths that
// produce a linked client — reading an existing session at startup, and a
// pairing attempt finishing successfully — so the wiring can't drift between
// them.
func (h *whatsAppHandle) attach(
	ctx context.Context,
	client *whatsmeow.Client,
	cfg config,
	pl *pipeline.Pipeline,
	broker *web.Broker,
	loc web.Locale,
	log *slog.Logger,
) error {
	salt, err := wa.LoadSalt(cfg.saltPath())
	if err != nil {
		return err
	}

	h.state.Store(wa.Disconnected.String())

	// Announced before transcription starts. The arrival is the part the user is
	// waiting to be told about; the transcript follows on its own event some
	// thirteen seconds later. The id comes from the ingest rather than being
	// left empty, so a client can resolve the note it was just told about.
	sink := newWASink(pl, log, func(noteID string) {
		broker.Publish(web.Event{
			Kind:   web.KindArrived,
			NoteID: noteID,
			Status: store.StatusPending,
			Text:   loc.AnnounceArrived,
		})
	})

	listener := &wa.Listener{
		Client:  client,
		Sink:    sink,
		Salt:    salt,
		Log:     log,
		OnState: func(s wa.State) { h.state.Store(s.String()) },
	}

	if err := listener.Start(ctx); err != nil {
		return err
	}

	h.mu.Lock()
	h.listener = listener
	h.disconn = client.Disconnect
	h.mu.Unlock()
	h.linked.Store(true)

	return nil
}

// startWhatsApp fills h from an existing session, if one is on disk. It
// leaves h untouched — still unpaired, still offering the pairing button —
// when no session exists yet, because that is an ordinary state, not an
// error: the user may be running maktoob purely on imported files, and
// pairing is a separate, deliberate step.
func startWhatsApp(
	ctx context.Context,
	cfg config,
	pl *pipeline.Pipeline,
	broker *web.Broker,
	loc web.Locale,
	log *slog.Logger,
	h *whatsAppHandle,
) error {
	if _, err := os.Stat(cfg.sessionPath()); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no device is linked: run \"maktoob pair\" or use the web button")
	}

	client, err := wa.Connect(ctx, cfg.sessionPath(), log, cfg.verbose)
	if err != nil {
		return err
	}
	if client.Store.ID == nil {
		client.Disconnect()
		return wa.ErrNotPaired
	}

	if err := h.attach(ctx, client, cfg, pl, broker, loc, log); err != nil {
		client.Disconnect()
		return err
	}
	if err := client.Connect(); err != nil {
		h.Close()
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

// pairingController runs pairing attempts started from the web interface.
//
// ctx outlives any single HTTP request: a pairing attempt has to keep running
// (and keep the connected client alive) after the handler that started it has
// already returned 202 to the browser. It is the server's own lifetime
// context, detached from cancellation, not the request's.
type pairingController struct {
	ctx    context.Context
	cfg    config
	pl     *pipeline.Pipeline
	broker *web.Broker
	loc    web.Locale
	log    *slog.Logger
	handle *whatsAppHandle

	mu     sync.Mutex
	active bool
}

// Start begins a pairing attempt in the background and returns immediately.
// It refuses a second attempt while one is already running, and refuses to
// start at all once a device is linked — "logout" is the way to relink, so
// that action stays a deliberate, separate step rather than something the
// pairing button does implicitly.
func (p *pairingController) Start(_ context.Context) error {
	if p.handle.linked.Load() {
		return fmt.Errorf("a device is already linked; run: maktoob logout")
	}

	p.mu.Lock()
	if p.active {
		p.mu.Unlock()
		return fmt.Errorf("a pairing attempt is already in progress")
	}
	p.active = true
	p.mu.Unlock()

	go p.run()
	return nil
}

func (p *pairingController) run() {
	defer func() {
		p.mu.Lock()
		p.active = false
		p.mu.Unlock()
	}()

	client, err := wa.Connect(p.ctx, p.cfg.sessionPath(), p.log, p.cfg.verbose)
	if err != nil {
		p.fail(err)
		return
	}

	err = wa.PairCode(p.ctx, client, func(code string) error {
		png, err := qrPNG(code)
		if err != nil {
			return err
		}
		p.broker.Publish(web.Event{
			Kind: web.KindPairQR,
			QR:   "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		})
		return nil
	})
	if err != nil {
		client.Disconnect()
		p.fail(err)
		return
	}

	if err := p.handle.attach(p.ctx, client, p.cfg, p.pl, p.broker, p.loc, p.log); err != nil {
		client.Disconnect()
		p.fail(err)
		return
	}

	p.broker.Publish(web.Event{Kind: web.KindPairLinked, Text: "WhatsApp linked."})
}

func (p *pairingController) fail(err error) {
	p.log.Warn("pairing failed", "err", err)
	p.broker.Publish(web.Event{Kind: web.KindPairError, Text: err.Error()})
}

// qrPNG renders a QR payload as a PNG. Encoded at qr.M rather than the
// default: the code is displayed small, on a phone camera, across a room at a
// conference, and M's extra error-correction bytes are cheap insurance
// against exactly that.
func qrPNG(code string) ([]byte, error) {
	c, err := qr.Encode(code, qr.M)
	if err != nil {
		return nil, fmt.Errorf("encode QR: %w", err)
	}
	return c.PNG(), nil
}
