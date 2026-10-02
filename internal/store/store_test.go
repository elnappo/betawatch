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

// delEl builds the Element a delete diff carries: id, type and version
// identify what is being deleted, but no tags, lat or lon.
func delEl(id int64, typ string, version int, at time.Time) Element {
	return Element{
		ID: id, Type: typ, Version: version,
		UID: 7, User: "u", Timestamp: at, Changeset: 100,
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

		if ok, err := tx.Delete(delEl(999, Node, 2, now)); err != nil || ok {
			t.Errorf("delete of unknown element = %v, %v; want ignored", ok, err)
		}
		// The same id as a different type is a different element.
		if ok, err := tx.Delete(delEl(1, Way, 2, now)); err != nil || ok {
			t.Errorf("delete of other type = %v, %v; want ignored", ok, err)
		}
		if ok, err := tx.Delete(delEl(1, Node, 2, now.Add(time.Minute))); err != nil || !ok {
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

// TestDeleteStampsItsOwnEvent checks that a delete's history row carries
// the delete event's own version, timestamp, user and changeset, not the
// live row's it replaces: a delete is distinct from the edit that made the
// element's last live version.
func TestDeleteStampsItsOwnEvent(t *testing.T) {
	s := open(t)
	now := time.Now()
	deletedAt := now.Add(time.Hour)
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(1, now)))

		d := Element{
			ID: 1, Type: Node, Version: 2,
			UID: 9, User: "deleter", Timestamp: deletedAt, Changeset: 200,
		}
		if ok, err := tx.Delete(d); err != nil || !ok {
			t.Fatalf("delete = %v, %v; want moved", ok, err)
		}
	})

	var version int
	var user string
	var uid, changeset int64
	var timestamp string
	err := s.db.QueryRow(`
		SELECT version, user, uid, changeset_id, timestamp
		FROM elements_history WHERE id = 1 AND type = 'n' AND deleted = 1`).
		Scan(&version, &user, &uid, &changeset, &timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}
	if user != "deleter" {
		t.Errorf("user = %q, want deleter", user)
	}
	if uid != 9 {
		t.Errorf("uid = %d, want 9", uid)
	}
	if changeset != 200 {
		t.Errorf("changeset = %d, want 200", changeset)
	}
	if want := ts(deletedAt); timestamp != want {
		t.Errorf("timestamp = %q, want %q", timestamp, want)
	}

	// lat/lon/tags still come from the live row, since the delete itself
	// carries none.
	if n := count(t, s, `SELECT count(*) FROM elements_history WHERE id = 1 AND type = 'n' AND deleted = 1 AND lat IS NOT NULL AND tags IS NOT NULL`); n != 1 {
		t.Errorf("lat/tags carried over = %d, want 1", n)
	}
}

