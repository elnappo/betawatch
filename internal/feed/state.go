package feed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State records how far the minute feed has been processed. It is kept
// apart from the change store because it advances on every diff, whereas
// the store only grows when a diff holds a matching change. Resuming
// from the store alone would re-download every quiet minute since the
// last match, which is most of them.
type State struct {
	// Minute is the last sequence number fully processed.
	Minute uint64 `json:"minute"`
	// Updated is when that happened, for anyone reading the file.
	Updated time.Time `json:"updated"`

	path string
}

// LoadState reads the state file. A missing file is not an error: it
// means a first run, and Minute stays 0.
func LoadState(path string) (*State, error) {
	s := &State{path: path}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		// A truncated or hand-edited file should not stop the program:
		// fall back to a first run rather than refusing to start.
		return &State{path: path}, nil
	}
	s.path = path
	return s, nil
}

// Save records that everything up to minute has been processed. The
// write goes through a temp file and rename, so an interrupted save
// leaves the previous state rather than a truncated one.
func (s *State) Save(minute uint64) error {
	s.Minute = minute
	s.Updated = time.Now().UTC()

	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}
	return nil
}
