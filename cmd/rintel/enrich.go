package main

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"unicode"
)

// Rows written by enrich are recomputed on every scan, so its source does not need to be part of the scan version.

type annotation struct {
	Name string
	Args map[string][]string
}

type symAttrs struct {
	Kind        string
	Modifiers   []string
	Annotations []annotation
	Type        string
	Initialized bool
	Synthetic   string
	Returns     string
	Params      []struct {
		Name, Type  string
		Annotations []annotation
	}
}

type sym struct {
	id, typ, parent, file string
	line                  int
	a                     symAttrs
}

func simpleName(q string) string { return q[strings.LastIndex(q, ".")+1:] }

// findAnn matches by simple name: without a classpath the adapter cannot always give the FQN.
func findAnn(as []annotation, names ...string) *annotation {
	for i := range as {
		for _, n := range names {
			if simpleName(as[i].Name) == n {
				return &as[i]
			}
		}
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func annValues(a *annotation, keys ...string) []string {
	for _, k := range keys {
		if v := a.Args[k]; len(v) > 0 {
			return v
		}
	}
	return nil
}

// clean bounds repository-controlled strings before they reach agents (prompt-injection surface).
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

const maxHierarchyTargets = 5

type injection struct {
	candidates []string
	evidence   string
}

// enrich derives framework relations (Spring beans, DI targets, endpoints) from the adapter facts of one scan.
// It is recomputed from scratch on every scan. shortcut: meta-annotations (custom stereotypes) are not followed.
func enrich(tx *sql.Tx, scan int64) error {
	syms := map[string]*sym{}
	children := map[string][]*sym{}
	rows, err := tx.Query(`SELECT id, type, COALESCE(parent,''), COALESCE(file,''), l1, attrs FROM symbols WHERE scan_id=? AND external=0 ORDER BY id`, scan)
	if err != nil {
		return err
	}
	for rows.Next() {
		s := &sym{}
		var attrs string
		rows.Scan(&s.id, &s.typ, &s.parent, &s.file, &s.line, &attrs)
		json.Unmarshal([]byte(attrs), &s.a)
		syms[s.id] = s
		children[s.parent] = append(children[s.parent], s)
	}
	rows.Close()

	type dep struct {
		src, dst, via, rel, file, ev string
		line                         int
	}
	var calls []dep
	type route struct {
		method, path, handler, file, ev string
		line                            int
	}
	var routes []route
	callsFrom := map[string][]dep{}
	supers := map[string][]string{}
	overriders := map[string][]string{} // method -> methods overriding it
	rows, err = tx.Query(`SELECT src, dst, via, relation, file, line, evidence_level FROM dependencies WHERE scan_id=? AND origin='adapter' ORDER BY src, line, dst`, scan)
	if err != nil {
		return err
	}
	for rows.Next() {
		var d dep
		rows.Scan(&d.src, &d.dst, &d.via, &d.rel, &d.file, &d.line, &d.ev)
		switch d.rel {
		case "routes": // an adapter saw a router registration: via carries "METHOD /path"
			if method, path, ok := strings.Cut(d.via, " "); ok {
				routes = append(routes, route{method, path, d.dst, d.file, d.ev, d.line})
			}
		case "calls":
			calls = append(calls, d)
			callsFrom[d.src] = append(callsFrom[d.src], d)
		case "extends", "implements":
			supers[d.src] = append(supers[d.src], d.dst)
		case "overrides":
			overriders[d.dst] = append(overriders[d.dst], d.src)
		}
	}
	rows.Close()

	var assignable func(c, t string, seen map[string]bool) bool
	assignable = func(c, t string, seen map[string]bool) bool {
		if c == t {
			return true
		}
		if seen[c] {
			return false
		}
		seen[c] = true
		for _, s := range supers[c] {
			if assignable(s, t, seen) {
				return true
			}
		}
		return false
	}

	// ---- beans ----
	beanName := map[string]string{}
	var beans []string
	for id, s := range syms {
		if s.typ != "class" {
			continue
		}
		st := findAnn(s.a.Annotations, "Component", "Service", "Repository", "Controller", "RestController", "Configuration", "Mapper", "FeignClient")
		if st == nil && s.a.Kind == "interface" { // Spring Data repositories are beans without a stereotype
			for _, sup := range supers[id] {
				if strings.HasSuffix(sup, "Repository") && syms[sup] == nil {
					st = &annotation{}
				}
			}
		}
		if st == nil {
			continue
		}
		name := simpleName(strings.TrimPrefix(id, "java:"))
		name = strings.ToLower(name[:1]) + name[1:]
		if v := annValues(st, "value"); len(v) > 0 && v[0] != "" {
			name = v[0]
		}
		beanName[id] = name
		beans = append(beans, id)
	}
	// @Bean factory methods: the bean is the declared return type, or what the method constructs
	type extraPoint struct{ typ, name string }
	factoryParams := map[string][]extraPoint{}
	for _, id := range sortedKeys(syms) {
		m := syms[id]
		b := findAnn(m.a.Annotations, "Bean")
		if m.typ != "method" || b == nil {
			continue
		}
		ret := "java:" + m.a.Returns
		made := map[string]bool{}
		if c := syms[ret]; c != nil && c.a.Kind == "class" && !contains(c.a.Modifiers, "abstract") {
			made[ret] = true
		}
		for _, d := range callsFrom[id] {
			if c := strings.SplitN(d.dst, "#", 2); strings.HasPrefix(c[len(c)-1], "<init>") && syms[c[0]] != nil && assignable(c[0], ret, map[string]bool{}) {
				made[c[0]] = true
			}
		}
		for _, c := range sortedKeys(made) {
			if _, ok := beanName[c]; ok {
				continue
			}
			beanName[c] = fqnOf(id)[strings.LastIndex(fqnOf(id), ".")+1:]
			if v := annValues(b, "value", "name"); len(v) > 0 && v[0] != "" {
				beanName[c] = v[0]
			}
			beans = append(beans, c)
		}
		for _, p := range m.a.Params {
			factoryParams[m.parent] = append(factoryParams[m.parent], extraPoint{p.Type, p.Name})
		}
	}
	sort.Strings(beans)

	stmt, err := tx.Prepare(`INSERT INTO dependencies VALUES (?,?,?,?,?,?,?,?,?,'enrich')`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	// ---- injection points ----
	inject := map[string]map[string]injection{} // bean class -> injected type -> targets
	for _, c := range beans {
		cls := syms[c]
		type point struct{ typ, name, qualifier string }
		var points []point
		qual := func(as []annotation) string {
			if q := findAnn(as, "Qualifier", "Named"); q != nil && len(annValues(q, "value")) > 0 {
				return annValues(q, "value")[0]
			}
			return ""
		}
		required := findAnn(cls.a.Annotations, "RequiredArgsConstructor") != nil
		all := findAnn(cls.a.Annotations, "AllArgsConstructor") != nil
		var ctors []*sym
		for _, m := range children[c] {
			switch {
			case m.typ == "field" && !contains(m.a.Modifiers, "static"):
				if findAnn(m.a.Annotations, "Autowired", "Inject", "Resource") != nil || all ||
					(required && contains(m.a.Modifiers, "final") && !m.a.Initialized) {
					points = append(points, point{m.a.Type, simpleName(strings.Replace(m.id, "#", ".", 1)), qual(m.a.Annotations)})
				}
			case m.typ == "constructor" && m.a.Synthetic == "":
				ctors = append(ctors, m)
			}
		}
		for _, m := range ctors {
			if len(ctors) == 1 || findAnn(m.a.Annotations, "Autowired", "Inject") != nil {
				for _, p := range m.a.Params {
					points = append(points, point{p.Type, p.Name, qual(p.Annotations)})
				}
			}
		}
		for _, p := range factoryParams[c] {
			points = append(points, point{p.typ, p.name, ""})
		}
		for _, p := range points {
			t := "java:" + p.typ
			var cands []string
			for _, b := range beans {
				if assignable(b, t, map[string]bool{}) {
					cands = append(cands, b)
				}
			}
			if len(cands) == 0 {
				continue
			}
			narrow := func(keep func(b string) bool) {
				if len(cands) < 2 {
					return
				}
				var out []string
				for _, b := range cands {
					if keep(b) {
						out = append(out, b)
					}
				}
				if len(out) > 0 {
					cands = out
				}
			}
			if p.qualifier != "" {
				narrow(func(b string) bool { return beanName[b] == p.qualifier })
			}
			narrow(func(b string) bool { return findAnn(syms[b].a.Annotations, "Primary") != nil })
			narrow(func(b string) bool { return beanName[b] == p.name }) // Spring's fallback: parameter/field name as qualifier
			inj := injection{cands, "resolved"}
			rule := "spring-di"
			if len(cands) > 1 {
				inj.evidence, rule = "heuristic", "spring-di-multiple-candidates"
			}
			if inject[c] == nil {
				inject[c] = map[string]injection{}
			}
			if _, dup := inject[c][t]; dup {
				continue
			}
			inject[c][t] = inj
			for _, b := range cands {
				if _, err := stmt.Exec(scan, c, b, t, "injects", inj.evidence, rule, cls.file, cls.line); err != nil {
					return err
				}
			}
		}
	}

	// ---- calls through an injected type reach the implementation ----
	for _, d := range calls {
		i := strings.Index(d.dst, "#")
		if i < 0 {
			continue
		}
		t := d.via
		if t == "" {
			t = d.dst[:i]
		}
		inj, ok := inject[strings.SplitN(d.src, "#", 2)[0]][t]
		if !ok {
			// no DI knowledge: fall back to the class hierarchy. Every override is a possible target, hence heuristic.
			var targets []string
			for _, o := range overriders[d.dst] {
				if d.via == "" || assignable(strings.SplitN(o, "#", 2)[0], d.via, map[string]bool{}) {
					targets = append(targets, o)
				}
			}
			sort.Strings(targets)
			if len(targets) > maxHierarchyTargets { // a widely implemented interface: listing every implementation is noise, not an answer
				continue
			}
			for _, o := range targets {
				if _, err := stmt.Exec(scan, d.src, o, t, "calls", "heuristic", "class-hierarchy", d.file, d.line); err != nil {
					return err
				}
			}
			continue
		}
		rule := "spring-di"
		if inj.evidence == "heuristic" {
			rule = "spring-di-multiple-candidates"
		}
		for _, b := range inj.candidates {
			target := b + d.dst[i:]
			if b == t || syms[target] == nil {
				continue
			}
			if _, err := stmt.Exec(scan, d.src, target, t, "calls", inj.evidence, rule, d.file, d.line); err != nil {
				return err
			}
		}
	}

	for _, r := range routes {
		if _, err := tx.Exec(`INSERT INTO api_endpoints VALUES (?,?,?,?,?,?,?)`, scan, r.method, clean(r.path), r.handler, r.ev, r.file, r.line); err != nil {
			return err
		}
	}

	// ---- REST endpoints ----
	httpMethod := map[string]string{"GetMapping": "GET", "PostMapping": "POST", "PutMapping": "PUT", "DeleteMapping": "DELETE", "PatchMapping": "PATCH"}
	for _, c := range beans {
		cls := syms[c]
		if findAnn(cls.a.Annotations, "Controller", "RestController") == nil {
			continue
		}
		prefixes := []string{""}
		if a := findAnn(cls.a.Annotations, "RequestMapping"); a != nil {
			if v := annValues(a, "value", "path"); len(v) > 0 {
				prefixes = v
			}
		}
		for _, m := range children[c] {
			a := findAnn(m.a.Annotations, "GetMapping", "PostMapping", "PutMapping", "DeleteMapping", "PatchMapping", "RequestMapping")
			if m.typ != "method" || a == nil {
				continue
			}
			methods := []string{httpMethod[simpleName(a.Name)]}
			if methods[0] == "" {
				methods = annValues(a, "method")
				if len(methods) == 0 {
					methods = []string{"ANY"}
				}
			}
			paths := annValues(a, "value", "path")
			if len(paths) == 0 {
				paths = []string{""}
			}
			for _, pre := range prefixes {
				for _, p := range paths {
					path := "/" + strings.Trim(strings.Trim(pre, "/")+"/"+strings.Trim(p, "/"), "/")
					for _, hm := range methods {
						if _, err := tx.Exec(`INSERT INTO api_endpoints VALUES (?,?,?,?,'declared',NULL,NULL)`, scan, simpleName(hm), clean(path), m.id); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}
