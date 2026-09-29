// Package store keeps climbing elements in SQLite: the live version of
// each in elements, and the versions it replaced in elements_history.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// Element types as stored: one letter, as in the schema.
const (
	Node     = "n"
	Way      = "w"
	Relation = "r"
)

// Element is one version of a node, way or relation.
type Element struct {
	ID        int64
	Type      string // Node, Way or Relation
	Version   int
	Lat, Lon  *float64 // nodes only
	UID       int64
	User      string
	Timestamp time.Time
	Changeset int64
	Tags      map[string]string
}

// Store is a SQLite database of elements.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path.
func Open(path string) (*Store, error) {
	// Foreign keys are off by default in SQLite, and the pragma applies to
	// one connection, so it goes in the DSN.
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Opaque: url.PathEscape(path), RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Tx is one transaction. A minute diff is applied in one, so a crash
// leaves either all of it or none.
type Tx struct {
	tx *sql.Tx
}

func (s *Store) Begin() (*Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx}, nil
}

func (t *Tx) Commit() error   { return t.tx.Commit() }
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// version returns the live version of an element, or 0 if it is not stored.
func (t *Tx) version(id int64, typ string) (int, error) {
	var v int
	err := t.tx.QueryRow(`SELECT version FROM elements WHERE id = ? AND type = ?`, id, typ).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

// Has reports whether an element is live in the store.
func (t *Tx) Has(id int64, typ string) (bool, error) {
	v, err := t.version(id, typ)
	return v > 0, err
}

// Save makes el the live version. The version it replaces moves to
// elements_history. A version that is not newer than the live one is
// ignored, so replaying a minute after a crash changes nothing.
func (t *Tx) Save(el Element) error {
	cur, err := t.version(el.ID, el.Type)
	if err != nil {
		return err
	}
	if cur >= el.Version {
		return nil
	}

	var tags any
	if len(el.Tags) > 0 {
		b, err := json.Marshal(el.Tags)
		if err != nil {
			return err
		}
		tags = string(b)
	}

	// The diffs do not carry the changeset itself, only which elements
	// belong to it, so timestamp and the other changeset columns stay NULL
	// until a metadata source is added.
	if _, err := t.tx.Exec(`
		INSERT INTO changesets (id, uid, user) VALUES (?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		el.Changeset, el.UID, el.User); err != nil {
		return err
	}

	if cur > 0 {
		if err := t.retire(el.ID, el.Type, false); err != nil {
			return err
		}
	}
	_, err = t.tx.Exec(`
		INSERT INTO elements (id, type, version, lat, lon, uid, user, timestamp, changeset_id, tags)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		el.ID, el.Type, el.Version, el.Lat, el.Lon, el.UID, el.User, ts(el.Timestamp), el.Changeset, tags)
	return err
}

// Delete moves a live element to elements_history with deleted set. A
// delete carries no tags, so it can only be matched by id and type; one
// for an element that is not stored is ignored. It reports whether an
// element was moved.
func (t *Tx) Delete(id int64, typ string, version int) (bool, error) {
	cur, err := t.version(id, typ)
	if err != nil || cur == 0 || cur >= version {
		return false, err
	}
	return true, t.retire(id, typ, true)
}

// retire moves the live row of an element into elements_history.
func (t *Tx) retire(id int64, typ string, deleted bool) error {
	if _, err := t.tx.Exec(`
		INSERT OR IGNORE INTO elements_history
			(id, type, version, lat, lon, uid, user, timestamp, changeset_id, tags, deleted)
		SELECT id, type, version, lat, lon, uid, user, timestamp, changeset_id, tags, ?
		FROM elements WHERE id = ? AND type = ?`, deleted, id, typ); err != nil {
		return err
	}
	_, err := t.tx.Exec(`DELETE FROM elements WHERE id = ? AND type = ?`, id, typ)
	return err
}

// ts formats a time so that text order is time order.
func ts(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// RecentChange is an element record with metadata from the database.
type RecentChange struct {
	ID        int64
	Type      string
	Version   int
	Lat, Lon  *float64 // nodes only
	Timestamp time.Time
	User      string
	UID       int64
	Changeset int64
	Tags      map[string]string
	Name      string
}

// QueryRecent returns all live elements modified since the given timestamp,
// ordered by timestamp ascending.
func (s *Store) QueryRecent(since time.Time) ([]RecentChange, error) {
	return scanChanges(s.db.Query(`
		SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags
		FROM elements
		WHERE timestamp > ?
		ORDER BY timestamp ASC
	`, ts(since)))
}

// QueryLatest returns the most recently modified live elements, newest
// first, up to limit. The (id, type) tiebreak matches QueryBefore's, so
// paging from this page into the next never skips or repeats a row that
// shares a timestamp with the page boundary.
func (s *Store) QueryLatest(limit int) ([]RecentChange, error) {
	return scanChanges(s.db.Query(`
		SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags
		FROM elements
		ORDER BY timestamp DESC, id DESC, type DESC
		LIMIT ?
	`, limit))
}

// QueryBefore returns live elements ordered before the given (timestamp,
// id, type), newest first, up to limit. It is how the web view pages into
// the past. The full triple is needed, not just the timestamp: many
// elements share a timestamp (a single backfill batch commonly holds
// dozens), so cutting off on timestamp alone would drop the rest of that
// group whenever a page boundary landed inside it.
func (s *Store) QueryBefore(before time.Time, id int64, typ string, limit int) ([]RecentChange, error) {
	return scanChanges(s.db.Query(`
		SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags
		FROM elements
		WHERE (timestamp, id, type) < (?, ?, ?)
		ORDER BY timestamp DESC, id DESC, type DESC
		LIMIT ?
	`, ts(before), id, typ, limit))
}

// PreviousVersion returns the version just before version for an element,
// read from elements_history, or nil if there is none: version is 1 (a
// create has nothing before it) or the history has a gap.
func (s *Store) PreviousVersion(id int64, typ string, version int) (*RecentChange, error) {
	changes, err := scanChanges(s.db.Query(`
		SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags
		FROM elements_history
		WHERE id = ? AND type = ? AND version = ?
	`, id, typ, version-1))
	if err != nil || len(changes) == 0 {
		return nil, err
	}
	return &changes[0], nil
}

// scanChanges reads the rows of a query selecting the columns QueryRecent,
// QueryLatest, QueryBefore and PreviousVersion all share.
func scanChanges(rows *sql.Rows, err error) ([]RecentChange, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var changes []RecentChange
	for rows.Next() {
		var c RecentChange
		var tagsJSON *string
		if err := rows.Scan(&c.ID, &c.Type, &c.Version, &c.Lat, &c.Lon, &c.Timestamp, &c.User, &c.UID, &c.Changeset, &tagsJSON); err != nil {
			return nil, err
		}
		c.Tags = make(map[string]string)
		if tagsJSON != nil {
			if err := json.Unmarshal([]byte(*tagsJSON), &c.Tags); err != nil {
				return nil, fmt.Errorf("unmarshaling tags: %w", err)
			}
		}
		c.Name = c.Tags["name"]
		changes = append(changes, c)
	}
	return changes, rows.Err()
}
