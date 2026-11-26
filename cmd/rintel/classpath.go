package main

import (
	"archive/zip"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// autoClasspath lets scans add jars from the local Gradle/Maven caches for packages javac could not find.
// Nothing is downloaded and no build tool runs; tests turn it off to stay machine-independent.
var autoClasspath = os.Getenv("RINTEL_NO_AUTO_CLASSPATH") == ""

const maxJarsPerPackage = 8

// classpath = .ri/classpath (written by the user; wins on conflicts because it comes first) + .ri/cache/classpath.auto.
// Entries are jars or directories searched recursively, separated by newlines or the OS path separator,
// so `mvn dependency:build-classpath -Dmdep.outputFile=.ri/classpath` output works as is.
func classpath(root string) (jars []string, key string) {
	seen := map[string]bool{}
	for _, file := range []string{filepath.Join(root, ".ri", "classpath"), filepath.Join(root, ".ri", "cache", "classpath.auto")} {
		b, _ := os.ReadFile(file)
		var found []string
		for _, line := range strings.FieldsFunc(string(b), func(r rune) bool { return r == '\n' || r == os.PathListSeparator }) {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "~/") {
				home, _ := os.UserHomeDir()
				line = filepath.Join(home, line[2:])
			}
			filepath.WalkDir(line, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jar") && !seen[p] {
					seen[p] = true
					found = append(found, p)
				}
				return nil
			})
		}
		sort.Strings(found)
		jars = append(jars, found...)
	}
	return jars, hashOf([]byte(strings.Join(jars, "\n")))[:12]
}

// extendClasspath appends cached jars that provide the missing packages to .ri/cache/classpath.auto.
func extendClasspath(root string, have, missing []string) (bool, error) {
	if len(missing) == 0 {
		return false, nil
	}
	idx, fresh, err := loadJarIndex(false)
	if err != nil {
		return false, err
	}
	for _, pkg := range missing {
		if _, ok := idx[pkg]; !ok && !fresh { // maybe the project was built since the index was made
			if idx, _, err = loadJarIndex(true); err != nil {
				return false, err
			}
			break
		}
	}
	seen := map[string]bool{}
	for _, j := range have {
		seen[j] = true
	}
	wanted := wantedVersions(root)
	artifacts := map[string]bool{} // one version of a library at a time: two would shadow each other class by class
	for _, j := range have {
		if key, _ := jarCoord(j); key != "" {
			artifacts[key] = true
		}
	}
	var add []string
	for _, pkg := range missing {
		// per artifact: the version the build declares if it is cached, else the newest one that has the package
		best := map[string]string{}
		for _, j := range idx[pkg] {
			key, v := jarCoord(j)
			if artifacts[key] {
				continue
			}
			want := wantedVersion(wanted, key)
			_, cur := jarCoord(best[key])
			if best[key] == "" || v == want || (cur != want && versionLess(cur, v)) {
				best[key] = j
			}
		}
		for i, key := range sortedKeys(best) {
			if i == maxJarsPerPackage { // usually shaded copies of a popular library
				break
			}
			if j := best[key]; !seen[j] {
				seen[j], artifacts[key] = true, true
				add = append(add, j)
			}
		}
	}
	if len(add) == 0 {
		return false, nil
	}
	file := filepath.Join(root, ".ri", "cache", "classpath.auto")
	if _, err := os.Stat(file); err != nil {
		add = append([]string{autoHeader(wanted)}, add...)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.WriteString(strings.Join(add, "\n") + "\n")
	return true, err
}

// Bump the rule number whenever the selection below changes, or old files are kept.
// autoHeader is the first line of classpath.auto: how the jars in it were chosen. When the selection rules or the
// versions the build declares change, the file no longer matches and is rebuilt from nothing.
func autoHeader(wanted map[string]string) string {
	b, _ := json.Marshal(wanted) // map keys are written sorted
	return "# chosen by rintel, rules 3, versions " + hashOf(b)[:12]
}

func resetStaleAutoClasspath(root string) {
	file := filepath.Join(root, ".ri", "cache", "classpath.auto")
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	wanted := wantedVersions(root)
	stale := !strings.HasPrefix(string(b), autoHeader(wanted)+"\n")
	for _, jar := range strings.Split(string(b), "\n") {
		// a stand-in was used because the declared version was not cached; the project has been built since, and it is
		key, v := jarCoord(jar)
		if key == "" || stale {
			continue
		}
		if want := wantedVersion(wanted, key); want != "" && want != v {
			gradle, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(jar))), want, "*", "*-"+want+".jar"))
			maven, _ := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(jar)), want, "*-"+want+".jar"))
			if stale = len(gradle)+len(maven) > 0; stale {
				// the jar index predates that jar: without a rebuild the same stand-in would be chosen again, on every scan
				if cache, err := os.UserCacheDir(); err == nil {
					os.Remove(filepath.Join(cache, "rintel", "jarindex2.json"))
				}
			}
		}
	}
	if stale {
		os.Remove(file)
	}
}

