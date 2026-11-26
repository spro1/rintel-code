package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Structure analysis shared by the CLI, MCP and the report: the class and package dependency graphs, cycles,
// and the checks. Everything is derived from one scan, so every surface gives the same answer.

type gnode struct {
	id, typ, parent, file string
	l1, l2                int
	ext, test             bool
	kind, role, synthetic string
	annotated, abstract   bool
	public                bool
	top                   *gnode // outermost class (Go/TS/Python: named type or file container)
	in, out               []*gedge
}

type gedge struct {
	src, dst      *gnode
	rel, evidence string
}

type graph struct {
	scan  int64
	nodes map[string]*gnode
	order []*gnode
	edges []*gedge
	blind map[*gnode]bool // classes with a type analysis could not resolve: "overrides nothing" is unprovable there
}

func (q *Q) loadGraph(scan int64) (*graph, error) {
	g := &graph{scan: scan, nodes: map[string]*gnode{}, blind: map[*gnode]bool{}}
	rows, err := q.db.Query(`SELECT id, type, COALESCE(parent,''), COALESCE(file,''), COALESCE(l1,0), COALESCE(l2,0), external, COALESCE(attrs,'{}') FROM symbols WHERE scan_id=? ORDER BY id`, scan)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		n := &gnode{}
		var attrs string
		if err := rows.Scan(&n.id, &n.typ, &n.parent, &n.file, &n.l1, &n.l2, &n.ext, &attrs); err != nil {
			return nil, err
		}
		var a symAttrs
		json.Unmarshal([]byte(attrs), &a)
		n.kind, n.synthetic, n.role, n.abstract, n.public = a.Kind, a.Synthetic, roleOf(a.Annotations), contains(a.Modifiers, "abstract"), contains(a.Modifiers, "public")
		for _, an := range a.Annotations {
			if s := simpleName(an.Name); s != "Override" && s != "SuppressWarnings" && s != "Deprecated" && s != "SafeVarargs" && s != "Generated" {
				n.annotated = true
			}
		}
		n.test = testPath.MatchString(n.file)
		g.nodes[n.id] = n
		g.order = append(g.order, n)
	}
	rows.Close()
	for _, n := range g.order {
		if n.ext {
			continue
		}
		for x := n; x != nil; x = g.nodes[x.parent] {
			if x.typ == "class" {
				n.top = x
			}
		}
	}
	rows, err = q.db.Query(`SELECT src, dst, relation, evidence_level FROM dependencies WHERE scan_id=? ORDER BY src, dst, relation`, scan)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s, d, rel, ev string
		rows.Scan(&s, &d, &rel, &ev)
		if a, b := g.nodes[s], g.nodes[d]; a != nil && b != nil {
			e := &gedge{a, b, rel, ev}
			g.edges = append(g.edges, e)
			a.out, b.in = append(a.out, e), append(b.in, e)
		}
	}
	rows.Close()
	for _, n := range g.order { // Spring Data style repositories carry no stereotype
		if n.typ == "class" && n.role == "" && n.kind == "interface" {
			for _, e := range n.out {
				if e.rel == "extends" && strings.HasSuffix(e.dst.id, "Repository") {
					n.role = "repository"
				}
			}
		}
	}
	rows, err = q.db.Query(`SELECT DISTINCT src FROM unresolved_refs WHERE scan_id=? AND reason='missing-type'`, scan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		if n := g.nodes[s]; n != nil && n.top != nil {
			g.blind[n.top] = true
		}
	}
	return g, rows.Err()
}

// pkgName is the package a class belongs to: the Java or Go package, or the directory for file-based languages.
func pkgName(id string) string {
	q := id[strings.Index(id, ":")+1:]
	i := strings.LastIndex(q, ".")
	if i < 0 {
		return ""
	}
	q = q[:i]
	if strings.HasPrefix(id, "ts:") || strings.HasPrefix(id, "py:") {
		if j := strings.LastIndex(q, "/"); j >= 0 {
			return q[:j]
		}
		return "."
	}
	return q
}

// ---- dependencies ----

