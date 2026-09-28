// Command betawatch watches the OSM minute diffs, keeps the changes whose
// tags match config.yaml, and prints each as a JSON object on its own
// line as it arrives.
//
// Usage:
//
//	betawatch [-config config.yaml]      watch for new diffs until interrupted
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/replication"

	"github.com/elnappo/betawatch/internal/config"
	"github.com/elnappo/betawatch/internal/feed"
)

func main() {
	configPath := flag.String("config", "config.yaml", "tag filter config")
	flag.Parse()

	// Stop cleanly on Ctrl-C so a partly written line is still flushed.
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

	ds := replication.NewDatasource(&http.Client{
		Timeout:   1 * time.Minute,
		Transport: &agentTransport{},
	})
	out := bufio.NewWriter(os.Stdout)

	// The state file records how far the feed was processed, so a restart
	// does not refetch diffs that held no matching change. It is separate
	// from the store, and kept even without -http.
	var cursor *feed.State
	if cfg.StateFile != "" {
		cursor, err = feed.LoadState(cfg.StateFile)
		if err != nil {
			return fmt.Errorf("reading state: %w", err)
		}
	}

	// With -http, changes are also pushed to browsers and kept so a page
	// opened later shows the recent ones.
	var broker *feed.Broker
	var store *feed.Store

	if cfg.HTTPAddr != "" {
		broker = feed.New(maxHistory)

		if cfg.StoreFile != "" {
			s, history, err := feed.OpenStore(cfg.StoreFile, cfg.RetainDuration)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			defer s.Close()
			store = s

			// classes and matched_keys are derived from the config, so
			// stored records hold whatever config was in force when they
			// were written. Recompute them, or a config change would only
			// ever affect new changes.
			reclassify(cfg, history)

			broker.Load(history)
			fmt.Fprintf(os.Stderr, "loaded %d changes from %s\n", len(history), cfg.StoreFile)
		}

		if err := serve(ctx, cfg.HTTPAddr, broker); err != nil {
			return err
		}
	}

	latest, state, err := ds.CurrentMinuteState(ctx)
	if err != nil {
		return fmt.Errorf("reading current minute state: %w", err)
	}
	fmt.Fprintf(os.Stderr, "config %s, at minute %d (%s)\n",
		cfg.Version(), latest, state.Timestamp.Format(time.RFC3339))

	// Catch up on what was missed. After a restart that is the gap since
	// the last processed minute; on a first run it is limited to
	// -backfill, because a week of minute diffs is about 0.7 GB.
	var resume replication.MinuteSeqNum
	if cursor != nil {
		resume = replication.MinuteSeqNum(cursor.Minute)
	}
	from := firstMissing(latest, resume, cfg.BackfillDuration)
	switch {
	case from > latest:
		fmt.Fprintf(os.Stderr, "up to date at minute %d\n", latest)
	case from < latest:
		fmt.Fprintf(os.Stderr, "catching up on minutes %d..%d\n", from, latest)
	}
	for n := from; n <= latest; n++ {
		if err := emit(ctx, out, broker, store, cursor, ds, cfg, n); err != nil {
			return err
		}
	}

	// next is the first diff not yet printed. Tracking it, rather than
	// asking for the current minute each time, means a slow or missed
	// tick catches up instead of leaving a gap.
	next := latest + 1

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		latest, _, err := ds.CurrentMinuteState(ctx)
		if err != nil {
			// A failed poll is not fatal: the server may be briefly
			// unavailable, and the next tick retries from the same
			// cursor.
			fmt.Fprintln(os.Stderr, "warning:", err)
			continue
		}

		for ; next <= latest; next++ {
			if err := emit(ctx, out, broker, store, cursor, ds, cfg, next); err != nil {
				return err
			}
		}
	}
}

const (
	// pollInterval is how often to ask for a new sequence number. Diffs
	// are published every minute, so checking a little more often keeps
	// latency down without meaningfully adding load.
	pollInterval = 30 * time.Second

	// maxHistory bounds what the web view holds in memory. A week runs to
	// roughly 1500 changes at the rate measured so far.
	maxHistory = 5000
)

// firstMissing returns the minute to resume from: the one after the last
// stored change, or limit before now on a first run. One diff is one
// minute, so the limit converts directly to a sequence count.
func firstMissing(latest, stored replication.MinuteSeqNum, limit time.Duration) replication.MinuteSeqNum {
	// The window can be longer than the feed itself, so clamp instead of
	// underflowing: MinuteSeqNum is unsigned.
	var oldest replication.MinuteSeqNum
	if n := replication.MinuteSeqNum(limit.Minutes()); n < latest {
		oldest = latest - n
	}
	if stored > 0 && stored+1 > oldest {
		oldest = stored + 1
	}
	if oldest > latest {
		return latest
	}
	return oldest
}

// serve starts the web view in the background.
func serve(ctx context.Context, addr string, broker *feed.Broker) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: broker.Handler()}

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

// emit fetches one minute diff and prints its matching changes.
func emit(ctx context.Context, out *bufio.Writer, broker *feed.Broker, store *feed.Store, cursor *feed.State, ds *replication.Datasource, cfg *config.Config, num replication.MinuteSeqNum) error {
	ch, err := ds.Minute(ctx, num)
	if err != nil {
		if replication.NotFound(err) {
			// The state file can name a sequence whose data file is not
			// published yet, and the minute feed has occasional gaps.
			// Record it as done so a restart does not retry it forever.
			fmt.Fprintf(os.Stderr, "warning: minute %d not available, skipping\n", num)
			return record(cursor, num)
		}
		return fmt.Errorf("fetching minute %d: %w", num, err)
	}
	n, err := report(out, broker, store, cfg, uint64(num), ch)
	if err != nil {
		return err
	}
	// Report progress on stderr. Most minutes hold no climbing change, so
	// without this a live view is indistinguishable from a hung one.
	fmt.Fprintf(os.Stderr, "minute %d: %d of %d elements matched\n", num, n, count(ch))

	// Flush every diff: in follow mode the output is a live feed, so it
	// must not sit in the buffer waiting for the next change.
	if err := out.Flush(); err != nil {
		return err
	}
	// Save only after the changes are durable, so a crash between the two
	// repeats a diff rather than losing it. The store drops the repeat on
	// its next load.
	return record(cursor, num)
}

