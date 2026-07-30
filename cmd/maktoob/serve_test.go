package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeTranscriber stands in for the pipeline so the worker's looping, backoff
// and shutdown behaviour can be tested without ffmpeg or a whisper server.
type fakeTranscriber struct {
	mu       sync.Mutex
	calls    int
	queue    int
	failWith error
}

func (f *fakeTranscriber) ProcessNext(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failWith != nil {
		return false, f.failWith
	}
	if f.queue > 0 {
		f.queue--
		return true, nil
	}
	return false, nil
}

func (f *fakeTranscriber) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestWorkerDrainsTheQueueThenIdles(t *testing.T) {
	pl := &fakeTranscriber{queue: 5}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		runWorker(ctx, nil, pl, quietLogger())
	}()

	// Five notes come back to back with no idle wait between them, so they are
	// drained well before a single poll interval elapses.
	deadline := time.Now().Add(5 * time.Second)
	for {
		pl.mu.Lock()
		remaining := pl.queue
		pl.mu.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker left %d notes in the queue", remaining)
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop when its context was cancelled")
	}
}

func TestWorkerStopsPromptlyOnCancel(t *testing.T) {
	pl := &fakeTranscriber{} // always empty, so the worker sits in the idle poll
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		runWorker(ctx, nil, pl, quietLogger())
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// A worker sleeping on a bare time.Sleep instead of a select would only
		// notice cancellation at the end of the interval, and Ctrl-C would feel
		// like a hang.
		t.Fatal("worker did not wake on cancellation")
	}
}

// A store-level failure must back off rather than spin. Without the backoff the
// loop burns a core until the user notices.
func TestWorkerBacksOffOnRepeatedFailure(t *testing.T) {
	pl := &fakeTranscriber{failWith: errors.New("database is locked")}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	runWorker(ctx, nil, pl, quietLogger())

	// With a two-second backoff, 300ms of failures is one or two attempts. A
	// spinning loop would be in the thousands.
	if got := pl.count(); got > 5 {
		t.Errorf("worker made %d attempts in 300ms; it is not backing off", got)
	}
	if pl.count() == 0 {
		t.Error("worker never called the pipeline at all")
	}
}

func TestSleepReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !sleep(ctx, nil, time.Hour) {
		t.Error("sleep on a cancelled context did not report cancellation")
	}

	if sleep(context.Background(), nil, time.Millisecond) {
		t.Error("sleep that ran to completion reported cancellation")
	}
}

// --- the WhatsApp badge --------------------------------------------------

// A nil handle means WhatsApp was never started. The badge must then be absent
// rather than reading "disconnected", which on a machine that was never meant
// to be linked looks like a fault the user needs to fix.
func TestAbsentWhatsAppReportsNoState(t *testing.T) {
	var h *whatsAppHandle
	if h.stateFunc() != nil {
		t.Error("a nil handle offered a state reporter")
	}
	// And Close on a nil handle must not panic, because the deferred Close in
	// cmdServe runs whether or not startWhatsApp succeeded.
	h.Close()
}

func TestWhatsAppStateIsReportedAsItChanges(t *testing.T) {
	h := &whatsAppHandle{}
	h.state.Store("disconnected")

	report := h.stateFunc()
	if report == nil {
		t.Fatal("a live handle offered no state reporter")
	}
	if got := report(); got != "disconnected" {
		t.Errorf("state = %q, want disconnected", got)
	}

	h.state.Store("connected")
	if got := report(); got != "connected" {
		t.Errorf("state = %q after reconnect, want connected", got)
	}
}

func TestWhatsAppStateIsSafeUnderConcurrentReads(t *testing.T) {
	h := &whatsAppHandle{}
	h.state.Store("disconnected")
	report := h.stateFunc()

	var wg sync.WaitGroup
	var reads atomic.Int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = report()
				reads.Add(1)
			}
		}()
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.state.Store("connected")
			}
		}()
	}
	wg.Wait()

	if reads.Load() != 800 {
		t.Errorf("read %d times, want 800", reads.Load())
	}
}

// --- serve wiring --------------------------------------------------------

func TestServeRefusesAnUnusableAddress(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := cmdServe(ctx, cfg, "127.0.0.1:-1")
	if err == nil {
		t.Fatal("serve accepted an invalid address")
	}
}

// freePort asks the kernel for an unused port and gives it straight back.
//
// There is a race between releasing it and cmdServe claiming it, but the
// alternative is a fixed port that collides with whatever the developer already
// has running, which fails far more often than this ever will.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return addr
}

// This is the only test that exercises the real process: a real listener, the
// real worker goroutine, the templates parsed at startup and the assets served
// out of the embedded filesystem. Everything else drives the handler directly,
// which would not catch a binary that builds and then fails to boot.
func TestServeAnswersOnItsListenerAndShutsDownCleanly(t *testing.T) {
	cfg := testConfig(t)
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- cmdServe(ctx, cfg, addr) }()

	base := "http://" + addr
	client := &http.Client{Timeout: 3 * time.Second}

	var res *http.Response
	deadline := time.Now().Add(15 * time.Second)
	for {
		var err error
		res, err = client.Get(base + "/")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve never answered on %s: %v", addr, err)
		}
		select {
		case err := <-done:
			t.Fatalf("serve exited before answering: %v", err)
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}

	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET / = %d, want 200", res.StatusCode)
	}
	if !strings.Contains(string(body), "No voice notes yet") {
		t.Errorf("GET / did not render the empty state:\n%s", body)
	}

	// The stylesheet comes out of the embedded filesystem. A go:embed pattern
	// that stopped matching would only show up here or in a browser.
	css, err := client.Get(base + "/static/app.css")
	if err != nil {
		t.Fatalf("GET /static/app.css: %v", err)
	}
	cssBody, _ := io.ReadAll(css.Body)
	css.Body.Close()
	if css.StatusCode != http.StatusOK || len(cssBody) == 0 {
		t.Errorf("stylesheet = %d with %d bytes, want 200 and content",
			css.StatusCode, len(cssBody))
	}

	api, err := client.Get(base + "/api/notes")
	if err != nil {
		t.Fatalf("GET /api/notes: %v", err)
	}
	api.Body.Close()
	if api.StatusCode != http.StatusOK {
		t.Errorf("GET /api/notes = %d, want 200", api.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve returned %v, want a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not shut down when its context was cancelled")
	}
}
