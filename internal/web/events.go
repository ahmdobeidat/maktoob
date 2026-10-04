package web

import (
	"sync"
)

// Event is one server-sent update.
//
// It carries an id and a kind rather than rendered HTML, so the client decides
// what to do with it. A note that arrives while the user is reading a different
// note should not rewrite the page under them.
type Event struct {
	Kind   string `json:"kind"` // "arrived" | "updated" | "pair_qr" | "pair_linked" | "pair_error"
	NoteID string `json:"note_id"`
	Status string `json:"status,omitempty"`
	// Text is a short human sentence for the aria-live region. Screen reader
	// users get the same notification sighted users get from the page changing,
	// which is the whole reason the region exists.
	Text string `json:"text,omitempty"`
	// QR is a data: URI PNG, set only on a pair_qr event. It travels as a data
	// URI rather than a separate image route because a QR code is single-use
	// and short-lived — a route would need its own cache-busting and cleanup
	// for an image nothing ever requests twice.
	QR string `json:"qr,omitempty"`
}

const (
	// KindArrived means a note was accepted and is queued.
	KindArrived = "arrived"
	// KindUpdated means a note reached a new state, usually a finished transcript.
	KindUpdated = "updated"
	// KindPairQR carries a fresh QR code to render while pairing is in progress.
	// whatsmeow refreshes the code periodically, so more than one of these can
	// arrive during a single pairing attempt.
	KindPairQR = "pair_qr"
	// KindPairLinked means pairing succeeded and WhatsApp is now connected.
	KindPairLinked = "pair_linked"
	// KindPairError means pairing failed or timed out; Text carries why.
	KindPairError = "pair_error"
)

// subscriberBuffer is how many events a slow client may fall behind before it
// starts losing them. Small on purpose: this is a localhost interface with one
// user, so a client this far behind is wedged, not busy.
const subscriberBuffer = 16

// Broker fans events out to connected browsers.
//
// Publish never blocks and never fails. It is called from the transcription
// worker, and a browser tab that stopped reading must not be able to stall
// transcription — losing a UI refresh is recoverable, wedging the pipeline is
// not. Dropped events cost a stale row until the next reload, and the client
// re-fetches on reconnect anyway.
type Broker struct {
	mu     sync.Mutex
	subs   map[chan Event]struct{}
	closed bool
}

// NewBroker returns a broker with no subscribers.
func NewBroker() *Broker {
	return &Broker{subs: make(map[chan Event]struct{})}
}

// Subscribe registers a listener and returns it with a function that removes
// it. The cancel function is idempotent and must be called, or the subscriber
// leaks for the lifetime of the process.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if _, ok := b.subs[ch]; ok {
				delete(b.subs, ch)
				close(ch)
			}
		})
	}
}

// Publish delivers an event to every current subscriber, dropping it for any
// subscriber whose buffer is full.
func (b *Broker) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			// Dropped deliberately. See the type comment.
		}
	}
}

// Close releases every subscriber. Subsequent Subscribe calls return a closed
// channel, so a handler racing shutdown exits its read loop instead of hanging.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}

// Subscribers reports how many listeners are attached. Used by tests and by
// the status endpoint; not part of the request path.
func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
