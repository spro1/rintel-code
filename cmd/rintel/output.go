package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Text, graph and SARIF renderings of the analyses, for terminals, documents and CI.

// commonPrefix is what all package names share up to a separator; labels drop it.
func commonPrefix(names []string) string {
	if len(names) < 2 {
		return ""
	}
	p := names[0]
	for _, n := range names {
		i := 0
		for i < len(p) && i < len(n) && p[i] == n[i] {
			i++
		}
		p = p[:i]
	}
	for _, n := range names {
		if n == p {
			return p + "\x00" // one package is the parent of the rest: keep its own name
		}
	}
	return p[:strings.LastIndexAny(p, "./")+1]
}

func pkgLabel(prefix, name string) string {
	if name == "" {
		return "(default)"
	}
	if name == "." {
		return "(root)"
	}
	if strings.HasSuffix(prefix, "\x00") {
		base := strings.TrimSuffix(prefix, "\x00")
		if name == base {
			return name[strings.LastIndexAny(name, "./")+1:]
		}
		return strings.TrimLeft(strings.TrimPrefix(name, base), "./")
	}
	return strings.TrimPrefix(name, prefix)
}

func writeDeps(w io.Writer, d *DepsResult, format string) {
	prefix := commonPrefix(d.Packages)
	label := func(p string) string { return pkgLabel(prefix, p) }
	switch format {
	case "dot":
		fmt.Fprintln(w, "digraph dependencies {\n  rankdir=LR;\n  node [shape=box, fontname=\"Helvetica\"];")
		for _, p := range d.Packages {
			fmt.Fprintf(w, "  %q;\n", label(p))
		}
		for _, e := range d.Dependencies {
			attr := ""
			if e.InCycle {
				attr = ", color=red, style=dashed"
			}
			fmt.Fprintf(w, "  %q -> %q [label=%d%s];\n", label(e.From), label(e.To), e.Count, attr)
		}
		fmt.Fprintln(w, "}")
	case "mermaid":
		id := map[string]string{}
		fmt.Fprintln(w, "graph LR")
		for i, p := range d.Packages {
			id[p] = fmt.Sprintf("p%d", i)
			fmt.Fprintf(w, "  %s[%q]\n", id[p], label(p))
		}
		for _, e := range d.Dependencies {
			arrow := "-->"
			if e.InCycle {
				arrow = "-.->"
			}
			fmt.Fprintf(w, "  %s %s|%d| %s\n", id[e.From], arrow, e.Count, id[e.To])
		}
	default:
		fmt.Fprintf(w, tr("%d packages · %d dependencies · %d package cycles · %d class cycles\n", "패키지 %d개 · 의존 %d개 · 패키지 순환 %d개 · 클래스 순환 %d개\n"),
			len(d.Packages), len(d.Dependencies), len(d.PackageCycles), len(d.ClassCycles))
		deps := append([]PackageDep{}, d.Dependencies...)
		sort.SliceStable(deps, func(i, j int) bool { return deps[i].Count > deps[j].Count })
		width := 0
		for _, e := range deps {
			width = max(width, len(label(e.From))+len(label(e.To)))
		}
		for _, e := range deps {
			mark := ""
			if e.InCycle {
				mark = tr("  (in a cycle)", "  (순환)")
			}
			fmt.Fprintf(w, "  %s → %s%s  %d%s\n", label(e.From), label(e.To), strings.Repeat(" ", width-len(label(e.From))-len(label(e.To))), e.Count, mark)
		}
		for i, c := range d.PackageCycles {
			if i == 0 {
				fmt.Fprintln(w, tr("\nPackage cycles", "\n패키지 순환"))
			}
			var names []string
			for _, p := range c {
				names = append(names, label(p))
			}
			fmt.Fprintf(w, "  %d. %s\n", i+1, strings.Join(names, " ⇄ "))
		}
		for i, c := range d.ClassCycles {
			if i == 0 {
				fmt.Fprintln(w, tr("\nClass cycles", "\n클래스 순환"))
			}
			if i == 20 {
				fmt.Fprintf(w, tr("  … %d more (--format json for all)\n", "  … %d개 더 (전체는 --format json)\n"), len(d.ClassCycles)-20)
				break
			}
			var names []string
			for j, id := range c {
				if j == 8 {
					names = append(names, fmt.Sprintf("+%d", len(c)-8))
					break
				}
				names = append(names, short(id))
			}
			fmt.Fprintf(w, "  %d. %s\n", i+1, strings.Join(names, " ⇄ "))
		}
	}
}

func writeChecks(w io.Writer, c *CheckResult) {
	by := map[string][]Finding{}
	for _, f := range c.Findings {
		by[f.Rule] = append(by[f.Rule], f)
	}
	for _, r := range c.Rules {
		if r.Count == 0 {
			continue
		}
		fmt.Fprintf(w, "%s  %d  %s\n", r.ID, r.Count, strings.TrimSpace(r.Title+"  "+r.CWE))
		if fix := ruleFix[r.ID]; fix[0] != "" {
			fmt.Fprintf(w, "  %s %s\n", tr("fix:", "수정:"), tr(fix[0], fix[1]))
		}
		for i, f := range by[r.ID] {
			if i == 15 {
				fmt.Fprintf(w, tr("  … %d more (--format json for all)\n", "  … %d건 더 (전체는 --format json)\n"), len(by[r.ID])-15)
				break
			}
			at := ""
			if f.File != "" {
				at = loc(&Location{File: f.File, StartLine: f.Line}) + "  "
			}
			fmt.Fprintf(w, "  %s%s\n", at, f.Message)
		}
	}
	if len(c.Disabled) > 0 { // a gate that was switched off should not look like a gate that passed
		fmt.Fprintf(w, tr("\nswitched off in .ri/config.json: %s\n", "\n.ri/config.json에서 끈 규칙: %s\n"), strings.Join(c.Disabled, ", "))
	}
	passed := 0
	for _, r := range c.Rules {
		if r.Count == 0 {
			passed++
		}
	}
	fmt.Fprintf(w, tr("\n%d rules: %d passed · %d warnings · %d notes (candidates)\n", "\n규칙 %d개: 통과 %d개 · 경고 %d건 · 참고(후보) %d건\n"), len(c.Rules), passed, c.Warnings, c.Notes)
}