// jarCoord reads "group:artifact" and the version from a jar's place in a Gradle or Maven cache.
func jarCoord(jar string) (key, version string) {
	for _, root := range jarRoots() {
		rel, err := filepath.Rel(root, jar)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for _, back := range []int{2, 3} { // Maven: group/…/artifact/version/file, Gradle: group/artifact/version/hash/file
			if n := len(parts) - back; n >= 2 && parts[len(parts)-1] == parts[n-1]+"-"+parts[n]+".jar" {
				return strings.Join(parts[:n-1], ".") + ":" + parts[n-1], parts[n]
			}
		}
	}
	return "", ""
}

// wantedVersion looks up an artifact, then its group and the groups above it: a BOM named com.fasterxml.jackson
// manages com.fasterxml.jackson.core, and the closest group wins (org.springframework.security before org.springframework).
func wantedVersion(wanted map[string]string, key string) string {
	if v := wanted[key]; v != "" {
		return v
	}
	for g := key[:strings.Index(key, ":")]; g != ""; g = g[:max(strings.LastIndex(g, "."), 0)] {
		if v := wanted[g+":*"]; v != "" {
			return v
		}
	}
	return ""
}

var (
	// gradle/libs.versions.toml: `name = "1.2"` under [versions]; libraries as module = "g:a" or group/name, with version or version.ref
	tomlVersion    = regexp.MustCompile(`(?m)^\s*([\w.-]+)\s*=\s*"([^"]+)"\s*$`)
	tomlEntry      = regexp.MustCompile(`(?:module\s*=\s*"([^":]+):([^":]+)"|group\s*=\s*"([^"]+)"\s*,\s*name\s*=\s*"([^"]+)"|id\s*=\s*"(org\.springframework\.boot)")[^}\n]*?version(?:\.ref\s*=\s*"([^"]+)"|\s*=\s*"([^"]+)")`)
	gradleVarCoord = regexp.MustCompile(`['"]([\w.-]+):([\w.-]+):\$\{?([\w.]+)\}?['"]`)
	// jacksonVersion = '2.15.0' | def x = "1.2" | set("x", "1.2") | x=1.2 in gradle.properties
	gradleVar   = regexp.MustCompile(`(?m)\b([A-Za-z_][\w.]*)\s*=\s*['"]?(\d[\w.-]*)|set\(\s*"(\w+)"\s*,\s*"(\d[^"]*)"\s*\)`)
	gradleCoord = regexp.MustCompile(`['"]([\w.-]+):([\w.-]+):(\d[\w.-]*)['"]`)
	bootPlugin  = regexp.MustCompile(`org\.springframework\.boot['")]+\s*version\s*\(?\s*['"]([^'"]+)|spring-boot-gradle-plugin:(\d[\w.-]*)|spring-boot-starter-parent</artifactId>\s*<version>([^<$]+)<`)
	pomDep      = regexp.MustCompile(`<groupId>([^<$]+)</groupId>\s*<artifactId>([^<$]+)</artifactId>\s*<version>([^<]+)</version>(\s*<type>pom</type>\s*<scope>import</scope>)?`)
	pomProp     = regexp.MustCompile(`<([\w.-]+)>([^<]+)</([\w.-]+)>`)
)

