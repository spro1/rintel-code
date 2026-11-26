package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Known vulnerabilities in the dependencies the repository pins, looked up at osv.dev. This is the one part of rintel
// that uses the network, so it only runs when asked for (check --vulns, or the MCP parameter).
//
// shortcut: only exact versions written in go.mod, package-lock.json (v2/v3), requirements*.txt, poetry.lock, uv.lock
// and the artifacts Maven/Gradle build files name are checked; yarn and pnpm locks, version ranges and transitive
// Java dependencies are not. A hit means the version is affected, not that the vulnerable code is reached (govulncheck answers that for Go).

var osvURL = "https://api.osv.dev"

type pinned struct {
	eco, name, version, file string
	line                     int
}

type DepCheck struct {
	Packages int    `json:"packages" jsonschema:"dependencies with an exact version that were looked up"`
	Error    string `json:"error,omitempty" jsonschema:"set when the lookup could not run, e.g. no network: the dependency findings are then missing, not zero"`
}

// repoFiles lists the files git tracks (an ignored .env is not a leak), or every file when this is not a git repository.
func repoFiles(root string) []string {
	var out []string
	keep := func(rel string) {
		if !strings.Contains("/"+rel, "/node_modules/") && !strings.Contains("/"+rel, "/vendor/") && !strings.Contains("/"+rel, "/testdata/") {
			out = append(out, rel)
		}
	}
	if s := gitOut(root, "ls-files"); s != "" {
		for _, rel := range strings.Split(s, "\n") {
			keep(rel)
		}
		return out
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); p != root && (strings.HasPrefix(n, ".") || n == "target" || n == "build" || n == "dist" || n == "venv") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".env") { // without git nobody can tell a committed .env from a local one
			rel, _ := filepath.Rel(root, p)
			keep(filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

var (
	goRequire = regexp.MustCompile(`^\s*(?:require\s+)?([^\s()]+)\s+v(\d[^\s]*)`)
	pyPin     = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^\]]*\])?\s*==\s*([^\s;#\\]+)`)
	gradleDep = regexp.MustCompile(`['"]([\w.-]+):([\w.-]+)(?::[^'"\s]*)?['"]`)
	pomCoord  = regexp.MustCompile(`<groupId>([^<$]+)</groupId>\s*<artifactId>([^<$]+)</artifactId>`)
	lockPkg   = regexp.MustCompile(`(?m)^\[\[package\]\]\r?\nname = "([^"]+)"\r?\nversion = "([^"]+)"`)
)

// pinnedDeps reads the exact dependency versions the repository declares. Private names stay local: Go modules
// matching GOPRIVATE and npm packages that did not come from the public registry are left out.
func pinnedDeps(root string, cfg *Config) []pinned {
	var out []pinned
	private := strings.Split(os.Getenv("GOPRIVATE"), ",")
	for _, rel := range repoFiles(root) {
		name := path.Base(rel)
		lock := name == "poetry.lock" || name == "uv.lock"
		req := strings.HasPrefix(name, "requirements") && strings.HasSuffix(name, ".txt")
		if (name != "go.mod" && name != "package-lock.json" && !lock && !req) || cfg.excluded(rel) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || len(b) > 32<<20 {
			continue
		}
		switch {
		case name == "go.mod":
		lines:
			for i, l := range strings.Split(string(b), "\n") {
				m := goRequire.FindStringSubmatch(l)
				if m == nil || strings.HasPrefix(strings.TrimSpace(l), "//") || strings.HasPrefix(strings.TrimSpace(l), "go ") {
					continue
				}
				for _, p := range private {
					if ok, _ := path.Match(p, m[1]); p != "" && (ok || strings.HasPrefix(m[1], strings.TrimSuffix(p, "/")+"/")) {
						continue lines
					}
				}
				out = append(out, pinned{"Go", m[1], strings.TrimSuffix(m[2], "+incompatible"), rel, i + 1})
			}
		case name == "package-lock.json":
			var lf struct {
				Packages map[string]struct {
					Version, Resolved string
				} `json:"packages"`
			}
			json.Unmarshal(b, &lf)
			for _, k := range sortedKeys(lf.Packages) {
				p := lf.Packages[k]
				if i := strings.LastIndex(k, "node_modules/"); i >= 0 && p.Version != "" && strings.HasPrefix(p.Resolved, "https://registry.npmjs.org/") {
					out = append(out, pinned{"npm", k[i+len("node_modules/"):], p.Version, rel, 0})
				}
			}
		case lock:
			for _, m := range lockPkg.FindAllStringSubmatch(string(b), -1) {
				out = append(out, pinned{"PyPI", m[1], m[2], rel, 0})
			}
		default:
			for i, l := range strings.Split(string(b), "\n") {
				if m := pyPin.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
					out = append(out, pinned{"PyPI", m[1], m[2], rel, i + 1})
				}
			}
		}
	}
	// Java: the artifacts the build files name, at the version written there or managed by the Spring Boot BOM.
	// Everything else a BOM lists is not a dependency of this repository.
	wanted := wantedVersions(root)
	for _, rel := range repoFiles(root) {
		name := path.Base(rel)
		if (name != "pom.xml" && name != "build.gradle" && name != "build.gradle.kts") || cfg.excluded(rel) {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		coord := gradleDep
		if name == "pom.xml" {
			coord = pomCoord
		}
		for _, m := range coord.FindAllSubmatchIndex(b, -1) {
			key := string(b[m[2]:m[3]]) + ":" + string(b[m[4]:m[5]])
			if v := wantedVersion(wanted, key); v != "" {
				out = append(out, pinned{"Maven", key, v, rel, 1 + bytes.Count(b[:m[0]], []byte("\n"))})
			}
		}
	}
	seen := map[string]bool{}
	uniq := out[:0]
	for _, p := range out {
		if k := p.eco + "\x00" + p.name + "\x00" + p.version; !seen[k] {
			seen[k] = true
			uniq = append(uniq, p)
		}
	}
	return uniq
}

type osvVuln struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary"`
	Aliases  []string `json:"aliases"`
	Affected []struct {
		Package struct{ Name, Ecosystem string } `json:"package"`
		Ranges  []struct {
			Events []struct{ Fixed string } `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
}

func osvCall(client *http.Client, method, url string, body, into any) error {
	var rd *bytes.Reader = bytes.NewReader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "rintel/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("osv.dev answered %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// vulnFindings asks osv.dev about every pinned dependency: one finding per package version and vulnerability.
func vulnFindings(deps []pinned) ([]Finding, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	hits := map[int][]string{} // index in deps -> vulnerability ids
	for start := 0; start < len(deps); start += 500 {
		chunk := deps[start:min(start+500, len(deps))]
		type query struct {
			Package struct {
				Name      string `json:"name"`
				Ecosystem string `json:"ecosystem"`
			} `json:"package"`
			Version string `json:"version"`
		}
		qs := make([]query, len(chunk))
		for i, d := range chunk {
			qs[i].Package.Name, qs[i].Package.Ecosystem, qs[i].Version = d.name, d.eco, d.version
		}
		var res struct {
			Results []struct {
				Vulns []struct{ ID string } `json:"vulns"`
			} `json:"results"`
		}
		if err := osvCall(client, "POST", osvURL+"/v1/querybatch", map[string]any{"queries": qs}, &res); err != nil {
			return nil, err
		}
		for i, r := range res.Results {
			for _, v := range r.Vulns {
				hits[start+i] = append(hits[start+i], v.ID)
			}
		}
	}
	detail := map[string]*osvVuln{}
	for _, ids := range hits {
		for _, id := range ids {
			detail[id] = nil
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	slots := make(chan struct{}, 8)
	for id := range detail {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			v := &osvVuln{}
			err := osvCall(client, "GET", osvURL+"/v1/vulns/"+id, nil, v)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			detail[id] = v
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	var out []Finding
	for i, d := range deps {
		ids := hits[i]
		sort.Strings(ids)
		told := map[string]bool{} // one database's entry is enough: GO-… and GHSA-… often describe the same flaw
		for _, id := range ids {
			v := detail[id]
			if told[id] {
				continue
			}
			told[id] = true
			name := id
			for _, a := range v.Aliases {
				told[a] = true
				if strings.HasPrefix(a, "CVE-") {
					name = id + " / " + a
				}
			}
			fixed := ""
			for _, a := range v.Affected {
				if a.Package.Name != d.name || a.Package.Ecosystem != d.eco {
					continue
				}
				for _, r := range a.Ranges {
					for _, e := range r.Events { // the lowest fixed version above the one in use
						if e.Fixed != "" && versionLess(d.version, e.Fixed) && (fixed == "" || versionLess(e.Fixed, fixed)) {
							fixed = e.Fixed
						}
					}
				}
			}
			msg := fmt.Sprintf("%s %s: %s", d.name, d.version, name)
			if v.Summary != "" {
				msg += " " + v.Summary
			}
			if fixed != "" {
				msg += " (fixed in " + fixed + ")"
			}
			out = append(out, Finding{Rule: "vulnerable-dependency", Severity: "warning", File: d.file, Line: d.line, Message: msg, CWE: "CWE-1395"})
		}
	}
	return out, nil
}

// addVulns runs the dependency lookup and puts its rule first in the result. A failed lookup is recorded, not hidden:
// the caller decides whether that fails the run.
func (q *Q) addVulns(c *CheckResult) {
	cfg := loadConfig(q.root)
	for _, id := range cfg.Disable {
		if id == "vulnerable-dependency" {
			return
		}
	}
	deps := pinnedDeps(q.root, cfg)
	c.Dependencies = &DepCheck{Packages: len(deps)}
	fs, err := vulnFindings(deps)
	if err != nil {
		c.Dependencies.Error = err.Error()
		return
	}
	c.Rules = append([]RuleSummary{{ID: "vulnerable-dependency", Severity: "warning", Title: ruleTitles["vulnerable-dependency"], Category: "security",
		CWE: "CWE-1395", Fix: ruleFix["vulnerable-dependency"][0], Count: len(fs)}}, c.Rules...)
	c.Findings = append(fs, c.Findings...)
	c.Warnings += len(fs)
}
