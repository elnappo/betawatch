# OSM Ingest Command Plan

## Overview
A new `cmd/ingest` command consumes the OSM minute diffs and stores the
elements selected by `config.yaml` in SQLite, with version history.

## Decisions
- Command name: `ingest`, in `cmd/ingest/main.go`.
- Filter: the `select` expressions of `config.yaml` (osmium tags-filter
  syntax), via `internal/config`, the same as `cmd/betawatch`.
- Minute diffs come from `github.com/paulmach/osm/replication`.
- SQLite driver: `modernc.org/sqlite` (pure Go, no cgo).
- `elements` holds the latest version only; `elements_history` holds
  superseded versions.
- Elements of all three kinds share one table each, unique by `id` and
  `type` together.
- Stored per element: id, type, version, lat, lon, uid, timestamp, user,
  changeset_id, and the tags as JSON in a text column (nullable).
- `type` is `CHAR(1)`; `id`, `changeset_id` and `uid` are `BIGINT`.
- `lat` and `lon` are only set for nodes.
- Each changeset tag has its own column, not a JSON blob.
- Changeset metadata is not in the diffs. Only `id`, `timestamp`, `uid`
  and `user` are stored; the other changeset columns stay NULL for now.
- No `users` table: `uid` and `user` are plain columns.
- Ways and relations do not store node refs or members.
- Deletes are matched by `(id, type)` against `elements`, since they
  carry no tags (see Write Rules).

## Database Schema
The reviewed schema is `SCHEMA.sql`. It will be embedded from
`internal/store/schema.sql`.

### `changesets`
| Column | Type | Notes |
|--------|------|-------|
| `id` | BIGINT | PRIMARY KEY |
| `timestamp` | DATETIME | NOT NULL; earliest element timestamp seen for it |
| `uid` | BIGINT | from the diff |
| `user` | TEXT | from the diff |
| `comment` | TEXT | why the changes were made; shown as the changeset headline |
| `created_by` | TEXT | editing software, e.g. `JOSM/1.5 (13367 en)` |
| `imagery_used` | TEXT | imagery shown in the editor |
| `source` | TEXT | source of the edits |
| `bot` | BOOLEAN | `bot=yes` in OSM |
| `locale` | TEXT | editor language |
| `review_requested` | BOOLEAN | `review_requested=yes` in OSM |
| `data_used` | TEXT | Esri/ArcGIS datasets (Rapid) |
| `hashtags` | TEXT | semicolon-delimited, e.g. `#MissingMaps;#Tanzania` |
| `host` | TEXT | address of the web editor (iD) |
| `changesets_count` | INTEGER | user's edit count, 0 on a first edit (iD) |
| `changes_count` | INTEGER | total changes in the changeset |
| `created_count` | INTEGER | created elements |
| `modified_count` | INTEGER | modified elements |
| `deleted_count` | INTEGER | deleted elements |

Only `id`, `timestamp`, `uid` and `user` are known from the diffs. The
rest are NULL. Sources considered for them later: the OSM API
(`/api/0.6/changeset/ID`) or the changeset replication feed. Neither is
in scope now.

### `elements` (live, latest version only)
| Column | Type | Notes |
|--------|------|-------|
| `id` | BIGINT | NOT NULL |
| `type` | CHAR(1) | NOT NULL, CHECK in `n`, `w`, `r` |
| `version` | INTEGER | NOT NULL |
| `lat`, `lon` | REAL | nodes only |
| `uid` | BIGINT | |
| `user` | TEXT | |
| `timestamp` | DATETIME | NOT NULL |
| `changeset_id` | BIGINT | NOT NULL, references `changesets(id)` |
| `tags` | TEXT | nullable JSON |

Primary key `(id, type)`.

### `elements_history` (superseded versions)
Same columns as `elements`, plus `deleted BOOLEAN NOT NULL DEFAULT 0`.
Primary key `(id, type, version)`.

### Relations and rules
- No `users` table: `uid` and `user` are plain columns everywhere.
- `changeset_id` in both element tables references `changesets.id`.
  SQLite needs `PRAGMA foreign_keys = ON` on every connection.
- Indexes: `elements(timestamp)` only; the primary keys cover
  `(id, type)` and `(id, type, version)`.

## Write Rules (one transaction per minute diff)
1. Insert the changeset row if missing, before its elements.
2. Create or modify that matches the filter: if `elements` has the
   `(id, type)`, move that row to `elements_history`, then write the new
   version to `elements`.
   Also keep an element already stored up to date when its new version
   no longer matches the filter, since a diff holds only the new state.
3. Deletes: matched by `(id, type)` against `elements`. A match moves
   the row to `elements_history` with `deleted = 1` and removes it from
   `elements`. Deletes of unknown elements are ignored.
4. A diff version not newer than the stored one is ignored, so replaying
   a minute after a crash is harmless.
5. Save the minute to the state file only after the transaction commits.

## Flags
- `-config`: filter config (default `config.yaml`)
- `-db`: SQLite file (default `./betawatch.db`)
- `-state`: state file (default `ingest-state.json`; separate from
  betawatch's, so the two commands do not overwrite each other's cursor)
- `-backfill`: how far to catch up on a first run (default `2h`)

## Resume
Same pattern as `cmd/betawatch`: `feed.State` records the last processed
minute, and `firstMissing` picks where to start. Resume from the state
file, not from the database, since most minutes hold no match.

## Implementation Steps
1. ✅ `go get modernc.org/sqlite` (resolved to v1.60.0).
2. Rewrite `internal/store/sqlite.go` for the final schema. The current
   file uses the old schema and is stale.
3. Write `cmd/ingest/main.go`: config, state, poll loop, per-minute
   transaction, progress on stderr.
4. Tests for `internal/store`: schema creation, create, modify (old
   version moves to history), delete of a known and an unknown element,
   replay of the same minute, foreign keys.
5. Run `go build ./...`, `go vet ./...` and `go test ./...`.

## Files
- Create: `internal/store/sqlite.go`, `internal/store/schema.sql`,
  `internal/store/sqlite_test.go`, `cmd/ingest/main.go`
- Modify: `go.mod`, `go.sum`
- Docs: mention the command in `README.md`
