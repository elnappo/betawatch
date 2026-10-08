// Package mcp exposes the store to LLMs as a Model Context Protocol
// server with two read-only tools: get_schema and query.
package mcp

import (
	"context"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/elnappo/betawatch/internal/store"
)

const (
	// maxRows is how many rows one query returns. An LLM can otherwise
	// pull millions of rows from elements into its context.
	maxRows = 10000

	// queryTimeout bounds one call. Cancelling the context interrupts the
	// statement inside SQLite, so a scan of the whole elements table, or
	// a runaway join, stops here.
	queryTimeout = 10 * time.Second
)

// NewHandler serves MCP over streamable HTTP, backed by db.
func NewHandler(db *store.Store) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "betawatch"}, &mcp.ServerOptions{
		Instructions: "Read-only access to BetaWatch, a database of OpenStreetMap climbing " +
			"elements and the changes made to them. Call get_schema first, then query.",
	})

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(false)}

	mcp.AddTool(srv, &mcp.Tool{
		Name: "get_schema",
		Description: "Returns the whole database schema: every table with its columns " +
			"(type, nullability, primary key position), indexes and foreign keys, and the " +
			"CREATE statement.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *store.Schema, error) {
		ctx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()
		schema, err := db.Schema(ctx)
		return nil, schema, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "query",
		Description: "Runs one read-only SQLite SELECT (or WITH) statement and returns its " +
			"rows. Anything that writes, plus ATTACH, PRAGMA and multiple statements, is " +
			"rejected. At most 10000 rows are returned, with truncated set when there were " +
			"more, and a query running longer than 10 seconds is cancelled: filter, " +
			"aggregate and use LIMIT. Element type is one letter: n for OSM node, w for OSM " +
			"way or r for OSM relation. Timestamps are " +
			"RFC 3339 UTC text. The tags column holds a JSON object as text: read it with " +
			"json_extract(tags, '$.name'), and quote a key containing a colon or dot as " +
			"'$.\"climbing:grade:uiaa\"'. elements holds the live version of each element, " +
			"elements_history the superseded versions and deletes. A regexp(pattern, value) " +
			"function is available, so `value REGEXP pattern` works too. If you output data to " +
			"the user, consider including a link to an element on OpenStreetMap, for example " +
			"https://www.openstreetmap.org/node/12345678.",
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, *store.QueryResult, error) {
		ctx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()
		res, err := db.Query(ctx, in.SQL, in.Args, maxRows)
		return nil, res, err
	})

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		// No session state is needed: every call is independent, and a
		// stateless server survives a restart without clients noticing.
		Stateless: true,
		// The reverse proxy connects over loopback while forwarding the
		// public Host header. The proxy must restrict access to this route;
		// otherwise disabling this check removes DNS-rebinding protection.
		DisableLocalhostProtection: true,
	})
}

type queryInput struct {
	SQL  string `json:"sql" jsonschema:"a single SELECT or WITH statement"`
	Args []any  `json:"args,omitempty" jsonschema:"values bound to the ? placeholders in sql, in order"`
}
