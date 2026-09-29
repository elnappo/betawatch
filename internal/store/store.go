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

	// The changeset's timestamp is the earliest element timestamp seen,
	// because the diffs do not carry the changeset itself.
	if _, err := t.tx.Exec(`
		INSERT INTO changesets (id, timestamp, uid, user) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET timestamp = min(timestamp, excluded.timestamp)`,
		el.Changeset, ts(el.Timestamp), el.UID, el.User); err != nil {
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