// writeSARIF emits SARIF 2.1.0, the format code-scanning dashboards ingest.
func writeSARIF(w io.Writer, c *CheckResult) {
	type msg struct {
		Text string `json:"text"`
	}
	rules := []map[string]any{}
	for _, r := range c.Rules {
		rule := map[string]any{"id": r.ID, "shortDescription": msg{r.Title}}
		if r.Fix != "" {
			rule["help"] = msg{r.Fix}
		}
		if r.CWE != "" { // the tags code-scanning dashboards use to file a result under security
			rule["properties"] = map[string]any{"tags": []string{"security", "external/cwe/" + strings.ToLower(r.CWE)}}
		}
		rules = append(rules, rule)
	}
	results := []map[string]any{}
	for _, f := range c.Findings {
		r := map[string]any{"ruleId": f.Rule, "level": f.Severity, "message": msg{f.Message}}
		if f.File != "" {
			r["locations"] = []any{map[string]any{"physicalLocation": map[string]any{
				"artifactLocation": map[string]any{"uri": f.File}, "region": map[string]any{"startLine": max(f.Line, 1)}}}}
		}
		results = append(results, r)
	}
	b, _ := json.MarshalIndent(map[string]any{
		"version": "2.1.0", "$schema": "https://json.schemastore.org/sarif-2.1.0.json",
		"runs": []any{map[string]any{"tool": map[string]any{"driver": map[string]any{"name": "rintel", "version": version, "rules": rules}}, "results": results}},
	}, "", "  ")
	fmt.Fprintln(w, string(b))
}

func writeImpact(w io.Writer, r *ImpactResult, markdown bool) {
	h := func(en, ko string, n int) {
		if markdown {
			fmt.Fprintf(w, "\n### %s (%d)\n\n", tr(en, ko), n)
		} else {
			fmt.Fprintf(w, "\n%s (%d)\n", tr(en, ko), n)
		}
	}
	item := func(left, right string) {
		if markdown {
			fmt.Fprintf(w, "- `%s` — %s\n", left, right)
		} else {
			fmt.Fprintf(w, "  %s  %s\n", left, right)
		}
	}
	if markdown {
		fmt.Fprintf(w, "## %s\n\n", tr("Impact of this change", "이 변경의 영향"))
	}
	fmt.Fprintf(w, tr("Compared with %s: %d files changed, %d symbols changed, %d affected, %d endpoints, %d tests reach it.\n",
		"%s 대비: 파일 %d개 변경, 심볼 %d개 변경, 영향받는 심볼 %d개, 엔드포인트 %d개, 닿는 테스트 %d개.\n"),
		r.Base, len(r.ChangedFiles), len(r.Changed), len(r.Affected), len(r.Endpoints), len(r.Tests))
	where := func(s Symbol) string {
		if s.Location == nil {
			return ""
		}
		return loc(s.Location)
	}
	list := func(xs []ImpactSymbol, depth bool) {
		for i, x := range xs {
			if i == 40 {
				fmt.Fprintf(w, tr("  … %d more (--format json for all)\n", "  … %d개 더 (전체는 --format json)\n"), len(xs)-40)
				break
			}
			right := where(x.Symbol)
			if depth {
				right = fmt.Sprintf(tr("%d step(s) away", "%d단계"), x.Depth) + "  " + right
				if x.Evidence == "heuristic" {
					right += "  ~" + tr("inferred path", "추정 경로")
				}
			}
			item(short(x.Symbol.ID), right)
		}
	}
	if len(r.Changed) > 0 {
		h("Changed", "변경된 심볼", len(r.Changed))
		list(r.Changed, false)
	}
	if len(r.Affected) > 0 {
		h("Affected", "영향받는 심볼", len(r.Affected))
		list(r.Affected, true)
	}
	if len(r.Endpoints) > 0 {
		h("Endpoints", "엔드포인트", len(r.Endpoints))
		for _, e := range r.Endpoints {
			item(e.Method+" "+e.Path, handlerLabel(e.Handler))
		}
	}
	if len(r.Tests) > 0 {
		h("Tests that reach the change", "변경에 닿는 테스트", len(r.Tests))
		list(r.Tests, true)
	}
	if len(r.NotAnalyzed) > 0 {
		h("Changed files not in the index", "인덱스에 없는 변경 파일", len(r.NotAnalyzed))
		for i, f := range r.NotAnalyzed {
			if i == 15 {
				fmt.Fprintf(w, "  … +%d\n", len(r.NotAnalyzed)-15)
				break
			}
			item(f, tr("deleted, not source, or a language rintel does not read", "삭제됐거나, 소스가 아니거나, 분석하지 않는 언어"))
		}
	}
	if r.Truncated {
		fmt.Fprintln(w, tr("\nThe search stopped at the depth limit; raise --depth to follow further.", "\n깊이 제한에서 탐색을 멈췄습니다. 더 따라가려면 --depth를 올리세요."))
	}
	if len(r.Changed) == 0 && len(r.ChangedFiles) == 0 {
		fmt.Fprintln(w, tr("No changes against this base.", "이 기준 대비 변경이 없습니다."))
	}
}