type ClassDep struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Count int    `json:"count" jsonschema:"references from one class to the other"`
}

type PackageDep struct {
	From    string     `json:"from"`
	To      string     `json:"to"`
	Count   int        `json:"count" jsonschema:"references between classes of the two packages"`
	InCycle bool       `json:"in_cycle,omitempty" jsonschema:"true when the two packages are part of the same dependency cycle"`
	Classes []ClassDep `json:"classes,omitempty" jsonschema:"the class pairs behind this dependency, strongest first; only with detail"`
}

type DepsResult struct {
	Scan          ScanInfo     `json:"scan"`
	Packages      []string     `json:"packages"`
	Dependencies  []PackageDep `json:"dependencies" jsonschema:"package A depends on package B; production code only unless tests were requested"`
	PackageCycles [][]string   `json:"package_cycles" jsonschema:"groups of packages that depend on each other in a loop"`
	ClassCycles   [][]string   `json:"class_cycles" jsonschema:"groups of classes that reference each other in a loop"`
}

type classGraph struct {
	classes []*gnode
	dep     map[[2]*gnode]int
}

func (g *graph) classDeps(tests bool) *classGraph {
	cg := &classGraph{dep: map[[2]*gnode]int{}}
	for _, n := range g.order {
		if !n.ext && n.typ == "class" && n.top == n && (tests || !n.test) {
			cg.classes = append(cg.classes, n)
		}
	}
	for _, e := range g.edges {
		a, b := e.src.top, e.dst.top
		if e.rel == "overrides" || a == nil || b == nil || a == b || (!tests && (a.test || b.test)) {
			continue
		}
		cg.dep[[2]*gnode{a, b}]++
	}
	return cg
}

