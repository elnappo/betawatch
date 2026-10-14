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

### Checking Wikimedia Commons metadata

```bash
go run ./cmd/ingest -config config.yaml -check-commons
```

Collects the distinct `File:` values from the `wikimedia_commons*` tags of
the stored elements that the `commons_files` table does not have a row for
yet, asks the Wikimedia Commons API about them in batches, and adds the
answers: dimensions, size and MIME type, or the status the API reported
(missing or invalid). Commons files are treated as immutable, so a file is
looked up once and never again; the same run also drops rows for files no
element references anymore. The command exits when done, and is safe to run
while `betawatch-ingest` is following the minute diffs.
