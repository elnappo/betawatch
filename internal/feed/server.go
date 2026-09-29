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

// HistoryFetcher returns one page of past changes, newest first, each
// already encoded as the JSON the browser expects. before is "" for the
// newest page, or a page's OldestCursor to keep paging into the past.
// more reports whether older changes remain beyond the page.
type HistoryFetcher func(before string, limit int) (changes []json.RawMessage, oldestCursor string, more bool, err error)

// PreviousVersion is the version just before the one being viewed, for the
// inline tag/coordinate diff shown when a row expands. Found is false when
// there is nothing to diff against: a create (version 1) or a gap in
// stored history look the same from here, so both just show no diff.
type PreviousVersion struct {
	Found bool              `json:"found"`
	Tags  map[string]string `json:"tags,omitempty"`
	Lat   *float64          `json:"lat,omitempty"`
	Lon   *float64          `json:"lon,omitempty"`
}

// DiffFetcher looks up the version just before the given one.
type DiffFetcher func(typ string, id int64, version int) (*PreviousVersion, error)

// NewHandler serves the review page, its history, its SSE stream and its
// per-element diffs. fetch supplies history pages, diff supplies the
// previous-version lookups, and the broker supplies the live stream.
func NewHandler(b *Broker, fetch HistoryFetcher, diff DiffFetcher) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", http.FileServerFS(static))
	mux.Handle("GET /favicon.svg", http.FileServerFS(static))
	mux.HandleFunc("GET /robots.txt", serveRobots)
	mux.HandleFunc("GET /api/changes", serveHistory(fetch))
	mux.HandleFunc("GET /api/diff", serveDiff(diff))
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

const (
	defaultPageSize = 50
	maxPageSize     = 1000
)

// serveHistory returns one page of changes from the database. "limit"
// sets the page size and "before" is the oldest_cursor of the previous
// page, so the page can keep scrolling into the past.
func serveHistory(fetch HistoryFetcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 {
			limit = defaultPageSize
		}
		limit = min(limit, maxPageSize)
		before := q.Get("before")

		changes, oldest, more, err := fetch(before, limit)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if changes == nil {
			changes = []json.RawMessage{}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(struct {
			OldestCursor string            `json:"oldest_cursor,omitempty"`
			More         bool              `json:"more"`
			Changes      []json.RawMessage `json:"changes"`
		}{
			OldestCursor: oldest,
			More:         more,
			Changes:      changes,
		})
	}
}

// serveDiff returns the version just before the one the browser is asking
// about, for the inline tag/coordinate diff shown when a row expands.
func serveDiff(diff DiffFetcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		typ := q.Get("type")
		id, idErr := strconv.ParseInt(q.Get("id"), 10, 64)
		version, versionErr := strconv.Atoi(q.Get("version"))
		if typ == "" || idErr != nil || versionErr != nil {
			http.Error(w, "type, id and version are required", http.StatusBadRequest)
			return
		}

		prev, err := diff(typ, id, version)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if prev == nil {
			prev = &PreviousVersion{}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(prev)
	}
}

// serveEvents streams changes to one browser as server-sent events. The
// stream is live only: a browser that missed changes while disconnected
// picks them up by re-fetching /api/changes.
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

	events, cancel := b.Subscribe()
	defer cancel()

	// Send the headers immediately: without a flush here, nothing reaches
	// the client until the first event or ping, and a client waiting on
	// the response (rather than just the body) blocks needlessly.
	w.WriteHeader(http.StatusOK)
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

// writeEvent writes one event in SSE wire format. Data is single-line
// JSON, so it needs no splitting across data: lines.
func writeEvent(w http.ResponseWriter, ev Event) error {
	_, err := fmt.Fprintf(w, "data: %s\n\n", ev.Data)
	return err
}
