package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/elnappo/betawatch/internal/store"
)

// connect serves a store holding one node over HTTP and returns an MCP
// client session to it, so the tests cover the wire format as well.
func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	lat, lon := 47.5, 11.2
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(store.Element{
		ID: 42, Type: store.Node, Version: 1, Lat: &lat, Lon: &lon,
		UID: 7, User: "u", Timestamp: time.Now(), Changeset: 100,
		Tags: map[string]string{"climbing": "crag", "name": "Frankenjura"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(NewHandler(db))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func call(t *testing.T, s *mcp.ClientSession, tool string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestListsTwoReadOnlyTools(t *testing.T) {
	s := connect(t)
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
	}
	if strings.Join(names, ",") != "get_schema,query" && strings.Join(names, ",") != "query,get_schema" {
		t.Errorf("tools = %v", names)
	}
}

func TestGetSchema(t *testing.T) {
	res := call(t, connect(t), "get_schema", map[string]any{})
	if res.IsError {
		t.Fatalf("error: %v", res.Content)
	}
	var schema store.Schema
	decode(t, res, &schema)

	names := map[string]bool{}
	for _, tb := range schema.Tables {
		names[tb.Name] = true
	}
	if !names["elements"] || !names["elements_history"] || !names["changesets"] {
		t.Errorf("tables = %v", names)
	}
}

func TestQuery(t *testing.T) {
	res := call(t, connect(t), "query", map[string]any{
		"sql":  `SELECT id, type, json_extract(tags, '$.name') AS name FROM elements WHERE id = ?`,
		"args": []any{42},
	})
	if res.IsError {
		t.Fatalf("error: %v", res.Content)
	}
	var out store.QueryResult
	decode(t, res, &out)

	if len(out.Rows) != 1 || out.Truncated {
		t.Fatalf("result = %+v", out)
	}
	if got := out.Rows[0]; got[0] != float64(42) || got[1] != "n" || got[2] != "Frankenjura" {
		t.Errorf("row = %#v", got)
	}
}

func TestQueryRejectsWrites(t *testing.T) {
	s := connect(t)
	for _, sql := range []string{
		`DELETE FROM elements`,
		`SELECT 1; DELETE FROM elements`,
		`ATTACH DATABASE 'x.db' AS x`,
		`PRAGMA query_only = 0`,
	} {
		if res := call(t, s, "query", map[string]any{"sql": sql}); !res.IsError {
			t.Errorf("%q was not rejected", sql)
		}
	}

	res := call(t, s, "query", map[string]any{"sql": `SELECT count(*) FROM elements`})
	var out store.QueryResult
	decode(t, res, &out)
	if out.Rows[0][0] != float64(1) {
		t.Errorf("elements changed: %v", out.Rows)
	}
}

func TestQueryCapsRows(t *testing.T) {
	res := call(t, connect(t), "query", map[string]any{
		"sql": `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 15000) SELECT i FROM n`,
	})
	var out store.QueryResult
	decode(t, res, &out)
	if len(out.Rows) != maxRows || !out.Truncated {
		t.Errorf("rows = %d, truncated = %v", len(out.Rows), out.Truncated)
	}
}

func decode(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decoding %s: %v", b, err)
	}
}
