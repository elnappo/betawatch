package commons

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient returns a Client pointed at handler, with the retry pause
// zeroed so tests do not sleep.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := NewClient(srv.Client())
	c.BaseURL = srv.URL
	c.backoff = func(int) time.Duration { return 0 }
	return c
}

func TestQueryReturnsOneFilePerTitleInOrder(t *testing.T) {
	const body = `{
  "batchcomplete": true,
  "query": {
    "normalized": [
      {"from": "File:Has_Info.jpg", "to": "File:Has Info.jpg"},
      {"from": "File:Bad%3A.jpg", "to": "File:Bad:.jpg"}
    ],
    "pages": [
      {"pageid": 1, "ns": 6, "title": "File:Has Info.jpg",
       "imageinfo": [{"width": 4000, "height": 3000, "size": 1234567, "mime": "image/jpeg"}]},
      {"pageid": 2, "ns": 6, "title": "File:Gone.jpg", "missing": true},
      {"pageid": 3, "ns": 6, "title": "File:Bad:.jpg", "invalid": true,
       "invalidreason": "The requested page title contains invalid characters"},
      {"pageid": 4, "ns": 6, "title": "File:Plain.jpg"}
    ]
  }
}`
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("action"); got != "query" {
			t.Errorf("action = %q, want query", got)
		}
		if got := q.Get("prop"); got != "imageinfo" {
			t.Errorf("prop = %q, want imageinfo", got)
		}
		if got := q.Get("iiprop"); got != "size|mime" {
			t.Errorf("iiprop = %q, want size|mime", got)
		}
		if got := q.Get("formatversion"); got != "2" {
			t.Errorf("formatversion = %q, want 2", got)
		}
		w.Write([]byte(body))
	}))

	files, err := c.Query(context.Background(), []string{
		"File:Has_Info.jpg", "File:Gone.jpg", "File:Bad%3A.jpg", "File:Plain.jpg",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("len(files) = %d, want 4", len(files))
	}

	f := files[0]
	if f.Title != "File:Has_Info.jpg" || f.Status != StatusOK {
		t.Errorf("files[0] = %+v, want the requested title with status ok", f)
	}
	if f.Width == nil || *f.Width != 4000 || f.Height == nil || *f.Height != 3000 ||
		f.SizeBytes == nil || *f.SizeBytes != 1234567 || f.MIME == nil || *f.MIME != "image/jpeg" {
		t.Errorf("files[0] imageinfo = %+v, want width 4000, height 3000, size 1234567, mime image/jpeg", f)
	}

	if f := files[1]; f.Title != "File:Gone.jpg" || f.Status != StatusMissing || f.Width != nil {
		t.Errorf("files[1] = %+v, want missing without dimensions", f)
	}
	if f := files[2]; f.Title != "File:Bad%3A.jpg" || f.Status != StatusInvalid {
		t.Errorf("files[2] = %+v, want invalid", f)
	}
	if f := files[3]; f.Title != "File:Plain.jpg" || f.Status != StatusNoImageInfo {
		t.Errorf("files[3] = %+v, want no imageinfo", f)
	}
}

func TestQueryRetriesTransientFailures(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.Write([]byte(`{"query": {"pages": [{"title": "File:A.jpg", "missing": true}]}}`))
		}
	}))

	files, err := c.Query(context.Background(), []string{"File:A.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("requests = %d, want 3: a 429 and a 502 each get one retry", calls)
	}
	if len(files) != 1 || files[0].Status != StatusMissing {
		t.Errorf("files = %+v, want one missing file", files)
	}
}

func TestQueryDoesNotRetryClientErrors(t *testing.T) {
	var calls int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))

	if _, err := c.Query(context.Background(), []string{"File:A.jpg"}); err == nil {
		t.Fatal("Query(403) succeeded, want an error")
	}
	if calls != 1 {
		t.Errorf("requests = %d, want 1: a 403 is not retried", calls)
	}
}

func TestQueryReturnsAPIError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error": {"code": "badvalue", "info": "Unrecognized value for parameter"}}`))
	}))

	_, err := c.Query(context.Background(), []string{"File:A.jpg"})
	if err == nil {
		t.Fatal("Query(error response) succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "badvalue") {
		t.Errorf("error = %q, want it to mention the API error code", err)
	}
}

func TestQueryRejectsTooManyTitles(t *testing.T) {
	c := NewClient(http.DefaultClient)
	if _, err := c.Query(context.Background(), make([]string, MaxTitles+1)); err == nil {
		t.Errorf("Query(%d titles) succeeded, want an error", MaxTitles+1)
	}
}
