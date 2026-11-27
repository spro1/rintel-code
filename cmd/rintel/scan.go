package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

//go:embed adapter/Adapter.java
var adapterSrc []byte

const maxFileSize = 2 << 20 // larger sources are skipped: a hostile repo must not exhaust the parser

func hashOf(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// adapterDir compiles the embedded adapter once per version into the user cache.
func adapterDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "rintel", "adapter-"+hashOf(adapterSrc)[:12])
	if _, err := os.Stat(filepath.Join(dir, "Adapter.class")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	src := filepath.Join(dir, "Adapter.java")
	if err := os.WriteFile(src, adapterSrc, 0o644); err != nil {
		return "", err
	}
	if out, err := exec.Command("javac", "-nowarn", "-d", dir, src).CombinedOutput(); err != nil {
		return "", fmt.Errorf("compiling Java adapter (a JDK 17+ with javac is required): %v\n%s", err, out)
	}
	return dir, nil
}

type srcFile struct {
	Path, Hash, Lang, Module, Root string
	Mtime, Size                    int64
}

// analyzed reports whether rintel has an adapter for the language.
func analyzed(lang string) bool {
	switch lang {
	case "java", "go", "typescript", "javascript", "vue", "python":
		return true
	}
	return false
}

// Languages that are recognized but not analyzed are only counted, so the report can say what it did not look at.
var otherLanguages = map[string]string{".kt": "kotlin", ".kts": "kotlin", ".rs": "rust", ".c": "c", ".h": "c", ".cc": "c++", ".cpp": "c++", ".hpp": "c++",
	".cs": "c#", ".rb": "ruby", ".php": "php", ".scala": "scala", ".swift": "swift", ".dart": "dart"}

var buildFiles = map[string]bool{"build.gradle": true, "build.gradle.kts": true, "pom.xml": true, "go.mod": true, "package.json": true, "tsconfig.json": true, "pyproject.toml": true}

func langOf(name string) string {
	if buildFiles[name] {
		return "build"
	}
	switch ext := filepath.Ext(name); ext {
	case ".java":
		return "java"
	case ".go":
		return "go"
	case ".ts", ".tsx", ".mts", ".cts":
		if strings.HasSuffix(name, ".d.ts") || strings.HasSuffix(name, ".d.mts") || strings.HasSuffix(name, ".d.cts") {
			return "" // declarations describe other code; they are read by the compiler, not reported
		}
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		if strings.HasSuffix(name, ".min.js") {
			return "" // bundled output
		}
		return "javascript"
	case ".vue":
		return "vue"
	case ".py":
		return "python"
	default:
		return otherLanguages[ext]
	}
}

func isModuleRoot(dir string) bool {
	for f := range buildFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

// walk visits analyzable files; symlinks are never followed.
func walk(root string, fn func(rel string, info fs.FileInfo) error) error {
	outputDirs := map[string]bool{}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() != printable(d.Name()) { // a name made to rewrite the terminal or hide its real spelling
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			name, parent := d.Name(), filepath.Dir(p)
			switch {
			case p == root:
			case strings.HasPrefix(name, "."), name == "node_modules", name == "vendor", name == "testdata", name == "dist",
				name == "venv", name == "__pycache__", name == "site-packages", name == "coverage":
				return filepath.SkipDir // third-party copies, fixtures and build output are not this repository's code
			case outputDirs[parent]: // inside build/ or target/: only generated sources (QueryDSL, MapStruct, ...) matter
				if !strings.HasPrefix(name, "generated") {
					return filepath.SkipDir
				}
			case (name == "build" || name == "target" || name == "out") && isModuleRoot(parent):
				// a build output directory, not a Java package that happens to be called "build"
				if name == "out" {
					return filepath.SkipDir
				}
				outputDirs[p] = true
			}
			return nil
		}
		if !d.Type().IsRegular() || langOf(d.Name()) == "" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		return fn(filepath.ToSlash(rel), info)
	})
}

var packageRe = regexp.MustCompile(`(?m)^\s*package\s+([\w.]+)\s*;`)

