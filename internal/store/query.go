package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

// QueryResult is what Query returns: the rows of one SELECT.
type QueryResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	// Truncated is set when the query had more rows than the limit.
	Truncated bool `json:"truncated"`
}

// Query runs one read-only statement written by an untrusted caller (an
// LLM, through the MCP endpoint) and returns at most limit rows. Unlike
// RunRule it does not trust its input: see checkReadOnly. A cancelled ctx
// interrupts the statement inside SQLite, which is what bounds a runaway
// scan of elements.
func (s *Store) Query(ctx context.Context, query string, args []any, limit int) (*QueryResult, error) {
	if err := checkReadOnly(query); err != nil {
		return nil, err
	}
	for i, a := range args {
		args[i] = bindArg(a)
	}

	rows, err := s.queryDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &QueryResult{Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		if len(res.Rows) == limit {
			// One row beyond the limit is read only to learn there is more.
			res.Truncated = true
			break
		}
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range row {
			// BLOBs and TEXT both arrive as []byte from an untyped scan,
			// and encoding/json would base64 them.
			if b, ok := v.([]byte); ok {
				row[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// bindArg turns a JSON number back into an integer where it is one: JSON
// has a single number type, so an id arrives as a float64, and SQLite
// would bind it as REAL.
func bindArg(a any) any {
	if f, ok := a.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return int64(f)
	}
	return a
}

// checkReadOnly rejects anything but a single SELECT, WITH or VALUES
// statement. The queryDB connection is already mode=ro, so SQLite refuses
// a write whatever this lets through (including a WITH ... DELETE). What
// mode=ro does not stop is ATTACH, which can create a new file, and
// PRAGMA, so the statement must start with one of those words and hold no
// second statement. The driver exposes no authorizer to do this properly.
func checkReadOnly(query string) error {
	rest, err := afterFirstStatement(query)
	if err != nil {
		return err
	}
	if rest != "" {
		return errors.New("only a single statement is allowed")
	}

	word := stripLeading(query)
	if end := strings.IndexFunc(word, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}); end >= 0 {
		word = word[:end]
	}
	switch strings.ToUpper(word) {
	case "SELECT", "WITH", "VALUES":
		return nil
	}
	return errors.New("only SELECT or WITH statements are allowed")
}

// stripLeading drops leading whitespace and comments, so that a comment
// in front of SELECT does not hide it.
func stripLeading(q string) string {
	for {
		q = strings.TrimLeft(q, " \t\r\n")
		switch {
		case strings.HasPrefix(q, "--"):
			_, after, found := strings.Cut(q, "\n")
			if !found {
				return ""
			}
			q = after
		case strings.HasPrefix(q, "/*"):
			_, after, found := strings.Cut(q[2:], "*/")
			if !found {
				return ""
			}
			q = after
		default:
			return q
		}
	}
}

// afterFirstStatement scans q as SQLite does, skipping string literals,
// quoted identifiers and comments so that a ";" inside one is not taken
// for a statement end. It returns what follows the first real ";" once
// whitespace and comments are dropped: "" for a single statement (with or
// without a trailing ";"), non-empty if a second one follows.
func afterFirstStatement(q string) (string, error) {
	for i := 0; i < len(q); i++ {
		switch c := q[i]; c {
		case '\'', '"', '`', '[':
			closer := c
			if c == '[' {
				closer = ']'
			}
			// A doubled quote inside a quoted string is an escaped quote;
			// brackets have no escape.
			j := i + 1
			for {
				k := strings.IndexByte(q[j:], closer)
				if k < 0 {
					return "", fmt.Errorf("unterminated %c in query", c)
				}
				j += k + 1
				if c == '[' || j >= len(q) || q[j] != closer {
					break
				}
				j++
			}
			i = j - 1
		case '-':
			if i+1 < len(q) && q[i+1] == '-' {
				nl := strings.IndexByte(q[i:], '\n')
				if nl < 0 {
					return "", nil
				}
				i += nl
			}
		case '/':
			if i+1 < len(q) && q[i+1] == '*' {
				end := strings.Index(q[i+2:], "*/")
				if end < 0 {
					return "", nil
				}
				i += 2 + end + 1
			}
		case ';':
			return stripLeading(q[i+1:]), nil
		}
	}
	return "", nil
}

// Schema describes the database for a caller that has not seen it.
type Schema struct {
	Tables []Table `json:"tables"`
}

type Table struct {
	Name        string       `json:"name"`
	SQL         string       `json:"sql"`
	Columns     []Column     `json:"columns"`
	Indexes     []Index      `json:"indexes"`
	ForeignKeys []ForeignKey `json:"foreign_keys"`
}

type Column struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	NotNull    bool    `json:"not_null"`
	Default    *string `json:"default,omitempty"`
	PrimaryKey int     `json:"primary_key,omitempty"` // 1-based position in the key, 0 if not part of it
}

type Index struct {
	Name    string   `json:"name"`
	Unique  bool     `json:"unique"`
	Origin  string   `json:"origin"` // "c" CREATE INDEX, "u" UNIQUE constraint, "pk" PRIMARY KEY
	Columns []string `json:"columns"`
}

type ForeignKey struct {
	Column    string `json:"column"`
	RefTable  string `json:"ref_table"`
	RefColumn string `json:"ref_column"`
}

// Schema reads the tables, columns, indexes and foreign keys from SQLite
// itself, so it cannot drift from schema.sql.
func (s *Store) Schema(ctx context.Context) (*Schema, error) {
	rows, err := s.queryDB.QueryContext(ctx, `
		SELECT name, COALESCE(sql, '') FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var tables []Table
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Name, &t.SQL); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range tables {
		t := &tables[i]
		if t.Columns, err = s.columns(ctx, t.Name); err != nil {
			return nil, err
		}
		if t.Indexes, err = s.indexes(ctx, t.Name); err != nil {
			return nil, err
		}
		if t.ForeignKeys, err = s.foreignKeys(ctx, t.Name); err != nil {
			return nil, err
		}
	}
	return &Schema{Tables: tables}, nil
}

func (s *Store) columns(ctx context.Context, table string) ([]Column, error) {
	rows, err := s.queryDB.QueryContext(ctx,
		`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := []Column{}
	for rows.Next() {
		var c Column
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &c.PrimaryKey); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func (s *Store) indexes(ctx context.Context, table string) ([]Index, error) {
	rows, err := s.queryDB.QueryContext(ctx,
		`SELECT name, "unique", origin FROM pragma_index_list(?) ORDER BY seq`, table)
	if err != nil {
		return nil, err
	}
	idxs := []Index{}
	for rows.Next() {
		var ix Index
		if err := rows.Scan(&ix.Name, &ix.Unique, &ix.Origin); err != nil {
			rows.Close()
			return nil, err
		}
		idxs = append(idxs, ix)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range idxs {
		cols, err := s.queryDB.QueryContext(ctx,
			`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, idxs[i].Name)
		if err != nil {
			return nil, err
		}
		idxs[i].Columns = []string{}
		for cols.Next() {
			var name string
			if err := cols.Scan(&name); err != nil {
				cols.Close()
				return nil, err
			}
			idxs[i].Columns = append(idxs[i].Columns, name)
		}
		cols.Close()
		if err := cols.Err(); err != nil {
			return nil, err
		}
	}
	return idxs, nil
}

func (s *Store) foreignKeys(ctx context.Context, table string) ([]ForeignKey, error) {
	rows, err := s.queryDB.QueryContext(ctx,
		`SELECT "from", "table", "to" FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fks := []ForeignKey{}
	for rows.Next() {
		var fk ForeignKey
		if err := rows.Scan(&fk.Column, &fk.RefTable, &fk.RefColumn); err != nil {
			return nil, err
		}
		fks = append(fks, fk)
	}
	return fks, rows.Err()
}
