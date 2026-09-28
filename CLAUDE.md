# BetaWatch

Watches OpenStreetMap for climbing-related edits and shows them for review.

## Dependencies

### github.com/paulmach/osm

All OSM parsing and fetching. Two packages are used:

- `osm` — the element types and `osm.Change`, the parsed osmChange file
- `osm/replication` — `Datasource`, `CurrentMinuteState`, `Minute`,
  `MinuteStateAt`, `NotFound`

Notes that took a while to learn:

- Decode a diff as a whole `osm.Change` and walk `Create`, `Modify` and
  `Delete`. The streaming `osmxml.Scanner` does not report which block an
  element came from, and a created element carries no `visible`
  attribute, so deriving the action from the element alone reports
  everything as a delete.
- `replication.NotFound(err)` is the check for a diff that is not
  published yet, or a gap in the feed. Both are normal; neither is fatal.
- `MinuteStateAt` binary-searches the feed and costs 20+ requests. Use it
  to find a range once, not per diff.
- The package handles gzip itself, so a `.osc.gz` needs no unwrapping.
- Give it an `*http.Client` with a custom `Transport` to add the
  User-Agent, or to cache, as `internal/cache` does.

### osmium tags-filter syntax

`internal/config` implements a subset of it by hand; osmium itself is not
a dependency. The point is that expressions can be copied between this
config and the `osmium tags-filter` command.

Read the
[syntax documentation](https://docs.osmcode.org/osmium/latest/osmium-tags-filter.html)
before changing `internal/config/expr.go`, and match it rather than
inventing behaviour. The wording is easy to misread: "a leading or
trailing asterisk can be used for substring or prefix matching,
respectively" means `*Paris` matches a name *containing* Paris, not one
ending in it. Implementing it as "ends with" was a real bug here.

Supported: type prefixes, key, key prefix, exact value, value lists,
value wildcards, and `!=`. Not supported: osmium's `a/` area prefix,
which needs polygon assembly this does not do — and note it fails
quietly, since `a/climbing=crag` parses as a key named `a/climbing`
and matches nothing.

## Things that will bite

**Deletes carry no tags.** Planet diffs strip them: on 2026-09-25 all
250,535 deletes were untagged. A deleted crag is indistinguishable from
any other delete, so deletes never match the filter. Seeing them needs a
state database of known climbing elements, which does not exist yet.

**Diffs hold only the new state.** There is no previous version, so a
change that *removes* `sport=climbing` is invisible, as is a moved child
node of a route or a member dropped from a crag relation.

**Derived fields must be recomputed on load.** `classes`, `matched_keys`
and `unwanted` come from the config but are written into each stored
record. `reclassify` in `cmd/betawatch/main.go` recomputes them when the
store is read, so editing the config applies to changes already stored.
Anything else derived from the config belongs there too.

**Resume from `state.json`, not the store.** The store only advances when
a diff holds a match, and most minutes hold none, so resuming from it
refetches everything since the last match.

**Backfill is capped.** A week of minute diffs is about 0.7 GB, and the
hourly and daily feeds cost the same for the same span. `-backfill`
limits a first run (2h by default); after that the state file makes
restarts cheap.

## Facts worth keeping

- About 200 climbing changes a day worldwide, against roughly 4.3 million
  elements — a hit rate near 0.005%. The review UI is the product; the
  pipeline is small. Expensive per-change work is affordable.
- A day of changes is about 120 KB of NDJSON, so a week fits comfortably
  in memory and on disk.
- 68 of 296 sampled changes carry a `wikimedia_commons` photo.
- Requests to OSM and Commons must send a descriptive User-Agent.

## Conventions

- Keep it simple: this code is read and reviewed by one person.
- Prefer the standard library and a well-known helper over hand-rolled
  plumbing.
- Comments say *why*, not what. Explain the OSM quirk or the trade-off,
  not the syntax.
- For page changes, drive it in a headless browser rather than reading
  the diff: several real bugs here were only visible when rendered.
- The web page has no build step and no dependencies. Keep it that way.
- Security headers are set by the proxy, not the app. If one is ever
  added back, `img-src` needs `https://*.wikimedia.org`, not just
  `commons.wikimedia.org`: CSP checks every URL in a redirect chain and
  `Special:FilePath` redirects to a separate thumbnail host.
