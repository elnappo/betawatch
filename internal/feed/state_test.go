package feed

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Minute != 0 {
		t.Errorf("new state Minute = %d, want 0", s.Minute)
	}

	if err := s.Save(7305000); err != nil {
		t.Fatal(err)
	}

	again, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Minute != 7305000 {
		t.Errorf("reloaded Minute = %d, want 7305000", again.Minute)
	}
	if again.Updated.IsZero() {
		t.Error("Updated was not recorded")
	}
}

func TestLoadStateMissingFile(t *testing.T) {
	// A first run must not be an error.
	s, err := LoadState(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing file returned an error: %v", err)
	}
	if s.Minute != 0 {
		t.Errorf("Minute = %d, want 0", s.Minute)
	}
}

func TestLoadStateCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A torn file should start over, not stop the program.
	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("corrupt file returned an error: %v", err)
	}
	if s.Minute != 0 {
		t.Errorf("Minute = %d, want 0", s.Minute)
	}
	// It must still be writable afterwards.
	if err := s.Save(42); err != nil {
		t.Fatal(err)
	}
	again, _ := LoadState(path)
	if again.Minute != 42 {
		t.Errorf("Minute after save = %d, want 42", again.Minute)
	}
}

func TestStateSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, _ := LoadState(path)
	for i := range 20 {
		if err := s.Save(uint64(i)); err != nil {
			t.Fatal(err)
		}
	}

	// No temp files should be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory holds %v, want only state.json", names)
	}
}

func TestStateSaveCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "state.json")
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state file was not created: %v", err)
	}
}

func TestStateUpdatedAdvances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := LoadState(path)

	s.Save(1)
	first := s.Updated
	time.Sleep(2 * time.Millisecond)
	s.Save(2)

	if !s.Updated.After(first) {
		t.Errorf("Updated did not advance: %v then %v", first, s.Updated)
	}
}
