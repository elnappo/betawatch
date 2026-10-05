package feed

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

//go:embed index.html problems.html map.html favicon.svg
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

// ProblemsFetcher runs every configured rule and returns the merged,
// already-JSON-encoded live elements, each annotated with which rule(s)
// it broke.
type ProblemsFetcher func() ([]json.RawMessage, error)

// NewHandler serves the review page, its history, its SSE stream, its
// per-element diffs, the problems page and the live map. fetch supplies history pages,
// diff supplies the previous-version lookups, problems supplies the
// problems page's data, and the broker supplies the live stream.
func NewHandler(b *Broker, fetch HistoryFetcher, diff DiffFetcher, problems ProblemsFetcher) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /{$}", http.FileServerFS(static))
	mux.Handle("GET /problems.html", http.FileServerFS(static))
	mux.Handle("GET /map.html", http.FileServerFS(static))
	mux.Handle("GET /favicon.svg", http.FileServerFS(static))
	mux.HandleFunc("GET /robots.txt", serveRobots)
	mux.HandleFunc("GET /api/changes", serveHistory(fetch))
	mux.HandleFunc("GET /api/diff", serveDiff(diff))
	mux.HandleFunc("GET /api/problems", serveProblems(problems))
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

// serveProblems returns every live element currently flagged by a
// configured rule.
func serveProblems(fetch ProblemsFetcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		problems, err := fetch()
		if err != nil {
			// fetch's error names the offending rule and its query (see
			// cmd/feed's fetchProblems), which is operator-authored SQL:
			// worth the operator's console, not the browser's.
			fmt.Fprintf(os.Stderr, "problems: %v\n", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if problems == nil {
			problems = []json.RawMessage{}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(problems)
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