// wantedVersions reads which library versions the build asks for: coordinates written in the build files, and what
// the Spring Boot version manages (from its BOM, if the build ever downloaded it). Keys are "group:artifact", or
// "group:*" for a whole group managed by an imported BOM. Nothing is run or downloaded.
// shortcut: text matching, not the build tool's own resolution: version ranges, conflict resolution, constraints and
// what BOMs other than Spring Boot's manage outside their own group are not followed. Run the build tool's classpath
// report into .ri/classpath when a project needs exact versions.
func wantedVersions(root string) map[string]string {
	wanted := map[string]string{}
	var files []string
	vars := map[string]string{} // Gradle variables holding versions: ext, def/val, gradle.properties
	walk(root, func(rel string, info fs.FileInfo) error {
		switch name := filepath.Base(rel); name {
		case "build.gradle", "build.gradle.kts", "pom.xml":
			files = append(files, filepath.Join(root, rel))
		case "gradle.properties":
			b, _ := os.ReadFile(filepath.Join(root, rel))
			for _, m := range gradleVar.FindAllStringSubmatch(string(b), -1) {
				vars[m[1]+m[3]] = m[2] + m[4]
			}
		}
		return nil
	})
	if b, err := os.ReadFile(filepath.Join(root, "gradle.properties")); err == nil { // not a build file to the scanner, so read here
		for _, m := range gradleVar.FindAllStringSubmatch(string(b), -1) {
			vars[m[1]+m[3]] = m[2] + m[4]
		}
	}
	for _, f := range files {
		if !strings.HasSuffix(f, "pom.xml") {
			b, _ := os.ReadFile(f)
			for _, m := range gradleVar.FindAllStringSubmatch(string(b), -1) {
				vars[m[1]+m[3]] = m[2] + m[4]
			}
		}
	}
	boot := ""
	declare := func(group, artifact, v string) {
		if v == "" || strings.Contains(v, "$") {
			return
		}
		wanted[group+":"+artifact] = v
		if strings.HasSuffix(artifact, "-bom") || strings.HasSuffix(artifact, "-dependencies") { // a BOM: its group moves together
			wanted[group+":*"] = v
		}
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		text := string(b)
		if m := bootPlugin.FindStringSubmatch(text); m != nil && boot == "" {
			boot = strings.TrimSpace(m[1] + m[2] + m[3])
		}
		for _, m := range gradleCoord.FindAllStringSubmatch(text, -1) {
			declare(m[1], m[2], m[3])
		}
		for _, m := range gradleVarCoord.FindAllStringSubmatch(text, -1) { // "group:artifact:$jacksonVersion"
			declare(m[1], m[2], vars[m[3]])
		}
		if strings.HasSuffix(f, "pom.xml") {
			props := map[string]string{}
			for _, m := range pomProp.FindAllStringSubmatch(text, -1) {
				if m[1] == m[3] {
					props[m[1]] = m[2]
				}
			}
			for _, m := range pomDep.FindAllStringSubmatch(text, -1) {
				v := m[3]
				if strings.HasPrefix(v, "${") {
					v = props[strings.Trim(v, "${}")]
				}
				declare(m[1], m[2], v)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "gradle", "libs.versions.toml")); err == nil {
		text := string(b)
		refs := map[string]string{}
		if i := strings.Index(text, "[versions]"); i >= 0 {
			section := text[i+len("[versions]"):]
			if j := strings.Index(section, "\n["); j >= 0 {
				section = section[:j]
			}
			for _, m := range tomlVersion.FindAllStringSubmatch(section, -1) {
				refs[m[1]] = m[2]
			}
		}
		for _, m := range gradleCoord.FindAllStringSubmatch(text, -1) {
			declare(m[1], m[2], m[3])
		}
		for _, m := range tomlEntry.FindAllStringSubmatch(text, -1) {
			v := m[7]
			if m[6] != "" {
				v = refs[m[6]]
			}
			switch {
			case m[5] != "":
				if boot == "" {
					boot = v
				}
			case m[1] != "":
				declare(m[1], m[2], v)
			default:
				declare(m[3], m[4], v)
			}
		}
	}
	if boot == "" {
		return wanted
	}
	set := func(key, v string) {
		if _, declared := wanted[key]; !declared { // what the build file says wins over what the BOM manages
			wanted[key] = v
		}
	}
	set("org.springframework.boot:*", boot)
	var boms []string
	for _, r := range jarRoots() {
		a, _ := filepath.Glob(filepath.Join(r, "org.springframework.boot", "spring-boot-dependencies", boot, "*", "*.pom"))
		b, _ := filepath.Glob(filepath.Join(r, "org", "springframework", "boot", "spring-boot-dependencies", boot, "*.pom"))
		boms = append(append(boms, a...), b...)
	}
	if len(boms) == 0 {
		return wanted
	}
	b, _ := os.ReadFile(boms[0])
	props := map[string]string{}
	for _, m := range pomProp.FindAllStringSubmatch(string(b), -1) {
		if m[1] == m[3] && strings.HasSuffix(m[1], ".version") {
			props[m[1]] = m[2]
		}
	}
	for _, m := range pomDep.FindAllStringSubmatch(string(b), -1) {
		v := m[3]
		if strings.HasPrefix(v, "${") {
			v = props[strings.Trim(v, "${}")]
		}
		if v == "" || strings.Contains(v, "$") {
			continue
		}
		if m[4] != "" { // an imported BOM (spring-framework-bom, jackson-bom…): its group moves together
			set(m[1]+":*", v)
		} else {
			set(m[1]+":"+m[2], v)
		}
	}
	return wanted
}

var jarRoots = func() []string {
	home, _ := os.UserHomeDir()
	return []string{filepath.Join(home, ".gradle", "caches", "modules-2", "files-2.1"), filepath.Join(home, ".m2", "repository")}
}

// loadJarIndex maps a Java package to the cached jars containing it, every version: which one is used is decided
// per repository, from the versions its build declares.
func loadJarIndex(rebuild bool) (idx map[string][]string, fresh bool, err error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, false, err
	}
	file := filepath.Join(cache, "rintel", "jarindex2.json")
	if st, err := os.Stat(file); err == nil && !rebuild && time.Since(st.ModTime()) < 24*time.Hour {
		if b, err := os.ReadFile(file); err == nil && json.Unmarshal(b, &idx) == nil {
			return idx, time.Since(st.ModTime()) < time.Minute, nil
		}
	}
	idx = buildJarIndex(jarRoots())
	if b, err := json.Marshal(idx); err == nil {
		os.MkdirAll(filepath.Dir(file), 0o755)
		os.WriteFile(file, b, 0o644)
	}
	return idx, true, nil
}

