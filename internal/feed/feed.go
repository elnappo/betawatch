// Package feed fans out changes to connected browsers.
package feed

import "sync"

// Event is one change, already encoded as JSON.
type Event struct {
	ID   int64
	Data []byte
}

// Broker delivers new events to subscribers as they are published. The
// history itself lives in SQLite, so the broker holds nothing but the
// live subscriber set. It is safe for concurrent use.
type Broker struct {
	mu     sync.Mutex
	nextID int64
	subs   map[chan Event]struct{}
}

// New returns an empty broker.
func New() *Broker {
	return &Broker{
		subs: make(map[chan Event]struct{}),
	}
}

// Publish sends an event to every subscriber.
func (b *Broker) Publish(data []byte) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	ev := Event{ID: b.nextID, Data: data}

	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// The subscriber is not keeping up. Drop it rather than
			// block everyone else; the browser reconnects.
			close(ch)
			delete(b.subs, ch)
		}
	}
	return ev
}

// Subscribe returns a channel carrying events published from this point
// on. Call cancel to stop.
func (b *Broker) Subscribe() (ch <-chan Event, cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Buffered so a brief stall does not immediately drop the client.
	c := make(chan Event, 64)
	b.subs[c] = struct{}{}

	return c, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[c]; ok {
			close(c)
			delete(b.subs, c)
		}
	}
}
