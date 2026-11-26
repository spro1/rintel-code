package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed report.html
var reportHTML string

type reportData struct {
	Repo       string               `json:"repo"`
	Lang       string               `json:"lang"`
	Generated  string               `json:"generated"`
	Overview   *Overview            `json:"overview"`
	Endpoints  []Endpoint           `json:"endpoints"`
	Files      []string             `json:"files"`
	Rules      []string             `json:"rules"`
	Syms       [][]any              `json:"syms"`       // id, type, parent index, file index, line, external, synthetic, kind, role, annotated
	Edges      [][]any              `json:"edges"`      // src, dst, relation, evidence, rule index, file index, line
	Unresolved [][]any              `json:"unresolved"` // src, text, reason, file index, line
	Metrics    *Metrics             `json:"metrics"`
	Checks     *CheckResult         `json:"checks"`
	Thresholds Thresholds           `json:"thresholds"`
	Fixes      map[string][2]string `json:"fixes"`             // rule id -> how to fix it, English and Korean
	Sources    map[int]string       `json:"sources,omitempty"` // file index -> text, only with --source
}

// roleOf names the architectural layer a class declares through its stereotype annotation.
func roleOf(as []annotation) string {
	for _, r := range []struct {
		role  string
		names []string
	}{
		{"controller", []string{"Controller", "RestController", "ControllerAdvice", "RestControllerAdvice"}},
		{"service", []string{"Service"}},
		{"repository", []string{"Repository", "Mapper"}},
		{"entity", []string{"Entity", "MappedSuperclass", "Embeddable", "Document"}},
		{"config", []string{"Configuration", "SpringBootApplication", "ConfigurationProperties"}},
		{"component", []string{"Component"}},
	} {
		if findAnn(as, r.names...) != nil {
			return r.role
		}
	}
	return ""
}

// String literals are the likeliest place for secrets and internal URLs; the report shows call shapes, not their contents.
var stringLiteral = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// Report renders the latest scan as one static HTML file with no external references.
func (q *Q) Report(lang string, withSource bool) ([]byte, error) {
	o, err := q.Overview()
	if err != nil {
		return nil, err
	}
	scan := o.Scan.ID
	abs, _ := filepath.Abs(q.root)
	d := reportData{Repo: filepath.Base(abs), Lang: lang, Generated: time.Now().Format("2006-01-02 15:04"), Overview: o,
		Endpoints: []Endpoint{}, Files: []string{}, Rules: []string{}, Syms: [][]any{}, Edges: [][]any{}, Unresolved: [][]any{}}

	intern := func(table *[]string, index map[string]int, s string) int {
		if s == "" {
			return -1
		}
		i, ok := index[s]
		if !ok {
			i = len(*table)
			index[s] = i
			*table = append(*table, s)
		}
		return i
	}
	files, rules, symIdx := map[string]int{}, map[string]int{}, map[string]int{}

	type row struct {
		id, typ, parent, file, attrs string
		line                         int
		ext                          bool
	}
	var syms []row
	rows, err := q.db.Query(`SELECT id, type, COALESCE(parent,''), COALESCE(file,''), COALESCE(l1,0), external, COALESCE(attrs,'{}')
		FROM symbols WHERE scan_id=? ORDER BY external, id`, scan)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.typ, &r.parent, &r.file, &r.line, &r.ext, &r.attrs); err != nil {
			return nil, err
		}
		symIdx[r.id] = len(syms)
		syms = append(syms, r)
	}
	rows.Close()
	for _, r := range syms {
		parent := -1
		if i, ok := symIdx[r.parent]; ok {
			parent = i
		}
		ext := 0
		if r.ext {
			ext = 1
		}
		var a symAttrs
		json.Unmarshal([]byte(r.attrs), &a)
		annotated := 0
		for _, an := range a.Annotations { // annotations that mean "something else calls this": framework callbacks, mappings, beans
			if n := simpleName(an.Name); n != "Override" && n != "SuppressWarnings" && n != "Deprecated" && n != "SafeVarargs" && n != "Generated" {
				annotated = 1
			}
		}
		d.Syms = append(d.Syms, []any{r.id, r.typ, parent, intern(&d.Files, files, r.file), r.line, ext, a.Synthetic, a.Kind, roleOf(a.Annotations), annotated})
	}

	rows, err = q.db.Query(`SELECT src, dst, relation, evidence_level, COALESCE(rule,''), COALESCE(file,''), COALESCE(line,0)
		FROM dependencies WHERE scan_id=? ORDER BY src, line, dst, relation`, scan)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src, dst, rel, ev, rule, file string
		var line int
		if err := rows.Scan(&src, &dst, &rel, &ev, &rule, &file, &line); err != nil {
			return nil, err
		}
		s, ok1 := symIdx[src]
		t, ok2 := symIdx[dst]
		if ok1 && ok2 {
			d.Edges = append(d.Edges, []any{s, t, rel, ev, intern(&d.Rules, rules, rule), intern(&d.Files, files, file), line})
		}
	}
	rows.Close()

	rows, err = q.db.Query(`SELECT src, text, reason, COALESCE(file,''), COALESCE(line,0) FROM unresolved_refs WHERE scan_id=? ORDER BY reason, file, line`, scan)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src, text, reason, file string
		var line int
		if err := rows.Scan(&src, &text, &reason, &file, &line); err != nil {
			return nil, err
		}
		if s, ok := symIdx[src]; ok {
			text = clean(stringLiteral.ReplaceAllString(strings.Join(strings.Fields(text), " "), `"…"`))
			d.Unresolved = append(d.Unresolved, []any{s, text, reason, intern(&d.Files, files, file), line})
		}
	}
	rows.Close()

	for cursor := ""; ; {
		page, err := q.APISpec("", 200, cursor)
		if err != nil {
			return nil, err
		}
		d.Endpoints = append(d.Endpoints, page.Endpoints...)
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if d.Metrics, err = q.Metrics(); err != nil {
		return nil, err
	}
	if d.Checks, err = q.Checks(); err != nil {
		return nil, err
	}
	d.Thresholds = loadConfig(q.root).Thresholds
	d.Fixes = ruleFix
	if withSource { // opt-in: the report then carries the code itself, not only names and locations
		d.Sources = map[int]string{}
		for i, f := range d.Files {
			if b, err := os.ReadFile(filepath.Join(q.root, f)); err == nil && len(b) <= maxFileSize {
				d.Sources[i] = string(b)
			}
		}
	}
	// encoding/json escapes <, > and & by default, so no repository string can close the <script> element
	data, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return []byte(strings.Replace(reportHTML, "__DATA__", string(data), 1)), nil
}
