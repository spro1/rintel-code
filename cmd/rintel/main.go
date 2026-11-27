// rintel: local-first repository intelligence (M1: Java/Spring).
package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var korean bool

// tr picks the message for the active language. Keeping both texts at the call site keeps them in sync.
func tr(en, ko string) string {
	if korean {
		return ko
	}
	return en
}

func detectLang() {
	for _, v := range []string{os.Getenv("RINTEL_LANG"), os.Getenv("LC_ALL"), os.Getenv("LC_MESSAGES"), os.Getenv("LANG")} {
		if v != "" {
			korean = strings.HasPrefix(strings.ToLower(v), "ko")
			return
		}
	}
}

func usage() string {
	return tr(`rintel - who calls what in this repository, with evidence

  rintel callers <symbol> [--depth N]   who calls it (a class: who calls any of its methods)
  rintel callees <symbol> [--depth N]   what it calls
  rintel refs <symbol>                  everything that references it: calls, type/field uses, injections, overrides
  rintel find <name>                    search classes, methods, fields
  rintel api [--prefix /path]           REST endpoints
  rintel stats                          how much of the code was resolved, and what was not
  rintel metrics [class|method|package|file]   complexity, size, coupling, cohesion (--top N, --tests to include test and generated code, --format csv)
  rintel deps                           package dependencies and cycles (--format dot|mermaid for a diagram)
  rintel check                          quality gate for CI: exits 1 on warnings (--fail-on, --changed-since, --category security|quality, --format sarif)
                                        --vulns also looks up known vulnerabilities in dependencies (uses the network: osv.dev)
  rintel impact [--base REF]            what a change affects: callers, endpoints, tests to run (--format md for a PR comment)
  rintel report [--open] [--out file] [--source]   one self-contained HTML page: metrics, graphs, checks, explorer
  rintel scan [--full]                  refresh the index (queries do this automatically)
  rintel mcp                            MCP server on stdio, for coding agents
  rintel doctor                         check which languages can be analyzed on this machine
  rintel version

A symbol can be written the way rintel prints it: UserService.createUser, UserService.createUser(String),
"new User(String)", or a full ID. Unmarked results are facts from static analysis; "~inferred" marks a guess and names its rule.

Languages: Java (Spring, Lombok), Go, TypeScript/JavaScript (React, Vue), Python. Run it anywhere inside a repository.
flags: -C <dir> repository root   --format text|json   --lang en|ko   --no-scan   --depth 1..3
exit codes: 0 ok, 1 symbol not found or check failed, 2 usage error, 3 analysis error`,
		`rintel - 이 저장소에서 누가 무엇을 호출하는지 근거와 함께 알려줍니다

  rintel callers <심볼> [--depth N]     호출하는 곳 (클래스면 그 메서드들을 호출하는 곳)
  rintel callees <심볼> [--depth N]     호출되는 대상
  rintel refs <심볼>                    참조하는 모든 곳: 호출, 타입·필드 사용, 주입, 오버라이드
  rintel find <이름>                    클래스·메서드·필드 검색
  rintel api [--prefix /path]           REST 엔드포인트
  rintel stats                          해석된 비율과 해석하지 못한 항목
  rintel metrics [class|method|package|file]   복잡도·크기·결합도·응집도 지표 (--top N, --tests는 테스트·생성 코드 포함, --format csv)
  rintel deps                           패키지 의존과 순환 (--format dot|mermaid로 다이어그램)
  rintel check                          CI 품질 게이트: 경고가 있으면 종료 코드 1 (--fail-on, --changed-since, --category security|quality, --format sarif)
                                        --vulns: 의존성의 알려진 취약점도 조회 (네트워크 사용: osv.dev)
  rintel impact [--base REF]            변경의 영향: 호출자, 엔드포인트, 돌릴 테스트 (--format md는 PR 코멘트용)
  rintel report [--open] [--out 파일] [--source]   HTML 한 장으로 보기: 지표, 그래프, 점검, 탐색
  rintel scan [--full]                  인덱스 갱신 (조회 명령이 자동으로 수행)
  rintel mcp                            코딩 에이전트용 MCP 서버 (stdio)
  rintel doctor                         이 PC에서 분석 가능한 언어 점검
  rintel version

심볼은 rintel이 출력하는 형태 그대로 쓸 수 있습니다: UserService.createUser, UserService.createUser(String),
"new User(String)", 또는 전체 ID. 표시가 없는 결과는 정적 분석으로 확정된 사실이고, "~추정"은 추론이며 사용한 규칙을 함께 보여줍니다.

지원 언어: Java(Spring, Lombok), Go, TypeScript/JavaScript(React, Vue), Python. 저장소 안 어디에서든 실행할 수 있습니다.
플래그: -C <디렉터리> 저장소 루트   --format text|json   --lang en|ko   --no-scan   --depth 1..3
종료 코드: 0 정상, 1 심볼 없음 또는 점검 실패, 2 사용법 오류, 3 분석 오류`)
}

