package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/elnappo/betawatch/internal/config"
	"github.com/elnappo/betawatch/internal/store"
)

const testConfig = `
select:
  - climbing
  - climbing:*
  - sport=climbing
classify:
  route:
    - climbing=route,route_bottom
  crag:
    - climbing=crag
  gym:
    - leisure=sports_centre
`

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// openDB returns an empty database and saves el, in order, one per
// transaction so their timestamps stay in the order given.
func openDB(t *testing.T, elements ...store.Element) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	for _, e := range elements {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Save(e); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func climber(id int64, at time.Time) store.Element {
	return store.Element{
		ID: id, Type: store.Way, Version: 1,
		UID: 7, User: "u", Timestamp: at, Changeset: 100,
		Tags: map[string]string{"climbing": "crag", "name": "crag"},
	}
}

func TestTypeToString(t *testing.T) {
	for in, want := range map[string]string{
		"n": "node", "w": "way", "r": "relation", "x": "x",
	} {
		if got := typeToString(in); got != want {
			t.Errorf("typeToString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChangeFromDB(t *testing.T) {
	got := changeFromDB(loadConfig(t), store.RecentChange{
		ID: 3, Type: "w", Version: 7, User: "bob", Changeset: 101,
		Timestamp: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
		Name:      "Kletterhalle",
		Tags: map[string]string{
			"sport": "climbing", "leisure": "sports_centre", "name": "Kletterhalle",
		},
	})

	if got.Type != "way" {
		t.Errorf("Type = %q, want way", got.Type)
	}
	if got.URL != "https://www.openstreetmap.org/way/3" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.ChangesetURL != "https://www.openstreetmap.org/changeset/101" {
		t.Errorf("ChangesetURL = %q", got.ChangesetURL)
	}
	if !reflect.DeepEqual(got.Classes, []string{"gym"}) {
		t.Errorf("Classes = %v, want [gym]", got.Classes)
	}
	if !reflect.DeepEqual(got.MatchedKeys, []string{"sport"}) {
		t.Errorf("MatchedKeys = %v, want [sport]", got.MatchedKeys)
	}
}

func TestChangeFromDBDeleteTakesPrecedence(t *testing.T) {
	// A delete of a version-1 element should report "delete", not "create":
	// Deleted must win over the Version == 1 check.
	got := changeFromDB(loadConfig(t), store.RecentChange{
		ID: 3, Type: "w", Version: 1, Deleted: true,
		Tags: map[string]string{"climbing": "crag"},
	})
	if got.Action != "delete" {
		t.Errorf("Action = %q, want delete", got.Action)
	}
}

func TestLatestTimestampOfEmptyDB(t *testing.T) {
	db := openDB(t)
	got, err := latestTimestamp(db)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Errorf("latestTimestamp = %v, want zero", got)
	}
}

func TestLatestTimestampIsTheNewestElement(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now.Add(-time.Hour)), climber(2, now))

	got, err := latestTimestamp(db)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(now) {
		t.Errorf("latestTimestamp = %v, want %v", got, now)
	}
}

func TestFetchHistoryReturnsNewestFirst(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now.Add(-2*time.Minute)), climber(2, now.Add(-time.Minute)), climber(3, now))

	fetch := fetchHistory(loadConfig(t), db)
	changes, oldest, more, err := fetch("", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2", len(changes))
	}
	var first, second change
	if err := json.Unmarshal(changes[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(changes[1], &second); err != nil {
		t.Fatal(err)
	}
	if first.ID != 3 || second.ID != 2 {
		t.Errorf("ids = %d, %d, want 3, 2", first.ID, second.ID)
	}
	if !more {
		t.Error("more = false, want true: one element remains")
	}
	if oldest == "" {
		t.Error("oldest cursor is empty")
	}
}

func TestFetchHistoryPagesWithBeforeCursor(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now.Add(-2*time.Minute)), climber(2, now.Add(-time.Minute)), climber(3, now))

	fetch := fetchHistory(loadConfig(t), db)
	_, oldest, _, err := fetch("", 2)
	if err != nil {
		t.Fatal(err)
	}

	changes, _, more, err := fetch(oldest, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("len(changes) = %d, want 1", len(changes))
	}
	var got change
	if err := json.Unmarshal(changes[0], &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 1 {
		t.Errorf("id = %d, want 1", got.ID)
	}
	if more {
		t.Error("more = true, want false: nothing older remains")
	}
}

func TestFetchHistoryPagesThroughSharedTimestamp(t *testing.T) {
	// A backfill batch commonly stores many elements at the exact same
	// timestamp. Paging must still see every one of them exactly once,
	// even when a page boundary falls in the middle of the group.
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now), climber(2, now), climber(3, now), climber(4, now))

	fetch := fetchHistory(loadConfig(t), db)
	seen := make(map[int64]bool)
	before := ""
	for {
		changes, oldest, more, err := fetch(before, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range changes {
			var c change
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			if seen[c.ID] {
				t.Fatalf("id %d returned twice", c.ID)
			}
			seen[c.ID] = true
		}
		if !more {
			break
		}
		before = oldest
	}
	for _, id := range []int64{1, 2, 3, 4} {
		if !seen[id] {
			t.Errorf("id %d was never returned", id)
		}
	}
}

