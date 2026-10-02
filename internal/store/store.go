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

// ChangesetMetadata is the changeset information the minute diffs never
// carry, fetched separately from the OSM API. changes_count has no field
// here: github.com/paulmach/osm's Changeset.ChangesCount is tagged
// `xml:"num_changes"`, the API's old attribute name, so it always decodes
// to 0 against the live API, which now sends changes_count instead. NULL
// is the honest value until that's fixed upstream.
type ChangesetMetadata struct {
	CreatedAt                                       time.Time
	Comment, CreatedBy, ImageryUsed, Source, Locale string
	DataUsed, Hashtags, Host                        string
	Bot, ReviewRequested                            bool
	ChangesetsCount                                 int
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
// elements_history. A version equal to the live one is a replay and is
// ignored, so replaying a minute after a crash changes nothing. An older
// version is not live state, but if it is the one right below the live
// version, it fills what would otherwise be a gap in elements_history, so
// it is stored there; anything older than that adds nothing
// PreviousVersion can use and is ignored.
func (t *Tx) Save(el Element) error {
	cur, err := t.version(el.ID, el.Type)
	if err != nil {
		return err
	}
	if el.Version < cur {
		if cur-el.Version == 1 {
			return t.saveHistory(el, false)
		}
		return nil
	}
	if el.Version == cur {
		return nil
	}

	if err := t.upsertChangeset(el); err != nil {
		return err
	}

	if cur > 0 {
		if err := t.retire(el.ID, el.Type); err != nil {
			return err
		}
	}
	tags, err := marshalTags(el.Tags)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`
		INSERT INTO elements (id, type, version, lat, lon, uid, user, timestamp, changeset_id, tags)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		el.ID, el.Type, el.Version, el.Lat, el.Lon, el.UID, el.User, ts(el.Timestamp), el.Changeset, tags)
	return err
}

// upsertChangeset inserts a changeset row if missing. The diffs do not
// carry the changeset itself, only which elements belong to it, so
// timestamp and the metadata columns stay NULL until SetChangesetMetadata
// fills them in from the OSM API.
func (t *Tx) upsertChangeset(el Element) error {
	_, err := t.tx.Exec(`
		INSERT INTO changesets (id, uid, user) VALUES (?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		el.Changeset, el.UID, el.User)
	return err
}

// NeedsChangesetMetadata reports whether a changeset has not yet had its
// OSM API metadata fetched. timestamp is the tell: it is unset until
// SetChangesetMetadata fills it with the changeset's own created_at, which
// the API always returns, so a changeset whose other fields (comment, say)
// turn out genuinely empty is still correctly seen as fetched. A changeset
// not stored at all is also reported as not needing it: with nothing in the
// diffs to attach the fetch to, there is nothing to call this before.
func (s *Store) NeedsChangesetMetadata(id int64) (bool, error) {
	var needed bool
	err := s.db.QueryRow(`SELECT timestamp IS NULL FROM changesets WHERE id = ?`, id).Scan(&needed)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return needed, err
}

// SetChangesetMetadata stores a changeset's OSM API metadata, including its
// created_at as timestamp, which also marks it fetched: see
// NeedsChangesetMetadata. Runs outside any element transaction: the fetch
// is a network call, which must not hold a SQLite write lock open while it
// waits.
func (s *Store) SetChangesetMetadata(id int64, m ChangesetMetadata) error {
	_, err := s.db.Exec(`
		UPDATE changesets SET
			timestamp = ?, comment = ?, created_by = ?, imagery_used = ?, source = ?, locale = ?,
			data_used = ?, hashtags = ?, host = ?, bot = ?, review_requested = ?,
			changesets_count = ?
		WHERE id = ?`,
		ts(m.CreatedAt), m.Comment, m.CreatedBy, m.ImageryUsed, m.Source, m.Locale,
		m.DataUsed, m.Hashtags, m.Host, m.Bot, m.ReviewRequested,
		m.ChangesetsCount, id)
	return err
}

// saveHistory writes el directly into elements_history, without touching
// the live elements row. Used for a version that arrives after a newer one
// is already live, so it can never become live itself. ON CONFLICT DO
// NOTHING makes repeating it, e.g. on replay, harmless.
func (t *Tx) saveHistory(el Element, deleted bool) error {
	if err := t.upsertChangeset(el); err != nil {
		return err
	}
	tags, err := marshalTags(el.Tags)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(`
		INSERT INTO elements_history
			(id, type, version, lat, lon, uid, user, timestamp, changeset_id, tags, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id, type, version) DO NOTHING`,
		el.ID, el.Type, el.Version, el.Lat, el.Lon, el.UID, el.User, ts(el.Timestamp), el.Changeset, tags, deleted)
	return err
}

// marshalTags encodes tags as the JSON stored in the tags column, or nil
// for an untagged element.
func marshalTags(tags map[string]string) (any, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Delete moves a live element to elements_history with deleted set, using
// el's own version, timestamp, user and changeset: a delete is its own
// event, distinct from the edit that made the element's last live version,
// and the feed needs that event's own timestamp to place it correctly. A
// delete carries no tags, so el can only be matched by id and type against
// what is stored; one for an element that is not stored, or that is not
// newer than what is live, is ignored. It reports whether an element was
// moved.
func (t *Tx) Delete(el Element) (bool, error) {
	cur, err := t.version(el.ID, el.Type)
	if err != nil || cur == 0 || cur >= el.Version {
		return false, err
	}
	return true, t.retireDeleted(el)
}

// retire moves the live row of an element into elements_history unchanged,
// keeping its own version, timestamp, user and changeset: used when a newer
// version is about to become live, so the row it replaces is still that
// version's own event.
func (t *Tx) retire(id int64, typ string) error {
	if _, err := t.tx.Exec(`
		INSERT OR IGNORE INTO elements_history
			(id, type, version, lat, lon, uid, user, timestamp, changeset_id, tags, deleted)
		SELECT id, type, version, lat, lon, uid, user, timestamp, changeset_id, tags, 0
		FROM elements WHERE id = ? AND type = ?`, id, typ); err != nil {
		return err
	}
	_, err := t.tx.Exec(`DELETE FROM elements WHERE id = ? AND type = ?`, id, typ)
	return err
}

// retireDeleted moves the live row of el's element into elements_history as
// a delete, stamped with el's own version, timestamp, uid, user and
// changeset rather than the live row's: the delete is its own event. lat,
// lon and tags come from the live row, since a delete carries none of its
// own.
func (t *Tx) retireDeleted(el Element) error {
	if err := t.upsertChangeset(el); err != nil {
		return err
	}
	if _, err := t.tx.Exec(`
		INSERT OR IGNORE INTO elements_history
			(id, type, version, lat, lon, uid, user, timestamp, changeset_id, tags, deleted)
		SELECT id, type, ?, lat, lon, ?, ?, ?, ?, tags, 1
		FROM elements WHERE id = ? AND type = ?`,
		el.Version, el.UID, el.User, ts(el.Timestamp), el.Changeset, el.ID, el.Type); err != nil {
		return err
	}
	_, err := t.tx.Exec(`DELETE FROM elements WHERE id = ? AND type = ?`, el.ID, el.Type)
	return err
}

// ts formats a time so that text order is time order.
func ts(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// RecentChange is an element record with metadata from the database.
type RecentChange struct {
	ID                 int64
	Type               string
	Version            int
	Lat, Lon           *float64 // nodes only
	Timestamp          time.Time
	User               string
	UID                int64
	Changeset          int64
	Tags               map[string]string
	Name               string
	Deleted            bool
	Comment, CreatedBy *string // nil until the changeset's OSM API metadata is fetched
}

// changesColumns is the column list QueryRecent, QueryLatest, QueryBefore
// and PreviousVersion all select, shared so their two arms (elements,
// elements_history) and scanChanges agree on shape. elements holds no
// deleted column, since a live row is by definition not deleted. Columns
// are qualified against the "u" alias every caller gives its element
// union/table, since the changesets join below adds a second id column.
const changesColumns = `u.id, u.type, u.version, u.lat, u.lon, u.timestamp, u.user, u.uid, u.changeset_id, u.tags, u.deleted, cs.comment, cs.created_by`

// changesetJoin attaches each element row's changeset metadata, left so a
// changeset not yet fetched (or, for an old row, not stored at all) still
// returns the element with NULL comment/created_by rather than dropping it.
const changesetJoin = `LEFT JOIN changesets cs ON cs.id = u.changeset_id`

// QueryRecent returns every element version recorded since the given
// timestamp, live or superseded, ordered by timestamp ascending. A version
// superseded within the polling interval, or a delete, is visible here as
// its own row, not just the version that ended up live.
func (s *Store) QueryRecent(since time.Time) ([]RecentChange, error) {
	return scanChanges(s.db.Query(`
		SELECT `+changesColumns+` FROM (
			SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags, 0 AS deleted FROM elements
			UNION ALL
			SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags, deleted FROM elements_history
		) u `+changesetJoin+`
		WHERE u.timestamp > ?
		ORDER BY u.timestamp ASC
	`, ts(since)))
}

// QueryLatest returns the most recently recorded element versions, live or
// superseded, newest first, up to limit. The (id, type, version) tiebreak
// matches QueryBefore's, so paging from this page into the next never skips
// or repeats a row that shares a timestamp with the page boundary.
func (s *Store) QueryLatest(limit int) ([]RecentChange, error) {
	return scanChanges(s.db.Query(`
		SELECT `+changesColumns+` FROM (
			SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags, 0 AS deleted FROM elements
			UNION ALL
			SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags, deleted FROM elements_history
		) u `+changesetJoin+`
		ORDER BY u.timestamp DESC, u.id DESC, u.type DESC, u.version DESC
		LIMIT ?
	`, limit))
}

// QueryBefore returns element versions, live or superseded, ordered before
// the given (timestamp, id, type, version), newest first, up to limit. It
// is how the web view pages into the past. The full tuple is needed, not
// just the timestamp: many versions share a timestamp (a single backfill
// batch commonly holds dozens), and since this change the same (id, type)
// can also appear twice at different versions, so cutting off on anything
// less than the full tuple would drop or repeat rows whenever a page
// boundary landed inside such a group.
func (s *Store) QueryBefore(before time.Time, id int64, typ string, version, limit int) ([]RecentChange, error) {
	return scanChanges(s.db.Query(`
		SELECT `+changesColumns+` FROM (
			SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags, 0 AS deleted FROM elements
			UNION ALL
			SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags, deleted FROM elements_history
		) u `+changesetJoin+`
		WHERE (u.timestamp, u.id, u.type, u.version) < (?, ?, ?, ?)
		ORDER BY u.timestamp DESC, u.id DESC, u.type DESC, u.version DESC
		LIMIT ?
	`, ts(before), id, typ, version, limit))
}

// PreviousVersion returns the version just before version for an element,
// read from elements_history, or nil if there is none: version is 1 (a
// create has nothing before it) or the history has a gap.
func (s *Store) PreviousVersion(id int64, typ string, version int) (*RecentChange, error) {
	changes, err := scanChanges(s.db.Query(`
		SELECT `+changesColumns+`
		FROM elements_history u `+changesetJoin+`
		WHERE u.id = ? AND u.type = ? AND u.version = ?
	`, id, typ, version-1))
	if err != nil || len(changes) == 0 {
		return nil, err
	}
	return &changes[0], nil
}

// scanChanges reads the rows of a query selecting changesColumns.
func scanChanges(rows *sql.Rows, err error) ([]RecentChange, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var changes []RecentChange
	for rows.Next() {
		var c RecentChange
		var tagsJSON *string
		if err := rows.Scan(&c.ID, &c.Type, &c.Version, &c.Lat, &c.Lon, &c.Timestamp, &c.User, &c.UID, &c.Changeset, &tagsJSON, &c.Deleted, &c.Comment, &c.CreatedBy); err != nil {
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