func buildJarIndex(roots []string) map[string][]string {
	var jars []string
	for _, root := range roots {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jar") {
				return nil
			}
			// Maven: group/path/artifact/version/artifact-version.jar, Gradle: group/artifact/version/hash/artifact-version.jar
			for _, vdir := range []string{filepath.Dir(p), filepath.Dir(filepath.Dir(p))} {
				if d.Name() == filepath.Base(filepath.Dir(vdir))+"-"+filepath.Base(vdir)+".jar" { // also rejects sources, javadoc and other classifiers
					jars = append(jars, p)
					break
				}
			}
			return nil
		})
	}
	idx := map[string][]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	work := make(chan string)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for jar := range work {
				pkgs := jarPackages(jar)
				mu.Lock()
				for _, pkg := range pkgs {
					idx[pkg] = append(idx[pkg], jar)
				}
				mu.Unlock()
			}
		}()
	}
	for _, p := range jars {
		work <- p
	}
	close(work)
	wg.Wait()
	for _, jars := range idx {
		sort.Strings(jars)
	}
	return idx
}

func jarPackages(jar string) []string {
	z, err := zip.OpenReader(jar)
	if err != nil {
		return nil
	}
	defer z.Close()
	set := map[string]bool{}
	for _, f := range z.File {
		if strings.HasSuffix(f.Name, ".class") && !strings.HasPrefix(f.Name, "META-INF/") && strings.Contains(f.Name, "/") {
			set[strings.ReplaceAll(path.Dir(f.Name), "/", ".")] = true
		}
	}
	return sortedKeys(set)
}

var versionPart = regexp.MustCompile(`\d+|[A-Za-z]+`)

// versionLess orders 6.2.1 < 6.10.0 and any pre-release (M1, RC2, SNAPSHOT, alpha) below its release.
func versionLess(a, b string) bool {
	pa, pb := versionPart.FindAllString(a, -1), versionPart.FindAllString(b, -1)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		nx, ex := strconv.Atoi(x)
		ny, ey := strconv.Atoi(y)
		switch {
		case x == y:
			continue
		case ex == nil && ey == nil:
			return nx < ny
		case ex == nil: // number vs qualifier or nothing
			return false
		case ey == nil:
			return true
		default: // qualifier vs qualifier/nothing: "RELEASE", "Final", "GA" and no qualifier are releases
			rel := func(s string) bool {
				s = strings.ToUpper(s)
				return s == "" || s == "RELEASE" || s == "FINAL" || s == "GA"
			}
			if rel(x) != rel(y) {
				return rel(y)
			}
			return x < y
		}
	}
	return false
}
