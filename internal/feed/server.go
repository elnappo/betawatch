package feed

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

//go:embed index.html favicon.svg
var static embed.FS

// Handler serves the review page, its history and its SSE stream.
func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", http.FileServerFS(static))
	mux.Handle("GET /favicon.svg", http.FileServerFS(static))
	mux.HandleFunc("GET /robots.txt", serveRobots)
	mux.HandleFunc("GET /api/changes", b.serveHistory)
	mux.HandleFunc("GET /events", b.serveEvents)
	return mux
}

// serveRobots keeps crawlers out. The page is a live view of data that
// is already public on openstreetmap.org, so there is nothing here worth
// indexing, and /events is a stream that never ends.
func serveRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "User-agent: *\nDisallow: /\n")
}

// serveHistory returns the changes held in memory, newest first, with
// the id of the newest event. The page opens its stream from that id, so
// a change arriving between the two requests is not missed.
func (b *Broker) serveHistory(w http.ResponseWriter, r *http.Request) {
	events := b.History()

	// json.RawMessage keeps each change exactly as published, so this
	// package never has to know the shape of a change.
	changes := make([]json.RawMessage, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- {
		changes = append(changes, json.RawMessage(events[i].Data))
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(struct {
		LastEventID int64             `json:"last_event_id"`
		Changes     []json.RawMessage `json:"changes"`
	}{
		LastEventID: b.LastID(),
		Changes:     changes,
	})
}

// serveEvents streams changes to one browser as server-sent events.
func (b *Broker) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Without this an intermediate proxy may buffer the whole stream.
	h.Set("X-Accel-Buffering", "no")

	// The browser resends the last id it saw when it reconnects, so a
	// dropped connection does not lose the changes sent meanwhile.
	after := lastEventID(r)

	backlog, events, cancel := b.Subscribe(after)
	defer cancel()

	for _, ev := range backlog {
		if err := writeEvent(w, ev); err != nil {
			return
		}
	}
	flusher.Flush()

	// Keep the connection alive through proxies that time out idle
	// requests. Most minutes carry no climbing change at all.
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return // dropped for being too slow; the browser reconnects
			}
			if err := writeEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// lastEventID reads the id the browser last saw, from the standard
// header or from the query string used on the first connection.
func lastEventID(r *http.Request) int64 {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		raw = r.URL.Query().Get("lastEventId")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// writeEvent writes one event in SSE wire format. Data is single-line
// JSON, so it needs no splitting across data: lines.
func writeEvent(w http.ResponseWriter, ev Event) error {
	_, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.ID, ev.Data)
	return err
}
