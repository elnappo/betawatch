// Package commons looks up file metadata from the Wikimedia Commons API.
package commons

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// API is the MediaWiki API endpoint at Wikimedia Commons.
const API = "https://commons.wikimedia.org/w/api.php"

// MaxTitles is the largest number of titles one Query may ask about. The
// MediaWiki API limits anonymous callers to 50 titles per request.
const MaxTitles = 50

// maxAttempts is how often a request is repeated before giving up, as in the
// Python helper this replaces.
const maxAttempts = 8

// maxBody caps how much of a response is read. Fifty pages of imageinfo are
// tens of KB, so this only guards against a surprising answer.
const maxBody = 1 << 20

// The statuses a File can carry: what the API said about the title.
const (
	StatusOK          = "ok"
	StatusMissing     = "missing"
	StatusInvalid     = "invalid"
	StatusNoImageInfo = "no imageinfo"
)

// File is what the Commons API says about one requested title. Width,
// Height, SizeBytes and MIME are set only when Status is StatusOK.
type File struct {
	Title     string // the requested title, unchanged
	Width     *int64
	Height    *int64
	SizeBytes *int64
	MIME      *string
	Status    string
}

// Client queries the Commons API. Construct it with NewClient.
type Client struct {
	// BaseURL is the API endpoint; empty means the live Commons API. Tests
	// point it at a stub server.
	BaseURL string

	http *http.Client

	// backoff is how long to wait before the next attempt. A field so tests
	// do not sleep.
	backoff func(attempt int) time.Duration
}

// NewClient returns a Client that queries through hc. hc's transport should
// send a descriptive User-Agent, which the Wikimedia APIs ask for.
func NewClient(hc *http.Client) *Client {
	return &Client{
		http: hc,
		backoff: func(attempt int) time.Duration {
			return time.Duration(1<<attempt) * time.Second
		},
	}
}

// Query returns what the API knows about each title: one File per title, in
// the same order. A title that is missing or invalid comes back with a non-ok
// status, not an error; an error means no answer was obtained, and the caller
// should leave any stored data alone.
func (c *Client) Query(ctx context.Context, titles []string) ([]File, error) {
	if len(titles) == 0 {
		return nil, nil
	}
	if len(titles) > MaxTitles {
		return nil, fmt.Errorf("commons: %d titles, the API takes at most %d", len(titles), MaxTitles)
	}

	params := url.Values{}
	params.Set("action", "query")
	params.Set("titles", strings.Join(titles, "|"))
	params.Set("prop", "imageinfo")
	params.Set("iiprop", "size|mime")
	params.Set("format", "json")
	params.Set("formatversion", "2")
	base := c.BaseURL
	if base == "" {
		base = API
	}
	requestURL := base + "?" + params.Encode()

	var lastErr error
	for attempt := range maxAttempts {
		files, retryAfter, err := c.queryOnce(ctx, requestURL, titles)
		if err == nil {
			return files, nil
		}
		if retryAfter < 0 {
			// The API answered, and repeating the request cannot change
			// the answer.
			return nil, err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		wait := retryAfter
		if wait <= 0 {
			wait = c.backoff(attempt)
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("commons: after %d attempts: %w", maxAttempts, lastErr)
}

// queryOnce performs one request. The returned duration is how long to wait
// before retrying: negative means the error is permanent, zero means the
// caller's own backoff, and positive is a Retry-After the server sent.
func (c *Client) queryOnce(ctx context.Context, requestURL string, titles []string) ([]File, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, -1, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The API only reads, so a request that may not even have arrived is
		// safe to send again.
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, 0, err
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, retryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("commons: HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, -1, fmt.Errorf("commons: HTTP %d", resp.StatusCode)
	}

	files, err := parse(body, titles)
	if err != nil {
		return nil, -1, err
	}
	return files, 0, nil
}

// retryAfter reads a Retry-After header, in seconds. Zero for a missing or
// unparsable value, falling back to the caller's backoff.
func retryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds < 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// sleep waits for d, or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// imageInfo and page mirror the parts of a query response the client reads.
type imageInfo struct {
	Width  int64  `json:"width"`
	Height int64  `json:"height"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime"`
}

type page struct {
	Title     string      `json:"title"`
	Missing   bool        `json:"missing"`
	Invalid   bool        `json:"invalid"`
	ImageInfo []imageInfo `json:"imageinfo"`
}

// parse maps an API response back to the requested titles. Titles are matched
// through the response's normalization: the API answers under its own
// spelling (spaces for underscores, for example), which is how a page is
// found.
func parse(body []byte, titles []string) ([]File, error) {
	var resp struct {
		Error *struct {
			Code string `json:"code"`
			Info string `json:"info"`
		} `json:"error"`
		Query struct {
			Normalized []struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"normalized"`
			Pages []page `json:"pages"`
		} `json:"query"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("commons: decoding response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("commons: API error %s: %s", resp.Error.Code, resp.Error.Info)
	}

	normalized := make(map[string]string, len(resp.Query.Normalized))
	for _, n := range resp.Query.Normalized {
		normalized[n.From] = n.To
	}
	pages := make(map[string]page, len(resp.Query.Pages))
	for _, p := range resp.Query.Pages {
		pages[p.Title] = p
	}

	files := make([]File, 0, len(titles))
	for _, title := range titles {
		key := title
		if to, ok := normalized[title]; ok {
			key = to
		}
		p, ok := pages[key]
		f := File{Title: title}
		switch {
		case !ok || p.Missing:
			// No page for the title at all, or the API says it does not
			// exist.
			f.Status = StatusMissing
		case p.Invalid:
			f.Status = StatusInvalid
		case len(p.ImageInfo) == 0:
			f.Status = StatusNoImageInfo
		default:
			info := p.ImageInfo[0]
			f.Width, f.Height, f.SizeBytes = &info.Width, &info.Height, &info.Size
			f.MIME = &info.MIME
			f.Status = StatusOK
		}
		files = append(files, f)
	}
	return files, nil
}
