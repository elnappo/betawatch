// Package config loads the tag filter that decides which OSM elements are
// climbing-related.
package config

import (
	"fmt"
	"slices"
	"strings"
)

// Expr matches a single tag. The syntax is a subset of osmium tags-filter:
//
//	climbing              key present, any value
//	climbing:*            key prefix
//	sport=climbing        exact value
//	sport=climbing,ice    any of these values
//	sport=*climbing       value contains "climbing"
//	name=Wand*            value starts with "Wand"
//
// Values are matched against each ";"-separated part of the tag, so
// sport=climbing also matches sport=bouldering;climbing.
//
//	climbing:boulder!=no  key present, value is not one of these
//
// A leading type restriction limits which element types can match:
//
//	r/climbing=crag       relations only ("n", "w", "r", combinable as "nw/")
type Expr struct {
	src    string
	types  string // "" means any; otherwise some combination of n, w, r
	key    string
	prefix bool     // key ended in "*"
	values []string // empty means "key present, any value"
	negate bool     // the "!=" form
}

// ParseExpr parses one filter expression.
func ParseExpr(s string) (Expr, error) {
	e := Expr{src: s}
	rest := strings.TrimSpace(s)
	if rest == "" {
		return e, fmt.Errorf("empty expression")
	}

	// Optional "nwr/" type prefix. A key can contain "/" (e.g. "name/old"),
	// so only treat it as a type prefix if it is made purely of n, w and r.
	if i := strings.Index(rest, "/"); i >= 0 && isTypePrefix(rest[:i]) {
		e.types, rest = rest[:i], rest[i+1:]
	}

	if i := strings.Index(rest, "="); i >= 0 {
		key, vals := rest[:i], rest[i+1:]
		if strings.HasSuffix(key, "!") {
			e.negate = true
			key = key[:len(key)-1]
		}
		e.key = key
		for _, v := range strings.Split(vals, ",") {
			e.values = append(e.values, strings.TrimSpace(v))
		}
	} else {
		e.key = rest
	}

	if strings.HasSuffix(e.key, "*") {
		e.prefix = true
		e.key = strings.TrimSuffix(e.key, "*")
	}
	if e.key == "" {
		return e, fmt.Errorf("expression %q has no key", s)
	}
	if e.negate && e.prefix {
		return e, fmt.Errorf("expression %q combines a key wildcard with !=", s)
	}
	return e, nil
}

func isTypePrefix(s string) bool {
	if s == "" {
		return false
	}
	return strings.Trim(s, "nwr") == ""
}

// String returns the expression as originally written.
func (e Expr) String() string { return e.src }

// MatchesType reports whether the expression applies to an element type
// ("node", "way" or "relation").
func (e Expr) MatchesType(elemType string) bool {
	if e.types == "" || elemType == "" {
		return true
	}
	return strings.ContainsRune(e.types, rune(elemType[0]))
}

// Match reports whether any tag satisfies the expression.
func (e Expr) Match(tags map[string]string) bool {
	_, ok := e.Find(tags)
	return ok
}

// Find returns the key of one tag satisfying the expression. When several
// tags match, which one is returned is unspecified; use FindAll for all.
func (e Expr) Find(tags map[string]string) (string, bool) {
	// A negated expression means "the key is present and holds some other
	// value", so a missing key is not a match.
	for k, v := range tags {
		if e.tagMatches(k, v) {
			return k, true
		}
	}
	return "", false
}

// FindAll returns every tag key satisfying the expression, sorted. A
// prefix expression such as climbing:* usually matches several.
func (e Expr) FindAll(tags map[string]string) []string {
	var out []string
	for k, v := range tags {
		if e.tagMatches(k, v) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func (e Expr) tagMatches(k, v string) bool {
	if !e.keyMatches(k) {
		return false
	}
	if len(e.values) == 0 {
		return true
	}
	return e.valueMatches(v) != e.negate
}

// valueMatches compares against each ";"-separated part, since OSM uses
// that to hold several values in one tag (sport=bouldering;climbing).
func (e Expr) valueMatches(v string) bool {
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		for _, want := range e.values {
			if matchValue(want, part) {
				return true
			}
		}
	}
	return false
}

// matchValue compares one value, honouring a leading or trailing "*".
// Following osmium tags-filter: a leading asterisk matches a substring
// anywhere in the value, a trailing one matches a prefix.
func matchValue(want, got string) bool {
	switch {
	case want == "*":
		return true
	case strings.HasPrefix(want, "*"):
		return strings.Contains(got, want[1:])
	case strings.HasSuffix(want, "*"):
		return strings.HasPrefix(got, want[:len(want)-1])
	default:
		return want == got
	}
}

func (e Expr) keyMatches(k string) bool {
	if e.prefix {
		return strings.HasPrefix(k, e.key)
	}
	return k == e.key
}

// Exprs is a list of expressions, matched as an OR.
type Exprs []Expr

// ParseExprs parses a list of expressions.
func ParseExprs(ss []string) (Exprs, error) {
	var out Exprs
	for _, s := range ss {
		e, err := ParseExpr(s)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// MatchAny reports whether any expression matches, respecting type prefixes.
func (es Exprs) MatchAny(elemType string, tags map[string]string) bool {
	_, ok := es.FindAny(elemType, tags)
	return ok
}

// FindAny returns the tag key matched by the first matching expression.
func (es Exprs) FindAny(elemType string, tags map[string]string) (string, bool) {
	for _, e := range es {
		if !e.MatchesType(elemType) {
			continue
		}
		if k, ok := e.Find(tags); ok {
			return k, true
		}
	}
	return "", false
}

// MatchAll reports whether every expression matches (an AND).
func (es Exprs) MatchAll(elemType string, tags map[string]string) bool {
	if len(es) == 0 {
		return false
	}
	for _, e := range es {
		if !e.MatchesType(elemType) || !e.Match(tags) {
			return false
		}
	}
	return true
}
