package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Code metrics, computed from the same facts as everything else. Definitions are the textbook ones
// (McCabe, Chidamber-Kemerer, Martin); each field's doc says exactly what is counted here.

type MethodMetric struct {
	ID         string `json:"id"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Test       bool   `json:"test,omitempty"`
	LOC        int    `json:"loc" jsonschema:"lines from the first to the last line of the declaration"`
	CC         int    `json:"cc" jsonschema:"cyclomatic complexity: 1 + if, loop, case, catch, ?:, && and ||"`
	Nesting    int    `json:"nesting" jsonschema:"deepest nesting of control structures"`
	Statements int    `json:"statements"`
	Params     int    `json:"params"`
	CalledBy   int    `json:"called_by" jsonschema:"call sites that reach this method"`
	Calls      int    `json:"calls" jsonschema:"call sites inside this method"`
}

type ClassMetric struct {
	ID      string  `json:"id"`
	Kind    string  `json:"kind"`
	Package string  `json:"package"`
	File    string  `json:"file"`
	Line    int     `json:"line"`
	Test    bool    `json:"test,omitempty"`
	LOC     int     `json:"loc"`
	Methods int     `json:"methods" jsonschema:"declared methods and constructors, generated ones excluded"`
	Fields  int     `json:"fields"`
	WMC     int     `json:"wmc" jsonschema:"weighted methods per class: sum of the methods' cyclomatic complexity"`
	MaxCC   int     `json:"max_cc"`
	AvgCC   float64 `json:"avg_cc"`
	CBO     int     `json:"cbo" jsonschema:"coupling between objects: other repository classes this class references or is referenced by"`
	RFC     int     `json:"rfc" jsonschema:"response for class: its methods plus the distinct methods they call"`
	DIT     int     `json:"dit" jsonschema:"depth of inheritance tree, counting superclasses outside the repository as one level"`
	NOC     int     `json:"noc" jsonschema:"direct subclasses and implementers in the repository"`
	LCOM    int     `json:"lcom" jsonschema:"LCOM4: groups of methods (getters and setters excluded) that share no field and do not call each other; 1 is cohesive, 0 means not applicable"`
	FanIn   int     `json:"fan_in" jsonschema:"repository classes that reference this class"`
	FanOut  int     `json:"fan_out" jsonschema:"repository classes this class references"`
}

type PackageMetric struct {
	Name         string  `json:"name"`
	Classes      int     `json:"classes"`
	LOC          int     `json:"loc"`
	Methods      int     `json:"methods"`
	AvgCC        float64 `json:"avg_cc"`
	MaxCC        int     `json:"max_cc"`
	Ca           int     `json:"ca" jsonschema:"afferent coupling: classes outside the package that depend on it"`
	Ce           int     `json:"ce" jsonschema:"efferent coupling: classes outside the package it depends on"`
	Instability  float64 `json:"instability" jsonschema:"Ce / (Ca + Ce)"`
	Abstractness float64 `json:"abstractness" jsonschema:"interfaces and abstract classes / classes"`
	Distance     float64 `json:"distance" jsonschema:"distance from the main sequence: |abstractness + instability - 1|"`
}

type FileMetric struct {
	Path    string `json:"path"`
	Test    bool   `json:"test,omitempty"`
	Lines   int    `json:"lines"`
	Code    int    `json:"code"`
	Comment int    `json:"comment"`
	Blank   int    `json:"blank"`
}

type Metrics struct {
	Scan     ScanInfo        `json:"scan"`
	Classes  []ClassMetric   `json:"classes"`
	Methods  []MethodMetric  `json:"methods"`
	Packages []PackageMetric `json:"packages" jsonschema:"production code only"`
	Files    []FileMetric    `json:"files"`
}

// test directories, Go test files, JS/TS spec files, pytest files
var testPath = regexp.MustCompile(`(^|/)(src/)?tests?/|(^|/)__tests__/|(^|/)e2e/|_test\.go$|\.(test|spec)\.[cm]?[jt]sx?$|(^|/)test_[^/]*\.py$|_test\.py$|(^|/)conftest\.py$`)
var accessor = regexp.MustCompile(`#(get|set|is)[A-Z]\w*\(`)

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func (q *Q) Metrics() (*Metrics, error) {
	si, err := q.scanInfo()
	if err != nil {
		return nil, err
	}
	m := &Metrics{Scan: si, Classes: []ClassMetric{}, Methods: []MethodMetric{}, Packages: []PackageMetric{}, Files: []FileMetric{}}

	type node struct {
		id, typ, parent, file string
		l1, l2                int
		ext                   bool
		a                     struct {
			Kind      string
			Modifiers []string
			Synthetic string
			Params    []json.RawMessage
			M         struct{ CC, Nest, Stmts int }
		}
		top *node // outermost class
	}
	nodes := map[string]*node{}
	var order []*node
	rows, err := q.db.Query(`SELECT id, type, COALESCE(parent,''), COALESCE(file,''), COALESCE(l1,0), COALESCE(l2,0), external, COALESCE(attrs,'{}') FROM symbols WHERE scan_id=? ORDER BY id`, si.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		n := &node{}
		var attrs string
		if err := rows.Scan(&n.id, &n.typ, &n.parent, &n.file, &n.l1, &n.l2, &n.ext, &attrs); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(attrs), &n.a)
		nodes[n.id] = n
		order = append(order, n)
	}
	rows.Close()
	for _, n := range order {
		if n.ext {
			continue
		}
		for x := n; x != nil; x = nodes[x.parent] {
			if x.typ == "class" {
				n.top = x
			}
		}
	}

	type edge struct {
		src, dst *node
		rel      string
	}
	var edges []edge
	rows, err = q.db.Query(`SELECT src, dst, relation FROM dependencies WHERE scan_id=?`, si.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s, d, rel string
		rows.Scan(&s, &d, &rel)
		if a, b := nodes[s], nodes[d]; a != nil && b != nil {
			edges = append(edges, edge{a, b, rel})
		}
	}
	rows.Close()

	type acc struct {
		methods, fields, wmc, maxCC int
		coupled, in, out            map[*node]bool
		called                      map[*node]bool // distinct methods called, for RFC
		sub                         int
		supers                      []*node
		link                        map[*node]*node // union-find over methods, for LCOM4
		own                         []*node
	}
	accs := map[*node]*acc{}
	get := func(c *node) *acc {
		a := accs[c]
		if a == nil {
			a = &acc{coupled: map[*node]bool{}, in: map[*node]bool{}, out: map[*node]bool{}, called: map[*node]bool{}, link: map[*node]*node{}}
			accs[c] = a
		}
		return a
	}
	calledBy, calls := map[*node]int{}, map[*node]int{}
	fieldUsers := map[*node][]*node{} // field -> methods of the same class that touch it
	var find func(a *acc, x *node) *node
	find = func(a *acc, x *node) *node {
		if p, ok := a.link[x]; ok && p != x {
			r := find(a, p)
			a.link[x] = r
			return r
		}
		a.link[x] = x
		return x
	}
	isMethod := func(n *node) bool { return (n.typ == "method" || n.typ == "constructor") && n.a.Synthetic == "" }
	for _, n := range order {
		if n.ext || n.top == nil {
			continue
		}
		a := get(n.top)
		switch {
		case isMethod(n):
			cc := max(n.a.M.CC, 1)
			a.methods++
			a.wmc += cc
			a.maxCC = max(a.maxCC, cc)
			// plain accessors would make every data class look incohesive: each touches only its own field
			if n.typ == "method" && !accessor.MatchString(n.id) {
				a.own = append(a.own, n)
			}
		case n.typ == "field" && n.a.Synthetic == "":
			a.fields++
		}
	}
	for _, e := range edges {
		if e.rel == "calls" {
			calledBy[e.dst]++
			calls[e.src]++
		}
		st, dt := e.src.top, e.dst.top
		if st != nil && e.rel == "calls" {
			get(st).called[e.dst] = true
		}
		if st != nil && st == e.src && dt != nil && dt == e.dst && (e.rel == "extends" || e.rel == "implements") {
			get(dt).sub++
		}
		if st != nil && st == e.src && (e.rel == "extends" || e.rel == "implements") {
			get(st).supers = append(get(st).supers, e.dst)
		}
		if st == nil || dt == nil || e.rel == "overrides" {
			continue
		}
		if st == dt { // cohesion: methods joined by a shared field or a call between them
			a := get(st)
			if e.rel == "calls" && e.src.typ == "method" && e.dst.typ == "method" {
				a.link[find(a, e.src)] = find(a, e.dst)
			} else if e.rel == "uses" && e.src.typ == "method" && e.dst.typ == "field" {
				fieldUsers[e.dst] = append(fieldUsers[e.dst], e.src)
			}
			continue
		}
		get(st).out[dt], get(dt).in[st] = true, true
		get(st).coupled[dt], get(dt).coupled[st] = true, true
	}
	for f, users := range fieldUsers {
		a := get(f.top)
		for _, u := range users[1:] {
			a.link[find(a, u)] = find(a, users[0])
		}
	}
	var dit func(c *node, seen map[*node]bool) int
	dit = func(c *node, seen map[*node]bool) int {
		if seen[c] {
			return 0
		}
		seen[c] = true
		d := 0
		for _, s := range get(c).supers {
			if s.ext {
				d = max(d, 1)
			} else {
				d = max(d, 1+dit(s, seen))
			}
		}
		return d
	}
	pkgOf := func(id string) string {
		q := id[strings.Index(id, ":")+1:]
		i := strings.LastIndex(q, ".")
		if i < 0 {
			return ""
		}
		q = q[:i]
		if strings.HasPrefix(id, "ts:") || strings.HasPrefix(id, "py:") { // the ID names the file; the package is its directory
			if j := strings.LastIndex(q, "/"); j >= 0 {
				return q[:j]
			}
			return "."
		}
		return q
	}

	type pacc struct {
		PackageMetric
		cc, abstract int
		ca, ce       map[*node]bool
	}
	pkgs := map[string]*pacc{}
	for _, n := range order {
		if n.ext || n.top == nil {
			continue
		}
		test := testPath.MatchString(n.file)
		if isMethod(n) {
			m.Methods = append(m.Methods, MethodMetric{ID: n.id, File: n.file, Line: n.l1, Test: test, LOC: n.l2 - n.l1 + 1, CC: max(n.a.M.CC, 1),
				Nesting: n.a.M.Nest, Statements: n.a.M.Stmts, Params: len(n.a.Params), CalledBy: calledBy[n], Calls: calls[n]})
		}
		if n.typ != "class" || n.top != n {
			continue
		}
		a := get(n)
		c := ClassMetric{ID: n.id, Kind: n.a.Kind, Package: pkgOf(n.id), File: n.file, Line: n.l1, Test: test, LOC: n.l2 - n.l1 + 1, Methods: a.methods, Fields: a.fields,
			WMC: a.wmc, MaxCC: a.maxCC, CBO: len(a.coupled), RFC: a.methods + len(a.called), DIT: dit(n, map[*node]bool{}), NOC: a.sub, FanIn: len(a.in), FanOut: len(a.out)}
		if a.methods > 0 {
			c.AvgCC = round2(float64(a.wmc) / float64(a.methods))
		}
		if a.fields > 0 && len(a.own) > 1 { // cohesion is only meaningful for a class with state and several methods
			groups := map[*node]bool{}
			for _, mth := range a.own {
				groups[find(a, mth)] = true
			}
			c.LCOM = len(groups)
		}
		m.Classes = append(m.Classes, c)
		if test {
			continue
		}
		p := pkgs[c.Package]
		if p == nil {
			p = &pacc{PackageMetric: PackageMetric{Name: c.Package}, ca: map[*node]bool{}, ce: map[*node]bool{}}
			pkgs[c.Package] = p
		}
		p.Classes++
		p.LOC += c.LOC
		p.Methods += c.Methods
		p.cc += c.WMC
		p.MaxCC = max(p.MaxCC, c.MaxCC)
		if c.Kind == "interface" || contains(n.a.Modifiers, "abstract") {
			p.abstract++
		}
		for o := range a.in {
			if !testPath.MatchString(o.file) && pkgOf(o.id) != c.Package {
				p.ca[o] = true
			}
		}
		for o := range a.out {
			if !testPath.MatchString(o.file) && pkgOf(o.id) != c.Package {
				p.ce[o] = true
			}
		}
	}
	for _, name := range sortedKeys(pkgs) {
		p := pkgs[name]
		p.Ca, p.Ce = len(p.ca), len(p.ce)
		if p.Methods > 0 {
			p.AvgCC = round2(float64(p.cc) / float64(p.Methods))
		}
		if p.Ca+p.Ce > 0 {
			p.Instability = round2(float64(p.Ce) / float64(p.Ca+p.Ce))
		}
		p.Abstractness = round2(float64(p.abstract) / float64(p.Classes))
		p.Distance = round2(math.Abs(p.Abstractness + p.Instability - 1))
		m.Packages = append(m.Packages, p.PackageMetric)
	}
	sort.SliceStable(m.Classes, func(i, j int) bool { return m.Classes[i].WMC > m.Classes[j].WMC })
	sort.SliceStable(m.Methods, func(i, j int) bool { return m.Methods[i].CC > m.Methods[j].CC })

	rows, err = q.db.Query(`SELECT path, COALESCE(metrics,'') FROM files WHERE scan_id=? AND metrics IS NOT NULL ORDER BY path`, si.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f FileMetric
		var js string
		rows.Scan(&f.Path, &js)
		json.Unmarshal([]byte(js), &f)
		f.Test = testPath.MatchString(f.Path)
		m.Files = append(m.Files, f)
	}
	sort.SliceStable(m.Files, func(i, j int) bool { return m.Files[i].Code > m.Files[j].Code })
	return m, rows.Err()
}

