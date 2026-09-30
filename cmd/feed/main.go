// Command feed reads climbing-related changes from the SQLite database
// and serves them via a web interface with Server-Sent Events.
//
// Usage:
//
//	feed [-config config.yaml]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/elnappo/betawatch/internal/config"
	"github.com/elnappo/betawatch/internal/feed"
	"github.com/elnappo/betawatch/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "tag filter config")
	flag.Parse()

	// Stop cleanly on Ctrl-C
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, *configPath); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// Open the SQLite database populated by the ingest command
	db, err := store.Open(cfg.DbPath)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	// Start from the newest element already in the database, so startup
	// does not republish everything the history page already shows.
	lastTimestamp, err := latestTimestamp(db)
	if err != nil {
		return fmt.Errorf("reading latest element: %w", err)
	}

	// Start the web server
	var broker *feed.Broker
	if cfg.HTTPAddr != "" {
		broker = feed.New()
		if err := serve(ctx, cfg.HTTPAddr, broker, fetchHistory(cfg, db), fetchDiff(db)); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "config %s, database %s\n", cfg.Version(), cfg.DbPath)

	// Poll the database every 10 seconds for new changes
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		// Query for changes since last timestamp
		changes, err := db.QueryRecent(lastTimestamp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: querying database: %v\n", err)
			continue
		}

		for _, dbChange := range changes {
			// Filter by config
			if !cfg.Selects(dbChange.Type, dbChange.Tags) {
				continue
			}

			// Build the change record
			c := changeFromDB(cfg, dbChange)
			if err := json.NewEncoder(os.Stdout).Encode(c); err != nil {
				return err
			}

			if broker != nil {
				data, err := json.Marshal(c)
				if err != nil {
					return err
				}
				broker.Publish(data)
			}

			lastTimestamp = dbChange.Timestamp
		}

		if len(changes) > 0 {
			fmt.Fprintf(os.Stderr, "found %d changes\n", len(changes))
		}
	}
}

// latestTimestamp returns the timestamp of the most recently modified
// element in the database, or the zero time if it holds none yet.
func latestTimestamp(db *store.Store) (time.Time, error) {
	latest, err := db.QueryLatest(1)
	if err != nil || len(latest) == 0 {
		return time.Time{}, err
	}
	return latest[0].Timestamp, nil
}

// serve starts the web view in the background.
func serve(ctx context.Context, addr string, broker *feed.Broker, fetch feed.HistoryFetcher, diff feed.DiffFetcher) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: feed.NewHandler(broker, fetch, diff)}

	go func() {
		<-ctx.Done()
		// The SSE handlers return as soon as their request context is
		// cancelled, so a short grace period is enough.
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "web server:", err)
		}
	}()

	fmt.Fprintf(os.Stderr, "web view on http://%s\n", ln.Addr())
	return nil
}

// encodeHistoryCursor and decodeHistoryCursor pack a row's (timestamp,
// id, type) into the "before" paging cursor, matching the tiebreak
// QueryBefore orders and cuts off on. A single backfill batch commonly
// stores dozens of elements at the same timestamp, so a cursor built
// from the timestamp alone would drop the rest of that group whenever a
// page boundary landed inside it.
func encodeHistoryCursor(c store.RecentChange) string {
	return c.Timestamp.UTC().Format(time.RFC3339) + "|" + c.Type + "|" + strconv.FormatInt(c.ID, 10)
}

func decodeHistoryCursor(cursor string) (t time.Time, id int64, typ string, err error) {
	parts := strings.SplitN(cursor, "|", 3)
	if len(parts) != 3 {
		return time.Time{}, 0, "", fmt.Errorf("malformed cursor: %q", cursor)
	}
	t, err = time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return time.Time{}, 0, "", fmt.Errorf("invalid cursor timestamp: %w", err)
	}
	id, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return time.Time{}, 0, "", fmt.Errorf("invalid cursor id: %w", err)
	}
	return t, id, parts[1], nil
}

