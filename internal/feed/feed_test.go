package feed

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// noHistory is a HistoryFetcher for tests that do not exercise /api/changes.
func noHistory(before string, limit int) ([]json.RawMessage, string, bool, error) {
	return nil, "", false, nil
}

// noDiff is a DiffFetcher for tests that do not exercise /api/diff.
func noDiff(typ string, id int64, version int) (*PreviousVersion, error) {
	return nil, nil
}

func TestPublishReachesSubscriber(t *testing.T) {
	b := New()
	events, cancel := b.Subscribe()
	defer cancel()

	b.Publish([]byte(`{"a":1}`))

	select {
	case ev := <-events:
		if string(ev.Data) != `{"a":1}` {
			t.Errorf("got %q", ev.Data)
		}
		if ev.ID != 1 {
			t.Errorf("id = %d, want 1", ev.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}

func TestSubscribeOnlySeesLaterEvents(t *testing.T) {
	b := New()
	b.Publish([]byte("one"))

	events, cancel := b.Subscribe()
	defer cancel()
	b.Publish([]byte("two"))

	select {
	case ev := <-events:
		if string(ev.Data) != "two" {
			t.Errorf("got %q, want \"two\"", ev.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}

func TestCancelUnsubscribes(t *testing.T) {
	b := New()
	events, cancel := b.Subscribe()
	cancel()

	if _, open := <-events; open {
		t.Error("channel still open after cancel")
	}
	// A publish after cancel must not panic on the closed channel.
	b.Publish([]byte("after"))
}

func TestCancelIsIdempotent(t *testing.T) {
	b := New()
	_, cancel := b.Subscribe()
	cancel()
	cancel() // must not panic by closing twice
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	b := New()
	events, cancel := b.Subscribe()
	defer cancel()

	// Overrun the 64-slot buffer without reading.
	for range 200 {
		b.Publish([]byte("x"))
	}
	// Drain: the channel is closed once it was dropped.
	for range events {
	}
}

func TestServeEventsStreamsSSE(t *testing.T) {
	b := New()
	srv := httptest.NewServer(NewHandler(b, noHistory, noDiff))
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	r := bufio.NewReader(resp.Body)
	// Give the subscription time to register before publishing.
	time.Sleep(50 * time.Millisecond)
	b.Publish([]byte(`{"id":1}`))

	data, _ := r.ReadString('\n')
	if strings.TrimSpace(data) != `data: {"id":1}` {
		t.Errorf("line = %q", strings.TrimSpace(data))
	}
}

func TestServeHistoryReturnsFetchedChanges(t *testing.T) {
	fetch := func(before string, limit int) ([]json.RawMessage, string, bool, error) {
		if before != "" {
			t.Errorf("before = %q, want empty", before)
		}
		if limit != defaultPageSize {
			t.Errorf("limit = %d, want %d", limit, defaultPageSize)
		}
		return []json.RawMessage{json.RawMessage(`{"id":1}`)}, "cursor-1", true, nil
	}

	srv := httptest.NewServer(NewHandler(New(), fetch, noDiff))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/changes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body struct {
		OldestCursor string            `json:"oldest_cursor"`
		More         bool              `json:"more"`
		Changes      []json.RawMessage `json:"changes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Changes) != 1 || string(body.Changes[0]) != `{"id":1}` {
		t.Errorf("changes = %v", body.Changes)
	}
	if body.OldestCursor != "cursor-1" || !body.More {
		t.Errorf("oldest_cursor = %q, more = %v", body.OldestCursor, body.More)
	}
}

func TestServeHistoryPassesBeforeAndLimit(t *testing.T) {
	fetch := func(before string, limit int) ([]json.RawMessage, string, bool, error) {
		if before != "cursor-1" {
			t.Errorf("before = %q, want cursor-1", before)
		}
		if limit != 5 {
			t.Errorf("limit = %d, want 5", limit)
		}
		return nil, "", false, nil
	}

	srv := httptest.NewServer(NewHandler(New(), fetch, noDiff))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/changes?before=cursor-1&limit=5")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestServeDiffReturnsPreviousVersion(t *testing.T) {
	lat, lon := 47.5, 11.2
	diff := func(typ string, id int64, version int) (*PreviousVersion, error) {
		if typ != "way" || id != 42 || version != 3 {
			t.Errorf("diff(%q, %d, %d), want (way, 42, 3)", typ, id, version)
		}
		return &PreviousVersion{Found: true, Tags: map[string]string{"sport": "climbing"}, Lat: &lat, Lon: &lon}, nil
	}

	srv := httptest.NewServer(NewHandler(New(), noHistory, diff))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/diff?type=way&id=42&version=3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got PreviousVersion
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Tags["sport"] != "climbing" || *got.Lat != lat || *got.Lon != lon {
		t.Errorf("got %+v", got)
	}
}

func TestServeDiffReportsNotFound(t *testing.T) {
	srv := httptest.NewServer(NewHandler(New(), noHistory, noDiff))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/diff?type=way&id=42&version=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var got PreviousVersion
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Found {
		t.Errorf("Found = true, want false")
	}
}

func TestServeDiffRejectsMissingParams(t *testing.T) {
	srv := httptest.NewServer(NewHandler(New(), noHistory, noDiff))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/diff?type=way&id=42")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServesIndexPage(t *testing.T) {
	srv := httptest.NewServer(NewHandler(New(), noHistory, noDiff))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	buf := make([]byte, 15)
	resp.Body.Read(buf)
	if !strings.HasPrefix(string(buf), "<!DOCTYPE html>") {
		t.Errorf("body starts with %q", buf)
	}
}

func TestServesRobotsTxt(t *testing.T) {
	srv := httptest.NewServer(NewHandler(New(), noHistory, noDiff))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/robots.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "User-agent: *\nDisallow: /\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestServesFavicon(t *testing.T) {
	srv := httptest.NewServer(NewHandler(New(), noHistory, noDiff))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "svg") {
		t.Errorf("Content-Type = %q, want an SVG type", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<svg") {
		t.Errorf("body is not an SVG: %.60q", body)
	}
}

func TestPageLinksTheFavicon(t *testing.T) {
	srv := httptest.NewServer(NewHandler(New(), noHistory, noDiff))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `href="favicon.svg"`) {
		t.Error("the page does not link the favicon")
	}
}
