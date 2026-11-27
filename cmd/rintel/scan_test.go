package main

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func copyDemo(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			if info.Name() == ".ri" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func mustScan(t *testing.T, dir string, full bool) *sql.DB {
	t.Helper()
	db, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := scan(dir, db, full); err != nil {
		t.Fatal(err)
	}
	return db
}

// dump is every analysis row of the latest scan, without scan IDs.
func dump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id, type, name, fqn, parent, module, file, l1, c1, l2, c2, external, attrs FROM symbols WHERE scan_id=? ORDER BY id`,
		`SELECT src, dst, via, relation, evidence_level, rule, file, line, origin FROM dependencies WHERE scan_id=? ORDER BY 1,2,3,4,5,6,7,8,9`,
		`SELECT src, text, reason, file, line FROM unresolved_refs WHERE scan_id=? ORDER BY 1,2,3,4,5`,
		`SELECT method, path, handler, evidence_level FROM api_endpoints WHERE scan_id=? ORDER BY 1,2,3`,
		`SELECT path, metrics FROM files WHERE scan_id=? ORDER BY 1`,
	} {
		rows, err := db.Query(q, latestScan(db))
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			fmt.Fprintln(&b, vals...)
		}
		rows.Close()
	}
	return b.String()
}

func findEdge(edges []Edge, from, to string) *Edge {
	for i := range edges {
		if strings.Contains(edges[i].From, from) && strings.Contains(edges[i].To, to) {
			return &edges[i]
		}
	}
	return nil
}

func TestScan(t *testing.T) {
	if _, err := exec.LookPath("javac"); err != nil {
		t.Skip("javac not found")
	}
	autoClasspath = false
	dir := copyDemo(t, "testdata/demo")
	db := mustScan(t, dir, false)
	q := &Q{dir, db}

	api, err := q.APISpec("/api", 0, "")
	if err != nil || len(api.Endpoints) != 2 || api.Endpoints[0].Method != "POST" || api.Endpoints[0].Path != "/api/users" ||
		api.Endpoints[1].Path != "/api/users/{email}" {
		t.Fatalf("endpoints: %+v %v", api, err)
	}
	if api.Scan.Stale {
		t.Fatal("fresh scan reported stale")
	}

	in, err := q.Neighbors("SmtpMailSender.send", "in", 2, "calls", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if e := findEdge(in.Edges, "UserService#createUser", "SmtpMailSender#send"); e == nil || e.Evidence != "heuristic" || e.Rule != "spring-di-multiple-candidates" {
		t.Fatalf("two MailSender beans must give a heuristic edge: %+v", in.Edges)
	}
	if e := findEdge(in.Edges, "UserController#create", "UserService#createUser"); e == nil || e.Evidence != "resolved" || e.PathEvidence != "heuristic" {
		t.Fatalf("evidence must degrade along the path: %+v", in.Edges)
	}

	out, err := q.Neighbors("UserService.createUser", "out", 1, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if e := findEdge(out.Edges, "createUser", "User#getEmail()"); e == nil || e.Evidence != "resolved" {
		t.Fatalf("Lombok getter not resolved: %+v", out.Edges)
	}
	if out.UnresolvedCount != 1 { // userRepository.save(): JpaRepository is not on the classpath
		t.Fatalf("unresolved_count = %d, want 1", out.UnresolvedCount)
	}

	// Lombok builder chain, record accessor and constructor: all inside OrderServiceImpl.place, none may stay unresolved
	place, err := q.Neighbors("OrderServiceImpl.place", "out", 1, "calls", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"Order#builder()", "Order.OrderBuilder#total(Money)", "Order.OrderBuilder#build()", "Order#getTotal()", "Money#amount()", "Money#<init>(long,String)"} {
		if e := findEdge(place.Edges, "place", to); e == nil || e.Evidence != "resolved" {
			t.Errorf("missing resolved edge to %s: %+v", to, place.Edges)
		}
	}
	// called through the interface from a non-bean: only the class hierarchy links it to the implementation
	if r, _ := q.Neighbors("OrderServiceImpl.place", "in", 1, "calls", 0, ""); len(r.Edges) != 1 || r.Edges[0].Evidence != "heuristic" || r.Edges[0].Rule != "class-hierarchy" {
		t.Errorf("class-hierarchy dispatch: %+v", r.Edges)
	}
	// OrderFacade has no stereotype: it is a bean only through the @Bean factory method
	if r, _ := q.Neighbors("com.acme.order.OrderFacade", "in", 1, "injects", 0, ""); findEdge(r.Edges, "OrderServiceImpl", "OrderFacade") == nil {
		t.Errorf("@Bean factory injection: %+v", r.Edges)
	}
	// impact of changing a type: everything that mentions it
	if r, _ := q.Neighbors("com.acme.order.Money", "in", 1, "uses", 0, ""); findEdge(r.Edges, "OrderServiceImpl#place", "Money") == nil || findEdge(r.Edges, "order.Order", "Money") == nil {
		t.Errorf("uses edges: %+v", r.Edges)
	}
	// name lookup is case-sensitive: the class, not OrderConfig.orderFacade()
	if r, _ := q.Neighbors("OrderFacade", "in", 1, "", 0, ""); len(r.Roots) != 1 {
		t.Errorf("roots: %v", r.Roots)
	}

	// every form the CLI prints must be accepted back as input
	for _, ref := range []string{"UserService.createUser", "UserService.createUser(String)", "UserService#createUser(String)",
		"java:com.acme.user.UserService#createUser(String)", "com.acme.user.UserService.createUser", "createUser"} {
		if ids, err := q.resolve(latestScan(db), ref); err != nil || len(ids) != 1 || ids[0] != "java:com.acme.user.UserService#createUser(String)" {
			t.Errorf("resolve(%q) = %v, %v", ref, ids, err)
		}
	}
	if ids, err := q.resolve(latestScan(db), "new OrderFacade(int)"); err != nil || len(ids) != 1 {
		t.Errorf("constructor display form: %v %v", ids, err)
	}
	// a typo gets a suggestion instead of a bare "not found"
	var nf *notFoundError
	if _, err := q.resolve(latestScan(db), "creatUser"); !errors.As(err, &nf) || len(nf.Hints) != 1 || !strings.HasSuffix(nf.Hints[0], "#createUser(String)") {
		t.Errorf("typo hint: %v", err)
	}
	// callers of a class = callers of its members; a class itself is never called
	if r, err := q.Neighbors("com.acme.user.UserService", "in", 1, "calls", 0, ""); err != nil || len(r.ExpandedFrom) != 1 ||
		findEdge(r.Edges, "UserController#create", "UserService#createUser") == nil {
		t.Errorf("class callers: %+v %v", r, err)
	}
	// callees come back in source order
	if r, _ := q.Neighbors("UserService.createUser", "out", 1, "calls", 0, ""); len(r.Edges) < 2 || r.Edges[0].At.StartLine > r.Edges[len(r.Edges)-1].At.StartLine {
		t.Errorf("callee order: %+v", r.Edges)
	}

	inj, _ := q.Neighbors("com.acme.user.UserController", "out", 1, "injects", 0, "")
	if e := findEdge(inj.Edges, "UserController", "UserService"); e == nil || e.Evidence != "resolved" {
		t.Fatalf("constructor injection: %+v", inj.Edges)
	}
}

// The incremental rule set is only trusted because of this: after every edit, an incremental scan must equal a full scan.
func TestIncrementalEqualsFull(t *testing.T) {
	if _, err := exec.LookPath("javac"); err != nil {
		t.Skip("javac not found")
	}
	autoClasspath = false
	dir := copyDemo(t, "testdata/demo")
	db := mustScan(t, dir, false)
	src := filepath.Join(dir, "src/main/java")
	write := func(rel, content string) {
		p := filepath.Join(src, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	edit := func(rel, old, new string) {
		b, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil || !strings.Contains(string(b), old) {
			t.Fatalf("edit %s: %v", rel, err)
		}
		write(rel, strings.Replace(string(b), old, new, 1))
	}
	steps := []struct {
		name  string
		apply func()
		check func(q *Q)
	}{
		{"whitespace only", func() { edit("com/acme/user/User.java", "@Data", "\n\n@Data") }, nil},
		{"new symbol resolves an old unresolved ref", func() {
			write("org/springframework/data/jpa/repository/JpaRepository.java",
				"package org.springframework.data.jpa.repository;\npublic interface JpaRepository<T, ID> { <S extends T> S save(S s); }\n")
		}, func(q *Q) {
			r, _ := q.Neighbors("UserService.createUser", "out", 1, "calls", 0, "")
			if findEdge(r.Edges, "createUser", "JpaRepository#save(Object)") == nil || r.UnresolvedCount != 0 {
				t.Errorf("save() still unresolved: %+v", r.Edges)
			}
		}},
		{"rename a called method", func() { edit("com/acme/mail/MailSender.java", "void send(", "void deliver(") }, nil},
		{"rename back", func() { edit("com/acme/mail/MailSender.java", "void deliver(", "void send(") }, nil},
		{"delete a bean", func() { os.Remove(filepath.Join(src, "com/acme/mail/LogMailSender.java")) }, func(q *Q) {
			r, _ := q.Neighbors("SmtpMailSender.send", "in", 1, "calls", 0, "")
			if e := findEdge(r.Edges, "createUser", "SmtpMailSender#send"); e == nil || e.Evidence != "resolved" {
				t.Errorf("single remaining bean must be resolved: %+v", r.Edges)
			}
		}},
		{"change endpoint path", func() { edit("com/acme/user/UserController.java", `"/api/users"`, `"/v2/users"`) }, func(q *Q) {
			if r, _ := q.APISpec("/v2", 0, ""); len(r.Endpoints) != 2 {
				t.Errorf("endpoints: %+v", r.Endpoints)
			}
		}},
		{"build file change", func() { os.WriteFile(filepath.Join(dir, "build.gradle"), []byte("// changed\n"), 0o644) }, nil},
	}
	for _, s := range steps {
		s.apply()
		q := &Q{dir, db}
		if si, _ := q.scanInfo(); !si.Stale {
			t.Errorf("%s: edit not reported as stale", s.name)
		}
		r, err := scan(dir, db, false)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if r.Full {
			t.Errorf("%s: expected an incremental scan", s.name)
		}
		if got, want := dump(t, db), dump(t, mustScan(t, copyDemo(t, dir), true)); got != want {
			t.Errorf("%s: incremental scan differs from full scan\n--- incremental\n%s\n--- full\n%s", s.name, got, want)
		}
		if s.check != nil {
			s.check(q)
		}
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"6.2.1", "6.10.0"}, {"3.5.0-M1", "3.5.0"}, {"3.5.0-RC2", "3.5.0"}, {"2.0.9", "2.0.10"}, {"1.0", "1.0.1"}, {"5.3.30.RELEASE", "6.0.0"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Errorf("want %s < %s", c[0], c[1])
		}
	}
}

func TestShort(t *testing.T) {
	for id, want := range map[string]string{
		"java:com.acme.user.UserService#createUser(UserDto)": "UserService.createUser(UserDto)",
		"java:com.acme.user.User#<init>(String)":             "new User(String)",
		"java:com.acme.order.Order.OrderBuilder#build()":     "Order.OrderBuilder.build()",
		"java:com.acme.user.User":                            "User",
	} {
		if got := short(id); got != want {
			t.Errorf("short(%s) = %s, want %s", id, got, want)
		}
	}
}

func TestReport(t *testing.T) {
	if _, err := exec.LookPath("javac"); err != nil {
		t.Skip("javac not found")
	}
	autoClasspath = false
	dir := copyDemo(t, "testdata/demo")
	// a string literal with a secret and a tag: neither may reach the page
	src := filepath.Join(dir, "src/main/java/com/acme/user/UserService.java")
	b, _ := os.ReadFile(src)
	os.WriteFile(src, []byte(strings.Replace(string(b), "userRepository.save(user);", `userRepository.save("</script><b>hunter2");`, 1)), 0o644)
	q := &Q{dir, mustScan(t, dir, false)}
	html, err := q.Report("ko", false)
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	if strings.Contains(page, "hunter2") || strings.Count(page, "</script>") != 2 {
		t.Error("string literal contents leaked into the report or broke out of the data element")
	}
	for _, ext := range []string{`src="http`, `href="http`, "url(http"} {
		if strings.Contains(page, ext) {
			t.Errorf("report references an external resource: %s", ext)
		}
	}
	start := strings.Index(page, `type="application/json">`) + len(`type="application/json">`)
	var d reportData
	if err := json.Unmarshal([]byte(page[start:start+strings.Index(page[start:], "</script>")]), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Endpoints) != 2 || len(d.Syms) == 0 || len(d.Edges) == 0 || len(d.Unresolved) == 0 || d.Lang != "ko" {
		t.Errorf("report data incomplete: %d endpoints, %d symbols, %d edges, %d unresolved", len(d.Endpoints), len(d.Syms), len(d.Edges), len(d.Unresolved))
	}
}

func TestMetrics(t *testing.T) {
	if _, err := exec.LookPath("javac"); err != nil {
		t.Skip("javac not found")
	}
	autoClasspath = false
	dir := copyDemo(t, "testdata/demo")
	m, err := (&Q{dir, mustScan(t, dir, false)}).Metrics()
	if err != nil {
		t.Fatal(err)
	}
	method := func(suffix string) MethodMetric {
		for _, x := range m.Methods {
			if strings.HasSuffix(x.ID, suffix) {
				return x
			}
		}
		t.Fatalf("no method %s", suffix)
		return MethodMetric{}
	}
	// `return order.getTotal().amount() > 0 ? order : null;` is the only branch
	if x := method("OrderServiceImpl#place(User)"); x.CC != 2 || x.Params != 1 || x.LOC != 7 || x.CalledBy != 1 {
		t.Errorf("place: %+v", x)
	}
	if x := method("UserService#createUser(String)"); x.CC != 1 || x.Statements != 5 {
		t.Errorf("createUser: %+v", x)
	}
	for _, c := range m.Classes {
		if !strings.HasSuffix(c.ID, ".UserService") {
			continue
		}
		// three declared methods (the Lombok constructor is generated, so not counted); createUser and find share
		// userRepository, reflect touches no field: two groups
		if c.Methods != 3 || c.Fields != 2 || c.WMC != 3 || c.LCOM != 2 || c.FanIn != 1 || c.FanOut < 3 {
			t.Errorf("UserService: %+v", c)
		}
	}
	var user *PackageMetric
	for i := range m.Packages {
		if m.Packages[i].Name == "com.acme.user" {
			user = &m.Packages[i]
		}
	}
	if user == nil || user.Classes != 4 || user.Ce == 0 || user.Abstractness != 0.25 {
		t.Errorf("package com.acme.user: %+v", user)
	}
	for _, f := range m.Files {
		if strings.HasSuffix(f.Path, "user/User.java") && (f.Lines != 10 || f.Code != 8 || f.Blank != 2 || f.Comment != 0) {
			t.Errorf("User.java lines: %+v", f)
		}
	}
	if head, rows := m.limit(2, false).table("class", short); len(rows) != 2 || head[4] != "wmc" {
		t.Errorf("table: %v %v", head, rows)
	}
}

func TestGo(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found")
	}
	dir := copyDemo(t, "testdata/godemo")
	db := mustScan(t, dir, false)
	q := &Q{dir, db}

	api, err := q.APISpec("", 0, "")
	if err != nil || len(api.Endpoints) != 2 || api.Endpoints[0].Method != "POST" || api.Endpoints[0].Path != "/users" ||
		api.Endpoints[1].Method != "GET" || api.Endpoints[1].Path != "/users/{id}" || !strings.HasSuffix(api.Endpoints[1].Handler, "web.Server#show") {
		t.Fatalf("endpoints: %+v %v", api, err)
	}
	// s.store.Find goes through the interface: the edge to the interface method is a fact, the one to the only
	// implementation is an inference from the type hierarchy
	in, err := q.Neighbors("Memory.Find", "in", 1, "calls", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if e := findEdge(in.Edges, "web.Server#show", "store.Memory#Find"); e == nil || e.Evidence != "heuristic" || e.Rule != "class-hierarchy" {
		t.Errorf("interface dispatch: %+v", in.Edges)
	}
	if r, _ := q.Neighbors("store.Store.Find", "in", 1, "calls", 0, ""); findEdge(r.Edges, "web.Server#show", "store.Store#Find") == nil {
		t.Errorf("call to the interface method: %+v", r.Edges)
	}
	// every name the CLI prints resolves back: package-qualified, bare, and the full ID
	for _, ref := range []string{"web.Server.create", "Server.create", "go:example.com/godemo/web.Server#create"} {
		if ids, err := q.resolve(latestScan(db), ref); err != nil || len(ids) != 1 || ids[0] != "go:example.com/godemo/web.Server#create" {
			t.Errorf("resolve(%q) = %v, %v", ref, ids, err)
		}
	}
	// s.notify(...) calls a function stored in a field: the target is unknown, and that is recorded.
	// fail(...) is a local closure: its body already belongs to show, nothing is hidden.
	out, _ := q.Neighbors("web.Server.create", "out", 1, "calls", 0, "")
	if out.UnresolvedCount != 1 {
		t.Errorf("create: unresolved_count = %d, want 1 (s.notify)", out.UnresolvedCount)
	}
	if r, _ := q.Neighbors("web.Server.show", "out", 1, "calls", 0, ""); r.UnresolvedCount != 0 {
		t.Errorf("show: unresolved_count = %d, want 0", r.UnresolvedCount)
	}

	m, err := q.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range m.Methods {
		switch {
		case strings.HasSuffix(x.ID, "store.Memory#Save") && (x.CC != 3 || x.Test): // if + ||
			t.Errorf("Save: %+v", x)
		case strings.HasSuffix(x.ID, "web.TestRoutes") && !x.Test:
			t.Errorf("TestRoutes must be test code: %+v", x)
		}
	}
	for _, f := range m.Files {
		if strings.HasSuffix(f.Path, "store/store.go") && (f.Lines != 41 || f.Comment != 3 || f.Code != 30) {
			t.Errorf("store.go lines: %+v", f)
		}
	}
	if got := short("go:example.com/godemo/web.Server#create"); got != "web.Server.create" {
		t.Errorf("short = %s", got)
	}
	if got := short("go:gateway.Gateway#chat"); got != "Gateway.chat" { // a one-element module path: the qualifier would only repeat the repository name
		t.Errorf("short (root package) = %s", got)
	}
	if got := short("go:example.com/godemo/web.server_go"); got != "web/server.go" {
		t.Errorf("short (file container) = %s", got)
	}

	// incremental == full, after edits that change what other files resolve to
	src := func(rel string) string { return filepath.Join(dir, rel) }
	edit := func(rel, old, new string) {
		b, err := os.ReadFile(src(rel))
		if err != nil || !strings.Contains(string(b), old) {
			t.Fatalf("edit %s: %v", rel, err)
		}
		os.WriteFile(src(rel), []byte(strings.Replace(string(b), old, new, 1)), 0o644)
	}
	steps := []struct {
		name  string
		apply func()
	}{
		{"comment only", func() { edit("store/store.go", "// Store is what", "// Store is exactly what") }},
		{"rename an interface method", func() {
			edit("store/store.go", "Find(id int) (*User, error)\n\tSave", "Get(id int) (*User, error)\n\tSave")
			edit("store/store.go", "func (m *Memory) Find(", "func (m *Memory) Get(")
			edit("web/server.go", "s.store.Find(1)", "s.store.Get(1)")
		}},
		{"second implementation", func() {
			os.WriteFile(src("store/null.go"), []byte("package store\n\ntype Null struct{}\n\nfunc (Null) Get(int) (*User, error) { return nil, ErrNotFound }\nfunc (Null) Save(*User) error          { return nil }\n"), 0o644)
		}},
		{"delete it again", func() { os.Remove(src("store/null.go")) }},
		{"route change", func() { edit("web/server.go", `"POST /users"`, `"PUT /users"`) }},
	}
	for _, s := range steps {
		s.apply()
		r, err := scan(dir, db, false)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if r.Full {
			t.Errorf("%s: expected an incremental scan", s.name)
		}
		if got, want := dump(t, db), dump(t, mustScan(t, copyDemo(t, dir), true)); got != want {
			t.Errorf("%s: incremental scan differs from full scan\n--- incremental\n%s\n--- full\n%s", s.name, got, want)
		}
	}
}

// A scan must not leave anything behind that `git add -A` would commit.
func TestCacheIsGitIgnored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found")
	}
	dir := copyDemo(t, "testdata/godemo")
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("add", "-A")
	git("-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "init")
	mustScan(t, dir, false)
	git("add", "-A")
	if staged := git("diff", "--cached", "--name-only"); strings.Contains(staged, "index.db") || strings.Contains(staged, ".ri/cache") {
		t.Errorf("the cache would be committed:\n%s", staged)
	}
}

// equalAfter applies edits one by one and requires an incremental scan to equal a full scan after each.
func equalAfter(t *testing.T, dir string, db *sql.DB, steps []struct {
	name  string
	apply func()
}) {
	t.Helper()
	for _, s := range steps {
		s.apply()
		r, err := scan(dir, db, false)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if r.Full {
			t.Errorf("%s: expected an incremental scan", s.name)
		}
		if got, want := dump(t, db), dump(t, mustScan(t, copyDemo(t, dir), true)); got != want {
			t.Errorf("%s: incremental scan differs from full scan\n--- incremental\n%s\n--- full\n%s", s.name, got, want)
		}
	}
}

func editor(t *testing.T, dir string) func(rel, old, new string) {
	return func(rel, old, new string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), old) {
			t.Fatalf("edit %s: %q not found (%v)", rel, old, err)
		}
		os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644)
	}
}

func TestTypeScript(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found")
	}
	dir := copyDemo(t, "testdata/tsdemo")
	db := mustScan(t, dir, false)
	q := &Q{dir, db}

	// this.store.save(): a fact up to the interface method, an inference from there to its only implementation
	in, err := q.Neighbors("MemoryStore.save", "in", 1, "calls", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if e := findEdge(in.Edges, "service.UserService#create", "store.MemoryStore#save"); e == nil || e.Evidence != "heuristic" || e.Rule != "class-hierarchy" {
		t.Errorf("interface dispatch: %+v", in.Edges)
	}
	out, _ := q.Neighbors("service.UserService.create", "out", 1, "calls", 0, "")
	if findEdge(out.Edges, "UserService#create", "store.Store#save") == nil || findEdge(out.Edges, "UserService#create", "service.describe") == nil {
		t.Errorf("create callees: %+v", out.Edges)
	}
	if out.UnresolvedCount != 1 { // this.notify(...): a function passed in from outside
		t.Errorf("create: unresolved_count = %d, want 1", out.UnresolvedCount)
	}
	// describe() calls a local arrow function: not unresolved
	if r, _ := q.Neighbors("service.describe", "out", 1, "calls", 0, ""); r.UnresolvedCount != 0 {
		t.Errorf("describe: unresolved_count = %d, want 0", r.UnresolvedCount)
	}
	// the <script> of a Vue component is analyzed, at the line numbers of the .vue file
	vue, _ := q.Neighbors("service.describe", "in", 1, "calls", 0, "")
	if e := findEdge(vue.Edges, "ts:src/Hello.greeting", "service.describe"); e == nil || e.At == nil || e.At.File != "src/Hello.vue" || e.At.StartLine != 8 {
		t.Errorf("Vue script: %+v", vue.Edges)
	}
	for _, ref := range []string{"service.UserService.create", "UserService.create", "ts:src/service.UserService#create"} {
		if ids, err := q.resolve(latestScan(db), ref); err != nil || len(ids) != 1 || ids[0] != "ts:src/service.UserService#create" {
			t.Errorf("resolve(%q) = %v, %v", ref, ids, err)
		}
	}
	if got := short("ts:src/service.UserService#create"); got != "service.UserService.create" {
		t.Errorf("short = %s", got)
	}
	m, err := q.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range m.Methods {
		switch {
		case strings.HasSuffix(x.ID, "store.MemoryStore#save") && x.CC != 3: // if + ||
			t.Errorf("save: %+v", x)
		case strings.HasSuffix(x.ID, "service.test.testCreate") && !x.Test:
			t.Errorf("a .test.ts file must be test code: %+v", x)
		}
	}
	pkgs := map[string]bool{}
	for _, p := range m.Packages {
		pkgs[p.Name] = true
	}
	if !pkgs["src"] || len(pkgs) != 1 {
		t.Errorf("packages are directories: %v", pkgs)
	}

	edit := editor(t, dir)
	equalAfter(t, dir, db, []struct {
		name  string
		apply func()
	}{
		{"comment only", func() { edit("src/store.ts", "// Users and", "// The users and") }},
		{"rename an interface method", func() {
			edit("src/store.ts", "save(user: User): void;", "put(user: User): void;")
			edit("src/store.ts", "  save(user: User): void {", "  put(user: User): void {")
			edit("src/service.ts", "this.store.save(user)", "this.store.put(user)")
		}},
		{"second implementation", func() {
			os.WriteFile(filepath.Join(dir, "src/null.ts"), []byte("import type { Store, User } from \"./store\";\n\nexport class NullStore implements Store {\n  find(): User | undefined { return undefined; }\n  put(): void {}\n}\n"), 0o644)
		}},
		{"delete it again", func() { os.Remove(filepath.Join(dir, "src/null.ts")) }},
		{"change inside a Vue component", func() { edit("src/Hello.vue", `name: "ann"`, `name: "bob"`) }},
	})
}

func TestPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	dir := copyDemo(t, "testdata/pydemo")
	db := mustScan(t, dir, false)
	q := &Q{dir, db}

	api, err := q.APISpec("", 0, "")
	if err != nil || len(api.Endpoints) != 2 || api.Endpoints[0].Method != "POST" || api.Endpoints[1].Path != "/users/{user_id}" ||
		api.Endpoints[1].Evidence != "heuristic" || !strings.HasSuffix(api.Endpoints[1].Handler, "web.get_user") {
		t.Fatalf("endpoints: %+v %v", api, err)
	}
	out, _ := q.Neighbors("service.UserService.create", "out", 1, "calls", 0, "")
	// self.repo is a Repo because the constructor parameter says so: a fact, not a guess
	if e := findEdge(out.Edges, "UserService#create", "repo.Repo#save"); e == nil || e.Evidence != "resolved" {
		t.Errorf("typed attribute: %+v", out.Edges)
	}
	if out.UnresolvedCount != 1 { // self.notify(...): a callable passed in
		t.Errorf("create: unresolved_count = %d, want 1", out.UnresolvedCount)
	}
	// describe(thing) has no type for thing: display() is matched by name only, and says so
	if r, _ := q.Neighbors("service.describe", "out", 1, "calls", 0, ""); len(r.Edges) != 1 || r.Edges[0].Evidence != "heuristic" || r.Edges[0].Rule != "method-name" {
		t.Errorf("method-name rule: %+v", r.Edges)
	}
	// `service` is a module-level UserService(...): calls on it are resolved, across files too
	if r, _ := q.Neighbors("web.get_user", "out", 1, "calls", 0, ""); findEdge(r.Edges, "web.get_user", "UserService#lookup") == nil || r.Edges[0].Evidence != "resolved" {
		t.Errorf("module-level object: %+v", r.Edges)
	}
	if r, _ := q.Neighbors("repo.Repo.find", "in", 1, "overrides", 0, ""); findEdge(r.Edges, "CachedRepo#find", "Repo#find") == nil {
		t.Errorf("overrides: %+v", r.Edges)
	}
	m, err := q.Metrics()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range m.Methods {
		switch {
		case strings.HasSuffix(x.ID, "repo.Repo#save") && x.CC != 3: // if + or
			t.Errorf("save: %+v", x)
		case strings.HasSuffix(x.ID, "test_service.test_create") && !x.Test:
			t.Errorf("tests/ must be test code: %+v", x)
		}
	}
	if got := short("py:app/service.UserService#create"); got != "service.UserService.create" {
		t.Errorf("short = %s", got)
	}

	edit := editor(t, dir)
	equalAfter(t, dir, db, []struct {
		name  string
		apply func()
	}{
		{"comment only", func() { edit("app/repo.py", "# in-memory storage", "# storage, in memory") }},
		{"rename a method", func() {
			edit("app/repo.py", "    def save(self, user: User) -> None:", "    def store(self, user: User) -> None:")
			edit("app/service.py", "self.repo.save(user)", "self.repo.store(user)")
		}},
		{"a second class with the same method name", func() {
			os.WriteFile(filepath.Join(dir, "app/other.py"), []byte("class Badge:\n    def display(self):\n        return \"badge\"\n"), 0o644)
		}},
		{"delete it again", func() { os.Remove(filepath.Join(dir, "app/other.py")) }},
		{"route change", func() { edit("app/web.py", `@app.post("/users")`, `@app.put("/users")`) }},
	})
}

// A language whose toolchain is missing is reported, not fatal, and the others are still analyzed.
func TestMissingToolchain(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found")
	}
	dir := copyDemo(t, "testdata/godemo")
	os.WriteFile(filepath.Join(dir, "script.py"), []byte("def main():\n    pass\n"), 0o644)
	bin := t.TempDir() // a PATH with go and git but nothing else
	for _, tool := range []string{"go", "git"} {
		if p, err := exec.LookPath(tool); err == nil {
			os.Symlink(p, filepath.Join(bin, tool))
		}
	}
	t.Setenv("PATH", bin)
	db, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := scan(dir, db, false)
	if err != nil {
		t.Fatalf("scan must not fail: %v", err)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "Python") {
		t.Errorf("warnings: %v", r.Warnings)
	}
	o, err := (&Q{dir, db}).Overview()
	if err != nil || o.Languages["go"] == 0 || o.ExcludedFiles["python"] != 1 || len(o.Warnings) != 1 {
		t.Errorf("overview: %+v %v", o, err)
	}
}

func gitRepo(t *testing.T, dir string) func(args ...string) string {
	t.Helper()
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	return git
}

func TestDepsAndChecks(t *testing.T) {
	if _, err := exec.LookPath("javac"); err != nil {
		t.Skip("javac not found")
	}
	autoClasspath = false
	dir := copyDemo(t, "testdata/demo")
	q := &Q{dir, mustScan(t, dir, false)}
	d, err := q.Deps(false, true)
	if err != nil {
		t.Fatal(err)
	}
	var userToMail *PackageDep
	for i := range d.Dependencies {
		if d.Dependencies[i].From == "com.acme.user" && d.Dependencies[i].To == "com.acme.mail" {
			userToMail = &d.Dependencies[i]
		}
	}
	if len(d.Packages) != 4 || userToMail == nil || userToMail.InCycle || len(userToMail.Classes) == 0 || userToMail.Classes[0].From != "java:com.acme.user.UserService" {
		t.Errorf("deps: packages %v, user->mail %+v", d.Packages, userToMail)
	}
	if len(d.PackageCycles) != 0 {
		t.Errorf("unexpected package cycles: %v", d.PackageCycles)
	}
	var dot, mmd strings.Builder
	writeDeps(&dot, d, "dot")
	writeDeps(&mmd, d, "mermaid")
	if !strings.Contains(dot.String(), `"user" -> "mail"`) || !strings.Contains(mmd.String(), "graph LR") {
		t.Errorf("exports:\n%s\n%s", dot.String(), mmd.String())
	}

	c, err := q.Checks()
	if err != nil {
		t.Fatal(err)
	}
	rule := func(id string) (n int) {
		for _, f := range c.Findings {
			if f.Rule == id {
				n++
			}
		}
		return
	}
	if len(c.Rules) != len(ruleOrder) || rule("low-cohesion") != 1 || rule("complex-method") != 0 || c.Warnings != 1 {
		t.Errorf("checks: %d rules, warnings %d, findings %+v", len(c.Rules), c.Warnings, c.Findings)
	}
	// candidates are notes: reflect() is called by nobody, OrderReport.run only by nobody too; neither fails a build
	if rule("unreferenced-method") == 0 || c.Notes == 0 {
		t.Errorf("expected unreferenced-method notes: %+v", c.Findings)
	}
	var sarif strings.Builder
	writeSARIF(&sarif, c)
	var doc struct {
		Version string
		Runs    []struct {
			Results []struct {
				RuleID string `json:"ruleId"`
				Level  string
			}
		}
	}
	if err := json.Unmarshal([]byte(sarif.String()), &doc); err != nil || doc.Version != "2.1.0" || len(doc.Runs) != 1 || len(doc.Runs[0].Results) != len(c.Findings) {
		t.Errorf("sarif: %v %+v", err, doc)
	}

	// thresholds, switched-off rules and excludes come from .ri/config.json; a typo must be an error, not a no-op
	cfg := filepath.Join(dir, ".ri", "config.json")
	os.WriteFile(cfg, []byte(`{"thresholds": {"lcom": 5}, "disable": ["untested-class"], "exclude": ["**/order/**"]}`), 0o644)
	if _, err := scan(dir, q.db, false); err != nil {
		t.Fatal(err)
	}
	c, _ = q.Checks()
	if len(c.Rules) != len(ruleOrder)-1 || c.Warnings != 0 {
		t.Errorf("config not applied: %d rules, %d warnings", len(c.Rules), c.Warnings)
	}
	r, _ := q.Search("Order", "", "", 0, "")
	for _, sym := range r.Symbols { // OrderConfig lives in config/ and stays; everything under order/ is gone
		if strings.Contains(sym.ID, ".order.") {
			t.Errorf("exclude not applied: %s", sym.ID)
		}
	}
	if len(r.Symbols) == 0 {
		t.Error("OrderConfig should still be indexed")
	}
	os.WriteFile(cfg, []byte(`{"threshold": {"cc": 3}}`), 0o644)
	if _, err := scan(dir, q.db, false); err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Errorf("a misspelled config key must fail the scan, got %v", err)
	}
}

func TestImpact(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found")
	}
	dir := copyDemo(t, "testdata/godemo")
	gitRepo(t, dir)
	db := mustScan(t, dir, false)
	q := &Q{dir, db}
	if r, err := q.Impact("HEAD", 3); err != nil || len(r.Changed) != 0 || len(r.ChangedFiles) != 0 {
		t.Fatalf("clean tree: %+v %v", r, err)
	}
	editor(t, dir)("store/store.go", "return nil, ErrNotFound", "return nil, ErrNotFound // changed")
	if _, err := scan(dir, db, false); err != nil {
		t.Fatal(err)
	}
	r, err := q.Impact("HEAD", 3)
	if err != nil {
		t.Fatal(err)
	}
	has := func(xs []ImpactSymbol, suffix string) *ImpactSymbol {
		for i := range xs {
			if strings.HasSuffix(xs[i].Symbol.ID, suffix) {
				return &xs[i]
			}
		}
		return nil
	}
	if len(r.Changed) != 1 || has(r.Changed, "store.Memory#Find") == nil {
		t.Fatalf("changed: %+v", r.Changed)
	}
	// Find is reached from the web layer only through the Store interface: the path is an inference and says so
	show := has(r.Affected, "web.Server#show")
	if show == nil || show.Depth != 1 || show.Evidence != "heuristic" {
		t.Errorf("affected: %+v", r.Affected)
	}
	if len(r.Endpoints) != 1 || r.Endpoints[0].Path != "/users/{id}" {
		t.Errorf("endpoints: %+v", r.Endpoints)
	}
	if has(r.Tests, "web.TestRoutes") == nil {
		t.Errorf("tests: %+v", r.Tests)
	}
	// a "revision" that is really a git option must be refused, not handed to git (it would write the file)
	trap := filepath.Join(t.TempDir(), "written")
	if _, err := q.Impact("--output="+trap, 3); err == nil {
		t.Error("an option was accepted as a revision")
	}
	if _, err := os.Stat(trap); err == nil {
		t.Error("git wrote a file named by the caller")
	}
	if _, err := q.Neighbors(strings.Repeat("x", 200000), "in", 1, "calls", 0, ""); err == nil || strings.Contains(err.Error(), "SQL") {
		t.Errorf("an oversized symbol should be a plain not-found: %v", err)
	}
	var md strings.Builder
	writeImpact(&md, r, true)
	if !strings.Contains(md.String(), "### ") || !strings.Contains(md.String(), "GET /users/{id}") {
		t.Errorf("markdown:\n%s", md.String())
	}
	// an inline handler is its own symbol, so a change behind POST /users does not drag GET along
	editor(t, dir)("store/store.go", `return errors.New("invalid user")`, `return errors.New("invalid")`)
	exec.Command("git", "-C", dir, "checkout", "-q", "--", ".").Run()
	editor(t, dir)("store/store.go", `return errors.New("invalid user")`, `return errors.New("invalid")`)
	scan(dir, db, false)
	r, _ = q.Impact("HEAD", 4)
	if len(r.Endpoints) != 1 || r.Endpoints[0].Method != "POST" {
		t.Errorf("only POST /users reaches Save: %+v", r.Endpoints)
	}
}

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"**/*.pb.go", "api/v1/x.pb.go", true}, {"**/*.pb.go", "x.pb.go", true}, {"**/*.pb.go", "x.go", false},
		{"legacy/**", "legacy/a/b.java", true}, {"legacy", "legacy/a.java", true}, {"legacy", "legacy2/a.java", false},
		{"src/*/gen", "src/a/gen/x.ts", true}, {"src/*/gen", "src/a/b/gen/x.ts", false},
	} {
		if got := globRegexp(c.glob).MatchString(c.path); got != c.want {
			t.Errorf("%q vs %q = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

// The classpath follows the versions the build declares (directly, or through the Spring Boot BOM), not the newest
// jar that happens to be in the cache.
func TestClasspathFollowsDeclaredVersions(t *testing.T) {
	cache, repo := t.TempDir(), t.TempDir()
	old := jarRoots
	jarRoots = func() []string { return []string{cache} }
	defer func() { jarRoots = old }()
	jar := func(group, artifact, version, pkg string) string {
		p := filepath.Join(cache, group, artifact, version, "abc", artifact+"-"+version+".jar")
		os.MkdirAll(filepath.Dir(p), 0o755)
		f, _ := os.Create(p)
		z := zip.NewWriter(f)
		z.Create(strings.ReplaceAll(pkg, ".", "/") + "/A.class")
		z.Close()
		f.Close()
		return p
	}
	web5 := jar("org.springframework", "spring-web", "5.3.15", "org.springframework.web")
	jar("org.springframework", "spring-web", "7.0.10", "org.springframework.web")
	joda := jar("joda-time", "joda-time", "2.10.13", "org.joda.time")
	jar("joda-time", "joda-time", "2.14.0", "org.joda.time")
	jar("com.acme", "util", "1.0", "com.acme.util")
	util2 := jar("com.acme", "util", "2.0", "com.acme.util")
	bom := filepath.Join(cache, "org.springframework.boot", "spring-boot-dependencies", "2.6.3", "abc", "spring-boot-dependencies-2.6.3.pom")
	os.MkdirAll(filepath.Dir(bom), 0o755)
	os.WriteFile(bom, []byte(`<project><properties><spring-framework.version>5.3.15</spring-framework.version></properties><dependencyManagement><dependencies>
<dependency><groupId>org.springframework</groupId><artifactId>spring-framework-bom</artifactId><version>${spring-framework.version}</version><type>pom</type><scope>import</scope></dependency>
</dependencies></dependencyManagement></project>`), 0o644)
	os.WriteFile(filepath.Join(repo, "build.gradle"), []byte("plugins { id 'org.springframework.boot' version '2.6.3' }\ndependencies { implementation 'joda-time:joda-time:2.10.13' }\n"), 0o644)
	os.MkdirAll(filepath.Join(repo, ".ri", "cache"), 0o755)

	idx := buildJarIndex([]string{cache})
	if len(idx["org.springframework.web"]) != 2 {
		t.Fatalf("index should hold every version: %v", idx)
	}
	b, _ := json.Marshal(idx)
	t.Setenv("HOME", t.TempDir()) // the index file goes to a throwaway user cache
	t.Setenv("XDG_CACHE_HOME", filepath.Join(os.Getenv("HOME"), "cache"))
	c, _ := os.UserCacheDir()
	os.MkdirAll(filepath.Join(c, "rintel"), 0o755)
	os.WriteFile(filepath.Join(c, "rintel", "jarindex2.json"), b, 0o644)

	added, err := extendClasspath(repo, nil, []string{"org.springframework.web", "org.joda.time", "com.acme.util"})
	if err != nil || !added {
		t.Fatal(added, err)
	}
	got, _ := classpath(repo)
	want := []string{util2, joda, web5} // undeclared: newest; declared in the build file; managed by the BOM
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("classpath:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// a second version of a library already on the classpath is never added
	if added, _ := extendClasspath(repo, got, []string{"org.springframework.web"}); added {
		t.Error("added another version of spring-web")
	}
	// group lookups go up the group name, closest first; version catalogs and Maven properties are read
	w := map[string]string{"com.fasterxml.jackson:*": "2.13.1", "org.springframework:*": "5.3.15", "org.springframework.security:*": "5.6.1"}
	for key, v := range map[string]string{"com.fasterxml.jackson.core:jackson-databind": "2.13.1", "org.springframework.security:spring-security-web": "5.6.1",
		"org.springframework:spring-web": "5.3.15", "org.other:x": ""} {
		if got := wantedVersion(w, key); got != v {
			t.Errorf("wantedVersion(%s) = %q, want %q", key, got, v)
		}
	}
	cat := t.TempDir()
	os.MkdirAll(filepath.Join(cat, "gradle"), 0o755)
	os.WriteFile(filepath.Join(cat, "gradle", "libs.versions.toml"), []byte(`[versions]
guava = "33.0.0-jre"
boot = "3.2.1"

[libraries]
guava = { module = "com.google.guava:guava", version.ref = "guava" }
okio = { group = "com.squareup.okio", name = "okio", version = "3.9.0" }
junit-bom = "org.junit:junit-bom:5.10.2"

[plugins]
spring-boot = { id = "org.springframework.boot", version.ref = "boot" }
`), 0o644)
	os.WriteFile(filepath.Join(cat, "pom.xml"), []byte(`<project><properties><jjwt.version>0.11.2</jjwt.version></properties><dependencies>
<dependency><groupId>io.jsonwebtoken</groupId><artifactId>jjwt-api</artifactId><version>${jjwt.version}</version></dependency></dependencies></project>`), 0o644)
	os.MkdirAll(filepath.Join(cat, "services", "api"), 0o755) // a module two directories down, with versions in variables
	os.WriteFile(filepath.Join(cat, "gradle.properties"), []byte("kafkaVersion=3.6.1\n"), 0o644)
	os.WriteFile(filepath.Join(cat, "services", "api", "build.gradle"), []byte(`ext { poiVersion = '5.2.5' }
dependencies {
  implementation "org.apache.poi:poi:$poiVersion"
  implementation "org.apache.kafka:kafka-clients:${kafkaVersion}"
}`), 0o644)
	w = wantedVersions(cat)
	if w["org.apache.poi:poi"] != "5.2.5" || w["org.apache.kafka:kafka-clients"] != "3.6.1" {
		t.Errorf("gradle variables: %v", w)
	}
	for key, v := range map[string]string{"com.google.guava:guava": "33.0.0-jre", "com.squareup.okio:okio": "3.9.0", "org.junit.jupiter:junit-jupiter-api": "5.10.2",
		"org.springframework.boot:spring-boot": "3.2.1", "io.jsonwebtoken:jjwt-api": "0.11.2"} {
		if got := wantedVersion(w, key); got != v {
			t.Errorf("catalog/pom: %s = %q, want %q (%v)", key, got, v, w)
		}
	}

	// the declared version was not cached when the classpath was chosen, and arrives later: chosen again
	late := t.TempDir()
	os.MkdirAll(filepath.Join(late, ".ri", "cache"), 0o755)
	os.WriteFile(filepath.Join(late, "build.gradle"), []byte("dependencies { implementation 'com.acme:util:3.0' }\n"), 0o644)
	if added, _ := extendClasspath(late, nil, []string{"com.acme.util"}); !added {
		t.Fatal("stand-in not added")
	}
	resetStaleAutoClasspath(late)
	if got, _ := classpath(late); len(got) != 1 || got[0] != util2 {
		t.Errorf("stand-in should stay while 3.0 is not cached: %v", got)
	}
	jar("com.acme", "util", "3.0", "com.acme.util")
	resetStaleAutoClasspath(late)
	if got, _ := classpath(late); len(got) != 0 {
		t.Errorf("classpath should be chosen again once 3.0 is cached: %v", got)
	}
	if added, _ := extendClasspath(late, nil, []string{"com.acme.util"}); !added {
		t.Fatal("nothing chosen after the reset")
	}
	if got, _ := classpath(late); len(got) != 1 || !strings.HasSuffix(got[0], "util-3.0.jar") {
		t.Errorf("the declared version should be chosen now (the index is rebuilt): %v", got)
	}

	// the build moves to another version: the automatic classpath is dropped and chosen again
	os.WriteFile(filepath.Join(repo, "build.gradle"), []byte("dependencies { implementation 'joda-time:joda-time:2.14.0' }\n"), 0o644)
	resetStaleAutoClasspath(repo)
	if got, _ := classpath(repo); len(got) != 0 {
		t.Errorf("stale automatic classpath kept: %v", got)
	}
}

// A handler written in place of a route registration is a symbol of its own, so each endpoint has its own callees.
func TestInlineRoutesTypeScript(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not found")
	}
	dir := copyDemo(t, "testdata/tsdemo")
	os.MkdirAll(filepath.Join(dir, "node_modules", "hono"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "hono", "package.json"), []byte(`{"name":"hono","version":"0.0.0","types":"index.d.ts"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "node_modules", "hono", "index.d.ts"), []byte("export declare class Hono {\n  get(path: string, ...handlers: any[]): this;\n  post(path: string, ...handlers: any[]): this;\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "api.ts"), []byte(`import { Hono } from "hono";
import { service } from "./main";

export function register(app: Hono) {
  app.get("/users/:id", () => service.lookup(1));
  app.post("/users", () => service.create("ann"));
}
`), 0o644)
	db := mustScan(t, dir, false)
	q := &Q{dir, db}
	api, err := q.APISpec("", 0, "")
	if err != nil || len(api.Endpoints) != 2 {
		t.Fatalf("endpoints: %+v %v", api, err)
	}
	in, _ := q.Neighbors("service.UserService.create", "in", 1, "calls", 0, "")
	if findEdge(in.Edges, "api.register#POST_users", "UserService#create") == nil || findEdge(in.Edges, "GET_users", "UserService#create") != nil {
		t.Errorf("create is called by the POST handler only: %+v", in.Edges)
	}
	if handlerLabel("ts:src/api.register#POST_users") != "api.register"+tr(" (inline)", " (인라인)") {
		t.Errorf("label: %s", handlerLabel("ts:src/api.register#POST_users"))
	}
	// editing one handler and scanning incrementally gives what a full scan gives
	equalAfter(t, dir, db, []struct {
		name  string
		apply func()
	}{{"edit a handler", func() { editor(t, dir)("src/api.ts", "service.lookup(1)", "service.lookup(2)") }}})
}

func TestGeneratedPath(t *testing.T) {
	for path, want := range map[string]bool{
		"build/generated/sources/dgs/io/acme/types/Article.java": true, "target/generated-sources/annotations/QUser.java": true,
		"api/v1/user.pb.go": true, "api/v1/user.pb.gw.go": true, "internal/zz_gen.go": true, "web/src/api.gen.ts": true, "proto/user_pb2.py": true,
		"src/main/java/io/acme/generator/Builder.java": false, "internal/generic/list.go": false, "web/src/general.ts": false, "cmd/gen.go": false,
	} {
		if got := generatedPath.MatchString(path); got != want {
			t.Errorf("generatedPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestSecurityFindings(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.go": `package a
import "crypto/md5"
var password = "hunter2hunter2"
var tokenHeader = "X-Auth-Token-Name"
var secret = "${JWT_SECRET}"
// password = "commented0out"
// AKIAABCDEFGHIJKLMNOP
func q(id string) { db.Query("SELECT * FROM users WHERE id = " + id) }
func ok(id string) { db.Query("SELECT * FROM users WHERE id = ?", id) }
func s(id string) { db.Query(fmt.Sprintf("DELETE FROM users WHERE id = %s", id)) }
var c = tls.Config{InsecureSkipVerify: true}
var cmd = exec.Command("ls")
`,
		"b.py": `import pickle, yaml, subprocess
x = pickle.loads(data)
y = yaml.load(f, Loader=yaml.SafeLoader)
z = eval(expr)
w = model.eval()
cur.execute("select * from t where id = %s", (i,))
cur.execute(f"select * from t where id = {i}")
requests.get(u, verify=False)
`,
		"c.ts": "const r = /a/.exec(s);\nel.innerHTML = html;\nif (el.innerHTML == '') {}\nawait sql`select * from t where id = ${id}`;\ndb.query(`select * from t where id = ${id}`);\neval(code);\n",
		"D.java": `class D { void f(String n) throws Exception {
  MessageDigest.getInstance("MD5");
  MessageDigest.getInstance("SHA-256");
  st.executeQuery("select * from t where name = '" + n + "'");
  String sql = "select a from t " +
  new ObjectInputStream(in);
} }`,
		"a_test.go": "package a\nvar password = \"hunter2hunter2\"\n",
	}
	var paths []string
	for name, body := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
		paths = append(paths, name)
	}
	sort.Strings(paths)
	var got []string
	for _, f := range securityFindings(dir, paths) {
		got = append(got, fmt.Sprintf("%s:%d %s", f.File, f.Line, f.Rule))
		if strings.Contains(f.Message, "hunter2") {
			t.Errorf("message leaks the secret: %s", f.Message)
		}
	}
	want := []string{
		"D.java:2 weak-crypto", "D.java:4 sql-concatenation", "D.java:6 unsafe-deserialization",
		"a.go:2 weak-crypto", "a.go:3 hardcoded-secret", "a.go:7 hardcoded-secret", "a.go:8 sql-concatenation", "a.go:10 sql-concatenation",
		"a.go:11 tls-verification-disabled", "a.go:12 command-execution",
		"b.py:2 unsafe-deserialization", "b.py:4 dynamic-code", "b.py:7 sql-concatenation", "b.py:8 tls-verification-disabled",
		"c.ts:2 html-injection", "c.ts:5 sql-concatenation", "c.ts:6 dynamic-code",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range securityRules { // every rule has a title and a fix, in both languages
		if ruleTitles[r.id] == "" || ruleFix[r.id][0] == "" || ruleFix[r.id][1] == "" {
			t.Errorf("rule %s lacks a title or fix", r.id)
		}
	}
	for _, id := range ruleOrder {
		if ruleFix[id][0] == "" {
			t.Errorf("rule %s has no fix text", id)
		}
	}
}

func TestSettingsSecretsAndSQLConstants(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"application.yml": "spring:\n  datasource:\n    password: s3cretPassw0rd\n    username: admin123456\n  jwt:\n    secret: ${JWT_SECRET}\n# password: old12345pw\n",
		".env":            "API_KEY=abcd1234efgh5678\nTOKEN=$OTHER\n",
		".env.example":    "API_KEY=abcd1234efgh5678\n",
		"pnpm-lock.yaml":  "token: abcd1234efgh5678\n",
		"q.ts":            "db.query(`UPDATE t SET a = $1 WHERE id = $2${claim ? ' AND b IS NULL' : ''}`)\ndb.query(`SELECT a FROM t WHERE x > now() - interval '${DAYS} days'`)\ndb.query(`SELECT a FROM t WHERE ${filter}`)\n",
		"m.go":            "package m\nfunc f(id string) {\n\tq := fmt.Sprintf(`SELECT a\n\t\tFROM t\n\t\tWHERE id = %s`, id)\n\tsafe := `SELECT a\n\t\tFROM t WHERE id = ?`\n\t_ = template.HTML(id)\n\t_ = template.HTML(\"<b>fixed</b>\")\n}\n",
		"n.js":            "var serialize = require('node-serialize')\nvar md5 = require('md5')\nserialize.unserialize(body)\n",
	}
	var paths []string
	for name, body := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
		paths = append(paths, name)
	}
	sort.Strings(paths)
	var got []string
	for _, f := range securityFindings(dir, paths) {
		got = append(got, fmt.Sprintf("%s:%d %s", f.File, f.Line, f.Rule))
	}
	want := ".env:1 hardcoded-secret\napplication.yml:3 hardcoded-secret\nm.go:3 sql-concatenation\nm.go:8 html-injection\n" +
		"n.js:2 weak-crypto\nn.js:3 unsafe-deserialization\nq.ts:3 sql-concatenation"
	if strings.Join(got, "\n") != want {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), want)
	}
}

func TestVulnerableDependencies(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n\nrequire (\n\tgolang.org/x/net v0.1.0\n\tcorp.example/internal/lib v1.0.0 // indirect\n)\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("# pins\nflask==0.12\nrequests>=2.0\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(`{"lockfileVersion":3,"packages":{"":{"name":"x"},
		"node_modules/lodash":{"version":"4.17.0","resolved":"https://registry.npmjs.org/lodash/-/lodash-4.17.0.tgz"},
		"node_modules/@corp/secret":{"version":"1.0.0","resolved":"https://npm.corp.example/secret-1.0.0.tgz"}}}`), 0o644)
	t.Setenv("GOPRIVATE", "corp.example")
	deps := pinnedDeps(dir, &Config{})
	var names []string
	for _, d := range deps {
		names = append(names, d.eco+" "+d.name+" "+d.version)
	}
	if got, want := strings.Join(names, "\n"), "Go golang.org/x/net 0.1.0\nnpm lodash 4.17.0\nPyPI flask 0.12"; got != want {
		t.Fatalf("pinned dependencies:\n%s\nwant:\n%s", got, want) // private names must not be sent anywhere
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/querybatch" {
			fmt.Fprint(w, `{"results":[{"vulns":[{"id":"GHSA-aaaa"},{"id":"GO-2023-1"}]},{},{}]}`)
			return
		}
		fmt.Fprintf(w, `{"id":%q,"summary":"HTTP/2 flaw","aliases":["CVE-2023-1","GO-2023-1","GHSA-aaaa"],"affected":[{"package":{"name":"golang.org/x/net","ecosystem":"Go"},"ranges":[{"events":[{"introduced":"0"},{"fixed":"0.7.0"}]}]}]}`, path.Base(r.URL.Path))
	}))
	defer srv.Close()
	old := osvURL
	defer func() { osvURL = old }()
	osvURL = srv.URL
	fs, err := vulnFindings(deps)
	if err != nil || len(fs) != 1 || fs[0].File != "go.mod" || fs[0].Line != 6 ||
		fs[0].Message != "golang.org/x/net 0.1.0: GHSA-aaaa / CVE-2023-1 HTTP/2 flaw (fixed in 0.7.0)" {
		t.Errorf("findings %+v, err %v", fs, err)
	}
	srv.Close() // no network: an error, never an empty "all clear"
	if _, err := vulnFindings(deps); err == nil {
		t.Error("an unreachable service must be reported")
	}
}

func TestMCPFirstScan(t *testing.T) {
	old := firstScan
	defer func() { firstScan = old }()
	scans, calls := 0, 0
	firstScan = func() (*ScanResult, error) { scans++; return nil, nil }
	out, err := withFirstScan(func() (string, error) {
		if calls++; calls == 1 {
			return "", errNoScan
		}
		return "answer", nil
	})
	if out != "answer" || err != nil || scans != 1 || calls != 2 {
		t.Errorf("out %q, err %v, scans %d, calls %d", out, err, scans, calls)
	}
	if _, err := withFirstScan(func() (string, error) { return "", errors.New("other") }); err == nil || scans != 1 {
		t.Errorf("another error must pass through without a scan: %v, scans %d", err, scans)
	}
}