// fetchHistory returns a HistoryFetcher reading pages of past changes
// straight from the database, newest first.
func fetchHistory(cfg *config.Config, db *store.Store) feed.HistoryFetcher {
	return func(before string, limit int) ([]json.RawMessage, string, bool, error) {
		var (
			dbChanges []store.RecentChange
			err       error
		)
		if before == "" {
			dbChanges, err = db.QueryLatest(limit + 1)
		} else {
			t, id, typ, perr := decodeHistoryCursor(before)
			if perr != nil {
				return nil, "", false, fmt.Errorf("invalid before cursor: %w", perr)
			}
			dbChanges, err = db.QueryBefore(t, id, typ, limit+1)
		}
		if err != nil {
			return nil, "", false, err
		}

		// One extra row was asked for, so its presence alone says
		// whether older changes remain, without a second query.
		more := len(dbChanges) > limit
		if more {
			dbChanges = dbChanges[:limit]
		}

		changes := make([]json.RawMessage, 0, len(dbChanges))
		var oldest string
		for _, dbChange := range dbChanges {
			data, err := json.Marshal(changeFromDB(cfg, dbChange))
			if err != nil {
				return nil, "", false, err
			}
			changes = append(changes, data)
			oldest = encodeHistoryCursor(dbChange)
		}
		return changes, oldest, more, nil
	}
}

// fetchDiff returns a DiffFetcher reading the previous version of an
// element straight from elements_history.
func fetchDiff(db *store.Store) feed.DiffFetcher {
	return func(typ string, id int64, version int) (*feed.PreviousVersion, error) {
		prev, err := db.PreviousVersion(id, stringToType(typ), version)
		if err != nil || prev == nil {
			return nil, err
		}
		return &feed.PreviousVersion{Found: true, Tags: prev.Tags, Lat: prev.Lat, Lon: prev.Lon}, nil
	}
}

// stringToType converts full element type names, as sent by the browser,
// to the single-letter codes the store uses.
func stringToType(t string) string {
	switch t {
	case "node":
		return store.Node
	case "way":
		return store.Way
	case "relation":
		return store.Relation
	default:
		return t
	}
}

// changeFromDB builds a change record from a database record.
func changeFromDB(cfg *config.Config, dbChange store.RecentChange) change {
	elemType := typeToString(dbChange.Type)

	action := "modify"
	if dbChange.Version == 1 {
		action = "create"
	}

	return change{
		Action:       action,
		Type:         elemType,
		ID:           dbChange.ID,
		Version:      dbChange.Version,
		Lat:          dbChange.Lat,
		Lon:          dbChange.Lon,
		Timestamp:    dbChange.Timestamp,
		User:         dbChange.User,
		UID:          dbChange.UID,
		Changeset:    dbChange.Changeset,
		Name:         dbChange.Name,
		Classes:      cfg.Classes(dbChange.Type, dbChange.Tags),
		Unwanted:     cfg.Unwanted(dbChange.Type, dbChange.Tags),
		BadValues:    cfg.BadValues(dbChange.Tags),
		MatchedKeys:  cfg.MatchedKeys(dbChange.Type, dbChange.Tags),
		Tags:         dbChange.Tags,
		URL:          fmt.Sprintf("https://www.openstreetmap.org/%s/%d", elemType, dbChange.ID),
		ChangesetURL: fmt.Sprintf("https://www.openstreetmap.org/changeset/%d", dbChange.Changeset),
	}
}

// typeToString converts single-letter type codes to full names.
func typeToString(t string) string {
	switch t {
	case "n":
		return "node"
	case "w":
		return "way"
	case "r":
		return "relation"
	default:
		return t
	}
}

// change is one matching element, as printed.
type change struct {
	Action string `json:"action"` // create, modify, delete
	Type   string `json:"type"`   // node, way, relation
	ID     int64  `json:"id"`

	Version      int               `json:"version"`
	Lat          *float64          `json:"lat,omitempty"`
	Lon          *float64          `json:"lon,omitempty"`
	Timestamp    time.Time         `json:"timestamp"`
	User         string            `json:"user,omitempty"`
	UID          int64             `json:"uid,omitempty"`
	Changeset    int64             `json:"changeset"`
	Name         string            `json:"name,omitempty"`
	Classes      []string          `json:"classes,omitempty"`
	Unwanted     []string          `json:"unwanted,omitempty"`
	BadValues    []config.BadValue `json:"bad_values,omitempty"`
	MatchedKeys  []string          `json:"matched_keys"`
	Tags         map[string]string `json:"tags"`
	URL          string            `json:"url"`
	ChangesetURL string            `json:"changeset_url"`
}