// limit keeps the first n rows of each list, optionally dropping test code first.
func (m *Metrics) limit(n int, tests bool) *Metrics {
	out := &Metrics{Scan: m.Scan, Classes: []ClassMetric{}, Methods: []MethodMetric{}, Packages: m.Packages, Files: []FileMetric{}}
	for _, c := range m.Classes {
		if (tests || !c.Test) && (n <= 0 || len(out.Classes) < n) {
			out.Classes = append(out.Classes, c)
		}
	}
	for _, c := range m.Methods {
		if (tests || !c.Test) && (n <= 0 || len(out.Methods) < n) {
			out.Methods = append(out.Methods, c)
		}
	}
	for _, c := range m.Files {
		if (tests || !c.Test) && (n <= 0 || len(out.Files) < n) {
			out.Files = append(out.Files, c)
		}
	}
	if n > 0 && len(out.Packages) > n {
		out.Packages = out.Packages[:n]
	}
	return out
}

// table renders one scope as header + rows, the single source for the text and CSV outputs.
func (m *Metrics) table(scope string, name func(id string) string) ([]string, [][]string) {
	s := func(v any) string { return fmt.Sprint(v) }
	var rows [][]string
	switch scope {
	case "method":
		for _, x := range m.Methods {
			rows = append(rows, []string{name(x.ID), s(x.CC), s(x.Nesting), s(x.LOC), s(x.Statements), s(x.Params), s(x.CalledBy), s(x.Calls), fmt.Sprintf("%s:%d", x.File, x.Line)})
		}
		return []string{"method", "cc", "nesting", "loc", "statements", "params", "called_by", "calls", "location"}, rows
	case "package":
		for _, x := range m.Packages {
			rows = append(rows, []string{x.Name, s(x.Classes), s(x.LOC), s(x.Methods), s(x.AvgCC), s(x.MaxCC), s(x.Ca), s(x.Ce), s(x.Instability), s(x.Abstractness), s(x.Distance)})
		}
		return []string{"package", "classes", "loc", "methods", "avg_cc", "max_cc", "ca", "ce", "instability", "abstractness", "distance"}, rows
	case "file":
		for _, x := range m.Files {
			rows = append(rows, []string{x.Path, s(x.Lines), s(x.Code), s(x.Comment), s(x.Blank)})
		}
		return []string{"file", "lines", "code", "comment", "blank"}, rows
	}
	for _, x := range m.Classes {
		rows = append(rows, []string{name(x.ID), s(x.LOC), s(x.Methods), s(x.Fields), s(x.WMC), s(x.MaxCC), s(x.AvgCC), s(x.CBO), s(x.RFC), s(x.DIT), s(x.NOC), s(x.LCOM), s(x.FanIn), s(x.FanOut), fmt.Sprintf("%s:%d", x.File, x.Line)})
	}
	return []string{"class", "loc", "methods", "fields", "wmc", "max_cc", "avg_cc", "cbo", "rfc", "dit", "noc", "lcom", "fan_in", "fan_out", "location"}, rows
}
