package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestCheckReadOnly(t *testing.T) {
	ok := []string{
		`SELECT 1`,
		`select 1;`,
		"  \n SELECT 1 ; ",
		`-- hi` + "\n" + `SELECT 1`,
		`/* hi */ SELECT 1`,
		`WITH x AS (SELECT 1) SELECT * FROM x`,
		`VALUES (1)`,
		`SELECT ';'`,
		`SELECT 'it''s; fine'`,
		`SELECT "a;b" FROM (SELECT 1 AS "a;b")`,
		`SELECT 1 -- ; DROP TABLE elements`,
		`SELECT 1; -- trailing comment`,
		`SELECT 1 /* ; */`,
	}
	for _, q := range ok {
		if err := checkReadOnly(q); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", q, err)
		}
	}

	bad := []string{
		``,
		`ATTACH DATABASE 'x.db' AS x`,
		`PRAGMA writable_schema = 1`,
		`INSERT INTO elements DEFAULT VALUES`,
		`DELETE FROM elements`,
		`DROP TABLE elements`,
		`VACUUM INTO 'x.db'`,
		`SELECT 1; DROP TABLE elements`,
		`SELECT 1; ATTACH 'x.db' AS x`,
		`SELECT 'a'; SELECT 'b'`,
		`SELECT 1;/* c */ATTACH 'x.db' AS x`,
		`/* SELECT */ ATTACH 'x.db' AS x`,
		`-- SELECT` + "\n" + `ATTACH 'x.db' AS x`,
		`SELECT 'unterminated`,
		`SELECTX 1`,
	}
	for _, q := range bad {
		if err := checkReadOnly(q); err == nil {
			t.Errorf("checkReadOnly(%q) = nil, want an error", q)
		}
	}
}

func TestQueryReturnsRowsAndBindsArgs(t *testing.T) {
	s := open(t)
	apply(t, s, func(tx *Tx) {
		if err := tx.Save(el(1, time.Now())); err != nil {
			t.Fatal(err)
		}
	})

	// 1 arrives from JSON as a float64, which must still match an
	// INTEGER column rather than binding as REAL.
	res, err := s.Query(context.Background(),
		`SELECT id, type, lat, json_extract(tags, '$.sport') FROM elements WHERE id = ?`,
		[]any{float64(1)}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Columns, []string{"id", "type", "lat", "json_extract(tags, '$.sport')"}) {
		t.Errorf("columns = %v", res.Columns)
	}
	if len(res.Rows) != 1 || res.Truncated {
		t.Fatalf("rows = %v, truncated = %v", res.Rows, res.Truncated)
	}
	row := res.Rows[0]
	if row[0] != int64(1) || row[1] != "n" || row[2] != 47.5 || row[3] != "climbing" {
		t.Errorf("row = %#v", row)
	}
}

func TestQueryTruncatesAtLimit(t *testing.T) {
	s := open(t)
	q := `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 10) SELECT i FROM n`

	res, err := s.Query(context.Background(), q, nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 3 || !res.Truncated {
		t.Errorf("limit 3: rows = %d, truncated = %v", len(res.Rows), res.Truncated)
	}

	// Exactly the limit is not truncated.
	res, err = s.Query(context.Background(), q, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 10 || res.Truncated {
		t.Errorf("limit 10: rows = %d, truncated = %v", len(res.Rows), res.Truncated)
	}
}

func TestQueryHonoursContext(t *testing.T) {
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// An unbounded recursive CTE never returns a row to stop at.
	start := time.Now()
	_, err := s.Query(ctx, `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n)
		SELECT count(*) FROM n`, nil, 10)
	if err == nil {
		t.Fatal("want an error from the expired context")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v to stop", d)
	}
}

// The guard is one layer; these cover the other, which is what actually
// holds if a statement slips past it.
func TestQueryCannotWrite(t *testing.T) {
	s := open(t)
	apply(t, s, func(tx *Tx) {
		if err := tx.Save(el(1, time.Now())); err != nil {
			t.Fatal(err)
		}
	})

	attempts := []string{
		`WITH x AS (SELECT 1) DELETE FROM elements`,
		`WITH x AS (SELECT 1) INSERT INTO changesets (id) VALUES (999)`,
		`WITH x AS (SELECT 1) UPDATE elements SET version = 9`,
		`SELECT 1; DELETE FROM elements`,
	}
	for _, q := range attempts {
		if _, err := s.Query(context.Background(), q, nil, 10); err == nil {
			t.Errorf("Query(%q) succeeded", q)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM elements WHERE version = 1`); n != 1 {
		t.Errorf("elements changed: %d rows at version 1, want 1", n)
	}
	if n := count(t, s, `SELECT count(*) FROM changesets WHERE id = 999`); n != 0 {
		t.Error("changeset 999 was inserted")
	}
}

func TestQueryCannotAttach(t *testing.T) {
	s := open(t)
	target := filepath.Join(t.TempDir(), "evil.db")
	for _, q := range []string{
		`ATTACH DATABASE '` + target + `' AS x`,
		`SELECT 1; ATTACH DATABASE '` + target + `' AS x`,
		`PRAGMA query_only = 0`,
	} {
		if _, err := s.Query(context.Background(), q, nil, 10); err == nil {
			t.Errorf("Query(%q) succeeded", q)
		}
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("ATTACH created a file")
	}
}

func TestSchema(t *testing.T) {
	s := open(t)
	sch, err := s.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]Table{}
	for _, tb := range sch.Tables {
		byName[tb.Name] = tb
	}
	for _, name := range []string{"changesets", "elements", "elements_history"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("table %s missing from %v", name, sch.Tables)
		}
	}

	el := byName["elements"]
	var tags *Column
	for i, c := range el.Columns {
		if c.Name == "tags" {
			tags = &el.Columns[i]
		}
	}
	if tags == nil || tags.Type != "TEXT" || tags.NotNull {
		t.Errorf("tags column = %+v", tags)
	}

	var sawPK, sawTimestamp bool
	for _, ix := range el.Indexes {
		switch {
		case ix.Origin == "pk" && slices.Equal(ix.Columns, []string{"id", "type"}):
			sawPK = true
		case ix.Name == "idx_elements_timestamp" && slices.Equal(ix.Columns, []string{"timestamp"}):
			sawTimestamp = true
		}
	}
	if !sawPK || !sawTimestamp {
		t.Errorf("elements indexes = %+v", el.Indexes)
	}

	if fks := el.ForeignKeys; len(fks) != 1 || fks[0] != (ForeignKey{"changeset_id", "changesets", "id"}) {
		t.Errorf("elements foreign keys = %+v", fks)
	}
}
