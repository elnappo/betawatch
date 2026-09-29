// Command ingest follows the OSM minute diffs and stores the elements
// selected by config.yaml in a SQLite database, keeping the versions they
// replace.
//
// Usage:
//
//	ingest [-config config.yaml] [-db betawatch.db] [-state ingest-state.json] [-backfill 2h]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/replication"

	"github.com/elnappo/betawatch/internal/config"
	"github.com/elnappo/betawatch/internal/feed"
	"github.com/elnappo/betawatch/internal/store"
)

// pollInterval is how often to ask for a new sequence number. Diffs are
// published every minute.
const pollInterval = 30 * time.Second

func main() {
	configPath := flag.String("config", "config.yaml", "tag filter config")
	dbPath := flag.String("db", "./betawatch.db", "SQLite database")
	// Not the state file of betawatch: the two commands would overwrite
	// each other's cursor.
	statePath := flag.String("state", "ingest-state.json", "file recording the last processed minute")
	backfill := flag.Duration("backfill", 2*time.Hour, "how far to catch up on a first run")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, *configPath, *dbPath, *statePath, *backfill); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath, dbPath, statePath string, backfill time.Duration) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	cursor, err := feed.LoadState(statePath)
	if err != nil {
		return fmt.Errorf("reading state: %w", err)
	}

	ds := replication.NewDatasource(&http.Client{
		Timeout:   1 * time.Minute,
		Transport: &agentTransport{},
	})

	latest, state, err := ds.CurrentMinuteState(ctx)
	if err != nil {
		return fmt.Errorf("reading current minute state: %w", err)
	}
	fmt.Fprintf(os.Stderr, "config %s, at minute %d (%s)\n",
		cfg.Version(), latest, state.Timestamp.Format(time.RFC3339))

	next := firstMissing(latest, replication.MinuteSeqNum(cursor.Minute), backfill)
	if next > latest {
		fmt.Fprintf(os.Stderr, "up to date at minute %d\n", latest)
	} else {
		fmt.Fprintf(os.Stderr, "catching up on minutes %d..%d\n", next, latest)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		for ; next <= latest; next++ {
			if err := ingest(ctx, ds, db, cfg, cursor, next); err != nil {
				return err
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		l, _, err := ds.CurrentMinuteState(ctx)
		if err != nil {
			// The server may be briefly unavailable; the next tick
			// retries from the same minute.
			fmt.Fprintln(os.Stderr, "warning:", err)
			continue
		}
		latest = l
	}
}

// ingest applies one minute diff in a single transaction.
func ingest(ctx context.Context, ds *replication.Datasource, db *store.Store, cfg *config.Config, cursor *feed.State, num replication.MinuteSeqNum) error {
	ch, err := ds.Minute(ctx, num)
	if err != nil {
		if replication.NotFound(err) {
			// The feed has occasional gaps. Record it as done so a
			// restart does not retry it forever.
			fmt.Fprintf(os.Stderr, "warning: minute %d not available, skipping\n", num)
			return cursor.Save(uint64(num))
		}
		return fmt.Errorf("fetching minute %d: %w", num, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	saved, deleted, err := apply(tx, cfg, ch)
	if err != nil {
		return fmt.Errorf("minute %d: %w", num, err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Report progress: most minutes hold nothing, so without this a live
	// run is indistinguishable from a hung one.
	fmt.Fprintf(os.Stderr, "minute %d: %d saved, %d deleted\n", num, saved, deleted)

	// Save only after the commit, so a crash repeats a diff rather than
	// losing it. The store ignores the repeat.
	return cursor.Save(uint64(num))
}

// apply writes a diff to the transaction and returns how many elements
// it saved and deleted.
func apply(tx *store.Tx, cfg *config.Config, ch *osm.Change) (saved, deleted int, err error) {
	for _, set := range []*osm.OSM{ch.Create, ch.Modify} {
		if set == nil {
			continue
		}
		for _, el := range set.Elements() {
			e := toElement(el)
			// An element already stored is kept up to date even when the
			// new version no longer matches: a diff holds only the new
			// state, so an edit that removes the climbing tags would
			// otherwise leave the stored version stale.
			keep := cfg.Selects(string(el.ElementID().Type()), e.Tags)
			if !keep {
				if keep, err = tx.Has(e.ID, e.Type); err != nil {
					return saved, deleted, err
				}
			}
			if !keep {
				continue
			}
			if err := tx.Save(e); err != nil {
				return saved, deleted, err
			}
			saved++
		}
	}

	if ch.Delete != nil {
		// Deletes carry no tags, so the filter cannot select them; they
		// match by id and type against what is stored.
		for _, el := range ch.Delete.Elements() {
			e := toElement(el)
			ok, err := tx.Delete(e.ID, e.Type, e.Version)
			if err != nil {
				return saved, deleted, err
			}
			if ok {
				deleted++
			}
		}
	}
	return saved, deleted, nil
}

func toElement(el osm.Element) store.Element {
	id := el.ElementID()
	e := store.Element{ID: id.Ref(), Version: id.Version(), Tags: el.TagMap()}

	switch v := el.(type) {
	case *osm.Node:
		e.Type = store.Node
		e.Lat, e.Lon = &v.Lat, &v.Lon
		e.UID, e.User, e.Timestamp, e.Changeset = int64(v.UserID), v.User, v.Timestamp, int64(v.ChangesetID)
	case *osm.Way:
		e.Type = store.Way
		e.UID, e.User, e.Timestamp, e.Changeset = int64(v.UserID), v.User, v.Timestamp, int64(v.ChangesetID)
	case *osm.Relation:
		e.Type = store.Relation
		e.UID, e.User, e.Timestamp, e.Changeset = int64(v.UserID), v.User, v.Timestamp, int64(v.ChangesetID)
	}
	return e
}

// firstMissing returns the minute to resume from: the one after the last
// processed, or limit before now on a first run. One diff is one minute,
// so the limit converts directly to a sequence count.
func firstMissing(latest, stored replication.MinuteSeqNum, limit time.Duration) replication.MinuteSeqNum {
	// MinuteSeqNum is unsigned, so clamp instead of underflowing.
	var oldest replication.MinuteSeqNum
	if n := replication.MinuteSeqNum(limit.Minutes()); n < latest {
		oldest = latest - n
	}
	if stored > 0 && stored+1 > oldest {
		oldest = stored + 1
	}
	if oldest > latest+1 {
		return latest + 1
	}
	return oldest
}

// agentTransport adds the User-Agent the OSM usage policy asks for.
type agentTransport struct{}

func (t *agentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", "BetaWatch/0.1 (OSM climbing change review; +https://github.com/elnappo/betawatch)")
	return http.DefaultTransport.RoundTrip(req)
}
