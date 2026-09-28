package config

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the tag filter, loaded from config.yaml.
type Config struct {
	// Select decides which elements are climbing-related. An element
	// matching none of these expressions is ignored entirely.
	Select []string `yaml:"select"`

	// Classify labels a selected element (route, crag, gym...). Labels are
	// descriptive only: they never affect whether an element is selected.
	Classify map[string][]string `yaml:"classify"`

	// UnwantedTags are tags worth a second look: deprecated spellings,
	// undocumented values, and the like. They never affect selection.
	UnwantedTags []string `yaml:"unwanted_tags"`

	// TagValues are the patterns tag values must match. They never
	// affect selection.
	TagValues []TagValueRule `yaml:"tag_values_regex"`

	// HTTPAddr is the address to serve the web view on (e.g., ":8080").
	// Empty to disable.
	HTTPAddr string `yaml:"http_addr"`

	// StoreFile is the path to the file keeping recent changes.
	// Empty to disable.
	StoreFile string `yaml:"store_file"`

	// StateFile is the path to the file recording the last processed minute.
	// Empty to disable.
	StateFile string `yaml:"state_file"`

	// RetainDuration is how far back to keep changes.
	RetainDuration time.Duration `yaml:"retain_duration"`

	// BackfillDuration is how far back to catch up on first run.
	BackfillDuration time.Duration `yaml:"backfill_duration"`

	version       string
	selectExprs   Exprs
	classifyExprs map[string]Exprs
	unwantedExprs Exprs
}

// Load reads and compiles a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{
		// Set defaults
		RetainDuration:   7 * 24 * time.Hour,
		BackfillDuration: 1 * time.Hour,
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	sum := sha256.Sum256(data)
	c.version = hex.EncodeToString(sum[:])[:12]

	if err := c.compile(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Version identifies the config, so results can be traced back to the exact
// filter that produced them.
func (c *Config) Version() string { return c.version }

func (c *Config) compile() error {
	if len(c.Select) == 0 {
		return fmt.Errorf("select: must list at least one expression")
	}
	var err error
	if c.selectExprs, err = ParseExprs(c.Select); err != nil {
		return fmt.Errorf("select: %w", err)
	}

	c.classifyExprs = make(map[string]Exprs, len(c.Classify))
	for name, list := range c.Classify {
		if c.classifyExprs[name], err = ParseExprs(list); err != nil {
			return fmt.Errorf("classify %s: %w", name, err)
		}
	}

	if c.unwantedExprs, err = ParseExprs(c.UnwantedTags); err != nil {
		return fmt.Errorf("unwanted_tags: %w", err)
	}

	for i := range c.TagValues {
		r := &c.TagValues[i]
		if r.Key == "" {
			return fmt.Errorf("tag_values_regex: entry %d has no key", i)
		}
		// A trailing "*" covers a family of keys, as in select.
		r.prefix = strings.HasSuffix(r.Key, "*")
		r.Key = strings.TrimSuffix(r.Key, "*")

		if r.re, err = regexp.Compile(r.Regex); err != nil {
			return fmt.Errorf("tag_values_regex %s: %w", r.Key, err)
		}
	}
	return nil
}

// TagValueRule is one entry of tag_values_regex: a key pattern and the
// pattern its value must match.
type TagValueRule struct {
	// Key is the tag key, which may end in "*" to cover a family such as
	// climbing:hazard:*.
	Key string
	// Regex is the pattern the value has to match.
	Regex string

	prefix bool
	re     *regexp.Regexp
}

// matches reports whether this rule applies to a tag key.
func (r *TagValueRule) matches(key string) bool {
	if r.prefix {
		return strings.HasPrefix(key, r.Key)
	}
	return key == r.Key
}

// UnmarshalYAML accepts the one-key mapping the config uses:
//
//   - climbing:bolts: "^[1-9][0-9]*$"
func (r *TagValueRule) UnmarshalYAML(node *yaml.Node) error {
	var m map[string]string
	if err := node.Decode(&m); err != nil {
		return err
	}
	if len(m) != 1 {
		return fmt.Errorf("want one key per entry, got %d", len(m))
	}
	for k, v := range m {
		r.Key, r.Regex = k, v
	}
	return nil
}

// BadValue is a tag whose value does not match its rule.
type BadValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Regex is the pattern the value failed, so the page can say why.
	Regex string `json:"regex"`
}

// BadValues returns the element's tags whose values fail their rule,
// sorted by key. A tag with no rule is never reported: the rules cover
// the keys worth checking, not every key in OSM.
func (c *Config) BadValues(tags map[string]string) []BadValue {
	var out []BadValue
	for k, v := range tags {
		for i := range c.TagValues {
			r := &c.TagValues[i]
			if !r.matches(k) {
				continue
			}
			if !r.re.MatchString(v) {
				out = append(out, BadValue{Key: k, Value: v, Regex: r.Regex})
			}
			// The first matching rule decides, so an exact key listed
			// alongside a wildcard is not reported twice.
			break
		}
	}
	slices.SortFunc(out, func(a, b BadValue) int {
		return cmp.Compare(a.Key, b.Key)
	})
	return out
}

// Selects reports whether an element is climbing-related. elemType is
// "node", "way" or "relation".
func (c *Config) Selects(elemType string, tags map[string]string) bool {
	return c.selectExprs.MatchAny(elemType, tags)
}

// MatchedKeys returns the tag keys that caused an element to be selected.
func (c *Config) MatchedKeys(elemType string, tags map[string]string) []string {
	var out []string
	for _, e := range c.selectExprs {
		if !e.MatchesType(elemType) {
			continue
		}
		for _, k := range e.FindAll(tags) {
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Unwanted returns the element's tag keys that match unwanted_tags,
// sorted. An empty result means nothing about the element looks off.
func (c *Config) Unwanted(elemType string, tags map[string]string) []string {
	var out []string
	for _, e := range c.unwantedExprs {
		if !e.MatchesType(elemType) {
			continue
		}
		for _, k := range e.FindAll(tags) {
			if !slices.Contains(out, k) {
				out = append(out, k)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Classes returns the labels matching an element, sorted for stable output.
func (c *Config) Classes(elemType string, tags map[string]string) []string {
	var out []string
	for name, exprs := range c.classifyExprs {
		if exprs.MatchAny(elemType, tags) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