func TestFetchHistoryIncludesSupersededVersions(t *testing.T) {
	// Saving two versions of the same element should surface both as
	// separate entries, not just the one that ended up live.
	now := time.Now().UTC().Truncate(time.Second)
	v1 := climber(1, now.Add(-time.Minute))
	v2 := climber(1, now)
	v2.Version = 2

	db := openDB(t, v1, v2)
	fetch := fetchHistory(loadConfig(t), db)
	changes, _, _, err := fetch("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("len(changes) = %d, want 2", len(changes))
	}
	var newest, oldest change
	if err := json.Unmarshal(changes[0], &newest); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(changes[1], &oldest); err != nil {
		t.Fatal(err)
	}
	if newest.Version != 2 || newest.Action != "modify" {
		t.Errorf("newest = v%d/%s, want v2/modify", newest.Version, newest.Action)
	}
	if oldest.Version != 1 || oldest.Action != "create" {
		t.Errorf("oldest = v%d/%s, want v1/create", oldest.Version, oldest.Action)
	}
}

func TestStringToType(t *testing.T) {
	for in, want := range map[string]string{
		"node": store.Node, "way": store.Way, "relation": store.Relation, "x": "x",
	} {
		if got := stringToType(in); got != want {
			t.Errorf("stringToType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchDiffFindsPreviousVersion(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now.Add(-time.Minute)))

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	c := climber(1, now)
	c.Version = 2
	c.Tags = map[string]string{"climbing": "crag", "name": "renamed"}
	if err := tx.Save(c); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	diff := fetchDiff(db)
	prev, err := diff("way", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || !prev.Found {
		t.Fatalf("prev = %+v, want found", prev)
	}
	if prev.Tags["name"] != "crag" {
		t.Errorf("Tags[name] = %q, want crag", prev.Tags["name"])
	}
}

func TestFetchDiffOfACreateIsNotFound(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now))

	diff := fetchDiff(db)
	prev, err := diff("way", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil {
		t.Errorf("prev = %+v, want nil", prev)
	}
}

func TestFetchProblemsMergesMultipleRuleHits(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now))

	cfg := loadConfigWithRules(t, `
rules:
  - name: "rule a"
    query: "SELECT id, type FROM elements WHERE id = 1"
  - name: "rule b"
    query: "SELECT id, type FROM elements WHERE id = 1"
`)

	raw, err := fetchProblems(cfg, db)()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("len(problems) = %d, want 1", len(raw))
	}
	var got problem
	if err := json.Unmarshal(raw[0], &got); err != nil {
		t.Fatal(err)
	}
	wantRules := []brokenRule{{Name: "rule a"}, {Name: "rule b"}}
	if !reflect.DeepEqual(got.Rules, wantRules) {
		t.Errorf("Rules = %+v, want %+v", got.Rules, wantRules)
	}
	want := "https://www.openstreetmap.org/edit?editor=id&way=1#comment=Fix+climbing+tags.&hashtags=climbing,betawatch"
	if got.URL != want {
		t.Errorf("URL = %q, want %q", got.URL, want)
	}
	if !reflect.DeepEqual(got.Tags, map[string]string{"climbing": "crag", "name": "crag"}) {
		t.Errorf("Tags = %v, want the element's tags", got.Tags)
	}
}

func TestEditURL(t *testing.T) {
	got := editURL("way", 1331899047)
	want := "https://www.openstreetmap.org/edit?editor=id&way=1331899047#comment=Fix+climbing+tags.&hashtags=climbing,betawatch"
	if got != want {
		t.Errorf("editURL() = %q, want %q", got, want)
	}
}

func TestFetchProblemsSkipsCleanElements(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now))

	cfg := loadConfigWithRules(t, `
rules:
  - name: "never matches"
    query: "SELECT id, type FROM elements WHERE id = 999"
`)

	raw, err := fetchProblems(cfg, db)()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Errorf("len(problems) = %d, want 0", len(raw))
	}
}

func TestFetchProblemsSkipsDisabledRules(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now))

	cfg := loadConfigWithRules(t, `
rules:
  - name: "disabled"
    query: "NOT VALID SQL"
    disabled: true
`)

	// A disabled rule's query is never even run, so its invalid SQL must
	// not surface as an error.
	raw, err := fetchProblems(cfg, db)()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Errorf("len(problems) = %d, want 0", len(raw))
	}
}

func TestFetchProblemsIncludesRuleDescription(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	db := openDB(t, climber(1, now))

	cfg := loadConfigWithRules(t, `
rules:
  - name: "rule a"
    description: "explains rule a"
    query: "SELECT id, type FROM elements WHERE id = 1"
`)

	raw, err := fetchProblems(cfg, db)()
	if err != nil {
		t.Fatal(err)
	}
	var got problem
	if err := json.Unmarshal(raw[0], &got); err != nil {
		t.Fatal(err)
	}
	want := []brokenRule{{Name: "rule a", Description: "explains rule a"}}
	if !reflect.DeepEqual(got.Rules, want) {
		t.Errorf("Rules = %+v, want %+v", got.Rules, want)
	}
}

func TestFetchProblemsPropagatesRuleError(t *testing.T) {
	db := openDB(t)
	cfg := loadConfigWithRules(t, `
rules:
  - name: "broken"
    query: "NOT VALID SQL"
`)

	_, err := fetchProblems(cfg, db)()
	if err == nil {
		t.Fatal("expected an error for a broken rule query")
	}
	if !strings.Contains(err.Error(), `"broken"`) {
		t.Errorf("error = %v, want it to name the rule", err)
	}
}

// loadConfigWithRules loads testConfig with the given extra YAML appended,
// so rule-specific tests don't have to restate the select/classify fixture.
func loadConfigWithRules(t *testing.T, extra string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(testConfig+extra), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFetchHistoryRejectsBadCursor(t *testing.T) {
	db := openDB(t)
	fetch := fetchHistory(loadConfig(t), db)
	if _, _, _, err := fetch("not-a-time", 10); err == nil {
		t.Error("expected an error for a malformed cursor")
	}
}