// version is set at release time with -ldflags "-X main.version=v1.2.3". Without that (`go install ...@v1.2.3`),
// the version of the module the binary was built from is used.
var version = "dev"

func init() {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	version = strings.TrimPrefix(version, "v")
}

func rootGiven(fs *flag.FlagSet) (given bool) {
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "C" })
	return
}

// findRoot walks up from dir to the nearest directory that already has an index or is a git work tree.
func findRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	for d := abs; ; d = filepath.Dir(d) {
		if fileExists(filepath.Join(d, ".ri", "cache", "index.db")) || fileExists(filepath.Join(d, ".git")) {
			if rel, err := filepath.Rel(abs, d); err == nil {
				return filepath.Join(dir, rel)
			}
			return d
		}
		if d == filepath.Dir(d) {
			return dir
		}
	}
}

func die(code int, msg any) {
	// An error can quote a file name, a config key or an argument: keep it from driving the terminal.
	lines := strings.Split(fmt.Sprint(msg), "\n")
	for i := range lines {
		lines[i] = printable(lines[i])
	}
	fmt.Fprintln(os.Stderr, "rintel:", strings.Join(lines, "\n"))
	if err, ok := msg.(error); ok && damaged(err) {
		fmt.Fprintln(os.Stderr, tr("The index file is damaged. It is only a cache: delete the .ri/cache directory and run again.", "인덱스 파일이 손상됐습니다. 캐시일 뿐이니 .ri/cache 디렉터리를 지우고 다시 실행하세요."))
	}
	os.Exit(code)
}

// usageErr names the one thing that was wrong; the full help is a command away, not dumped on every typo.
func usageErr(msg string) {
	fmt.Fprintln(os.Stderr, "rintel:", msg)
	fmt.Fprintln(os.Stderr, tr("Run `rintel help` for usage.", "사용법은 `rintel help`로 볼 수 있습니다."))
	os.Exit(2)
}

