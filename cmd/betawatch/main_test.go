package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/replication"

	"github.com/elnappo/betawatch/internal/config"
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

// sample covers all three action blocks, a named and an unnamed element,
// and an element that must not be selected.
const sample = `<?xml version="1.0" encoding="UTF-8"?>
<osmChange version="0.6" generator="test">
  <create>
    <node id="1" version="1" changeset="100" user="alice" lat="1" lon="1">
      <tag k="climbing" v="crag"/>
      <tag k="name" v="Oli Kahn"/>
    </node>
    <node id="2" version="1" changeset="100" user="alice" lat="1" lon="1">
      <tag k="amenity" v="cafe"/>
      <tag k="name" v="Not Climbing"/>
    </node>
  </create>
  <modify>
    <way id="3" version="7" changeset="101" user="bob">
      <tag k="sport" v="climbing"/>
      <tag k="leisure" v="sports_centre"/>
      <tag k="name" v="Kletterhalle"/>
    </way>
  </modify>
  <delete>
    <relation id="4" version="3" changeset="102" user="carol" visible="false">
      <tag k="climbing" v="crag"/>
    </relation>
  </delete>
</osmChange>
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

// sampleChange parses the sample osmChange, standing in for what the
// replication client returns.
func sampleChange(t *testing.T) *osm.Change {
	t.Helper()
	ch := &osm.Change{}
	if err := xml.Unmarshal([]byte(sample), ch); err != nil {
		t.Fatal(err)
	}
	return ch
}

// reportChanges runs the sample through report and decodes the output.
func reportChanges(t *testing.T) []change {
	t.Helper()
	var buf bytes.Buffer
	n, err := report(&buf, nil, nil, loadConfig(t), 42, sampleChange(t))
	if err != nil {
		t.Fatal(err)
	}

	var out []change
	dec := json.NewDecoder(&buf)
	for dec.More() {
		var c change
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("output is not valid JSON: %v", err)
		}
		out = append(out, c)
	}
	if n != len(out) {
		t.Errorf("report returned %d, but wrote %d objects", n, len(out))
	}
	return out
}

func TestReport(t *testing.T) {
	got := reportChanges(t)

	want := []change{
		{
			Minute: 42,
			Action: "create", Type: "node", ID: 1, Version: 1, User: "alice",
			Changeset: 100, Name: "Oli Kahn",
			Classes: []string{"crag"}, MatchedKeys: []string{"climbing"},
			URL:          "https://www.openstreetmap.org/node/1",
			ChangesetURL: "https://www.openstreetmap.org/changeset/100",
		},
		{
			Minute: 42,
			Action: "modify", Type: "way", ID: 3, Version: 7, User: "bob",
			Changeset: 101, Name: "Kletterhalle",
			Classes: []string{"gym"}, MatchedKeys: []string{"sport"},
			URL:          "https://www.openstreetmap.org/way/3",
			ChangesetURL: "https://www.openstreetmap.org/changeset/101",
		},
		{
			Minute: 42,
			Action: "delete", Type: "relation", ID: 4, Version: 3, User: "carol",
			Changeset: 102,
			Classes:   []string{"crag"}, MatchedKeys: []string{"climbing"},
			URL:          "https://www.openstreetmap.org/relation/4",
			ChangesetURL: "https://www.openstreetmap.org/changeset/102",
		},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d", len(got), len(want))
	}
	for i := range want {
		// Tags and Timestamp are checked separately; compare the rest.
		g := got[i]
		g.Tags, g.Timestamp, g.UID = nil, time.Time{}, 0
		if !reflect.DeepEqual(g, want[i]) {
			t.Errorf("change %d:\n got %+v\nwant %+v", i, g, want[i])
		}
	}
}

func TestReportIncludesAllTags(t *testing.T) {
	got := reportChanges(t)
	want := map[string]string{"climbing": "crag", "name": "Oli Kahn"}
	if !reflect.DeepEqual(got[0].Tags, want) {
		t.Errorf("Tags = %v, want %v", got[0].Tags, want)
	}
}

func TestReportSkipsNonClimbing(t *testing.T) {
	for _, c := range reportChanges(t) {
		if c.Name == "Not Climbing" {
			t.Errorf("non-climbing element was reported: %+v", c)
		}
	}
}

func TestReportEmitsOneJSONObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	if _, err := report(&buf, nil, nil, loadConfig(t), 42, sampleChange(t)); err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var c change
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Errorf("line %d is not a complete JSON object: %v", i, err)
		}
	}
}

func TestReportEmptyChange(t *testing.T) {
	var buf bytes.Buffer
	n, err := report(&buf, nil, nil, loadConfig(t), 42, &osm.Change{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || buf.Len() != 0 {
		t.Errorf("empty change produced %d changes, output %q", n, buf.String())
	}
}

func TestCount(t *testing.T) {
	if got := count(sampleChange(t)); got != 4 {
		t.Errorf("count() = %d, want 4", got)
	}
	if got := count(&osm.Change{}); got != 0 {
		t.Errorf("count(empty) = %d, want 0", got)
	}
}

func TestFirstMissing(t *testing.T) {
	const latest = 1000

	tests := []struct {
		name     string
		stored   replication.MinuteSeqNum
		backfill time.Duration
		want     replication.MinuteSeqNum
	}{
		// No history: go back by the backfill window, not further.
		{"first run, one hour", 0, time.Hour, 940},
		// A window longer than the whole sequence must not underflow.
		{"first run, longer than history", 0, 24 * time.Hour, 0},
		// A restart resumes right after the last stored minute.
		{"restart with small gap", 995, time.Hour, 996},
		// An old store still cannot pull more than the backfill window.
		{"restart with huge gap", 10, time.Hour, 940},
		// Nothing missed.
		{"already current", 1000, time.Hour, 1000},
		{"ahead of latest", 1005, time.Hour, 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstMissing(latest, tt.stored, tt.backfill)
			if got != tt.want {
				t.Errorf("firstMissing = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestReclassifyUpdatesStoredRecords(t *testing.T) {
	// A record written before the config had a classify section.
	stored := []byte(`{"minute":1,"action":"create","type":"relation","id":9,` +
		`"version":1,"changeset":5,"user":"alice","name":"Old Crag",` +
		`"matched_keys":["climbing"],"tags":{"climbing":"crag","sport":"climbing"},` +
		`"url":"https://www.openstreetmap.org/relation/9"}`)

	history := [][]byte{stored}
	reclassify(loadConfig(t), history)

	var got change
	if err := json.Unmarshal(history[0], &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Classes, []string{"crag"}) {
		t.Errorf("Classes = %v, want [crag]", got.Classes)
	}
	// Fields that are not derived must survive untouched.
	if got.Name != "Old Crag" || got.User != "alice" || got.Minute != 1 {
		t.Errorf("non-derived fields changed: %+v", got)
	}
	if got.URL != "https://www.openstreetmap.org/relation/9" {
		t.Errorf("URL = %q", got.URL)
	}
}

func TestReclassifyDropsStaleClasses(t *testing.T) {
	// A record whose class no longer matches the config must lose it.
	stored := []byte(`{"minute":1,"action":"create","type":"node","id":1,` +
		`"version":1,"changeset":5,"classes":["gone"],` +
		`"tags":{"climbing":"bolt"}}`)

	history := [][]byte{stored}
	reclassify(loadConfig(t), history)

	var got change
	if err := json.Unmarshal(history[0], &got); err != nil {
		t.Fatal(err)
	}
	// The test config has no rule for bolt, so no class should remain.
	if len(got.Classes) != 0 {
		t.Errorf("Classes = %v, want none", got.Classes)
	}
}

func TestReclassifySkipsUnparseableLines(t *testing.T) {
	history := [][]byte{[]byte("{not json")}
	reclassify(loadConfig(t), history)
	if string(history[0]) != "{not json" {
		t.Errorf("a bad line was rewritten: %q", history[0])
	}
}
