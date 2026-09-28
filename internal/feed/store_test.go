package feed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// line builds a stored change record.
func line(minute uint64, id int64, age time.Duration) []byte {
	b, _ := json.Marshal(map[string]any{
		"minute":    minute,
		"type":      "node",
		"id":        id,
		"version":   1,
		"action":    "create",
		"timestamp": time.Now().UTC().Add(-age).Format(time.RFC3339),
		"name":      fmt.Sprintf("crag %d", id),
	})
	return b
}

func writeLines(t *testing.T, path string, lines ...[]byte) {
	t.Helper()
	if err := writeAll(path, lines); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changes.ndjson")

	s, history, err := OpenStore(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Errorf("new store returned %d records, want 0", len(history))
	}
	if err := s.Append(line(100, 1, 0)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	_, history, err = OpenStore(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("reopened store returned %d records, want 1", len(history))
	}
}

func TestStoreDropsOldRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changes.ndjson")
	writeLines(t, path,
		line(1, 1, 8*24*time.Hour), // older than a week
		line(2, 2, 2*time.Hour),
		line(3, 3, 0),
	)

	_, history, err := OpenStore(path, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("kept %d records, want 2", len(history))
	}

	// The file itself must shrink, not just the returned slice.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := countLines(data); got != 2 {
		t.Errorf("file holds %d lines, want 2", got)
	}
}

func TestStoreDropsDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changes.ndjson")
	dup := line(5, 42, 0)
	writeLines(t, path, dup, dup, line(6, 43, 0))

	_, history, err := OpenStore(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Errorf("kept %d records, want 2 (the repeat should be dropped)", len(history))
	}
}

func TestStoreSkipsTornLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "changes.ndjson")
	if err := os.WriteFile(path, []byte(`{"minute":1,"timestamp":"`+
		time.Now().UTC().Format(time.RFC3339)+`","type":"node","id":1,"version":1,"action":"create"}`+
		"\n{ this is not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, history, err := OpenStore(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Errorf("kept %d records, want 1 (the torn line should be skipped)", len(history))
	}
}

func TestLastMinute(t *testing.T) {
	history := [][]byte{line(10, 1, 0), line(42, 2, 0), line(7, 3, 0)}
	if got := LastMinute(history); got != 42 {
		t.Errorf("LastMinute() = %d, want 42", got)
	}
	if got := LastMinute(nil); got != 0 {
		t.Errorf("LastMinute(nil) = %d, want 0", got)
	}
}

func TestBrokerLoad(t *testing.T) {
	b := New(10)
	b.Load([][]byte{line(1, 1, 0), line(2, 2, 0)})

	if b.LastID() != 2 {
		t.Errorf("LastID = %d, want 2", b.LastID())
	}
	// A change published after loading continues the numbering.
	if ev := b.Publish([]byte("new")); ev.ID != 3 {
		t.Errorf("published id = %d, want 3", ev.ID)
	}
	if got := len(b.History()); got != 3 {
		t.Errorf("history = %d events, want 3", got)
	}
}

func TestBrokerLoadRespectsLimit(t *testing.T) {
	b := New(2)
	b.Load([][]byte{line(1, 1, 0), line(2, 2, 0), line(3, 3, 0)})
	if got := len(b.History()); got != 2 {
		t.Errorf("history = %d events, want 2", got)
	}
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}