func main() {
	detectLang()
	fs := flag.NewFlagSet("rintel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("C", ".", "")
	format := fs.String("format", "text", "")
	lang := fs.String("lang", "", "")
	full := fs.Bool("full", false, "")
	noScan := fs.Bool("no-scan", false, "")
	depth := fs.Int("depth", 1, "")
	prefix := fs.String("prefix", "", "")
	out := fs.String("out", "", "")
	open := fs.Bool("open", false, "")
	top := fs.Int("top", 30, "")
	tests := fs.Bool("tests", false, "")
	source := fs.Bool("source", false, "")
	base := fs.String("base", "HEAD", "")
	failOn := fs.String("fail-on", "warning", "")
	since := fs.String("changed-since", "", "")
	vulns := fs.Bool("vulns", false, "")
	category := fs.String("category", "", "")
	var pos []string // flags may come anywhere: before the command, or before or after its argument
	for args := os.Args[1:]; len(args) > 0; {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Println(usage())
				return
			}
			usageErr(err.Error())
		}
		if args = fs.Args(); len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	switch *lang {
	case "":
	case "en", "ko":
		korean = *lang == "ko"
	default:
		usageErr(fmt.Sprintf(tr("unsupported language %q (use en or ko)", "지원하지 않는 언어 %q (en 또는 ko)"), *lang))
	}
	if *failOn != "warning" && *failOn != "note" && *failOn != "never" {
		usageErr(fmt.Sprintf(tr("unsupported --fail-on %q (use warning, note or never)", "지원하지 않는 --fail-on %q (warning, note, never)"), *failOn))
	}
	if len(pos) > 0 && pos[0] == "query" { // long form kept for scripts: rintel query callers X
		pos = pos[1:]
	}
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, usage())
		os.Exit(2)
	}
	cmd, pos := pos[0], pos[1:]
	if cmd == "symbol" {
		cmd = "find"
	}
	wantArgs := map[string]int{"help": 0, "doctor": 0, "scan": 0, "mcp": 0, "stats": 0, "api": 0, "report": 0, "metrics": 0, "version": 0, "deps": 0, "check": 0, "impact": 0, "find": 1, "callers": 1, "callees": 1, "refs": 1}
	n, known := wantArgs[cmd]
	scope := "class"
	if cmd == "metrics" && len(pos) == 1 { // the one command with an optional argument
		scope, pos = pos[0], nil
		if scope != "class" && scope != "method" && scope != "package" && scope != "file" {
			usageErr(fmt.Sprintf(tr("unknown metrics scope %q (use class, method, package or file)", "알 수 없는 지표 범위 %q (class, method, package, file)"), scope))
		}
	}
	formats := map[string]string{"metrics": "text json csv", "deps": "text json dot mermaid", "check": "text json sarif", "impact": "text json md"}[cmd]
	if formats == "" {
		formats = "text json"
	}
	if !strings.Contains(" "+formats+" ", " "+*format+" ") {
		usageErr(fmt.Sprintf(tr("`rintel %s` does not support --format %s (use %s)", "`rintel %s`는 --format %s를 지원하지 않습니다 (%s 중에서)"), cmd, *format, strings.ReplaceAll(formats, " ", ", ")))
	}
	switch {
	case !known:
		usageErr(fmt.Sprintf(tr("unknown command %q", "알 수 없는 명령 %q"), cmd))
	case len(pos) < n:
		usageErr(fmt.Sprintf(tr("`rintel %s` needs a name, e.g. `rintel %[1]s UserService.createUser`", "`rintel %s`에는 이름이 필요합니다. 예: `rintel %[1]s UserService.createUser`"), cmd))
	case len(pos) > n:
		usageErr(fmt.Sprintf(tr("unexpected argument %q (quote names that contain spaces or parentheses)", "예상하지 못한 인자 %q (공백이나 괄호가 있는 이름은 따옴표로 감싸세요)"), pos[n]))
	case n == 1 && strings.TrimSpace(pos[0]) == "":
		usageErr(tr("the name is empty", "이름이 비어 있습니다"))
	}
	if cmd == "impact" { // a change is worth following further than a single symbol's callers
		given := false
		fs.Visit(func(f *flag.Flag) { given = given || f.Name == "depth" })
		if !given {
			*depth = 3
		}
		*depth = min(max(*depth, 1), 6)
	} else if *depth < 1 || *depth > 3 {
		fmt.Fprintf(os.Stderr, tr("note: --depth is limited to 1..3; using %d\n", "참고: --depth는 1~3만 가능합니다. %d로 진행합니다\n"), min(max(*depth, 1), 3))
		*depth = min(max(*depth, 1), 3)
	}

	switch cmd {
	case "help":
		fmt.Println(usage())
		return
	case "doctor":
		doctor()
		return
	}

	if cmd == "version" {
		fmt.Println("rintel", version)
		return
	}
	if !rootGiven(fs) { // like git: work from anywhere inside the repository
		*root = findRoot(*root)
	}
	if st, err := os.Stat(*root); err != nil || !st.IsDir() {
		die(3, fmt.Sprintf(tr("not a directory: %s", "디렉터리가 아닙니다: %s"), *root))
	}
	// Nothing to analyze: say so instead of creating .ri/ and printing a table of zeros.
	if _, err := os.Stat(filepath.Join(*root, ".ri", "cache", "index.db")); err != nil && cmd != "mcp" && !hasSources(*root) {
		abs, _ := filepath.Abs(*root)
		found := otherSources(*root)
		die(3, fmt.Sprintf(tr("nothing to analyze under %s: rintel reads Java, Go, TypeScript/JavaScript (and Vue) and Python.%s Run it in a repository root, or pass -C <dir>.",
			"%s 아래에 분석할 소스가 없습니다. rintel은 Java, Go, TypeScript/JavaScript(Vue 포함), Python을 분석합니다.%s 저장소 루트에서 실행하거나 -C <디렉터리>를 지정하세요."), abs, found))
	}
	displayRoot = *root
	db, err := openDB(*root)
	if err != nil {
		die(3, err)
	}
	defer db.Close()
	q := &Q{*root, db}
	doScan := func(full bool) *ScanResult {
		fmt.Fprintln(os.Stderr, tr("Analyzing repository…", "저장소를 분석하는 중…"))
		r, err := scan(*root, db, full)
		if err != nil {
			die(3, err)
		}
		return r
	}
	if cmd == "mcp" {
		if err := serveMCP(q); err != nil {
			die(3, err)
		}
		return
	}
	if cmd == "scan" {
		r := doScan(*full)
		if *format == "json" {
			printJSON(r)
			return
		}
		mode := tr("incremental", "증분")
		if r.Full {
			mode = tr("full", "전체")
		}
		fmt.Printf(tr("Scan %d (%s): %d files, %d re-analyzed, %d ms\n", "스캔 %d (%s): 파일 %d개, 재분석 %d개, %d ms\n"), r.ScanID, mode, r.Files, r.Parsed, r.DurationMs)
		if o, err := q.Overview(); err == nil {
			printStats(o)
		}
		return
	}
	// queries just work: no separate scan step, and never an answer from a stale index unless asked for
	if !*noScan {
		if si, err := q.scanInfo(); errors.Is(err, errNoScan) || (err == nil && si.Stale) {
			doScan(false)
		}
	}
	if cmd == "report" {
		html, err := q.Report(map[bool]string{true: "ko", false: "en"}[korean], *source)
		if err != nil {
			die(3, err)
		}
		if *out == "" { // under .ri/cache: regenerable, and already ignored by git, so it is not committed by accident
			*out = filepath.Join(*root, ".ri", "cache", "report.html")
		}
		if err := os.WriteFile(*out, html, 0o644); err != nil {
			die(3, err)
		}
		abs, _ := filepath.Abs(*out)
		fmt.Printf(tr("Report written to %s (%d KB)\n", "보고서를 만들었습니다: %s (%d KB)\n"), abs, len(html)/1024)
		if *source {
			fmt.Println(tr("It contains the source code. Check before sharing it outside the team.", "소스 코드가 포함되어 있습니다. 팀 밖으로 공유하기 전에 확인하세요."))
		}
		if *open {
			opener := "xdg-open"
			if _, err := exec.LookPath("open"); err == nil {
				opener = "open"
			}
			if err := exec.Command(opener, abs).Start(); err != nil {
				die(3, err)
			}
		} else {
			fmt.Println(tr("Open it in a browser, or run `rintel report --open`.", "브라우저로 열거나 `rintel report --open`을 실행하세요."))
		}
		return
	}
	if cmd == "metrics" {
		m, err := q.Metrics()
		if err != nil {
			die(3, err)
		}
		n := *top
		if *format != "text" {
			n = 0 // machine formats carry everything unless --top was given
			fs.Visit(func(f *flag.Flag) {
				if f.Name == "top" {
					n = *top
				}
			})
		}
		m = m.limit(n, *tests)
		switch *format {
		case "json":
			printJSON(m)
		case "csv":
			head, rows := m.table(scope, func(id string) string { return id })
			w := csv.NewWriter(os.Stdout)
			w.Write(head)
			w.WriteAll(rows)
		default:
			staleNote(m.Scan)
			head, rows := m.table(scope, short)
			width := make([]int, len(head))
			for _, r := range append([][]string{head}, rows...) {
				for i, c := range r {
					if i == 0 && utf8.RuneCountInString(c) > 60 { // one long signature must not push every column off screen
						r[i] = string([]rune(c)[:59]) + "…"
					}
					width[i] = max(width[i], utf8.RuneCountInString(r[i]))
				}
			}
			for _, r := range append([][]string{head}, rows...) {
				for i, c := range r {
					if i == 0 || i == len(r)-1 && scope != "package" && scope != "file" {
						fmt.Printf("%-*s  ", width[i], c)
					} else {
						fmt.Printf("%*s  ", width[i], c)
					}
				}
				fmt.Println()
			}
			if len(rows) == 0 {
				fmt.Println(tr("(nothing to show)", "(표시할 항목 없음)"))
			}
		}
		return
	}
	switch cmd {
	case "deps":
		d, err := q.Deps(*tests, *format == "json")
		if err != nil {
			die(3, err)
		}
		if *format == "json" {
			printJSON(d)
		} else {
			staleNote(d.Scan)
			writeDeps(os.Stdout, d, *format)
		}
		return
	case "check":
		c, err := q.Checks()
		if err != nil {
			die(3, err)
		}
		if *vulns {
			// The only network use in rintel, so say so before it happens.
			fmt.Fprintln(os.Stderr, tr("Looking up dependency versions at osv.dev: package names and versions are sent over the network.",
				"osv.dev에서 의존성 버전을 조회합니다. 패키지 이름과 버전이 네트워크로 전송됩니다."))
			q.addVulns(c)
			if d := c.Dependencies; d != nil && d.Error != "" {
				fmt.Fprintf(os.Stderr, tr("Dependency check NOT done: osv.dev could not be reached (%s).\nCheck the network or proxy (HTTPS_PROXY), or run without --vulns. The other checks below are complete.\n",
					"의존성 검사를 하지 못했습니다: osv.dev에 연결할 수 없습니다 (%s).\n네트워크나 프록시(HTTPS_PROXY)를 확인하거나 --vulns 없이 실행하세요. 아래의 다른 점검은 정상 완료됐습니다.\n"), d.Error)
			} else if d != nil {
				fmt.Fprintf(os.Stderr, tr("Network ok: %d dependency versions checked.\n", "네트워크 정상: 의존성 버전 %d개를 확인했습니다.\n"), d.Packages)
			}
		}
		keep := func(ok func(Finding) bool) { // narrow the result and recount
			kept := c.Findings[:0]
			count := map[string]int{}
			c.Warnings, c.Notes = 0, 0
			for _, f := range c.Findings {
				if !ok(f) {
					continue
				}
				kept = append(kept, f)
				count[f.Rule]++
				if f.Severity == "note" {
					c.Notes++
				} else {
					c.Warnings++
				}
			}
			c.Findings = kept
			for i := range c.Rules {
				c.Rules[i].Count = count[c.Rules[i].ID]
			}
		}
		if *category != "" {
			if *category != "security" && *category != "quality" {
				usageErr(fmt.Sprintf(tr("unsupported --category %q (use security or quality)", "지원하지 않는 --category %q (security, quality)"), *category))
			}
			in := map[string]bool{}
			rules := c.Rules[:0]
			for _, r := range c.Rules {
				if r.Category == *category {
					in[r.ID] = true
					rules = append(rules, r)
				}
			}
			c.Rules = rules
			keep(func(f Finding) bool { return in[f.Rule] })
		}
		if *since != "" { // a legacy code base: hold only the files this branch touched to the rules
			ranges, _, err := changedLines(*root, *since)
			if err != nil {
				die(3, err)
			}
			keep(func(f Finding) bool { _, ok := ranges[f.File]; return ok })
		}
		switch *format {
		case "json":
			printJSON(c)
		case "sarif":
			writeSARIF(os.Stdout, c)
		default:
			staleNote(c.Scan)
			writeChecks(os.Stdout, c)
		}
		if (*failOn == "warning" && c.Warnings > 0) || (*failOn == "note" && c.Warnings+c.Notes > 0) {
			os.Exit(1)
		}
		if c.Dependencies != nil && c.Dependencies.Error != "" { // asked for and not done: a gate must not pass on that
			os.Exit(3)
		}
		return
	case "impact":
		r, err := q.Impact(*base, *depth)
		if err != nil {
			die(3, err)
		}
		if *format == "json" {
			printJSON(r)
		} else {
			writeImpact(os.Stdout, r, *format == "md")
		}
		return
	}
	var result any
	switch cmd {
	case "stats":
		result, err = q.Overview()
	case "api":
		result, err = q.APISpec(*prefix, 200, "")
	case "find":
		result, err = q.Search(pos[0], "", "", 200, "")
	case "callers":
		result, err = q.Neighbors(pos[0], "in", *depth, "calls", 200, "")
	case "callees":
		result, err = q.Neighbors(pos[0], "out", *depth, "calls", 200, "")
	case "refs":
		result, err = q.Neighbors(pos[0], "in", 1, "", 200, "")
	}
	var nf *notFoundError
	if errors.As(err, &nf) {
		fmt.Fprintf(os.Stderr, tr("rintel: no symbol named %q\n", "rintel: %q라는 심볼이 없습니다\n"), nf.Ref)
		if len(nf.Hints) > 0 {
			fmt.Fprintln(os.Stderr, tr("Did you mean:", "혹시 이것을 찾으셨나요:"))
			for _, h := range nf.Hints {
				fmt.Fprintln(os.Stderr, "  "+short(h))
			}
		} else {
			fmt.Fprintln(os.Stderr, tr("Try `rintel find <part of the name>`.", "`rintel find <이름 일부>`로 검색해 보세요."))
		}
		os.Exit(1)
	}
	if errors.Is(err, errNoScan) {
		die(3, tr("no index yet: run `rintel scan` (or drop --no-scan)", "인덱스가 없습니다. `rintel scan`을 실행하거나 --no-scan을 빼세요"))
	}
	if err != nil {
		die(3, err)
	}
	if *format == "json" {
		printJSON(result)
		return
	}
	switch r := result.(type) {
	case *Overview:
		staleNote(r.Scan)
		printStats(r)
	case *SearchResult:
		staleNote(r.Scan)
		var rows [][2]string
		for _, s := range r.Symbols {
			name := short(s.ID)
			switch s.Synthetic { // explain symbols the reader will not find in the source
			case "lombok":
				name += tr("  (generated by Lombok)", "  (Lombok 생성)")
			case "implicit":
				name += tr("  (implicit)", "  (암시적)")
			}
			kind := s.Type
			if s.Type == "class" && s.Kind != "" { // interface, struct, file, ... says more than "class"
				kind = s.Kind
			}
			rows = append(rows, [2]string{fmt.Sprintf("%-11s %s", kind, name), loc(s.Location)})
		}
		printRows(rows, tr("No symbols match.", "일치하는 심볼이 없습니다."))
		truncated(r.Page)
	case *APIResult:
		staleNote(r.Scan)
		var rows [][2]string
		for _, e := range r.Endpoints {
			rows = append(rows, [2]string{fmt.Sprintf("%-6s %-34s %s", e.Method, e.Path, handlerLabel(e.Handler)), loc(e.Location)})
		}
		printRows(rows, tr("No REST endpoints found.", "REST 엔드포인트를 찾지 못했습니다."))
		truncated(r.Page)
	case *NeighborsResult:
		staleNote(r.Scan)
		printTree(q, r, cmd)
	}
}

