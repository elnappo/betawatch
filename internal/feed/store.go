package feed

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Store keeps the recent changes on disk as newline-delimited JSON, the
// same format the command prints, so the history survives a restart and
// stays greppable.
type Store struct {
	path string
	f    *os.File
}

// Record is the part of a change the store itself needs. The rest of the
// fields are preserved verbatim, so the file keeps whatever the command
// writes without this package having to know about it.
type Record struct {
	Minute    uint64    `json:"minute"`
	Type      string    `json:"type"`
	ID        int64     `json:"id"`
	Version   int       `json:"version"`
	Action    string    `json:"action"`
	Timestamp time.Time `json:"timestamp"`
}

// key identifies one element version, so a diff processed twice does not
// produce a duplicate row.
func (r Record) key() string {
	return fmt.Sprintf("%s/%d/%d/%s", r.Type, r.ID, r.Version, r.Action)
}

// OpenStore loads the history at path, dropping anything older than
// retain, and rewrites the file with what remains. It returns the kept
// lines in file order, oldest first.
func OpenStore(path string, retain time.Duration) (*Store, [][]byte, error) {
	kept, err := readKept(path, retain)
	if err != nil {
		return nil, nil, err
	}
	// Rewrite so the file shrinks as records age out, rather than growing
	// forever.
	if err := writeAll(path, kept); err != nil {
		return nil, nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return &Store{path: path, f: f}, kept, nil
}

// readKept returns the lines worth keeping: recent enough, parseable, and
// not a duplicate of an earlier line.
func readKept(path string, retain time.Duration) ([][]byte, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cutoff := time.Now().UTC().Add(-retain)
	seen := map[string]bool{}
	var kept [][]byte

	sc := bufio.NewScanner(f)
	// Change records are small, but a tag set can be large; allow room.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue // skip a torn or hand-edited line rather than fail
		}
		if r.Timestamp.Before(cutoff) || seen[r.key()] {
			continue
		}
		seen[r.key()] = true
		kept = append(kept, append([]byte(nil), line...))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return kept, nil
}

// writeAll replaces path atomically, so an interrupted rewrite cannot
// lose the history.
func writeAll(path string, lines [][]byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".changes-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	w := bufio.NewWriter(tmp)
	for _, line := range lines {
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Append adds one change to the file.
func (s *Store) Append(line []byte) error {
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// Close flushes and closes the file.
func (s *Store) Close() error { return s.f.Close() }

// LastMinute returns the sequence number of the newest stored record, or
// 0 if there is none. It is where a restart resumes from.
func LastMinute(lines [][]byte) uint64 {
	var last uint64
	for _, line := range lines {
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.Minute > last {
			last = r.Minute
		}
	}
	return last
}
