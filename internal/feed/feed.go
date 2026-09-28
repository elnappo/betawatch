// Package feed fans out changes to connected browsers.
package feed

import "sync"

// Event is one change, already encoded as JSON.
type Event struct {
	ID   int64
	Data []byte
}

// Broker keeps the recent events and delivers new ones to subscribers.
// It is safe for concurrent use.
type Broker struct {
	mu      sync.Mutex
	nextID  int64
	keep    int
	history []Event
	subs    map[chan Event]struct{}
}

// New returns a broker remembering the last keep events, so a browser
// that connects late, or reconnects, still sees them.
func New(keep int) *Broker {
	return &Broker{
		keep: keep,
		subs: make(map[chan Event]struct{}),
	}
}

// Publish records an event and sends it to every subscriber.
func (b *Broker) Publish(data []byte) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	ev := Event{ID: b.nextID, Data: data}

	b.history = append(b.history, ev)
	if len(b.history) > b.keep {
		b.history = b.history[len(b.history)-b.keep:]
	}

	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// The subscriber is not keeping up. Drop it rather than
			// block everyone else; the browser reconnects and resumes
			// from its last id.
			close(ch)
			delete(b.subs, ch)
		}
	}
	return ev
}

// Subscribe returns the events after id, plus a channel carrying later
// ones. Pass 0 for id to get the whole history. Call cancel to stop.
func (b *Broker) Subscribe(after int64) (backlog []Event, ch <-chan Event, cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, ev := range b.history {
		if ev.ID > after {
			backlog = append(backlog, ev)
		}
	}

	// Buffered so a brief stall does not immediately drop the client.
	c := make(chan Event, 64)
	b.subs[c] = struct{}{}

	return backlog, c, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[c]; ok {
			close(c)
			delete(b.subs, c)
		}
	}
}

// History returns the events held in memory, oldest first.
func (b *Broker) History() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Event(nil), b.history...)
}

// Load seeds the history, for restoring it from disk at startup. Events
// are numbered in the order given, and it must be called before any
// Publish.
func (b *Broker) Load(lines [][]byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, line := range lines {
		b.nextID++
		b.history = append(b.history, Event{ID: b.nextID, Data: line})
	}
	if len(b.history) > b.keep {
		b.history = b.history[len(b.history)-b.keep:]
	}
}

// LastID returns the id of the most recent event.
func (b *Broker) LastID() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextID
}
