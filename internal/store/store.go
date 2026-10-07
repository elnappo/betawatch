// Package store keeps climbing elements in SQLite: the live version of
// each in elements, and the versions it replaced in elements_history.
package store

import (
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"modernc.org/sqlite"
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

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(
		"regexp",
		2,
		func(
			_ *sqlite.FunctionContext,
			args []driver.Value,
		) (driver.Value, error) {
			if args[0] == nil || args[1] == nil {
				return false, nil
			}

			pattern := fmt.Sprint(args[0])
			value := fmt.Sprint(args[1])

			return regexp.MatchString(pattern, value)
		},
	)
}

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

	// roDB is a second, read-only connection to the same file, used only
	// to run operator-authored rule queries (RunRule). SQLite itself
	// rejects a write through it, so a mistake in a rule's SQL cannot
	// corrupt the store it is reading.
	roDB *sql.DB
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

	// mode=ro opens after the schema above has run, so the file already
	// exists; journal_mode is a database-level setting already fixed by
	// the read-write connection, so it is not repeated here.
	roq := url.Values{}
	roq.Add("_pragma", "busy_timeout(5000)")
	roq.Add("mode", "ro")
	roDB, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Opaque: url.PathEscape(path), RawQuery: roq.Encode()}).String())
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, roDB: roDB}, nil
}

func (s *Store) Close() error {
	err := s.db.Close()
	if roErr := s.roDB.Close(); err == nil {
		err = roErr
	}
	return err
}

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

// RuleMatch is one element a problem rule's query returned.
type RuleMatch struct {
	ID   int64
	Type string
}

