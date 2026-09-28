package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// testConfig is a fixture covering every expression form. The tests use
// it rather than the repo config.yaml, which is data the user edits: a
// filter change should not break the test suite.
const testConfig = `
select:
  - climbing
  - climbing:*
  - sport=climbing
  - wikimedia_commons:path
  - path=climbing_access
  - tower:type=climbing
  - playground=climbingwall
classify:
  route:
    - climbing=route,route_bottom,route_top
  crag:
    - climbing=crag
  area:
    - climbing=area
  boulder:
    - climbing=boulder
    - climbing:boulder!=no
  bolt:
    - climbing=bolt
  gym:
    - leisure=sports_centre,sports_hall,pitch
    - tower:type=climbing
    - playground=climbingwall
    - indoor=yes
  cliff:
    - natural=cliff,rock,stone,bare_rock
  access_path:
    - path=climbing_access
`

func loadString(t *testing.T, body string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func realConfig(t *testing.T) *Config {
	t.Helper()
	return loadString(t, testConfig)
}

// TestRepoConfigLoads checks the shipped config.yaml is valid, without
// asserting anything about which tags it selects.
func TestRepoConfigLoads(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "config.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestSelects(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		tags map[string]string
		want bool
	}{
		{"sport climbing", "node", map[string]string{"sport": "climbing"}, true},
		{"climbing route", "node", map[string]string{"climbing": "route"}, true},
		{"grade", "node", map[string]string{"climbing:grade:french": "7a"}, true},
		{"hazard", "node", map[string]string{"climbing:hazard:loose_rock": "yes"}, true},
		{"topo path", "way", map[string]string{"wikimedia_commons:path": "0.1,0.2"}, true},
		{"access path", "way", map[string]string{"highway": "path", "path": "climbing_access"}, true},
		{"climbing tower", "node", map[string]string{"man_made": "tower", "tower:type": "climbing"}, true},
		{"playground wall", "node", map[string]string{"playground": "climbingwall"}, true},

		// Must not swamp the output.
		{"plain cliff", "way", map[string]string{"natural": "cliff"}, false},
		{"plain rock", "node", map[string]string{"natural": "rock"}, false},
		{"football pitch", "way", map[string]string{"leisure": "pitch", "sport": "soccer"}, false},
		{"sports centre", "way", map[string]string{"leisure": "sports_centre"}, false},
		{"plain path", "way", map[string]string{"highway": "path"}, false},
		{"bouldering only", "node", map[string]string{"sport": "bouldering"}, false},
		{"sport list", "node", map[string]string{"sport": "bouldering;climbing"}, true},
		{"empty", "node", map[string]string{}, false},
		{"untagged", "node", nil, false},
	}

	c := realConfig(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.Selects(tt.typ, tt.tags); got != tt.want {
				t.Errorf("Selects(%v) = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}

func TestClasses(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		tags map[string]string
		want []string
	}{
		{"route", "node", map[string]string{"climbing": "route"}, []string{"route"}},
		{"route bottom", "node", map[string]string{"climbing": "route_bottom"}, []string{"route"}},
		{"crag", "relation", map[string]string{"climbing": "crag"}, []string{"crag"}},
		{"area", "relation", map[string]string{"climbing": "area"}, []string{"area"}},
		{"boulder on stone", "node", map[string]string{"climbing": "boulder", "natural": "stone"}, []string{"boulder", "cliff"}},
		{"crag with boulders", "relation", map[string]string{"climbing": "crag", "climbing:boulder": "12"}, []string{"boulder", "crag"}},
		{"climbing:boulder=no", "relation", map[string]string{"climbing": "crag", "climbing:boulder": "no"}, []string{"crag"}},
		{"gym", "way", map[string]string{"leisure": "sports_centre", "sport": "climbing"}, []string{"gym"}},
		{"cliff", "way", map[string]string{"natural": "cliff", "sport": "climbing"}, []string{"cliff"}},
		{"access path", "way", map[string]string{"path": "climbing_access"}, []string{"access_path"}},
		{"bolt", "node", map[string]string{"climbing": "bolt"}, []string{"bolt"}},
		{"unclassified", "node", map[string]string{"sport": "climbing"}, nil},
	}

	c := realConfig(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.Classes(tt.typ, tt.tags); !slices.Equal(got, tt.want) {
				t.Errorf("Classes(%v) = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}

func TestMatchedKeys(t *testing.T) {
	c := realConfig(t)
	got := c.MatchedKeys("node", map[string]string{
		"sport":                 "climbing",
		"climbing:grade:french": "7a",
		"climbing:rock":         "granite",
		"climbing":              "route",
		"name":                  "Südwand",
		"natural":               "cliff",
	})
	// Every matching key, not just one per expression.
	want := []string{"climbing", "climbing:grade:french", "climbing:rock", "sport"}
	if !slices.Equal(got, want) {
		t.Errorf("MatchedKeys() = %v, want %v", got, want)
	}
}

func TestVersionChangesWithContent(t *testing.T) {
	a := loadString(t, "select: [climbing]\n").Version()
	b := loadString(t, "select: [climbing, sport=climbing]\n").Version()
	if a == b {
		t.Error("version did not change when the config changed")
	}
}

func TestLoadErrors(t *testing.T) {
	tests := map[string]string{
		"no select":      "classify:\n  route: [climbing=route]\n",
		"bad expression": "select: [\"=nokey\"]\n",
		"bad classify":   "select: [climbing]\nclassify:\n  route: [\"\"]\n",
		"not yaml":       "select: [climbing\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Error("Load succeeded, want an error")
			}
		})
	}
}

// TestFlowStyleSplitsOnCommas documents a YAML trap: in flow style
// [a=b,c] the comma is a list separator, so the value list is lost.
// Block style keeps it as one expression.
func TestFlowStyleSplitsOnCommas(t *testing.T) {
	flow := loadString(t, "select: [climbing]\nclassify:\n  cliff: [natural=cliff,rock]\n")
	block := loadString(t, "select: [climbing]\nclassify:\n  cliff:\n    - natural=cliff,rock\n")

	tags := map[string]string{"climbing": "boulder", "natural": "rock"}
	if got := flow.Classes("node", tags); len(got) != 0 {
		t.Errorf("flow style: Classes() = %v, want none (the ,rock became a separate expression)", got)
	}
	if got := block.Classes("node", tags); !slices.Equal(got, []string{"cliff"}) {
		t.Errorf("block style: Classes() = %v, want [cliff]", got)
	}
}

func TestUnwanted(t *testing.T) {
	c := loadString(t, `
select:
  - climbing
  - climbing:*
  - sport=*climbing
unwanted_tags:
  - climbing:bouldering
  - climbing=bouldering
  - sport=*boulder
  - site=climbing_area
`)

	tests := []struct {
		name string
		tags map[string]string
		want []string
	}{
		{"clean", map[string]string{"climbing": "crag", "sport": "climbing"}, nil},
		{"key form", map[string]string{"climbing:bouldering": "yes"}, []string{"climbing:bouldering"}},
		{"value form", map[string]string{"climbing": "bouldering"}, []string{"climbing"}},
		{"wildcard value", map[string]string{"sport": "boulder"}, []string{"sport"}},
		{"wildcard suffix", map[string]string{"sport": "rock_boulder"}, []string{"sport"}},
		{"relation type", map[string]string{"site": "climbing_area"}, []string{"site"}},
		{
			"several at once",
			map[string]string{"climbing:bouldering": "yes", "site": "climbing_area"},
			[]string{"climbing:bouldering", "site"},
		},
		// climbing=crag is wanted, so the key must not be reported.
		{"right value", map[string]string{"climbing": "crag"}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.Unwanted("node", tt.tags); !slices.Equal(got, tt.want) {
				t.Errorf("Unwanted(%v) = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}

func TestUnwantedIsOptional(t *testing.T) {
	c := loadString(t, "select: [climbing]\n")
	if got := c.Unwanted("node", map[string]string{"climbing": "crag"}); got != nil {
		t.Errorf("Unwanted() = %v, want nil when no unwanted_tags are configured", got)
	}
}

func TestUnwantedBadExpressionFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte("select: [climbing]\nunwanted_tags: [\"=nokey\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("Load succeeded, want an error for a bad unwanted_tags expression")
	}
}

func TestBadValues(t *testing.T) {
	c := loadString(t, `
select:
  - climbing
  - climbing:*
  - ele
tag_values_regex:
  - climbing: "^(route|crag|boulder)$"
  - climbing:bolts: "^[1-9][0-9]*$"
  - climbing:hazard:*: "^yes$"
  - ele: "^[1-9][0-9]*$"
`)

	tests := []struct {
		name string
		tags map[string]string
		want []string // the keys expected to fail
	}{
		{"all good", map[string]string{"climbing": "crag", "climbing:bolts": "12"}, nil},
		{"bad enum", map[string]string{"climbing": "bouldering"}, []string{"climbing"}},
		{"bad number", map[string]string{"climbing:bolts": "lots"}, []string{"climbing:bolts"}},
		{"zero is not allowed", map[string]string{"ele": "0"}, []string{"ele"}},
		{"wildcard key matches", map[string]string{"climbing:hazard:loose_rock": "no"},
			[]string{"climbing:hazard:loose_rock"}},
		{"wildcard key passes", map[string]string{"climbing:hazard:wet": "yes"}, nil},
		// A key with no rule is never reported.
		{"unruled key", map[string]string{"name": "whatever", "climbing:rock": "granite"}, nil},
		{
			"several at once, sorted",
			map[string]string{"ele": "x", "climbing": "gym", "climbing:bolts": "0"},
			[]string{"climbing", "climbing:bolts", "ele"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, b := range c.BadValues(tt.tags) {
				got = append(got, b.Key)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("BadValues(%v) keys = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}

func TestBadValueCarriesTheRegex(t *testing.T) {
	c := loadString(t, "select: [climbing]\ntag_values_regex:\n  - climbing:bolts: \"^[0-9]+$\"\n")
	got := c.BadValues(map[string]string{"climbing:bolts": "many"})
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if got[0].Value != "many" || got[0].Regex != "^[0-9]+$" {
		t.Errorf("finding = %+v, want the offending value and its pattern", got[0])
	}
}

// An exact key listed alongside a wildcard must be reported once, by the
// first rule that matches it.
func TestBadValuesFirstRuleWins(t *testing.T) {
	c := loadString(t, `
select: [climbing]
tag_values_regex:
  - climbing:hazard:wet: "^(yes|no)$"
  - climbing:hazard:*: "^yes$"
`)
	if got := c.BadValues(map[string]string{"climbing:hazard:wet": "no"}); len(got) != 0 {
		t.Errorf("BadValues() = %v, want none: the exact rule is listed first", got)
	}
}

func TestBadRegexFailsToLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	body := "select: [climbing]\ntag_values_regex:\n  - climbing: \"^([unclosed$\"\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("Load succeeded, want an error for an uncompilable regex")
	}
}

// The config the command actually ships must load and its patterns must
// compile.
func TestShippedConfigRegexesCompile(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "cmd", "changes", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TagValues) == 0 {
		t.Error("no tag_values_regex rules were loaded")
	}
}
