# BetaWatch

A live view for climbing-related changes on OpenStreetMap (OSM). BetaWatch watches the OSM minute diffs in real-time, filters for climbing-related changes, and provides a web interface to review them as they happen.

## How to Run

```bash
cd cmd/betawatch
go run . -config config.yaml
```

This watches for new climbing-related changes from OSM and starts the web view (if configured).

### Ingest into SQLite

```bash
go run ./cmd/ingest -config cmd/betawatch/config.yaml -db betawatch.db
```

`ingest` follows the same minute diffs and stores the elements matched by
`select` in SQLite: the live version in `elements`, replaced versions in
`elements_history`, and the changeset each came from in `changesets`. A
delete of a stored element moves it to `elements_history` with `deleted`
set. Flags: `-config`, `-db` (default `./betawatch.db`), `-state` (default
`ingest-state.json`) and `-backfill` (default `2h`). The schema is in
`internal/store/schema.sql`.

### Configuration

The application is configured entirely through `config.yaml`. Key settings include:

**Web and Storage:**
- `http_addr` — Address to serve the live web view on (e.g., `:8080`, empty to disable)
- `store_file` — File to keep recent changes (e.g., `changes.ndjson`, empty to disable)
- `state_file` — File recording the last processed minute (e.g., `state.json`, empty to disable)

**Retention:**
- `retain_duration` — How far back to keep changes (e.g., `168h` for 7 days)
- `backfill_duration` — How far back to catch up on first run (e.g., `2h`)

**Climbing Tags:**
- `select` — Which OSM objects to track (climbing routes, crags, areas, etc.)
- `classify` — Labels for different types of climbing locations (route, crag, gym, etc.)
- `unwanted_tags` — Tags to we don't want on climbing objects.
- `tag_values_regex` — Validation patterns for tag values