func TestChangesetTimestampStaysNull(t *testing.T) {
	// The diffs never carry the changeset itself, so Save must never derive
	// a changeset timestamp from an element's; only SetChangesetMetadata,
	// from the OSM API's created_at, may set it.
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

func TestNeedsChangesetMetadata(t *testing.T) {
	s := open(t)
	apply(t, s, func(tx *Tx) { must(t, tx.Save(el(1, time.Now()))) })

	needed, err := s.NeedsChangesetMetadata(100)
	if err != nil {
		t.Fatal(err)
	}
	if !needed {
		t.Error("NeedsChangesetMetadata = false right after Save, want true")
	}

	// Comment left empty: even a changeset whose comment genuinely turns
	// out blank must still count as fetched, since CreatedAt is what
	// NeedsChangesetMetadata actually keys on.
	if err := s.SetChangesetMetadata(100, ChangesetMetadata{CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	needed, err = s.NeedsChangesetMetadata(100)
	if err != nil {
		t.Fatal(err)
	}
	if needed {
		t.Error("NeedsChangesetMetadata = true after SetChangesetMetadata, want false")
	}
}

func TestNeedsChangesetMetadataOfUnknownChangeset(t *testing.T) {
	s := open(t)
	needed, err := s.NeedsChangesetMetadata(999)
	if err != nil {
		t.Fatal(err)
	}
	if needed {
		t.Error("NeedsChangesetMetadata of an unstored changeset = true, want false")
	}
}

func TestSetChangesetMetadataStoresAllFields(t *testing.T) {
	s := open(t)
	apply(t, s, func(tx *Tx) { must(t, tx.Save(el(1, time.Now()))) })

	createdAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	m := ChangesetMetadata{
		CreatedAt: createdAt,
		Comment:   "fixed a wall", CreatedBy: "JOSM/1.5", ImageryUsed: "Bing",
		Source: "survey", Locale: "en", DataUsed: "Esri", Hashtags: "#climbing",
		Host: "https://www.openstreetmap.org/edit", Bot: true, ReviewRequested: true,
		ChangesetsCount: 42,
	}
	if err := s.SetChangesetMetadata(100, m); err != nil {
		t.Fatal(err)
	}

	var got ChangesetMetadata
	var timestamp string
	err := s.db.QueryRow(`
		SELECT timestamp, comment, created_by, imagery_used, source, locale, data_used,
			hashtags, host, bot, review_requested, changesets_count
		FROM changesets WHERE id = 100`).
		Scan(&timestamp, &got.Comment, &got.CreatedBy, &got.ImageryUsed, &got.Source, &got.Locale,
			&got.DataUsed, &got.Hashtags, &got.Host, &got.Bot, &got.ReviewRequested,
			&got.ChangesetsCount)
	if err != nil {
		t.Fatal(err)
	}
	// CreatedAt round-trips through the timestamp column as text, checked
	// separately below, so it is excluded from this comparison.
	m.CreatedAt = time.Time{}
	if got != m {
		t.Errorf("stored metadata = %+v, want %+v", got, m)
	}
	if want := ts(createdAt); timestamp != want {
		t.Errorf("timestamp = %q, want %q", timestamp, want)
	}

	// changes_count is never written: see the comment on ChangesetMetadata.
	if n := count(t, s, `SELECT count(*) FROM changesets WHERE id = 100 AND changes_count IS NULL`); n != 1 {
		t.Errorf("changes_count rows = %d, want 1 (still NULL)", n)
	}
}

func TestQueriesIncludeChangesetMetadata(t *testing.T) {
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) { must(t, tx.Save(el(1, now))) })

	before, err := s.QueryLatest(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].Comment != nil || before[0].CreatedBy != nil {
		t.Fatalf("before fetch: comment/created_by = %+v, want both nil", before[0])
	}

	if err := s.SetChangesetMetadata(100, ChangesetMetadata{CreatedAt: now, Comment: "fixed a wall", CreatedBy: "JOSM"}); err != nil {
		t.Fatal(err)
	}

	after, err := s.QueryLatest(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("len(after) = %d, want 1", len(after))
	}
	if after[0].Comment == nil || *after[0].Comment != "fixed a wall" {
		t.Errorf("Comment = %v, want fixed a wall", after[0].Comment)
	}
	if after[0].CreatedBy == nil || *after[0].CreatedBy != "JOSM" {
		t.Errorf("CreatedBy = %v, want JOSM", after[0].CreatedBy)
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

func TestQueryRecentIncludesSupersededVersions(t *testing.T) {
	// Both versions were recorded since the given timestamp, so both
	// should appear, not just the one that ended up live.
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(1, now)))
		must(t, tx.Save(el(2, now.Add(time.Minute))))
	})

	changes, err := s.QueryRecent(now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2", len(changes))
	}
	if changes[0].Version != 1 || changes[1].Version != 2 {
		t.Errorf("versions = %d, %d, want 1, 2", changes[0].Version, changes[1].Version)
	}
	if changes[0].Deleted || changes[1].Deleted {
		t.Error("neither version is a delete")
	}
}

func TestQueryLatestIncludesSupersededVersions(t *testing.T) {
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(1, now)))
		must(t, tx.Save(el(2, now.Add(time.Minute))))
	})

	changes, err := s.QueryLatest(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2", len(changes))
	}
	// Newest first.
	if changes[0].Version != 2 || changes[1].Version != 1 {
		t.Errorf("versions = %d, %d, want 2, 1", changes[0].Version, changes[1].Version)
	}
}

func TestQueryLatestIncludesDeletes(t *testing.T) {
	// Deleting version 1 retires its row straight into elements_history
	// stamped as the delete event (version 2): there was never a separate
	// live row for version 1 to leave behind, so only one row results.
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(1, now)))
		if _, err := tx.Delete(delEl(1, Node, 2, now.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
	})

	changes, err := s.QueryLatest(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	if !changes[0].Deleted {
		t.Error("change should be the delete")
	}
	if changes[0].Version != 2 {
		t.Errorf("version = %d, want 2", changes[0].Version)
	}
}

func TestQueryBeforePagesAcrossSharedTimestampAcrossVersions(t *testing.T) {
	// Two versions of the same element at the same timestamp: the
	// (timestamp, id, type, version) tiebreak must still see both exactly
	// once when a page boundary falls between them.
	s := open(t)
	now := time.Now()
	apply(t, s, func(tx *Tx) {
		must(t, tx.Save(el(1, now)))
		must(t, tx.Save(el(2, now)))
	})

	first, err := s.QueryLatest(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Version != 2 {
		t.Fatalf("first page = %+v, want version 2", first)
	}

	rest, err := s.QueryBefore(first[0].Timestamp, first[0].ID, first[0].Type, first[0].Version, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].Version != 1 {
		t.Fatalf("rest = %+v, want version 1", rest)
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
