package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBrokerDeliversToEverySubscriber(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	a, cancelA := b.Subscribe()
	defer cancelA()
	c, cancelC := b.Subscribe()
	defer cancelC()

	if got := b.Subscribers(); got != 2 {
		t.Fatalf("Subscribers = %d, want 2", got)
	}

	b.Publish(Event{Kind: KindArrived, NoteID: "n1"})

	for i, ch := range []<-chan Event{a, c} {
		select {
		case e := <-ch:
			if e.NoteID != "n1" {
				t.Errorf("subscriber %d got note %q, want n1", i, e.NoteID)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d received nothing", i)
		}
	}
}

// The pipeline calls Publish from the transcription worker. A browser tab that
// stopped reading must not be able to wedge transcription, so a full buffer
// drops the event rather than waiting for room.
func TestPublishNeverBlocksOnAStalledSubscriber(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	_, cancel := b.Subscribe() // never read from
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberBuffer*10; i++ {
			b.Publish(Event{Kind: KindUpdated, NoteID: "n1"})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading")
	}
}

// A live subscriber must keep receiving even while another one is stalled.
func TestOneStalledSubscriberDoesNotStarveTheOthers(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	_, cancelStalled := b.Subscribe()
	defer cancelStalled()
	live, cancelLive := b.Subscribe()
	defer cancelLive()

	for i := 0; i < subscriberBuffer*3; i++ {
		b.Publish(Event{Kind: KindUpdated, NoteID: "n1"})
		select {
		case <-live:
		case <-time.After(time.Second):
			t.Fatalf("live subscriber stopped receiving at event %d", i)
		}
	}
}

func TestUnsubscribeIsIdempotent(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	ch, cancel := b.Subscribe()
	cancel()
	// A double cancel must not panic on a closed channel, and handlers with a
	// deferred cancel plus an early return do exactly that.
	cancel()

	if got := b.Subscribers(); got != 0 {
		t.Errorf("Subscribers = %d after cancel, want 0", got)
	}
	if _, open := <-ch; open {
		t.Error("cancelled subscriber channel is still open")
	}

	// Publishing to nobody must be harmless.
	b.Publish(Event{Kind: KindArrived, NoteID: "n1"})
}

func TestCloseReleasesSubscribersAndLaterSubscribesFail(t *testing.T) {
	b := NewBroker()

	ch, cancel := b.Subscribe()
	defer cancel()

	b.Close()
	b.Close() // must be safe twice

	if _, open := <-ch; open {
		t.Error("subscriber channel survived Close")
	}

	// A handler that subscribes while the process is shutting down needs a
	// closed channel, not one that never delivers, or its read loop hangs.
	after, cancelAfter := b.Subscribe()
	defer cancelAfter()
	select {
	case _, open := <-after:
		if open {
			t.Error("Subscribe after Close returned a live channel")
		}
	case <-time.After(time.Second):
		t.Error("Subscribe after Close returned a channel that hangs")
	}
}

func TestBrokerIsSafeUnderConcurrency(t *testing.T) {
	b := NewBroker()
	defer b.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe()
			defer cancel()
			for j := 0; j < 20; j++ {
				b.Publish(Event{Kind: KindUpdated, NoteID: "n"})
				select {
				case <-ch:
				default:
				}
			}
		}()
	}
	wg.Wait()
}

// --- the stream itself ---------------------------------------------------

func TestEventStreamSendsPublishedEvents(t *testing.T) {
	f := newFixture(t)

	server := httptest.NewServer(f.srv)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/events", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer res.Body.Close()

	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}

	// Wait for the handler to actually register before publishing, otherwise
	// the event is sent into an empty broker and the test races.
	deadline := time.Now().Add(5 * time.Second)
	for f.srv.Broker().Subscribers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("handler never subscribed to the broker")
		}
		time.Sleep(5 * time.Millisecond)
	}

	f.srv.Broker().Publish(Event{
		Kind: KindArrived, NoteID: "note-1", Status: "pending", Text: "queued",
	})

	scanner := bufio.NewScanner(res.Body)
	var sawEvent, sawData bool
	for scanner.Scan() {
		line := scanner.Text()
		if line == "event: "+KindArrived {
			sawEvent = true
		}
		if strings.HasPrefix(line, "data: ") {
			if !strings.Contains(line, `"note_id":"note-1"`) {
				t.Errorf("data frame = %q, want the published note id", line)
			}
			sawData = true
			break
		}
	}
	if !sawEvent || !sawData {
		t.Errorf("stream did not deliver the event (event=%v data=%v): %v",
			sawEvent, sawData, scanner.Err())
	}
}

func TestEventStreamStopsWhenTheClientGoesAway(t *testing.T) {
	f := newFixture(t)

	server := httptest.NewServer(f.srv)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for f.srv.Broker().Subscribers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("handler never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	res.Body.Close()

	// A handler that ignored request cancellation would hold its subscription
	// forever, and every reloaded tab would leak one.
	deadline = time.Now().Add(5 * time.Second)
	for f.srv.Broker().Subscribers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscription outlived the client that opened it")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
