package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Impact analysis: which symbols a change touches, and what depends on them.

type ImpactSymbol struct {
	Symbol   Symbol `json:"symbol"`
	Depth    int    `json:"depth" jsonschema:"0 = changed itself, 1 = references a changed symbol, 2 = references one of those, ..."`
	Evidence string `json:"evidence" jsonschema:"weakest evidence on the path from the change to this symbol"`
	Via      string `json:"via,omitempty" jsonschema:"the symbol one step closer to the change"`
	Test     bool   `json:"test,omitempty"`
}

type ImpactResult struct {
	Scan         ScanInfo       `json:"scan"`
	Base         string         `json:"base" jsonschema:"the git revision the working tree was compared with"`
	ChangedFiles []string       `json:"changed_files"`
	NotAnalyzed  []string       `json:"not_analyzed" jsonschema:"changed files with no symbols in the index: deleted, not source code, or in a language rintel does not read"`
	Changed      []ImpactSymbol `json:"changed" jsonschema:"symbols whose lines were modified or added"`
	Affected     []ImpactSymbol `json:"affected" jsonschema:"production symbols that depend on the change, nearest first"`
	Endpoints    []Endpoint     `json:"endpoints" jsonschema:"endpoints whose handler changed or depends on the change"`
	Tests        []ImpactSymbol `json:"tests" jsonschema:"test functions that reach the change: the tests worth running"`
	Truncated    bool           `json:"truncated" jsonschema:"true when the depth limit cut the search short"`
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// changedLines parses `git diff --unified=0`: file -> changed line ranges on the new side.
func changedLines(root, base string) (map[string][][2]int, []string, error) {
	// The revision comes from a command line or an MCP caller. Passed as is, a value like "--output=/some/file" is a git
	// option, not a revision, and makes git write there.
	if base == "" || strings.HasPrefix(base, "-") {
		return nil, nil, fmt.Errorf("not a git revision: %q", base)
	}
	out, err := exec.Command("git", append(gitSafe(root), "diff", "--unified=0", "--no-color", "--no-ext-diff", "--no-textconv", "--relative", "--end-of-options", base, "--", ".")...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, nil, fmt.Errorf("git diff %s: %s", base, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, nil, fmt.Errorf("impact analysis needs git: %v", err)
	}
	ranges := map[string][][2]int{}
	var deleted []string
	file, old := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			old = strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "a/")
		case strings.HasPrefix(line, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
			if file == "/dev/null" {
				deleted = append(deleted, old)
				file = ""
			} else if _, ok := ranges[file]; !ok {
				ranges[file] = nil
			}
		case file != "" && strings.HasPrefix(line, "@@"):
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				start, _ := strconv.Atoi(m[1])
				n := 1
				if m[2] != "" {
					n, _ = strconv.Atoi(m[2])
				}
				if n == 0 { // pure deletion: the line it happened after
					ranges[file] = append(ranges[file], [2]int{start, start + 1})
				} else {
					ranges[file] = append(ranges[file], [2]int{start, start + n - 1})
				}
			}
		}
	}
	// files git does not track yet are new code in full
	if untracked, err := exec.Command("git", append(gitSafe(root), "ls-files", "--others", "--exclude-standard", "--", ".")...).Output(); err == nil {
		for _, f := range strings.Split(strings.TrimSpace(string(untracked)), "\n") {
			if f != "" && !strings.HasPrefix(f, ".ri/") {
				ranges[f] = [][2]int{{1, 1 << 30}}
			}
		}
	}
	return ranges, deleted, nil
}