// reclassify updates the derived fields of stored changes in place,
// leaving every other field as written. The tags are in the record, so
// this needs no network.
func reclassify(cfg *config.Config, history [][]byte) {
	for i, line := range history {
		// Decode into a map so fields this code does not know about
		// survive the round trip.
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}

		elemType, _ := rec["type"].(string)
		raw, _ := rec["tags"].(map[string]any)
		tags := make(map[string]string, len(raw))
		for k, v := range raw {
			if sv, ok := v.(string); ok {
				tags[k] = sv
			}
		}

		if classes := cfg.Classes(elemType, tags); len(classes) > 0 {
			rec["classes"] = classes
		} else {
			delete(rec, "classes") // omitempty in the original encoding
		}
		if unwanted := cfg.Unwanted(elemType, tags); len(unwanted) > 0 {
			rec["unwanted"] = unwanted
		} else {
			delete(rec, "unwanted")
		}
		if bad := cfg.BadValues(tags); len(bad) > 0 {
			rec["bad_values"] = bad
		} else {
			delete(rec, "bad_values")
		}
		rec["matched_keys"] = cfg.MatchedKeys(elemType, tags)

		if updated, err := json.Marshal(rec); err == nil {
			history[i] = updated
		}
	}
}

// record notes that a minute has been fully processed.
func record(cursor *feed.State, num replication.MinuteSeqNum) error {
	if cursor == nil {
		return nil
	}
	return cursor.Save(uint64(num))
}

// count returns how many elements a diff holds.
func count(ch *osm.Change) int {
	n := 0
	for _, set := range []*osm.OSM{ch.Create, ch.Modify, ch.Delete} {
		if set != nil {
			n += len(set.Elements())
		}
	}
	return n
}

// agentTransport adds the User-Agent the OSM usage policy asks for.
type agentTransport struct{}

func (t *agentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", "BetaWatch/0.1 (OSM climbing change review; +https://github.com/elnappo/betawatch)")
	return http.DefaultTransport.RoundTrip(req)
}

// change is one matching element, as printed.
type change struct {
	Minute       uint64            `json:"minute"` // replication sequence it came from
	Action       string            `json:"action"` // create, modify, delete
	Type         string            `json:"type"`   // node, way, relation
	ID           int64             `json:"id"`
	Version      int               `json:"version"`
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

// report writes one JSON object per matching element, one per line, and
// returns how many it wrote. A non-nil broker also receives each one.
func report(w io.Writer, broker *feed.Broker, store *feed.Store, cfg *config.Config, minute uint64, ch *osm.Change) (int, error) {
	n := 0
	enc := json.NewEncoder(w)
	for _, group := range []struct {
		action string
		set    *osm.OSM
	}{
		{"create", ch.Create},
		{"modify", ch.Modify},
		{"delete", ch.Delete},
	} {
		if group.set == nil {
			continue
		}
		for _, el := range group.set.Elements() {
			tags := el.TagMap()
			elemType := string(el.ElementID().Type())
			if !cfg.Selects(elemType, tags) {
				continue
			}
			c := newChange(cfg, minute, group.action, el)
			if err := enc.Encode(c); err != nil {
				return n, err
			}
			if broker != nil {
				data, err := json.Marshal(c)
				if err != nil {
					return n, err
				}
				broker.Publish(data)
				if store != nil {
					if err := store.Append(data); err != nil {
						return n, err
					}
				}
			}
			n++
		}
	}
	return n, nil
}

// newChange builds the printed record for one element.
func newChange(cfg *config.Config, minute uint64, action string, el osm.Element) change {
	tags := el.TagMap()
	id := el.ElementID()
	elemType := string(id.Type())
	m := meta(el)

	return change{
		Minute:       minute,
		Action:       action,
		Type:         elemType,
		ID:           id.Ref(),
		Version:      id.Version(),
		Timestamp:    m.timestamp,
		User:         m.user,
		UID:          m.uid,
		Changeset:    m.changeset,
		Name:         tags["name"],
		Classes:      cfg.Classes(elemType, tags),
		Unwanted:     cfg.Unwanted(elemType, tags),
		BadValues:    cfg.BadValues(tags),
		MatchedKeys:  cfg.MatchedKeys(elemType, tags),
		Tags:         tags,
		URL:          fmt.Sprintf("https://www.openstreetmap.org/%s/%d", elemType, id.Ref()),
		ChangesetURL: fmt.Sprintf("https://www.openstreetmap.org/changeset/%d", m.changeset),
	}
}

// elemMeta is the per-element metadata that osm.Element does not expose.
type elemMeta struct {
	user      string
	uid       int64
	changeset int64
	timestamp time.Time
}

func meta(el osm.Element) elemMeta {
	switch e := el.(type) {
	case *osm.Node:
		return elemMeta{e.User, int64(e.UserID), int64(e.ChangesetID), e.Timestamp}
	case *osm.Way:
		return elemMeta{e.User, int64(e.UserID), int64(e.ChangesetID), e.Timestamp}
	case *osm.Relation:
		return elemMeta{e.User, int64(e.UserID), int64(e.ChangesetID), e.Timestamp}
	}
	return elemMeta{}
}
