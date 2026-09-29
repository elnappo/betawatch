# BetaWatch

A live view for climbing-related changes on OpenStreetMap (OSM). BetaWatch watches the OSM minute diffs in real-time, filters for climbing-related changes, and provides a web interface to review them as they happen.

## How to Run

```bash
go run ./cmd/feed -config config.yaml
```

`betawatch-feed` starts a web server on `localhost:8080` with a live view of climbing-related changes.

### Ingest into SQLite

```bash
go run ./cmd/ingest -config config.yaml
```

`betawatch-ingest` follows the minute diffs and stores the climbing related elements in SQLite. The
schema is in `internal/store/schema.sql`.

### Importing a planet PBF file

```bash
go run ./cmd/ingest -config config.yaml -import-pbf climbing.osm.pbf
```

Imports a planet PBF file into the database and exits, instead of
following the minute diffs. The file is expected to already be filtered to
climbing-related elements (e.g. with `osmium tags-filter`), so every
element in it is stored unconditionally, without consulting `select`. As
with the minute diffs, an element version already stored is never
overwritten by an older one. The import commits periodically, so a rerun
after a crash or interruption is cheap: only the elements committed since
the last save reprocess.
