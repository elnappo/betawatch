package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func apply(t *testing.T, s *Store, f func(*Tx)) {
	t.Helper()
	tx, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	f(tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func el(version int, at time.Time) Element {
	lat, lon := 47.5, 11.2
	return Element{
		ID: 1, Type: Node, Version: version, Lat: &lat, Lon: &lon,
		UID: 7, User: "u", Timestamp: at, Changeset: 100,
		Tags: map[string]string{"sport": "climbing"},
	}
}

func TestSaveMovesOldVersionToHistory(t *testing.T) {
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(1, now)))
		must(t, tx.Save(el(2, now.Add(time.Minute))))
	})

	if n := count(t, s, `SELECT version FROM elements WHERE id = 1 AND type = 'n'`); n != 2 {
		t.Errorf("live version = %d, want 2", n)
	}
	if n := count(t, s, `SELECT count(*) FROM elements_history WHERE version = 1 AND deleted = 0`); n != 1 {
		t.Errorf("history rows for version 1 = %d, want 1", n)
	}
}

func TestSaveOfPriorVersionFillsHistoryGap(t *testing.T) {
	// Out-of-order arrival: version 2 goes live first, then version 1
	// arrives after. It never becomes live, but it is the one version
	// PreviousVersion(2) would otherwise be missing.
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(2, now.Add(time.Minute))))
		must(t, tx.Save(el(1, now)))
	})

	if n := count(t, s, `SELECT version FROM elements WHERE id = 1 AND type = 'n'`); n != 2 {
		t.Errorf("live version = %d, want 2", n)
	}
	if n := count(t, s, `SELECT count(*) FROM elements_history WHERE version = 1 AND deleted = 0`); n != 1 {
		t.Errorf("history rows for version 1 = %d, want 1", n)
	}
}

func TestSaveOfOlderVersionIsIgnored(t *testing.T) {
	// A version more than one behind live adds nothing PreviousVersion can
	// use, so it is dropped rather than stored.
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(3, now.Add(2*time.Minute))))
		must(t, tx.Save(el(1, now)))
	})

	if n := count(t, s, `SELECT count(*) FROM elements_history`); n != 0 {
		t.Errorf("history rows = %d, want 0", n)
	}
}

func TestReplayIsIgnored(t *testing.T) {
	s := open(t)
	now := time.Now()
	for range 2 {
		apply(t, s, func(tx *Tx) { must(t, tx.Save(el(1, now))) })
	}
	if n := count(t, s, `SELECT count(*) FROM elements`); n != 1 {
		t.Errorf("elements = %d, want 1", n)
	}
	if n := count(t, s, `SELECT count(*) FROM elements_history`); n != 0 {
		t.Errorf("history = %d, want 0", n)
	}
}

func TestDelete(t *testing.T) {
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(1, now)))

		if ok, err := tx.Delete(999, Node, 2); err != nil || ok {
			t.Errorf("delete of unknown element = %v, %v; want ignored", ok, err)
		}
		// The same id as a different type is a different element.
		if ok, err := tx.Delete(1, Way, 2); err != nil || ok {
			t.Errorf("delete of other type = %v, %v; want ignored", ok, err)
		}
		if ok, err := tx.Delete(1, Node, 2); err != nil || !ok {
			t.Errorf("delete = %v, %v; want moved", ok, err)
		}
	})

	if n := count(t, s, `SELECT count(*) FROM elements`); n != 0 {
		t.Errorf("elements = %d, want 0", n)
	}
	if n := count(t, s, `SELECT count(*) FROM elements_history WHERE deleted = 1 AND tags IS NOT NULL`); n != 1 {
		t.Errorf("deleted history rows = %d, want 1", n)
	}
}

func TestChangesetTimestampStaysNull(t *testing.T) {
	// The diffs never carry the changeset itself, so nothing should ever
	// derive a changeset timestamp from an element's.
	s := open(t)
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(1, time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))))
	})
	var got sql.NullString
	if err := s.db.QueryRow(`SELECT timestamp FROM changesets WHERE id = 100`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Errorf("changeset timestamp = %q, want NULL", got.String)
	}
}

func TestPreviousVersion(t *testing.T) {
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(1, now)))
		must(t, tx.Save(el(2, now.Add(time.Minute))))
	})

	prev, err := s.PreviousVersion(1, Node, 2)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil {
		t.Fatal("PreviousVersion = nil, want version 1")
	}
	if prev.Version != 1 {
		t.Errorf("Version = %d, want 1", prev.Version)
	}
	if prev.Lat == nil || prev.Lon == nil {
		t.Error("Lat/Lon not read from history")
	}
}

func TestPreviousVersionOfACreate(t *testing.T) {
	s := open(t)
	apply(t, s, func(tx *Tx) { must(t, tx.Save(el(1, time.Now()))) })

	prev, err := s.PreviousVersion(1, Node, 1)
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil {
		t.Errorf("PreviousVersion of version 1 = %+v, want nil", prev)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s := open(t)
	_, err := s.db.Exec(`INSERT INTO elements (id, type, version, timestamp, changeset_id) VALUES (1, 'n', 1, 'x', 5)`)
	if err == nil {
		t.Error("insert without changeset succeeded, want foreign key error")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a b#c")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "x.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database not created at the requested path: %v", err)
	}
}
