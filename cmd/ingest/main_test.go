package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmapi"

	"github.com/elnappo/betawatch/internal/commons"
	"github.com/elnappo/betawatch/internal/config"
	"github.com/elnappo/betawatch/internal/store"
)

const ingestTestConfig = `
select:
  - climbing
`

func loadIngestConfig(t *testing.T) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(ingestTestConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func openIngestDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func climbingNode(id, changeset int64, at time.Time) *osm.Node {
	return &osm.Node{
		Version: 1, Lat: 47.5, Lon: 11.2,
		ChangesetID: osm.ChangesetID(changeset),
		Timestamp:   at,
		Tags:        osm.Tags{{Key: "climbing", Value: "crag"}},
	}
}

func TestToChangesetMetadata(t *testing.T) {
	createdAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	cs := &osm.Changeset{
		ID: 123, CreatedAt: createdAt,
		Tags: osm.Tags{
			{Key: "comment", Value: "fixed a wall"},
			{Key: "created_by", Value: "JOSM/1.5"},
			{Key: "imagery_used", Value: "Bing"},
			{Key: "source", Value: "survey"},
			{Key: "locale", Value: "en"},
			{Key: "data_used", Value: "Esri"},
			{Key: "hashtags", Value: "#climbing;#MissingMaps"},
			{Key: "host", Value: "https://www.openstreetmap.org/edit"},
			{Key: "bot", Value: "yes"},
			{Key: "review_requested", Value: "yes"},
			{Key: "changesets_count", Value: "42"},
		},
	}

	got := toChangesetMetadata(cs)
	want := store.ChangesetMetadata{
		CreatedAt: createdAt,
		Comment:   "fixed a wall", CreatedBy: "JOSM/1.5", ImageryUsed: "Bing",
		Source: "survey", Locale: "en", DataUsed: "Esri",
		Hashtags: "#climbing;#MissingMaps", Host: "https://www.openstreetmap.org/edit",
		Bot: true, ReviewRequested: true, ChangesetsCount: 42,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("toChangesetMetadata = %+v, want %+v", got, want)
	}
}

func TestToChangesetMetadataOfMinimalChangeset(t *testing.T) {
	// A changeset with no tags at all is a valid API response, not a
	// malformed one: every field should come back zero, not error.
	got := toChangesetMetadata(&osm.Changeset{ID: 1})
	if got != (store.ChangesetMetadata{}) {
		t.Errorf("toChangesetMetadata of a tagless changeset = %+v, want zero value", got)
	}
}

func TestApplyCollectsChangesetsOfSavedAndDeletedElements(t *testing.T) {
	cfg := loadIngestConfig(t)
	db := openIngestDB(t)
	now := time.Now()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	ch := &osm.Change{
		Create: &osm.OSM{
			Nodes: osm.Nodes{
				withID(climbingNode(1, 100, now), 1),
				withID(climbingNode(2, 100, now), 2), // same changeset as node 1
			},
		},
	}
	_, _, changesets, err := apply(tx, cfg, ch)
	if err != nil {
		t.Fatal(err)
	}
	if len(changesets) != 1 {
		t.Fatalf("changesets = %v, want exactly {100}", changesets)
	}
	if _, ok := changesets[100]; !ok {
		t.Errorf("changesets = %v, want to contain 100", changesets)
	}
}

func TestApplyExcludesChangesetsOfSkippedElements(t *testing.T) {
	cfg := loadIngestConfig(t)
	db := openIngestDB(t)
	now := time.Now()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	nonClimbing := &osm.Node{Version: 1, ChangesetID: 200, Timestamp: now, Tags: osm.Tags{{Key: "shop", Value: "bakery"}}}
	ch := &osm.Change{Create: &osm.OSM{Nodes: osm.Nodes{withID(nonClimbing, 3)}}}

	_, _, changesets, err := apply(tx, cfg, ch)
	if err != nil {
		t.Fatal(err)
	}
	if len(changesets) != 0 {
		t.Errorf("changesets = %v, want empty: the element was never saved", changesets)
	}
}

func withID(n *osm.Node, id int64) *osm.Node {
	n.ID = osm.NodeID(id)
	return n
}

func TestEnrichChangesetFetchesAndStoresMetadata(t *testing.T) {
	db := openIngestDB(t)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Save(store.Element{
		ID: 1, Type: store.Node, Version: 1, UID: 7, User: "u",
		Timestamp: time.Now(), Changeset: 100, Tags: map[string]string{"climbing": "crag"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !strings.Contains(r.URL.Path, "/changeset/100") {
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
		w.Write([]byte(`<osm><changeset id="100" created_at="2026-09-01T10:00:00Z"><tag k="comment" v="fixed a wall"/></changeset></osm>`))
	}))
	defer srv.Close()

	apiDS := osmapi.NewDatasource(srv.Client())
	apiDS.BaseURL = srv.URL

	if err := enrichChangeset(context.Background(), apiDS, db, 100); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}

	needed, err := db.NeedsChangesetMetadata(100)
	if err != nil {
		t.Fatal(err)
	}
	if needed {
		t.Error("NeedsChangesetMetadata after enrichChangeset = true, want false")
	}

	// A second call must not fetch again: the changeset is already marked.
	if err := enrichChangeset(context.Background(), apiDS, db, 100); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Errorf("requests after second call = %d, want still 1", requests)
	}
}

func TestFetchCommonsBatchesAtTheAPILimit(t *testing.T) {
	var batches [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		titles := strings.Split(r.URL.Query().Get("titles"), "|")
		batches = append(batches, titles)

		pages := make([]map[string]any, 0, len(titles))
		for _, title := range titles {
			if title == "File:F00.jpg" {
				pages = append(pages, map[string]any{
					"title":     title,
					"imageinfo": []map[string]any{{"width": 100, "height": 200, "size": 300, "mime": "image/png"}},
				})
				continue
			}
			pages = append(pages, map[string]any{"title": title, "missing": true})
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"query": map[string]any{"pages": pages}}); err != nil {
			t.Errorf("encoding response: %v", err)
		}
	}))
	defer srv.Close()

	client := commons.NewClient(srv.Client())
	client.BaseURL = srv.URL

	titles := make([]string, commons.MaxTitles+1)
	for i := range titles {
		titles[i] = fmt.Sprintf("File:F%02d.jpg", i)
	}

	files, err := fetchCommons(context.Background(), client, titles, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 {
		t.Fatalf("requests = %d, want 2", len(batches))
	}
	if len(batches[0]) != commons.MaxTitles || len(batches[1]) != 1 {
		t.Errorf("batch sizes = %d and %d, want %d and 1", len(batches[0]), len(batches[1]), commons.MaxTitles)
	}
	if len(files) != len(titles) {
		t.Fatalf("len(files) = %d, want %d", len(files), len(titles))
	}
	if f := files[0]; f.Title != titles[0] || f.Status != commons.StatusOK || f.Width == nil || *f.Width != 100 {
		t.Errorf("files[0] = %+v, want the first title with status ok and width 100", f)
	}
	if f := files[commons.MaxTitles]; f.Title != titles[commons.MaxTitles] || f.Status != commons.StatusMissing {
		t.Errorf("files[%d] = %+v, want the second batch's title with status missing", commons.MaxTitles, f)
	}
}