func (q *Q) Impact(base string, depth int) (*ImpactResult, error) {
	si, err := q.scanInfo()
	if err != nil {
		return nil, err
	}
	if base == "" {
		base = "HEAD"
	}
	depth = min(max(depth, 1), 6)
	ranges, deleted, err := changedLines(q.root, base)
	if err != nil {
		return nil, err
	}
	g, err := q.loadGraph(si.ID)
	if err != nil {
		return nil, err
	}
	r := &ImpactResult{Scan: si, Base: base, ChangedFiles: sortedKeys(ranges), NotAnalyzed: deleted, Changed: []ImpactSymbol{}, Affected: []ImpactSymbol{}, Endpoints: []Endpoint{}, Tests: []ImpactSymbol{}}
	if r.NotAnalyzed == nil {
		r.NotAnalyzed = []string{}
	}

	// the innermost symbol covering each changed range: a method if there is one, else its class, else the file
	byFile := map[string][]*gnode{}
	for _, n := range g.order {
		if !n.ext && n.file != "" && n.synthetic == "" {
			byFile[n.file] = append(byFile[n.file], n)
		}
	}
	changed := map[*gnode]bool{}
	for _, f := range r.ChangedFiles {
		syms := byFile[f]
		if len(syms) == 0 {
			r.NotAnalyzed = append(r.NotAnalyzed, f)
			continue
		}
		for _, rg := range ranges[f] {
			hit := false
			for _, n := range syms {
				if n.typ != "class" && n.l1 <= rg[1] && n.l2 >= rg[0] {
					changed[n], hit = true, true
				}
			}
			if hit {
				continue
			}
			var best *gnode
			for _, n := range syms {
				if n.typ == "class" && n.l1 <= rg[1] && n.l2 >= rg[0] && (best == nil || n.l2-n.l1 < best.l2-best.l1) {
					best = n
				}
			}
			if best != nil {
				changed[best] = true
			}
		}
	}
	sort.Strings(r.NotAnalyzed)

	type reach struct {
		depth int
		ev    string
		via   *gnode
	}
	seen := map[*gnode]reach{}
	var frontier []*gnode
	for n := range changed {
		seen[n] = reach{0, "declared", nil}
		frontier = append(frontier, n)
	}
	sort.Slice(frontier, func(i, j int) bool { return frontier[i].id < frontier[j].id })
	for d := 1; len(frontier) > 0; d++ {
		if d > depth {
			r.Truncated = true
			break
		}
		var next []*gnode
		for _, n := range frontier {
			for _, e := range n.in {
				if e.rel == "overrides" || e.rel == "routes" {
					continue
				}
				if _, ok := seen[e.src]; ok || e.src.ext {
					continue
				}
				ev := seen[n].ev
				if evidenceRank[e.evidence] > evidenceRank[ev] {
					ev = e.evidence
				}
				seen[e.src] = reach{d, ev, n}
				next = append(next, e.src)
			}
		}
		sort.Slice(next, func(i, j int) bool { return next[i].id < next[j].id })
		frontier = next
	}

	var all []*gnode
	for n := range seen {
		all = append(all, n)
	}
	sort.Slice(all, func(i, j int) bool {
		if seen[all[i]].depth != seen[all[j]].depth {
			return seen[all[i]].depth < seen[all[j]].depth
		}
		return all[i].id < all[j].id
	})
	hit := map[string]bool{}
	for _, n := range all {
		rc := seen[n]
		is := ImpactSymbol{Symbol: q.symbol(si.ID, n.id), Depth: rc.depth, Evidence: rc.ev, Test: n.test}
		if rc.via != nil {
			is.Via = rc.via.id
		}
		hit[n.id] = true
		switch {
		case rc.depth == 0:
			r.Changed = append(r.Changed, is)
		case n.test:
			// a test function, or a test file whose tests are top-level calls (test("...", () => ...)); not fixture types
			if n.typ == "method" || n.kind == "file" {
				r.Tests = append(r.Tests, is)
			}
		default:
			r.Affected = append(r.Affected, is)
		}
	}
	for cursor := ""; ; {
		page, err := q.APISpec("", 200, cursor)
		if err != nil {
			return nil, err
		}
		for _, e := range page.Endpoints {
			if hit[e.Handler] {
				r.Endpoints = append(r.Endpoints, e)
			}
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	return r, nil
}
