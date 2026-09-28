package feed

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPublishReachesSubscriber(t *testing.T) {
	b := New(10)
	_, events, cancel := b.Subscribe(0)
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

func TestSubscribeReplaysBacklog(t *testing.T) {
	b := New(10)
	b.Publish([]byte("one"))
	b.Publish([]byte("two"))

	backlog, _, cancel := b.Subscribe(0)
	defer cancel()
	if len(backlog) != 2 {
		t.Fatalf("backlog = %d events, want 2", len(backlog))
	}
}

func TestSubscribeResumesAfterID(t *testing.T) {
	b := New(10)
	b.Publish([]byte("one"))
	b.Publish([]byte("two"))
	b.Publish([]byte("three"))

	// A browser that already saw event 1 must get only 2 and 3.
	backlog, _, cancel := b.Subscribe(1)
	defer cancel()
	if len(backlog) != 2 {
		t.Fatalf("backlog = %d events, want 2", len(backlog))
	}
	if string(backlog[0].Data) != "two" {
		t.Errorf("first replayed = %q, want \"two\"", backlog[0].Data)
	}
}

func TestHistoryIsBounded(t *testing.T) {
	b := New(3)
	for range 10 {
		b.Publish([]byte("x"))
	}
	backlog, _, cancel := b.Subscribe(0)
	defer cancel()
	if len(backlog) != 3 {
		t.Errorf("backlog = %d events, want 3", len(backlog))
	}
	if b.LastID() != 10 {
		t.Errorf("LastID = %d, want 10", b.LastID())
	}
}

func TestCancelUnsubscribes(t *testing.T) {
	b := New(10)
	_, events, cancel := b.Subscribe(0)
	cancel()

	if _, open := <-events; open {
		t.Error("channel still open after cancel")
	}
	// A publish after cancel must not panic on the closed channel.
	b.Publish([]byte("after"))
}

func TestCancelIsIdempotent(t *testing.T) {
	b := New(10)
	_, _, cancel := b.Subscribe(0)
	cancel()
	cancel() // must not panic by closing twice
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	b := New(1000)
	_, events, cancel := b.Subscribe(0)
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
	b := New(10)
	b.Publish([]byte(`{"id":1}`))

	srv := httptest.NewServer(b.Handler())
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
	id, _ := r.ReadString('\n')
	data, _ := r.ReadString('\n')
	if strings.TrimSpace(id) != "id: 1" {
		t.Errorf("first line = %q, want \"id: 1\"", strings.TrimSpace(id))
	}
	if strings.TrimSpace(data) != `data: {"id":1}` {
		t.Errorf("second line = %q", strings.TrimSpace(data))
	}
}

func TestServeEventsHonoursLastEventID(t *testing.T) {
	b := New(10)
	b.Publish([]byte(`{"n":1}`))
	b.Publish([]byte(`{"n":2}`))

	srv := httptest.NewServer(b.Handler())
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	id, _ := r.ReadString('\n')
	if strings.TrimSpace(id) != "id: 2" {
		t.Errorf("resumed at %q, want \"id: 2\"", strings.TrimSpace(id))
	}
}

func TestServesIndexPage(t *testing.T) {
	srv := httptest.NewServer(New(10).Handler())
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
	srv := httptest.NewServer(New(10).Handler())
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
	srv := httptest.NewServer(New(10).Handler())
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
	srv := httptest.NewServer(New(10).Handler())
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
