package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config is the optional, committed .ri/config.json. Every field has a default; unknown fields are an error so a
// typo in a threshold name does not silently do nothing.
type Config struct {
	Exclude    []string   `json:"exclude"`    // glob patterns of paths to leave out, e.g. "**/*.pb.go", "legacy/**"
	Thresholds Thresholds `json:"thresholds"` // limits used by `rintel check`
	Disable    []string   `json:"disable"`    // rule ids to switch off
	exclude    []*regexp.Regexp
	err        error
}

func loadConfig(root string) *Config {
	c := &Config{Thresholds: defaultThresholds}
	b, err := os.ReadFile(filepath.Join(root, ".ri", "config.json"))
	if err != nil {
		return c
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return &Config{Thresholds: defaultThresholds, err: fmt.Errorf(".ri/config.json: %v", err)}
	}
	for name, v := range map[string]int{"cc": c.Thresholds.CC, "method_loc": c.Thresholds.MethodLOC, "nesting": c.Thresholds.Nesting, "params": c.Thresholds.Params,
		"wmc": c.Thresholds.WMC, "class_loc": c.Thresholds.ClassLOC, "cbo": c.Thresholds.CBO, "lcom": c.Thresholds.LCOM} {
		if v < 1 { // a zero or negative limit would flag everything; to switch a rule off there is "disable"
			c.err = fmt.Errorf(".ri/config.json: threshold %q must be 1 or more, got %d", name, v)
		}
	}
	for _, id := range c.Disable {
		if ruleTitles[id] == "" {
			c.err = fmt.Errorf(".ri/config.json: unknown rule %q in disable", id)
		}
	}
	for _, g := range c.Exclude {
		c.exclude = append(c.exclude, globRegexp(g))
	}
	return c
}

// globRegexp turns a path glob into a regexp: ** crosses directories, * and ? stay inside one path element.
func globRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; {
		case c == '*' && i+1 < len(glob) && glob[i+1] == '*':
			b.WriteString(".*")
			i++
			if i+1 < len(glob) && glob[i+1] == '/' { // "**/" also matches no directory at all
				b.WriteString("/?")
				i++
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("(/.*)?$") // a pattern naming a directory covers what is inside it
	return regexp.MustCompile(b.String())
}

func (c *Config) excluded(rel string) bool {
	for _, re := range c.exclude {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}
