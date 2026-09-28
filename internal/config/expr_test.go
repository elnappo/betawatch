package config

import (
	"strings"
	"testing"
)

func TestExpr(t *testing.T) {
	tests := []struct {
		expr string
		typ  string
		tags map[string]string
		want bool
	}{
		// Key present, any value.
		{"climbing", "node", map[string]string{"climbing": "route"}, true},
		{"climbing", "node", map[string]string{"climbing": ""}, true},
		{"climbing", "node", map[string]string{"sport": "climbing"}, false},

		// Key prefix.
		{"climbing:*", "node", map[string]string{"climbing:grade:french": "7a"}, true},
		{"climbing:*", "node", map[string]string{"climbing": "route"}, false},

		// Exact value.
		{"sport=climbing", "node", map[string]string{"sport": "climbing"}, true},
		{"sport=climbing", "node", map[string]string{"sport": "soccer"}, false},
		{"sport=climbing", "node", map[string]string{}, false},

		// Value list.
		{"climbing=route,route_bottom", "node", map[string]string{"climbing": "route_bottom"}, true},
		{"climbing=route,route_bottom", "node", map[string]string{"climbing": "crag"}, false},

		// Negation: the key must be present but hold another value.
		{"climbing:boulder!=no", "node", map[string]string{"climbing:boulder": "12"}, true},
		{"climbing:boulder!=no", "node", map[string]string{"climbing:boulder": "no"}, false},
		{"climbing:boulder!=no", "node", map[string]string{}, false},

		// Type restriction.
		{"r/climbing=crag", "relation", map[string]string{"climbing": "crag"}, true},
		{"r/climbing=crag", "node", map[string]string{"climbing": "crag"}, false},
		{"nw/climbing=crag", "way", map[string]string{"climbing": "crag"}, true},
		{"nw/climbing=crag", "relation", map[string]string{"climbing": "crag"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.expr+"/"+tt.typ, func(t *testing.T) {
			e, err := ParseExpr(tt.expr)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tt.expr, err)
			}
			got := e.MatchesType(tt.typ) && e.Match(tt.tags)
			if got != tt.want {
				t.Errorf("%q against %v = %v, want %v", tt.expr, tt.tags, got, tt.want)
			}
		})
	}
}

func TestParseExprErrors(t *testing.T) {
	for _, expr := range []string{"", "   ", "=value", "climbing:*!=no"} {
		if _, err := ParseExpr(expr); err == nil {
			t.Errorf("ParseExpr(%q) succeeded, want an error", expr)
		}
	}
}

func TestParseExprKeyWithSlash(t *testing.T) {
	// "xyz/" is not a type prefix, so the whole string is the key.
	e, err := ParseExpr("addr:street/name=x")
	if err != nil {
		t.Fatal(err)
	}
	if e.key != "addr:street/name" {
		t.Errorf("key = %q, want %q", e.key, "addr:street/name")
	}
}

func TestSemicolonValues(t *testing.T) {
	// OSM packs several values into one tag with ";".
	e, err := ParseExpr("sport=climbing")
	if err != nil {
		t.Fatal(err)
	}
	for _, tags := range []map[string]string{
		{"sport": "climbing"},
		{"sport": "climbing;bouldering"},
		{"sport": "bouldering;climbing"},
		{"sport": "bouldering; climbing"},
	} {
		if !e.Match(tags) {
			t.Errorf("sport=climbing did not match %v", tags)
		}
	}
	for _, tags := range []map[string]string{
		{"sport": "bouldering"},
		{"sport": "soccer;tennis"},
	} {
		if e.Match(tags) {
			t.Errorf("sport=climbing matched %v, want no match", tags)
		}
	}
}

func TestFindAllReturnsEveryMatch(t *testing.T) {
	e, err := ParseExpr("climbing:*")
	if err != nil {
		t.Fatal(err)
	}
	got := e.FindAll(map[string]string{
		"climbing:grade:french": "7a",
		"climbing:bolts":        "12",
		"climbing:rock":         "granite",
		"sport":                 "climbing",
	})
	want := []string{"climbing:bolts", "climbing:grade:french", "climbing:rock"}
	if len(got) != len(want) {
		t.Fatalf("FindAll() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FindAll() = %v, want %v", got, want)
		}
	}
}

func TestValueWildcards(t *testing.T) {
	tests := []struct {
		expr string
		val  string
		want bool
	}{
		// Leading wildcard: the text appears anywhere in the value, which
		// is what osmium tags-filter means by "*Paris".
		{"sport=*climbing", "climbing", true},
		{"sport=*climbing", "rock_climbing", true},
		{"sport=*climbing", "bouldering;climbing", true},
		{"sport=*climbing", "climbing_gym", true},
		{"sport=*climbing", "soccer", false},
		{"old_name=*Werburgh", "St Werburgh's Church, Bristol", true},
		{"old_name=*Werburgh", "Werburgh", true},
		{"old_name=*Werburgh", "St Anne's", false},

		// Trailing wildcard: the value starts with the text.
		{"name=Wand*", "Wandberg", true},
		{"name=Wand*", "Nordwand", false},

		// A bare "*" matches any value, like the key-only form.
		{"sport=*", "anything", true},

		// No wildcard is still an exact match.
		{"sport=climbing", "rock_climbing", false},
	}

	for _, tt := range tests {
		t.Run(tt.expr+"/"+tt.val, func(t *testing.T) {
			e, err := ParseExpr(tt.expr)
			if err != nil {
				t.Fatal(err)
			}
			key, _, _ := strings.Cut(tt.expr, "=")
			if got := e.Match(map[string]string{key: tt.val}); got != tt.want {
				t.Errorf("%s against %s=%s = %v, want %v", tt.expr, key, tt.val, got, tt.want)
			}
		})
	}
}