// discover lists the files to consider. known is the previous scan: a file whose size and modification time are
// unchanged keeps its hash without being read again, the same shortcut git takes.
func discover(root string, known map[string]srcFile) (files []srcFile, unreadable []string, err error) {
	moduleDirs := map[string]bool{}
	cfg := loadConfig(root)
	if cfg.err != nil {
		return nil, nil, cfg.err
	}
	err = walk(root, func(rel string, info fs.FileInfo) error {
		if cfg.excluded(rel) {
			return nil
		}
		f := srcFile{Path: rel, Lang: langOf(info.Name()), Mtime: info.ModTime().UnixNano(), Size: info.Size()}
		if p, ok := known[rel]; ok && p.Hash != "" && p.Mtime == f.Mtime && p.Size == f.Size && f.Lang != "java" && f.Lang != "build" {
			f.Hash = p.Hash // Java files are still read: their source root comes from the package line
			files = append(files, f)
			return nil
		}
		if analyzed(f.Lang) || f.Lang == "build" {
			if f.Size > maxFileSize {
				return nil
			}
			b, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil { // no permission, a dangling link: one such file must not stop the analysis of the rest
				unreadable = append(unreadable, rel)
				return nil
			}
			f.Hash = hashOf(b)
			dir := filepath.ToSlash(filepath.Dir(rel))
			if f.Lang == "build" {
				moduleDirs[dir] = true
			} else if f.Lang == "java" {
				// source root = file dir minus the package path; works for any layout without reading the build
				f.Root = dir
				if m := packageRe.FindSubmatch(b); m != nil {
					f.Root = strings.TrimSuffix(strings.TrimSuffix(dir, strings.ReplaceAll(string(m[1]), ".", "/")), "/")
				}
				f.Root = filepath.Join(root, f.Root)
			}
		}
		files = append(files, f)
		return nil
	})
	for i := range files {
		files[i].Module = "."
		for d := filepath.ToSlash(filepath.Dir(files[i].Path)); ; d = filepath.ToSlash(filepath.Dir(d)) {
			if moduleDirs[d] {
				files[i].Module = d
				break
			}
			if d == "." {
				break
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, unreadable, err
}

type irLine struct {
	Kind, ID, Type, Parent, File string
	Range                        []int
	Attrs                        json.RawMessage
	From, To, Via                string
	Relation, Evidence, Rule     string
	Text, Reason                 string
	Lines, Code, Comment, Blank  int
}

// printable replaces control characters and text-direction overrides. Names and strings read from a repository are
// printed to terminals and handed to agents, so they must not be able to move the cursor, clear the screen, or show
// a different spelling than the bytes they are made of.
func printable(s string) string {
	bad := func(r rune) bool {
		return unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
	}
	if strings.IndexFunc(s, bad) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if bad(r) {
			return '?'
		}
		return r
	}, s)
}

func (l *irLine) clean() {
	for _, f := range []*string{&l.ID, &l.Parent, &l.File, &l.From, &l.To, &l.Via, &l.Rule, &l.Text, &l.Reason} {
		*f = printable(*f)
	}
	// Attributes are JSON: only decode them when an escape or a direction mark could be inside.
	if a := l.Attrs; bytes.Contains(a, []byte(`\u`)) || bytes.Contains(a, []byte("\xe2\x80")) || bytes.Contains(a, []byte("\xe2\x81")) {
		var v any
		if json.Unmarshal(a, &v) == nil {
			if b, err := json.Marshal(cleanJSON(v)); err == nil {
				l.Attrs = b
			}
		}
	}
}

func cleanJSON(v any) any {
	switch x := v.(type) {
	case string:
		return printable(x)
	case []any:
		for i := range x {
			x[i] = cleanJSON(x[i])
		}
	case map[string]any:
		for k, e := range x {
			x[k] = cleanJSON(e)
		}
	}
	return v
}

// readRegular reads a file of the repository, but never through a symbolic link: a link can point outside the
// repository, and nothing outside it is rintel's to read.
func readRegular(root, rel string, limit int64) ([]byte, bool) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, false
	}
	b, err := os.ReadFile(p)
	return b, err == nil
}

func runAdapter(root string, roots, cp, emit []string) (lines []irLine, err error) {
	dir, err := adapterDir()
	if err != nil {
		return nil, err
	}
	return runProcess("Java", map[string]any{"protocol": 1, "root": root, "source_roots": roots, "classpath": cp, "emit": emit}, "java", "-Xmx2g", "-cp", dir, "Adapter")
}