// cycles returns the strongly connected components with more than one member (Tarjan), each sorted, largest first.
func cycles[T comparable](nodes []T, edges [][2]T, name func(T) string) [][]string {
	adj := map[T][]T{}
	for _, e := range edges {
		adj[e[0]] = append(adj[e[0]], e[1])
	}
	index, low := map[T]int{}, map[T]int{}
	on := map[T]bool{}
	var stack []T
	var out [][]string
	n := 0
	var visit func(v T)
	visit = func(v T) {
		index[v], low[v] = n, n
		n++
		stack = append(stack, v)
		on[v] = true
		for _, w := range adj[v] {
			if _, seen := index[w]; !seen {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if on[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				on[w] = false
				comp = append(comp, name(w))
				if w == v {
					break
				}
			}
			if len(comp) > 1 {
				sort.Strings(comp)
				out = append(out, comp)
			}
		}
	}
	for _, v := range nodes {
		if _, seen := index[v]; !seen {
			visit(v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i][0] < out[j][0]
	})
	return out
}

func (g *graph) deps(tests, detail bool) *DepsResult {
	cg := g.classDeps(tests)
	r := &DepsResult{Packages: []string{}, Dependencies: []PackageDep{}, PackageCycles: [][]string{}, ClassCycles: [][]string{}}
	pkgs := map[string]bool{}
	for _, c := range cg.classes {
		pkgs[pkgName(c.id)] = true
	}
	r.Packages = sortedKeys(pkgs)
	pd := map[[2]string]*PackageDep{}
	var cedges [][2]*gnode
	for k, n := range cg.dep {
		cedges = append(cedges, k)
		a, b := pkgName(k[0].id), pkgName(k[1].id)
		if a == b {
			continue
		}
		d := pd[[2]string{a, b}]
		if d == nil {
			d = &PackageDep{From: a, To: b}
			pd[[2]string{a, b}] = d
		}
		d.Count += n
		if detail {
			d.Classes = append(d.Classes, ClassDep{k[0].id, k[1].id, n})
		}
	}
	sort.Slice(cedges, func(i, j int) bool {
		return cedges[i][0].id+"\x00"+cedges[i][1].id < cedges[j][0].id+"\x00"+cedges[j][1].id
	})
	var pedges [][2]string
	for k := range pd {
		pedges = append(pedges, k)
	}
	sort.Slice(pedges, func(i, j int) bool { return pedges[i][0]+"\x00"+pedges[i][1] < pedges[j][0]+"\x00"+pedges[j][1] })
	if c := cycles(r.Packages, pedges, func(s string) string { return s }); c != nil {
		r.PackageCycles = c
	}
	if c := cycles(cg.classes, cedges, func(n *gnode) string { return n.id }); c != nil {
		r.ClassCycles = c
	}
	comp := map[string]int{}
	for i, c := range r.PackageCycles {
		for _, p := range c {
			comp[p] = i + 1
		}
	}
	for _, k := range pedges {
		d := pd[k]
		d.InCycle = comp[d.From] != 0 && comp[d.From] == comp[d.To]
		sort.Slice(d.Classes, func(i, j int) bool {
			if d.Classes[i].Count != d.Classes[j].Count {
				return d.Classes[i].Count > d.Classes[j].Count
			}
			return d.Classes[i].From+d.Classes[i].To < d.Classes[j].From+d.Classes[j].To
		})
		r.Dependencies = append(r.Dependencies, *d)
	}
	return r
}

func (q *Q) Deps(tests, detail bool) (*DepsResult, error) {
	si, err := q.scanInfo()
	if err != nil {
		return nil, err
	}
	g, err := q.loadGraph(si.ID)
	if err != nil {
		return nil, err
	}
	r := g.deps(tests, detail)
	r.Scan = si
	return r, nil
}

// ---- checks ----

type Finding struct {
	Rule      string  `json:"rule"`
	Severity  string  `json:"severity" jsonschema:"warning: a rule was broken; note: a candidate worth a look, found with static relations only"`
	Symbol    string  `json:"symbol,omitempty"`
	File      string  `json:"file,omitempty"`
	Line      int     `json:"line,omitempty"`
	Message   string  `json:"message"`
	Value     float64 `json:"value,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}

type RuleSummary struct {
	ID        string  `json:"id"`
	Severity  string  `json:"severity"`
	Title     string  `json:"title"`
	Count     int     `json:"count"`
	Threshold float64 `json:"threshold,omitempty"`
}

type CheckResult struct {
	Scan     ScanInfo      `json:"scan"`
	Rules    []RuleSummary `json:"rules" jsonschema:"every enabled rule with how many findings it produced"`
	Findings []Finding     `json:"findings" jsonschema:"production code only; grouped by rule, worst first"`
	Warnings int           `json:"warnings"`
	Notes    int           `json:"notes"`
}

// Thresholds are the defaults; .ri/config.json can change them.
type Thresholds struct {
	CC        int `json:"cc"`
	MethodLOC int `json:"method_loc"`
	Nesting   int `json:"nesting"`
	Params    int `json:"params"`
	WMC       int `json:"wmc"`
	ClassLOC  int `json:"class_loc"`
	CBO       int `json:"cbo"`
	LCOM      int `json:"lcom"`
}

var defaultThresholds = Thresholds{CC: 10, MethodLOC: 60, Nesting: 4, Params: 5, WMC: 50, ClassLOC: 500, CBO: 14, LCOM: 1}

var ruleTitles = map[string]string{
	"complex-method":      "Method with high cyclomatic complexity",
	"long-method":         "Long method",
	"deep-nesting":        "Deeply nested control flow",
	"many-parameters":     "Method with many parameters",
	"large-class":         "Large class",
	"high-coupling":       "Class coupled to many others",
	"low-cohesion":        "Class doing unrelated things",
	"package-cycle":       "Package dependency cycle",
	"class-cycle":         "Class dependency cycle",
	"layer-violation":     "Lower layer depending on a higher one",
	"layer-skip":          "Controller using a repository directly",
	"unreferenced-method": "Method nothing references (candidate)",
	"test-only-code":      "Production code only tests use (candidate)",
	"untested-class":      "Class no test touches (candidate)",
}

var ruleOrder = []string{"complex-method", "long-method", "deep-nesting", "many-parameters", "large-class", "high-coupling", "low-cohesion",
	"package-cycle", "class-cycle", "layer-violation", "layer-skip", "unreferenced-method", "test-only-code", "untested-class"}

var noteRules = map[string]bool{"unreferenced-method": true, "test-only-code": true, "untested-class": true, "layer-skip": true}

var (
	accessorName = regexp.MustCompile(`^(get|set|is)[A-Z]`)
	layers       = map[string]int{"controller": 0, "service": 1, "repository": 2, "entity": 3}
)

func memberName(id string) string {
	s := id
	if i := strings.Index(s, "("); i >= 0 {
		s = s[:i]
	}
	return s[strings.LastIndexAny(s, "#.")+1:]
}

func (q *Q) Checks() (*CheckResult, error) {
	si, err := q.scanInfo()
	if err != nil {
		return nil, err
	}
	cfg := loadConfig(q.root)
	th := cfg.Thresholds
	m, err := q.Metrics()
	if err != nil {
		return nil, err
	}
	g, err := q.loadGraph(si.ID)
	if err != nil {
		return nil, err
	}
	r := &CheckResult{Scan: si, Rules: []RuleSummary{}, Findings: []Finding{}}
	by := map[string][]Finding{}
	add := func(rule, symbol, file string, line int, value, threshold float64, msg string) {
		sev := "warning"
		if noteRules[rule] {
			sev = "note"
		}
		by[rule] = append(by[rule], Finding{Rule: rule, Severity: sev, Symbol: symbol, File: file, Line: line, Message: msg, Value: value, Threshold: threshold})
	}

	for _, x := range m.Methods {
		if x.Test {
			continue
		}
		name := short(x.ID)
		if x.CC > th.CC {
			add("complex-method", x.ID, x.File, x.Line, float64(x.CC), float64(th.CC), fmt.Sprintf("%s has cyclomatic complexity %d (limit %d)", name, x.CC, th.CC))
		}
		if x.LOC > th.MethodLOC {
			add("long-method", x.ID, x.File, x.Line, float64(x.LOC), float64(th.MethodLOC), fmt.Sprintf("%s is %d lines long (limit %d)", name, x.LOC, th.MethodLOC))
		}
		if x.Nesting > th.Nesting {
			add("deep-nesting", x.ID, x.File, x.Line, float64(x.Nesting), float64(th.Nesting), fmt.Sprintf("%s nests control flow %d deep (limit %d)", name, x.Nesting, th.Nesting))
		}
		if x.Params > th.Params {
			add("many-parameters", x.ID, x.File, x.Line, float64(x.Params), float64(th.Params), fmt.Sprintf("%s takes %d parameters (limit %d)", name, x.Params, th.Params))
		}
	}
	for _, c := range m.Classes {
		if c.Test {
			continue
		}
		name := short(c.ID)
		if c.WMC > th.WMC || c.LOC > th.ClassLOC {
			add("large-class", c.ID, c.File, c.Line, float64(c.WMC), float64(th.WMC), fmt.Sprintf("%s has complexity sum %d and %d lines (limits %d and %d)", name, c.WMC, c.LOC, th.WMC, th.ClassLOC))
		}
		if c.CBO > th.CBO {
			add("high-coupling", c.ID, c.File, c.Line, float64(c.CBO), float64(th.CBO), fmt.Sprintf("%s is coupled to %d classes (limit %d)", name, c.CBO, th.CBO))
		}
		if c.LCOM > th.LCOM {
			add("low-cohesion", c.ID, c.File, c.Line, float64(c.LCOM), float64(th.LCOM), fmt.Sprintf("%s falls into %d groups of methods that share nothing", name, c.LCOM))
		}
	}

	d := g.deps(false, false)
	shorts := func(ids []string, f func(string) string) string {
		var out []string
		for i, id := range ids {
			if i == 6 {
				out = append(out, fmt.Sprintf("+%d more", len(ids)-6))
				break
			}
			out = append(out, f(id))
		}
		return strings.Join(out, " ⇄ ")
	}
	for _, c := range d.PackageCycles {
		add("package-cycle", "", "", 0, float64(len(c)), 0, "packages depend on each other in a loop: "+shorts(c, func(s string) string { return s }))
	}
	for _, c := range d.ClassCycles {
		n := g.nodes[c[0]]
		add("class-cycle", c[0], n.file, n.l1, float64(len(c)), 0, "classes reference each other in a loop: "+shorts(c, short))
	}

	cg := g.classDeps(false)
	hasService := false
	for _, c := range cg.classes {
		hasService = hasService || c.role == "service"
	}
	var pairs [][2]*gnode
	for k := range cg.dep {
		pairs = append(pairs, k)
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0].id+pairs[i][1].id < pairs[j][0].id+pairs[j][1].id })
	for _, k := range pairs {
		a, b := k[0], k[1]
		la, oka := layers[a.role]
		lb, okb := layers[b.role]
		if !oka || !okb {
			continue
		}
		if la > lb {
			add("layer-violation", a.id, a.file, a.l1, 0, 0, fmt.Sprintf("%s (%s) depends on %s (%s)", short(a.id), a.role, short(b.id), b.role))
		} else if a.role == "controller" && b.role == "repository" && hasService {
			add("layer-skip", a.id, a.file, a.l1, 0, 0, fmt.Sprintf("%s uses %s directly although the repository has a service layer", short(a.id), short(b.id)))
		}
	}

	touched := map[*gnode]bool{} // classes some test references, directly or through a member
	for _, e := range g.edges {
		if e.src.test && e.dst.top != nil {
			touched[e.dst.top] = true
		}
	}
	for _, n := range g.order {
		if n.ext || n.test || n.top == nil {
			continue
		}
		switch {
		case n.typ == "method":
			name := memberName(n.id)
			if n.synthetic != "" || n.annotated || n.abstract || g.blind[n.top] || strings.HasPrefix(n.top.kind, "annotation") || accessorName.MatchString(name) ||
				name == "main" || name == "init" || name == "default" || strings.HasPrefix(name, "__") {
				continue
			}
			// Go: an exported name is the package's API, meant for callers this repository may not contain. Like the Go
			// tools, only unexported declarations are reported as unused.
			if strings.HasPrefix(n.id, "go:") && n.public {
				continue
			}
			prod, test, overrides := 0, 0, false
			for _, e := range n.in {
				if e.src == n {
					continue
				}
				if e.src.test {
					test++
				} else {
					prod++
				}
			}
			for _, e := range n.out {
				overrides = overrides || e.rel == "overrides"
			}
			if prod > 0 || overrides {
				continue
			}
			if test > 0 {
				add("test-only-code", n.id, n.file, n.l1, 0, 0, short(n.id)+" is only referenced from tests")
			} else {
				add("unreferenced-method", n.id, n.file, n.l1, 0, 0, short(n.id)+" is not referenced anywhere analysis can see")
			}
		case n.typ == "class" && n.top == n && !strings.HasPrefix(n.kind, "annotation"):
			if !touched[n] {
				add("untested-class", n.id, n.file, n.l1, 0, 0, short(n.id)+" is not referenced by any test")
			}
		}
	}

	off := map[string]bool{}
	for _, id := range cfg.Disable {
		off[id] = true
	}
	limit := map[string]float64{"complex-method": float64(th.CC), "long-method": float64(th.MethodLOC), "deep-nesting": float64(th.Nesting), "many-parameters": float64(th.Params),
		"large-class": float64(th.WMC), "high-coupling": float64(th.CBO), "low-cohesion": float64(th.LCOM)}
	for _, id := range ruleOrder {
		if off[id] {
			continue
		}
		fs := by[id]
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].Value > fs[j].Value })
		sev := "warning"
		if noteRules[id] {
			sev = "note"
			r.Notes += len(fs)
		} else {
			r.Warnings += len(fs)
		}
		r.Rules = append(r.Rules, RuleSummary{ID: id, Severity: sev, Title: ruleTitles[id], Count: len(fs), Threshold: limit[id]})
		r.Findings = append(r.Findings, fs...)
	}
	return r, nil
}