// RunRule executes an operator-authored rule query (see internal/config's
// Rule) and returns the (id, type) pairs it selected. It runs against
// roDB, a connection SQLite itself rejects writes on, so a mistake in a
// rule's SQL cannot corrupt the store it is reading; the query must
// select exactly two columns, id then type.
func (s *Store) RunRule(query string) ([]RuleMatch, error) {
	rows, err := s.roDB.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RuleMatch
	for rows.Next() {
		var m RuleMatch
		if err := rows.Scan(&m.ID, &m.Type); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Stats holds aggregate counts for the stats page. In-scope means the
// element's climbing tag is one of: route, route_bottom, boulder, crag, area.
type Stats struct {
	// When these numbers were computed.
	UpdatedAt time.Time `json:"updated_at"`

	// Total in-scope elements, counted once.
	Total int `json:"total"`

	// Totals by climbing tag value.
	ByClimbing map[string]ByType `json:"by_climbing"`

	// Elements per style. climbing:style != 'no' counts as present.
	Styles map[string]int `json:"styles"`

	// Elements per grade system (the third segment of climbing:grade:*).
	GradeSystems map[string]int `json:"grade_systems"`

	// Completeness metrics, as "have/total" pairs.
	RoutesWithName      [2]int `json:"routes_with_name"`
	CragsWithName       [2]int `json:"crags_with_name"`
	RoutesWithStyle     [2]int `json:"routes_with_style"`
	CragsWithStyle      [2]int `json:"crags_with_style"`
	RoutesWithGrade     [2]int `json:"routes_with_grade"`
	RoutesWithLength    [2]int `json:"routes_with_length"`
	CragsAreasWithPhoto [2]int `json:"crags_areas_with_photo"`
	CragsAreasWithURL   [2]int `json:"crags_areas_with_url"`
}

// ByType splits a count into nodes, ways and relations.
type ByType struct {
	Total     int `json:"total"`
	Nodes     int `json:"nodes"`
	Ways      int `json:"ways"`
	Relations int `json:"relations"`
}

// Stats returns aggregate statistics about in-scope climbing elements.
// The scope is climbing tag values: route, route_bottom, boulder, crag, area.
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{
		UpdatedAt:    time.Now().UTC(),
		ByClimbing:   make(map[string]ByType),
		Styles:       make(map[string]int),
		GradeSystems: make(map[string]int),
	}

	// Total in-scope elements.
	if err := s.roDB.QueryRow(`
		SELECT COUNT(*)
		FROM elements
		WHERE json_extract(tags, '$.climbing') IN ('route', 'route_bottom', 'boulder', 'crag', 'area', 'anchor')
	`).Scan(&st.Total); err != nil {
		return nil, err
	}

	// Totals by climbing tag, split by type.
	rows, err := s.roDB.Query(`
		SELECT
			json_extract(tags, '$.climbing') AS c,
			type,
			COUNT(*) AS cnt
		FROM elements
		WHERE json_extract(tags, '$.climbing') IN ('route', 'route_bottom', 'boulder', 'crag', 'area', 'anchor')
		GROUP BY c, type
		ORDER BY c
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var c string
		var typ string
		var cnt int
		if err := rows.Scan(&c, &typ, &cnt); err != nil {
			return nil, err
		}
		bt := st.ByClimbing[c]
		bt.Total += cnt
		switch typ {
		case "n":
			bt.Nodes = cnt
		case "w":
			bt.Ways = cnt
		case "r":
			bt.Relations = cnt
		}
		st.ByClimbing[c] = bt
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Styles: sport, trad, boulder, deepwater, ice, mixed, dry, aid.
	styles := []string{"sport", "trad", "boulder", "deepwater", "ice", "mixed", "dry", "aid"}
	for _, style := range styles {
		var count int
		key := "climbing:" + style
		if err := s.roDB.QueryRow(`
			SELECT COUNT(DISTINCT id || ':' || type)
			FROM elements
			WHERE json_extract(tags, '$.climbing') IN ('route', 'route_bottom', 'boulder', 'crag', 'area')
			  AND json_extract(tags, '$."` + key + `"') IS NOT NULL
			  AND json_extract(tags, '$."` + key + `"') != 'no'
		`).Scan(&count); err != nil {
			return nil, err
		}
		st.Styles[key] = count
	}

	// Grade systems: extract the third segment from climbing:grade:* keys.
	rows, err = s.roDB.Query(`
		SELECT
			SUBSTR(je.key, 16) AS system,
			COUNT(DISTINCT e.id || ':' || e.type) AS cnt
		FROM elements e, json_each(e.tags) AS je
		WHERE json_extract(e.tags, '$.climbing') IN ('route', 'route_bottom', 'boulder', 'crag', 'area')
		  AND je.key LIKE 'climbing:grade:%'
		  AND je.key NOT LIKE '%:min'
		  AND je.key NOT LIKE '%:max'
		  AND je.key NOT LIKE '%:mean'
		GROUP BY system
		ORDER BY cnt DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var system string
		var count int
		if err := rows.Scan(&system, &count); err != nil {
			return nil, err
		}
		st.GradeSystems["climbing:grade:"+system] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Completeness: crags with name.
	if err := s.roDB.QueryRow(`
		SELECT
			COUNT(CASE WHEN json_extract(tags, '$.name') IS NOT NULL THEN 1 END),
			COUNT(*)
		FROM elements
		WHERE json_extract(tags, '$.climbing') = 'crag'
	`).Scan(&st.CragsWithName[0], &st.CragsWithName[1]); err != nil {
		return nil, err
	}

	// Completeness: routes and crags with at least one style set.
	const hasStyle = `EXISTS (
		SELECT 1 FROM json_each(tags)
		WHERE key IN ('climbing:sport', 'climbing:trad', 'climbing:boulder', 'climbing:deepwater', 'climbing:ice', 'climbing:mixed', 'climbing:dry', 'climbing:aid')
		  AND value != 'no'
	)`
	if err := s.roDB.QueryRow(`
		SELECT
			COUNT(CASE WHEN `+hasStyle+` THEN 1 END),
			COUNT(*)
		FROM elements
		WHERE json_extract(tags, '$.climbing') IN ('route', 'route_bottom')
	`).Scan(&st.RoutesWithStyle[0], &st.RoutesWithStyle[1]); err != nil {
		return nil, err
	}
	if err := s.roDB.QueryRow(`
		SELECT
			COUNT(CASE WHEN `+hasStyle+` THEN 1 END),
			COUNT(*)
		FROM elements
		WHERE json_extract(tags, '$.climbing') = 'crag'
	`).Scan(&st.CragsWithStyle[0], &st.CragsWithStyle[1]); err != nil {
		return nil, err
	}

	// Completeness: routes with name.
	if err := s.roDB.QueryRow(`
		SELECT
			COUNT(CASE WHEN json_extract(tags, '$.name') IS NOT NULL THEN 1 END),
			COUNT(*)
		FROM elements
		WHERE json_extract(tags, '$.climbing') IN ('route', 'route_bottom')
	`).Scan(&st.RoutesWithName[0], &st.RoutesWithName[1]); err != nil {
		return nil, err
	}

	// Completeness: routes with grade.
	if err := s.roDB.QueryRow(`
		SELECT COUNT(DISTINCT id || ':' || type)
		FROM elements
		WHERE json_extract(tags, '$.climbing') IN ('route', 'route_bottom')
		  AND EXISTS (SELECT 1 FROM json_each(tags) WHERE key LIKE 'climbing:grade:%')
	`).Scan(&st.RoutesWithGrade[0]); err != nil {
		return nil, err
	}
	if err := s.roDB.QueryRow(`
		SELECT COUNT(DISTINCT id || ':' || type)
		FROM elements
		WHERE json_extract(tags, '$.climbing') IN ('route', 'route_bottom')
	`).Scan(&st.RoutesWithGrade[1]); err != nil {
		return nil, err
	}

	// Completeness: routes with length.
	if err := s.roDB.QueryRow(`
		SELECT
			COUNT(CASE WHEN json_extract(tags, '$."climbing:length"') IS NOT NULL THEN 1 END),
			COUNT(*)
		FROM elements
		WHERE json_extract(tags, '$.climbing') IN ('route', 'route_bottom')
	`).Scan(&st.RoutesWithLength[0], &st.RoutesWithLength[1]); err != nil {
		return nil, err
	}

	// Completeness: crags and areas with photo.
	if err := s.roDB.QueryRow(`
		SELECT
			COUNT(CASE WHEN json_extract(tags, '$.wikimedia_commons') IS NOT NULL THEN 1 END),
			COUNT(*)
		FROM elements
		WHERE json_extract(tags, '$.climbing') IN ('crag', 'area')
	`).Scan(&st.CragsAreasWithPhoto[0], &st.CragsAreasWithPhoto[1]); err != nil {
		return nil, err
	}

	// Completeness: crags and areas with URL.
	if err := s.roDB.QueryRow(`
		SELECT
			COUNT(CASE WHEN json_extract(tags, '$.website') IS NOT NULL OR json_extract(tags, '$.url') IS NOT NULL THEN 1 END),
			COUNT(*)
		FROM elements
		WHERE json_extract(tags, '$.climbing') IN ('crag', 'area')
	`).Scan(&st.CragsAreasWithURL[0], &st.CragsAreasWithURL[1]); err != nil {
		return nil, err
	}

	return st, nil
}

// queryElementsBatch is how many (id, type) pairs go in one query's IN
// clause. SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 999 (2 bind
// parameters per pair), and a single broad rule can match tens of
// thousands of elements, so QueryElements must page through its keys
// rather than bind them all at once.
const queryElementsBatch = 400

// QueryElements returns the live elements matching the given (id, type)
// pairs, for turning a rule's raw id/type hits into full records to
// display. A pair that is not currently live (a rule can point at
// something since deleted) is silently omitted.
func (s *Store) QueryElements(keys []RuleMatch) ([]RecentChange, error) {
	var out []RecentChange
	for len(keys) > 0 {
		n := min(len(keys), queryElementsBatch)
		batch, err := s.queryElementsBatch(keys[:n])
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		keys = keys[n:]
	}
	return out, nil
}

func (s *Store) queryElementsBatch(keys []RuleMatch) ([]RecentChange, error) {
	placeholders := make([]string, len(keys))
	args := make([]any, 0, len(keys)*2)
	for i, k := range keys {
		placeholders[i] = "(?, ?)"
		args = append(args, k.ID, k.Type)
	}
	return scanChanges(s.db.Query(`
		SELECT `+changesColumns+` FROM (
			SELECT id, type, version, lat, lon, timestamp, user, uid, changeset_id, tags, 0 AS deleted FROM elements
		) u `+changesetJoin+`
		WHERE (u.id, u.type) IN (`+strings.Join(placeholders, ",")+`)
	`, args...))
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
