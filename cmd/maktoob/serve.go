package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

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

func cmdServe(ctx context.Context, cfg config, addr string) error {
	log := slog.Default()

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
		publishNoteUpdate(ctx, st, broker, noteID)
	}

	waHandle, err := startWhatsApp(ctx, cfg, pl, broker, log)
	if err != nil {
		// Not fatal. Imported files and stored transcripts do not need WhatsApp,
		// and a server that refuses to start because a phone is unreachable is
		// worse than one that starts and says so.
		log.Warn("WhatsApp is not available; serving stored notes only", "err", err)
	}
	if waHandle != nil {
		defer waHandle.Close()
	}

	srv, err := web.New(web.Options{
		Store:         st,
		Pipeline:      pl,
		Broker:        broker,
		Logger:        log,
		WhatsAppState: waHandle.stateFunc(),
	})
	if err != nil {
		return err
	}

	// Bound the worker and the HTTP server to the same lifetime, so that either
	// one stopping brings the process down rather than leaving half of it up.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		runWorker(runCtx, pl, log)
	}()

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout. The event stream is a long-lived response by
		// design, and a write deadline would sever it on a schedule.
		IdleTimeout: 120 * time.Second,
		BaseContext: func(net.Listener) context.Context { return runCtx },
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
		stop()
		<-workerDone
		return err

	case <-ctx.Done():
		fmt.Println("\nstopping…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()

		// Shut the server first so no new work arrives, then let the worker
		// finish the note it is on. Cancelling the worker first would abandon a
		// transcription that was seconds from done, and the note would come back
		// as a retry on next launch.
		shutErr := httpSrv.Shutdown(shutdownCtx)
		stop()
		<-workerDone
		return shutErr
	}
}

// runWorker drains the transcription queue until the context is cancelled.
func runWorker(ctx context.Context, pl transcriber, log *slog.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}

		did, err := pl.ProcessNext(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			// Shutting down mid-note. Not a fault worth reporting.
			return
		case err != nil:
			// ProcessNext already recorded a per-note failure against the row;
			// reaching here means the store itself refused, so backing off is
			// better than spinning on it.
			log.Error("transcription worker", "err", err)
			if sleep(ctx, 2*time.Second) {
				return
			}
		case !did:
			if sleep(ctx, idlePoll) {
				return
			}
		}
	}
}

// sleep waits for d or until ctx is done, reporting whether it was cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
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
func publishNoteUpdate(ctx context.Context, st *store.Store, broker *web.Broker, noteID string) {
	note, _, err := st.GetNote(ctx, noteID)
	if err != nil {
		broker.Publish(web.Event{Kind: web.KindUpdated, NoteID: noteID})
		return
	}
	broker.Publish(web.Event{
		Kind:   web.KindUpdated,
		NoteID: noteID,
		Status: note.Status,
		Text:   web.English.StatusLabel(note.Status),
	})
}

// --- WhatsApp ------------------------------------------------------------

// whatsAppHandle owns the pieces that only exist when a device is linked.
type whatsAppHandle struct {
	listener *wa.Listener
	state    atomic.Value // string
	disconn  func()
}

// stateFunc returns a reporter for the interface badge, or nil when WhatsApp is
// not running at all. Nil is meaningful: the badge is then absent rather than
// showing a permanent "disconnected", which would read as a fault on a machine
// that was never meant to be linked.
func (h *whatsAppHandle) stateFunc() func() string {
	if h == nil {
		return nil
	}
	return func() string {
		s, _ := h.state.Load().(string)
		return s
	}
}

func (h *whatsAppHandle) Close() {
	if h == nil {
		return
	}
	if h.listener != nil {
		h.listener.Close()
	}
	if h.disconn != nil {
		h.disconn()
	}
}

// startWhatsApp links the listener to the pipeline when a session exists.
//
// An unpaired install is an ordinary state, not an error: the user may be
// running maktoob purely on imported files, and pairing is a separate,
// deliberate step.
func startWhatsApp(
	ctx context.Context,
	cfg config,
	pl *pipeline.Pipeline,
	broker *web.Broker,
	log *slog.Logger,
) (*whatsAppHandle, error) {
	if _, err := os.Stat(cfg.sessionPath()); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no device is linked: run `maktoob pair` first")
	}

	client, err := wa.Connect(ctx, cfg.sessionPath(), log, cfg.verbose)
	if err != nil {
		return nil, err
	}
	if client.Store.ID == nil {
		client.Disconnect()
		return nil, wa.ErrNotPaired
	}

	salt, err := wa.LoadSalt(cfg.saltPath())
	if err != nil {
		client.Disconnect()
		return nil, err
	}

	h := &whatsAppHandle{disconn: client.Disconnect}
	h.state.Store(wa.Disconnected.String())

	sink := newWASink(pl, log)
	h.listener = &wa.Listener{
		Client: client,
		Sink: wa.SinkFunc(func(ctx context.Context, n wa.VoiceNote) error {
			if err := sink(ctx, n); err != nil {
				return err
			}
			// Announced before transcription starts. The arrival is the part the
			// user is waiting to be told about; the transcript follows on its own
			// event thirteen seconds later.
			broker.Publish(web.Event{
				Kind:   web.KindArrived,
				Status: store.StatusPending,
				Text:   web.English.StatusPending,
			})
			return nil
		}),
		Salt:    salt,
		Log:     log,
		OnState: func(s wa.State) { h.state.Store(s.String()) },
	}

	if err := h.listener.Start(ctx); err != nil {
		client.Disconnect()
		return nil, err
	}
	if err := client.Connect(); err != nil {
		h.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}

	return h, nil
}