// doctor reports which languages can be analyzed on this machine. Each needs its own toolchain; none is required for the others.
func doctor() {
	have := func(bin string) bool {
		p, err := exec.LookPath(bin)
		if err != nil {
			fmt.Printf("--   %-6s %s\n", bin, tr("not found on PATH", "PATH에 없습니다"))
			return false
		}
		fmt.Printf("ok   %-6s %s\n", bin, p)
		return true
	}
	have("git") // optional: without it scans just carry no commit
	java, javac := have("java"), have("javac")
	usable := 0
	if java && javac {
		if _, err := adapterDir(); err != nil {
			fmt.Println("FAIL Java: " + err.Error())
		} else {
			fmt.Println("ok   " + tr("Java analysis ready (adapter compiled)", "Java 분석 가능 (어댑터 컴파일됨)"))
			usable++
		}
	} else {
		fmt.Println("--   " + tr("Java analysis unavailable: needs a JDK 17+ with javac", "Java 분석 불가: JDK 17 이상(javac 포함)이 필요합니다"))
	}
	if have("go") {
		if goroot, modcache, err := goEnv(); err != nil {
			fmt.Println("FAIL Go: " + err.Error())
		} else {
			fmt.Printf("ok   "+tr("Go analysis ready (GOROOT %s, module cache %s)", "Go 분석 가능 (GOROOT %s, 모듈 캐시 %s)")+"\n", goroot, modcache)
			usable++
		}
	} else {
		fmt.Println("--   " + tr("Go analysis unavailable: needs the Go toolchain (its standard library sources are read)", "Go 분석 불가: Go 툴체인이 필요합니다 (표준 라이브러리 소스를 읽습니다)"))
	}
	if have("node") {
		if _, err := tsAdapterDir(); err != nil {
			fmt.Println("FAIL TypeScript: " + err.Error())
		} else {
			fmt.Println("ok   " + tr("TypeScript/JavaScript/Vue analysis ready (bundled TypeScript 5.9.3 compiler)", "TypeScript/JavaScript/Vue 분석 가능 (내장 TypeScript 5.9.3 컴파일러)"))
			usable++
		}
	} else {
		fmt.Println("--   " + tr("TypeScript/JavaScript analysis unavailable: needs Node.js 18+", "TypeScript/JavaScript 분석 불가: Node.js 18 이상이 필요합니다"))
	}
	if have("python3") {
		fmt.Println("ok   " + tr("Python analysis ready", "Python 분석 가능"))
		usable++
	} else {
		fmt.Println("--   " + tr("Python analysis unavailable: needs python3 (3.8+)", "Python 분석 불가: python3(3.8 이상)가 필요합니다"))
	}
	if usable == 0 {
		die(3, tr("no language can be analyzed on this machine", "이 PC에서 분석할 수 있는 언어가 없습니다"))
	}
}

