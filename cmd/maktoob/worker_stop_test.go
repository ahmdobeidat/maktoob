package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// blockingTranscriber holds the first note until released, and records whether
// its context was cancelled while it was working.
type blockingTranscriber struct {
	started chan struct{}
	release chan struct{}

	mu     sync.Mutex
	ctxErr error
	calls  int
}

func (b *blockingTranscriber) ProcessNext(ctx context.Context) (bool, error) {
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	b.mu.Unlock()

	if !first {
		return false, nil
	}

	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
	}

	b.mu.Lock()
	b.ctxErr = ctx.Err()
	b.mu.Unlock()
	return true, nil
}

func (b *blockingTranscriber) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctxErr
}

// Asking the worker to stop must not cancel the note it is already
// transcribing.
//
// This is the distinction the whole stopping path is built on. An earlier
// version derived the worker's context from the signal context, and a context
// derived that way is cancelled synchronously with its parent — so Ctrl-C
// killed the in-flight ffmpeg and whisper calls the instant it was pressed. A
// note fourteen seconds into a fifteen-second transcription was thrown away and
// charged one of its two attempts; the second time it happened the note was
// marked permanently failed. The comment above that code claimed the opposite
// was happening, which is why this test asserts the context rather than the
// timing.
func TestStoppingLetsTheNoteInFlightFinish(t *testing.T) {
	pl := &blockingTranscriber{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}

	ctx, kill := context.WithCancel(context.Background())
	defer kill()
	stop := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		runWorker(ctx, stop, pl, quietLogger())
	}()

	select {
	case <-pl.started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started a note")
	}

	// The stop request arrives mid-note, exactly as a signal would.
	close(stop)

	// The worker must still be inside that note rather than having abandoned it.
	select {
	case <-done:
		t.Fatal("worker exited while a note was still being transcribed")
	case <-time.After(200 * time.Millisecond):
	}

	close(pl.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not exit after finishing its note")
	}

	if err := pl.err(); err != nil {
		t.Errorf("the note's context was cancelled while it was working: %v", err)
	}
}

// The harsher fallback still has to work: cancelling the context, rather than
// closing stop, interrupts the note. Stopping relies on this when a note
// outlasts its grace period and the process would otherwise never exit.
func TestCancellingTheContextInterruptsTheNoteInFlight(t *testing.T) {
	pl := &blockingTranscriber{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}

	ctx, kill := context.WithCancel(context.Background())
	stop := make(chan struct{})
	defer close(stop)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runWorker(ctx, stop, pl, quietLogger())
	}()

	select {
	case <-pl.started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started a note")
	}

	kill()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not interrupt the note")
	}

	if err := pl.err(); err == nil {
		t.Error("the note reported no cancellation after its context was cancelled")
	}
}