// gitOut returns "" when the directory is not a git repository (git then prints the argument back and fails).
func gitOut(root string, args ...string) string {
	out, err := exec.Command("git", append(gitSafe(root), args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fqnOf(id string) string {
	s := id[strings.Index(id, ":")+1:]
	if i := strings.Index(s, "("); i >= 0 {
		s = s[:i]
	}
	return strings.Replace(s, "#", ".", 1)
}

type ScanResult struct {
	MissingPackages []string `json:"missing_packages,omitempty" jsonschema:"Java packages the sources import but no jar on this machine provides; the main cause of unresolved references"`
	Warnings        []string `json:"warnings,omitempty" jsonschema:"languages that were found but could not be analyzed on this machine, and why"`
	ScanID          int64    `json:"scan_id"`
	Classpath       int      `json:"classpath_jars" jsonschema:"jars used for type resolution"`
	Files           int      `json:"files"`
	Parsed          int      `json:"parsed"`
	Full            bool     `json:"full"`
	DurationMs      int64    `json:"duration_ms"`
}

var scanMu sync.Mutex // MCP clients may call tools concurrently; two scans must not interleave

func scan(root string, db *sql.DB, full bool) (*ScanResult, error) {
	scanMu.Lock()
	defer scanMu.Unlock()
	// adapters run as separate processes and compare paths: give them an absolute root, whatever -C was
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if full && autoClasspath { // from scratch includes the library choice: the cache may hold the declared versions by now
		os.Remove(filepath.Join(root, ".ri", "cache", "classpath.auto"))
	}
	return scanRound(root, db, full, 0)
}

var errStop = errors.New("stop")

// hasSources reports whether there is anything to analyze, without reading file contents.
func hasSources(root string) bool {
	return walk(root, func(rel string, _ fs.FileInfo) error {
		if analyzed(langOf(filepath.Base(rel))) {
			return errStop
		}
		return nil
	}) == errStop
}

func scanRound(root string, db *sql.DB, full bool, round int) (*ScanResult, error) {
	start := time.Now()
	known := map[string]srcFile{}
	if last := latestScan(db); last != 0 {
		rows, err := db.Query("SELECT path, content_hash, mtime, size FROM files WHERE scan_id=?", last)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f srcFile
			rows.Scan(&f.Path, &f.Hash, &f.Mtime, &f.Size)
			known[f.Path] = f
		}
		rows.Close()
	}
	files, unreadable, err := discover(root, known)
	if err != nil {
		return nil, err
	}
	version, cp, tools := scanVersion(root)

	base := latestScan(db)
	if base != 0 && !full {
		var prevVersion string
		db.QueryRow("SELECT adapter_version FROM scans WHERE id=?", base).Scan(&prevVersion)
		full = prevVersion != version
	}
	if full {
		base = 0
	}

	// ---- which files go to the adapter ----
	cur := map[string]srcFile{}
	for _, f := range files {
		cur[f.Path] = f
	}
	emit := map[string]bool{}
	holders := map[string]bool{} // files with unresolved references, revisited only if the change declares something new
	var changedFiles []string    // files whose content changed (not the ones pulled in as dependents)
	structural := false          // a file appeared or disappeared: declarations certainly changed
	same := false
	if base == 0 {
		for _, f := range files {
			if analyzed(f.Lang) {
				emit[f.Path] = true
			}
		}
	} else {
		prev := map[string]srcFile{}
		rows, err := db.Query("SELECT path, content_hash, language, module FROM files WHERE scan_id=?", base)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f srcFile
			rows.Scan(&f.Path, &f.Hash, &f.Lang, &f.Module)
			prev[f.Path] = f
		}
		rows.Close()
		same = len(prev) == len(files) // nothing added, removed or modified, in any language
		var touched []any              // changed, added or deleted source files
		modules := map[string]bool{}   // modules needing a wider look
		rebuild := map[string]bool{}   // modules whose build definition changed
		for _, f := range files {
			if p, ok := prev[f.Path]; ok && p.Hash == f.Hash {
				continue
			}
			same = false
			modules[f.Module] = true
			if analyzed(f.Lang) {
				emit[f.Path] = true
				touched = append(touched, f.Path)
				if _, existed := prev[f.Path]; existed {
					changedFiles = append(changedFiles, f.Path)
				} else {
					structural = true
				}
			} else if f.Lang == "build" {
				rebuild[f.Module] = true
			}
		}
		for p, f := range prev {
			if _, ok := cur[p]; ok {
				continue
			}
			same = false
			modules[f.Module] = true
			if analyzed(f.Lang) {
				touched = append(touched, p)
				structural = true
			} else if f.Lang == "build" {
				rebuild[f.Module] = true
			}
		}
		for _, f := range files {
			if analyzed(f.Lang) && rebuild[f.Module] {
				emit[f.Path] = true
				structural = true
			}
		}
		into := emit
		add := func(q string, args ...any) error {
			rows, err := db.Query(q, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var p string
				rows.Scan(&p)
				if f, ok := cur[p]; ok && analyzed(f.Lang) {
					into[p] = true
				}
			}
			return rows.Err()
		}
		// files that referenced a symbol declared in a touched file
		for i := 0; i < len(touched); i += 500 {
			chunk := touched[i:min(i+500, len(touched))]
			// the symbols of the touched files are listed once; targets are then found through the index on dst
			// (joining on "dst or via" made this read every relation of the repository per symbol)
			marks := make([]string, len(chunk)) // numbered, because the list appears twice in the statement
			for j := range chunk {
				marks[j] = fmt.Sprintf("?%d", j+2)
			}
			ids := `(SELECT id FROM symbols WHERE scan_id=?1 AND file IN (` + strings.Join(marks, ",") + `))`
			q := `SELECT file FROM dependencies WHERE scan_id=?1 AND origin='adapter' AND dst IN ` + ids + `
			      UNION SELECT file FROM dependencies WHERE scan_id=?1 AND origin='adapter' AND via<>'' AND via IN ` + ids
			if err := add(q, append([]any{base}, chunk...)...); err != nil {
				return nil, err
			}
		}
		// files with unresolved refs in an affected module: a new symbol may now resolve them
		into = holders
		for m := range modules {
			if err := add(`SELECT DISTINCT u.file FROM unresolved_refs u JOIN files f ON f.scan_id=u.scan_id AND f.path=u.file
			               WHERE u.scan_id=? AND f.module=?`, base, m); err != nil {
				return nil, err
			}
		}
	}

	if same && base != 0 {
		// Nothing changed: the existing snapshot is still the answer. Only what git and the file system report is refreshed
		// (a touched file would otherwise look stale forever).
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		for _, f := range files {
			if k := known[f.Path]; k.Mtime != f.Mtime || k.Size != f.Size {
				if _, err := tx.Exec("UPDATE files SET mtime=?, size=? WHERE scan_id=? AND path=?", f.Mtime, f.Size, base, f.Path); err != nil {
					return nil, err
				}
			}
		}
		dirty := gitOut(root, "status", "--porcelain", "--", ".", ":!.ri") != ""
		if _, err := tx.Exec("UPDATE scans SET commit_sha=?, branch=?, dirty=?, finished_at=? WHERE id=?",
			gitOut(root, "rev-parse", "HEAD"), gitOut(root, "rev-parse", "--abbrev-ref", "HEAD"), dirty, time.Now().UTC().Format(time.RFC3339), base); err != nil {
			return nil, err
		}
		var stats string
		tx.QueryRow("SELECT COALESCE(stats_json,'{}') FROM scans WHERE id=?", base).Scan(&stats)
		r := &ScanResult{}
		json.Unmarshal([]byte(stats), r)
		r.ScanID, r.Files, r.Parsed, r.Full, r.DurationMs = base, len(files), 0, false, time.Since(start).Milliseconds()
		return r, tx.Commit()
	}

	// ---- parse + type resolution, one adapter run per module and language family ----
	var warnings []string
	if n := len(unreadable); n > 0 {
		warnings = append(warnings, fmt.Sprintf("%d file(s) could not be read and were left out (%s)", n, strings.Join(unreadable[:min(n, 3)], ", ")))
	}
	warned := false
	analyze := func(emit map[string]bool) ([]irLine, error) {
		rootSet := map[string]bool{}
		moduleRoots := map[string]map[string]bool{}
		type group struct{ all, emit []string }
		groups := map[string]map[string]*group{} // family -> module -> files
		present := map[string]int{}
		for _, f := range files {
			if !analyzed(f.Lang) {
				continue
			}
			fam := family(f.Lang)
			present[fam]++
			if groups[fam] == nil {
				groups[fam] = map[string]*group{}
			}
			g := groups[fam][f.Module]
			if g == nil {
				g = &group{}
				groups[fam][f.Module] = g
			}
			g.all = append(g.all, f.Path)
			if emit[f.Path] {
				g.emit = append(g.emit, f.Path)
			}
			if f.Lang == "java" {
				rootSet[f.Root] = true
				if moduleRoots[f.Module] == nil {
					moduleRoots[f.Module] = map[string]bool{}
				}
				moduleRoots[f.Module][f.Root] = true
			}
		}
		for _, fam := range sortedKeys(present) {
			if !tools[fam] && !warned {
				warnings = append(warnings, fmt.Sprintf("%s (%d files)", toolchainHelp[fam], present[fam]))
			}
		}
		warned = true
		var ir []irLine
		for _, fam := range sortedKeys(groups) {
			if !tools[fam] {
				continue
			}
			for _, m := range sortedKeys(groups[fam]) {
				g := groups[fam][m]
				if len(g.emit) == 0 {
					continue
				}
				var lines []irLine
				var err error
				switch fam {
				case "java":
					// shortcut: every other module's sources are visible, the module's own first; read inter-module deps from the build when name clashes show up.
					roots := sortedKeys(moduleRoots[m])
					for _, r := range sortedKeys(rootSet) {
						if !moduleRoots[m][r] {
							roots = append(roots, r)
						}
					}
					lines, err = runAdapter(root, roots, cp, g.emit)
				case "go":
					if !fileExists(filepath.Join(root, m, "go.mod")) {
						warnings = append(warnings, fmt.Sprintf("Go files under %q were not analyzed: they are not inside a module (no go.mod)", m))
						continue
					}
					lines, err = goAdapter(root, filepath.Join(root, m), g.emit)
				case "ts":
					lines, err = tsAdapter(root, filepath.Join(root, m), g.all, g.emit)
				case "py":
					lines, err = pyAdapter(root, g.all, g.emit)
				}
				if err != nil {
					return nil, err
				}
				ir = append(ir, lines...)
			}
		}
		for i := range ir {
			ir[i].clean()
		}
		return ir, nil
	}
	ir, err := analyze(emit)
	if err != nil {
		return nil, err
	}
	// Second pass, only when it can matter: files with unresolved references are revisited if the change added or
	// removed a declaration, because only a new name can resolve what was unresolved.
	if base != 0 && len(holders) > 0 {
		if !structural {
			structural, err = declarationsChanged(db, base, changedFiles, ir)
			if err != nil {
				return nil, err
			}
		}
		extra := map[string]bool{}
		for f := range holders {
			if structural && !emit[f] {
				extra[f] = true
			}
		}
		if len(extra) > 0 {
			more, err := analyze(extra)
			if err != nil {
				return nil, err
			}
			ir = append(ir, more...)
			for f := range extra {
				emit[f] = true
			}
		}
	}
	// javac named packages it could not find: look them up in the local Gradle/Maven caches and start over with a wider classpath
	var missing []string
	for _, l := range ir {
		if l.Kind == "missing" {
			missing = append(missing, l.Text)
		}
	}
	if autoClasspath && round < 6 {
		if added, err := extendClasspath(root, cp, missing); err != nil {
			return nil, err
		} else if added {
			return scanRound(root, db, true, round+1)
		}
	}

	// ---- write the snapshot in one transaction ----
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		if err == nil {
			_, err = tx.Exec(q, args...)
		}
	}
	dirty := gitOut(root, "status", "--porcelain", "--", ".", ":!.ri") != ""
	// A full scan writes a new snapshot. An incremental scan edits the current one in place, inside this transaction:
	// rows of unaffected files stay where they are instead of being copied to a new snapshot, which was most of the
	// cost of a small change in a large repository. Readers see the old state until the commit.
	id := base
	commit, branch, started := gitOut(root, "rev-parse", "HEAD"), gitOut(root, "rev-parse", "--abbrev-ref", "HEAD"), start.UTC().Format(time.RFC3339)
	if base == 0 {
		res, err := tx.Exec(`INSERT INTO scans (commit_sha, branch, dirty, adapter_version, started_at) VALUES (?,?,?,?,?)`, commit, branch, dirty, version, started)
		if err != nil {
			return nil, err
		}
		id, _ = res.LastInsertId()
	} else {
		exec(`UPDATE scans SET commit_sha=?, branch=?, dirty=?, adapter_version=?, started_at=? WHERE id=?`, commit, branch, dirty, version, started, id)
		exec("DELETE FROM modules WHERE scan_id=?", id)
	}
	exec("CREATE TEMP TABLE IF NOT EXISTS keep (path TEXT PRIMARY KEY)")
	exec("CREATE TEMP TABLE IF NOT EXISTS present (path TEXT PRIMARY KEY)")
	exec("DELETE FROM keep")
	exec("DELETE FROM present")
	for _, f := range files {
		// metrics of an unaffected file stay; everything else about the file is as found now
		exec(`INSERT INTO files VALUES (?,?,?,?,?,?,?,NULL) ON CONFLICT (scan_id, path) DO UPDATE
		      SET content_hash=excluded.content_hash, language=excluded.language, module=excluded.module, mtime=excluded.mtime, size=excluded.size`,
			id, f.Path, f.Hash, f.Lang, f.Module, f.Mtime, f.Size)
		exec("INSERT OR IGNORE INTO modules VALUES (?,?,?,?)", id, f.Module, f.Module, "")
		exec("INSERT INTO present VALUES (?)", f.Path)
		if analyzed(f.Lang) && !emit[f.Path] {
			exec("INSERT INTO keep VALUES (?)", f.Path)
		}
	}
	if base != 0 { // drop what is re-read, removed or derived; the rest is already in place
		exec("DELETE FROM files WHERE scan_id=? AND path NOT IN (SELECT path FROM present)", id)
		exec("UPDATE files SET metrics=NULL WHERE scan_id=? AND path NOT IN (SELECT path FROM keep)", id)
		exec("DELETE FROM symbols WHERE scan_id=? AND (file IS NULL OR file NOT IN (SELECT path FROM keep))", id)
		exec(`UPDATE symbols SET module=(SELECT f.module FROM files f WHERE f.scan_id=?1 AND f.path=symbols.file)
		      WHERE scan_id=?1 AND module IS NOT (SELECT f.module FROM files f WHERE f.scan_id=?1 AND f.path=symbols.file)`, id)
		exec("DELETE FROM dependencies WHERE scan_id=? AND (origin<>'adapter' OR file IS NULL OR file NOT IN (SELECT path FROM keep))", id)
		exec("DELETE FROM unresolved_refs WHERE scan_id=? AND (file IS NULL OR file NOT IN (SELECT path FROM keep))", id)
		exec("DELETE FROM api_endpoints WHERE scan_id=?", id)
	}
	for _, l := range ir {
		line := 0
		if len(l.Range) == 4 {
			line = l.Range[0]
		} else {
			l.Range = make([]int, 4)
		}
		switch l.Kind {
		case "symbol":
			fqn := fqnOf(l.ID)
			exec(`INSERT OR IGNORE INTO symbols VALUES (?,?,?,?,?,?,?,?,?,?,?,?,0,?)`, id, l.ID, l.Type, fqn[strings.LastIndex(fqn, ".")+1:], fqn,
				l.Parent, cur[l.File].Module, l.File, l.Range[0], l.Range[1], l.Range[2], l.Range[3], string(l.Attrs))
		case "ref":
			var rule any // NULL unless the adapter inferred the edge with a named rule
			if l.Rule != "" {
				rule = l.Rule
			}
			exec(`INSERT INTO dependencies VALUES (?,?,?,?,?,?,?,?,?,'adapter')`, id, l.From, l.To, l.Via, l.Relation, l.Evidence, rule, l.File, line)
		case "file":
			exec(`UPDATE files SET metrics=json_object('lines',?,'code',?,'comment',?,'blank',?) WHERE scan_id=? AND path=?`, l.Lines, l.Code, l.Comment, l.Blank, id, l.File)
		case "unresolved":
			exec(`INSERT INTO unresolved_refs VALUES (?,?,?,?,?,?)`, id, l.From, l.Text, l.Reason, l.File, line)
		}
	}
	if err != nil {
		return nil, err
	}
	if err = enrich(tx, id); err != nil {
		return nil, err
	}
	// anything referenced but not declared in the repository is an external symbol
	rows, err := tx.Query(`SELECT DISTINCT dst FROM dependencies d WHERE scan_id=? AND NOT EXISTS (SELECT 1 FROM symbols s WHERE s.scan_id=d.scan_id AND s.id=d.dst)`, id)
	if err != nil {
		return nil, err
	}
	var ext []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		ext = append(ext, s)
	}
	rows.Close()
	for _, s := range ext {
		typ, fqn := "class", fqnOf(s)
		if strings.Contains(s, "#") {
			typ = "method"
		}
		// a member of a repository class that has no declaration (enum values(), ...) is implicit, not external
		exec(`INSERT INTO symbols (scan_id, id, type, name, fqn, parent, external, attrs)
		      SELECT ?1, ?2, ?3, ?4, ?5, o.id, o.id IS NULL, CASE WHEN o.id IS NULL THEN '{}' ELSE '{"synthetic":"implicit"}' END
		      FROM (SELECT 1) LEFT JOIN symbols o ON o.scan_id=?1 AND o.external=0 AND o.id=?6`,
			id, s, typ, fqn[strings.LastIndex(fqn, ".")+1:], fqn, strings.SplitN(s, "#", 2)[0])
	}
	if base != 0 {
		// shortcut: an incremental scan sees only the files it re-read, so the previous list is carried along and a
		// library that has since been installed stays listed until `scan --full`; recompute from imports if that confuses.
		var prev string
		var p ScanResult
		tx.QueryRow("SELECT COALESCE(stats_json,'{}') FROM scans WHERE id=?", base).Scan(&prev)
		json.Unmarshal([]byte(prev), &p)
		missing = append(missing, p.MissingPackages...)
	}
	sort.Strings(missing)
	missing = slices.Compact(missing)
	r := &ScanResult{MissingPackages: missing, Warnings: warnings, Classpath: len(cp), ScanID: id, Files: len(files), Parsed: len(emit), Full: base == 0, DurationMs: time.Since(start).Milliseconds()}
	stats, _ := json.Marshal(r)
	exec("UPDATE scans SET finished_at=?, stats_json=? WHERE id=?", time.Now().UTC().Format(time.RFC3339), string(stats), id)
	// Only the current snapshot is kept: nothing reads older ones, and each costs as much disk as the index itself.
	// shortcut: no history, keep one snapshot per compared revision when a snapshot `diff` is built.
	if base == 0 {
		for _, t := range []string{"files", "modules", "symbols", "dependencies", "unresolved_refs", "api_endpoints"} {
			exec("DELETE FROM "+t+" WHERE scan_id<?", id)
		}
		exec("DELETE FROM scans WHERE id<?", id)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if base == 0 { // hand back the space of dropped snapshots once more than half of the file is unused
		var pages, free int
		db.QueryRow("PRAGMA page_count").Scan(&pages)
		db.QueryRow("PRAGMA freelist_count").Scan(&free)
		if free > pages/2 {
			db.Exec("VACUUM")
		}
	}
	return r, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// declarationsChanged reports whether the changed files declare a different set of symbols than in the base scan.
func declarationsChanged(db *sql.DB, base int64, files []string, ir []irLine) (bool, error) {
	now := map[string]map[string]bool{}
	for _, l := range ir {
		if l.Kind == "symbol" {
			if now[l.File] == nil {
				now[l.File] = map[string]bool{}
			}
			now[l.File][l.ID] = true
		}
	}
	for _, f := range files {
		rows, err := db.Query("SELECT id FROM symbols WHERE scan_id=? AND file=?", base, f)
		if err != nil {
			return false, err
		}
		n := 0
		for rows.Next() {
			var id string
			rows.Scan(&id)
			n++
			if !now[f][id] {
				rows.Close()
				return true, nil
			}
		}
		rows.Close()
		if n != len(now[f]) {
			return true, nil
		}
	}
	return false, nil
}

// scanVersion identifies what produced a scan: the adapters, the Java classpath, and which toolchains were available.
// A scan made under a different version is redone in full, and is reported as stale until then.
func scanVersion(root string) (version string, cp []string, tools map[string]bool) {
	if autoClasspath { // here, not only in scan: a query must also see that the index was made with an outdated classpath
		resetStaleAutoClasspath(root)
	}
	cp, cpKey := classpath(root)
	tools = toolchains()
	version = hashOf(adapterSrc)[:12] + ":" + cpKey + ":" + goAdapterVersion + ":" + hashOf(append(append(append([]byte{}, tsAdapterSrc...), tsCompilerArchive...), pyAdapterSrc...))[:12]
	for _, k := range sortedKeys(tools) { // a toolchain appearing later must trigger a rescan of what it could not do before
		version += fmt.Sprintf(":%s=%t", k, tools[k])
	}
	return
}

// gitSafe starts a git command line for a repository that may not be trusted: its own config can name programs for
// git to run (a file-system monitor, hooks), and reading a repository must never run them.
func gitSafe(root string) []string {
	return []string{"-C", root, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}
}