// otherSources names the languages found that rintel does not analyze, so "nothing to analyze" is not a dead end.
func otherSources(root string) string {
	counts := map[string]int{}
	walk(root, func(rel string, _ fs.FileInfo) error {
		if l := langOf(filepath.Base(rel)); l != "" && l != "build" && !analyzed(l) {
			counts[l]++
		}
		return nil
	})
	if len(counts) == 0 {
		return ""
	}
	var parts []string
	for _, l := range sortedKeys(counts) {
		parts = append(parts, fmt.Sprintf("%s %d", l, counts[l]))
	}
	return tr(" Found but not supported yet: ", " 발견했지만 아직 지원하지 않는 언어: ") + strings.Join(parts, ", ") + "."
}

func printJSON(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

var inlineRoute = regexp.MustCompile(`[#~](GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS|ANY|WS)(_[A-Za-z0-9_]*)?$`)

// handlerLabel names an endpoint's handler; one written in place is shown by the function that registers it.
func handlerLabel(id string) string {
	if m := inlineRoute.FindStringIndex(id); m != nil {
		return short(id[:m[0]]) + tr(" (inline)", " (인라인)")
	}
	return short(id)
}

// short drops the language prefix and the package: text output is for reading, JSON keeps full IDs.
// Everything short prints is accepted back by Q.resolve.
func short(id string) string {
	if strings.HasPrefix(id, "ts:") || strings.HasPrefix(id, "py:") { // file name + declaration: client.UserService.getUser, models.py
		s := id[3:]
		return strings.Replace(s[strings.LastIndex(s, "/")+1:], "#", ".", 1)
	}
	if strings.HasPrefix(id, "go:") { // Go: package name + declaration, the way Go code refers to things (web.Server.handle)
		s := id[3:]
		i := strings.LastIndex(s, "/")
		s = s[i+1:]
		pkg, name, _ := strings.Cut(s, ".")
		if i < 0 { // the module's root package: its directory name says nothing, so leave the qualifier out
			pkg = ""
		}
		if strings.HasSuffix(name, "_go") { // the per-file container of package-level declarations
			return strings.TrimPrefix(pkg+"/", "/") + strings.TrimSuffix(name, "_go") + ".go"
		}
		name = strings.Replace(strings.Replace(name, "_go#", ".go:", 1), "#", ".", 1)
		return strings.TrimPrefix(pkg+"."+name, ".")
	}
	s, sig := id[strings.Index(id, ":")+1:], ""
	if i := strings.Index(s, "("); i >= 0 {
		s, sig = s[:i], s[i:]
	}
	parts := strings.Split(strings.Replace(s, "#", ".", 1), ".")
	i := 0
	for i < len(parts)-1 && parts[i] != "" && unicode.IsLower(rune(parts[i][0])) {
		i++
	}
	if n := len(parts); parts[n-1] == "<init>" && n-i >= 2 { // constructors read as `new User(String)`
		return "new " + strings.Join(parts[i:n-1], ".") + sig
	}
	return strings.Join(parts[i:], ".") + sig
}

var displayRoot = "." // paths are printed relative to the working directory, so terminals can open them

func loc(l *Location) string {
	if l == nil {
		return tr("(library)", "(라이브러리)")
	}
	return fmt.Sprintf("%s:%d", filepath.Join(displayRoot, l.File), l.StartLine)
}

func staleNote(s ScanInfo) {
	if s.Stale {
		fmt.Fprintf(os.Stderr, tr("note: %d file(s) changed since the last scan; results may be out of date (drop --no-scan to refresh)\n",
			"참고: 마지막 스캔 이후 파일 %d개가 바뀌어 결과가 최신이 아닐 수 있습니다 (--no-scan을 빼면 자동 갱신)\n"), s.StaleFiles)
	}
}

func truncated(p Page) {
	if p.Truncated {
		fmt.Println(tr("… more results not shown; narrow the query or use --format json", "… 결과가 더 있습니다. 검색어를 좁히거나 --format json을 쓰세요"))
	}
}

// printRows aligns the location column, without letting one long name push everything off screen.
func printRows(rows [][2]string, empty string) {
	if len(rows) == 0 {
		fmt.Println(empty)
		return
	}
	w := 0
	for _, r := range rows {
		if n := utf8.RuneCountInString(r[0]); n > w && n <= 90 && r[1] != "" {
			w = n
		}
	}
	for _, r := range rows {
		if r[1] == "" {
			fmt.Println(r[0])
			continue
		}
		fmt.Printf("%s%s  %s\n", r[0], strings.Repeat(" ", max(w-utf8.RuneCountInString(r[0]), 0)), r[1])
	}
}

func printTree(q *Q, r *NeighborsResult, cmd string) {
	children := map[string][]Edge{}
	for _, e := range r.Edges {
		parent := e.From
		if r.Direction == "in" {
			parent = e.To
		}
		children[parent] = append(children[parent], e)
	}
	arrow := "→ "
	if r.Direction == "in" {
		arrow = "← "
	}
	none := map[string]string{
		"callers": tr("no callers found", "호출하는 곳 없음"),
		"callees": tr("calls nothing", "호출하는 대상 없음"),
		"refs":    tr("no references found", "참조하는 곳 없음"),
	}[cmd]
	var rows [][2]string
	seen := map[string]bool{}
	var walk func(node string, indent string)
	walk = func(node, indent string) {
		for _, e := range children[node] {
			child := e.To
			if r.Direction == "in" {
				child = e.From
			}
			line := indent + arrow + short(child)
			if e.Relation != "calls" {
				line += "  [" + e.Relation + "]"
			}
			if e.Evidence == "heuristic" { // facts carry no badge; only inferences are marked
				line += "  ~" + tr("inferred", "추정") + ": " + e.Rule
			}
			rows = append(rows, [2]string{line, loc(e.At)})
			if !seen[child] {
				seen[child] = true
				walk(child, indent+"   ")
			}
		}
	}
	for _, root := range r.Roots {
		seen[root] = true
	}
	for _, c := range r.ExpandedFrom {
		rows = append(rows, [2]string{fmt.Sprintf(tr("%s is a class: showing its methods and constructors.", "%s는 클래스이므로 메서드와 생성자 기준으로 보여줍니다."), short(c)), ""})
	}
	shown := 0
	for _, root := range r.Roots {
		if len(children[root]) == 0 && len(r.Roots) > 1 {
			continue // with several roots, list only the ones that have something
		}
		if shown > 0 {
			rows = append(rows, [2]string{"", ""})
		}
		shown++
		head := short(root)
		if s := q.symbol(r.Scan.ID, root); s.Location != nil {
			rows = append(rows, [2]string{head, loc(s.Location)})
		} else {
			rows = append(rows, [2]string{head, loc(nil)})
		}
		if len(children[root]) == 0 {
			rows = append(rows, [2]string{"  (" + none + ")", ""})
		}
		walk(root, "  ")
	}
	if shown == 0 {
		rows = append(rows, [2]string{fmt.Sprintf(tr("(%s for any of %d symbols)", "(심볼 %[2]d개 모두 %[1]s)"), none, len(r.Roots)), ""})
	}
	printRows(rows, "")
	truncated(r.Page)
	// an empty or short list is only trustworthy if the reader knows what analysis could not see
	if r.Direction == "out" && r.UnresolvedCount > 0 {
		fmt.Printf(tr("\n%d call(s) inside this symbol could not be resolved, so the list may be incomplete.\n",
			"\n이 심볼 안의 호출 %d건을 해석하지 못해 목록이 불완전할 수 있습니다.\n"), r.UnresolvedCount)
	}
	if r.Direction == "in" {
		if o, err := q.Overview(); err == nil && o.UnresolvedCalls > 0 {
			fmt.Printf(tr("\n%d call site(s) in this repository are unresolved (%.1f%% resolved), so some callers may be missing. See `rintel stats`.\n",
				"\n이 저장소의 호출 %d건이 미해석 상태(해석률 %.1f%%)라 일부 호출자가 빠졌을 수 있습니다. `rintel stats` 참고.\n"), o.UnresolvedCalls, o.ResolutionRate*100)
		}
	}
}

func kv(m map[string]int, unit string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d%s", k, m[k], unit))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " · ")
}

func printStats(o *Overview) {
	files := 0
	for _, n := range o.Languages {
		files += n
	}
	commit := o.Scan.Commit
	if len(commit) > 7 {
		commit = commit[:7]
	}
	if commit == "" {
		commit = tr("no git", "git 없음")
	}
	row := func(en, ko, value string) { fmt.Printf("%s  %s\n", pad(tr(en, ko), 16), value) }
	row("Scan", "스캔", fmt.Sprintf(tr("#%d · %s · %d files · %d symbols · %d endpoints", "#%d · %s · 파일 %d개 · 심볼 %d개 · 엔드포인트 %d개"),
		o.Scan.ID, commit, files, o.Symbols, o.Endpoints))
	if o.ResolvedCalls+o.UnresolvedCalls > 0 {
		row("Resolved calls", "호출 해석률", fmt.Sprintf(tr("%.1f%%  (%d of %d call sites)", "%.1f%%  (호출 %[3]d건 중 %[2]d건)"),
			o.ResolutionRate*100, o.ResolvedCalls, o.ResolvedCalls+o.UnresolvedCalls))
	}
	row("Unresolved", "미해석", kv(o.Unresolved, ""))
	row("Edges", "관계", kv(o.Edges, ""))
	row("Languages", "언어", kv(o.Languages, tr(" files", "개")))
	row("Modules", "모듈", kv(o.Modules, tr(" files", "개")))
	if o.Languages["java"] > 0 {
		row("Classpath", "클래스패스", fmt.Sprintf(tr("%d jars", "JAR %d개"), o.ClasspathJars))
	}
	if len(o.ExcludedFiles) > 0 {
		row("Not analyzed", "분석 제외", kv(o.ExcludedFiles, tr(" files", "개"))+tr("  (the graph has holes where these are used)", "  (이 파일들이 관여한 관계는 그래프에 없습니다)"))
	}
	if len(o.MissingPackages) > 0 {
		row("Missing libraries", "없는 라이브러리", strings.Join(o.MissingPackages, ", "))
	}
	if o.Unresolved["missing-classpath"]+o.Unresolved["missing-type"] > 0 {
		fmt.Println(tr("\nHint: some libraries were not found in the local Gradle/Maven cache. Build the project once, or list the jars in .ri/classpath.",
			"\n힌트: 일부 라이브러리를 로컬 Gradle/Maven 캐시에서 찾지 못했습니다. 프로젝트를 한 번 빌드하거나 .ri/classpath에 JAR 경로를 적어 주세요."))
	}
	if o.Unresolved["missing-module"] > 0 {
		fmt.Println(tr("\nHint: some dependencies are not installed (Go: `go mod download`; TypeScript/JavaScript: `npm install`). Install them, then scan again.",
			"\n힌트: 일부 의존성이 설치되어 있지 않습니다 (Go: `go mod download`, TypeScript/JavaScript: `npm install`). 설치한 뒤 다시 스캔하세요."))
	}
	for _, w := range o.Warnings {
		fmt.Println(tr("\nWarning: ", "\n경고: ") + printable(w))
	}
}

// pad by display width: Hangul is two columns wide.
func pad(s string, w int) string {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x1100 {
			n++
		}
	}
	return s + strings.Repeat(" ", max(w-n, 0))
}
