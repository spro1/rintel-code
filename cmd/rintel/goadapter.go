package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// The Go adapter runs in-process: the core is Go and the standard library already contains the parser and the
// type checker. Like the Java adapter it executes nothing from the repository and no build tool: dependencies are
// type-checked from the sources already in the module cache (signatures only), and a module that is not there
// turns into unresolved("missing-module") references.

// The adapter's own source is its version: any change to it invalidates earlier scans, with nothing to remember to bump.
//
//go:embed goadapter.go
var goAdapterSrc []byte

var goAdapterVersion = hashOf(goAdapterSrc)[:12]

type goMod struct {
	dir, path string
	requires  map[string]string // module path -> version
	replaces  map[string]string // module path -> directory or "path@version"
}

func parseGoMod(dir string) (*goMod, error) {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil, err
	}
	m := &goMod{dir: dir, requires: map[string]string{}, replaces: map[string]string{}}
	block := ""
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if block != "" {
			if f[0] == ")" {
				block = ""
				continue
			}
			f = append([]string{block}, f...)
		} else if len(f) == 2 && f[1] == "(" {
			block = f[0]
			continue
		}
		switch {
		case f[0] == "module" && len(f) >= 2:
			m.path = strings.Trim(f[1], `"`)
		case f[0] == "require" && len(f) >= 3:
			m.requires[f[1]] = f[2]
		case f[0] == "replace":
			for i, x := range f {
				if x == "=>" && i+1 < len(f) {
					target := f[i+1]
					if i+2 < len(f) {
						target += "@" + f[i+2]
					}
					m.replaces[f[1]] = target
				}
			}
		}
	}
	if m.path == "" {
		return nil, fmt.Errorf("%s: no module line", filepath.Join(dir, "go.mod"))
	}
	return m, nil
}

// goEnv asks the installed toolchain where the standard library and the module cache are.
// GOTOOLCHAIN=local keeps it from downloading a toolchain a go.mod might ask for.
var goEnv = func() (goroot, modcache string, err error) {
	cmd := exec.Command("go", "env", "GOROOT", "GOMODCACHE")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("the Go toolchain is needed to analyze Go code (its standard library sources are read): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return "", "", fmt.Errorf("unexpected `go env` output")
	}
	return lines[0], lines[1], nil
}

type goPkg struct {
	path, dir string
	repo      bool
	types     *types.Package
	info      *types.Info
	files     []*ast.File
	names     []string // file names, parallel to files
	imports   []string
	loading   bool
}

type goLoader struct {
	root, goroot, modcache string
	mod                    *goMod
	fset                   *token.FileSet
	ctx                    build.Context
	pkgs                   map[string]*goPkg
	targets                map[string]bool   // import paths whose function bodies are needed: the packages being emitted
	src                    map[string][]byte // contents of the files being emitted
	missing                map[string]bool
	fieldOwner             map[*types.Var]string
}

func newGoLoader(root, modDir string) (*goLoader, error) {
	mod, err := parseGoMod(modDir)
	if err != nil {
		return nil, err
	}
	goroot, modcache, err := goEnv()
	if err != nil {
		return nil, err
	}
	ctx := build.Default
	ctx.GOROOT = goroot
	ctx.CgoEnabled = false // the pure-Go variants of packages are enough for signatures
	return &goLoader{root: root, goroot: goroot, modcache: modcache, mod: mod, fset: token.NewFileSet(), src: map[string][]byte{}, ctx: ctx,
		pkgs: map[string]*goPkg{}, missing: map[string]bool{}, fieldOwner: map[*types.Var]string{}}, nil
}

func escapeModPath(p string) string { // the module cache spells upper-case letters as !lower
	var b strings.Builder
	for _, r := range p {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// dirFor maps an import path to a source directory: the module itself, a vendor copy, the standard library, or the module cache.
func (l *goLoader) dirFor(p string) (dir string, repo bool) {
	if p == l.mod.path || strings.HasPrefix(p, l.mod.path+"/") {
		return filepath.Join(l.mod.dir, strings.TrimPrefix(strings.TrimPrefix(p, l.mod.path), "/")), true
	}
	if d := filepath.Join(l.mod.dir, "vendor", p); isDir(d) {
		return d, false
	}
	if first, _, _ := strings.Cut(p, "/"); !strings.Contains(first, ".") {
		if d := filepath.Join(l.goroot, "src", p); isDir(d) {
			return d, false
		}
	}
	if d := filepath.Join(l.goroot, "src", "vendor", p); isDir(d) { // golang.org/x packages the standard library carries
		return d, false
	}
	best := ""
	for m := range l.mod.requires {
		if (p == m || strings.HasPrefix(p, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	for m := range l.mod.replaces {
		if (p == m || strings.HasPrefix(p, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	if best == "" {
		return "", false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(p, best), "/")
	target, version := best, l.mod.requires[best]
	if r, ok := l.mod.replaces[best]; ok {
		if strings.HasPrefix(r, ".") || strings.HasPrefix(r, "/") { // replaced by a local directory
			if !filepath.IsAbs(r) {
				r = filepath.Join(l.mod.dir, r)
			}
			return filepath.Join(r, rest), false
		}
		target, version, _ = strings.Cut(r, "@")
	}
	if d := filepath.Join(l.modcache, escapeModPath(target)+"@"+version, rest); isDir(d) {
		return d, false
	}
	return "", false
}

func isDir(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

// Import implements types.Importer over source directories.
func (l *goLoader) Import(p string) (*types.Package, error) {
	if p == "unsafe" {
		return types.Unsafe, nil
	}
	pkg, err := l.load(p)
	if err != nil {
		return nil, err
	}
	return pkg.types, nil
}

func (l *goLoader) load(p string) (*goPkg, error) {
	if pkg, ok := l.pkgs[p]; ok {
		if pkg.loading {
			return nil, fmt.Errorf("import cycle through %s", p)
		}
		if pkg.types == nil {
			return nil, fmt.Errorf("package %s could not be loaded", p)
		}
		return pkg, nil
	}
	dir, repo := l.dirFor(p)
	pkg := &goPkg{path: p, dir: dir, repo: repo, loading: true}
	l.pkgs[p] = pkg
	defer func() { pkg.loading = false }()
	if dir == "" {
		l.missing[p] = true
		return nil, fmt.Errorf("module providing %s is not in the module cache", p)
	}
	bp, err := l.ctx.ImportDir(dir, 0)
	if err != nil && len(bp.GoFiles) == 0 {
		return nil, err
	}
	l.check(pkg, bp.GoFiles, p, l.targets[p])
	return pkg, nil
}

// check parses and type-checks files as one package. Function bodies are checked only when full is set: that is where
// the time goes, and an incremental scan needs them for the changed packages alone.
func (l *goLoader) check(pkg *goPkg, names []string, checkPath string, full bool) {
	// files are parsed side by side: parsing is most of the work for a package that is only needed for its signatures
	parsed := make([]*ast.File, len(names))
	srcs := make([][]byte, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			parseSlots <- struct{}{}
			defer func() { <-parseSlots }()
			abs := filepath.Join(pkg.dir, n)
			var src any
			if full { // emitted files are read once: the parser, the line counts and unresolved-call texts share the bytes
				if b, err := os.ReadFile(abs); err == nil {
					srcs[i], src = b, b
				}
			}
			// unreadable files give nil; a file with syntax errors still yields a partial tree
			parsed[i], _ = parser.ParseFile(l.fset, abs, src, parser.ParseComments|parser.SkipObjectResolution)
		}()
	}
	wg.Wait()
	for i, n := range names {
		f, abs := parsed[i], filepath.Join(pkg.dir, n)
		if f == nil {
			continue
		}
		if srcs[i] != nil {
			l.src[abs] = srcs[i]
		}
		pkg.files = append(pkg.files, f)
		pkg.names = append(pkg.names, abs)
		for _, im := range f.Imports {
			pkg.imports = append(pkg.imports, strings.Trim(im.Path.Value, `"`))
		}
	}
	conf := types.Config{Importer: l, FakeImportC: true, IgnoreFuncBodies: !full, Error: func(error) {}, Sizes: types.SizesFor("gc", build.Default.GOARCH)}
	if full {
		pkg.info = &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	}
	pkg.types, _ = conf.Check(checkPath, l.fset, pkg.files, pkg.info)
	if !full { // only the signatures are needed from here on: the syntax trees can go
		pkg.files, pkg.names = nil, nil
	}
	if pkg.repo && pkg.types != nil {
		l.registerFields(pkg.types)
	}
}

var parseSlots = make(chan struct{}, runtime.GOMAXPROCS(0))

// release drops what only the emit step needed. Every package of a module is loaded before the first is emitted, so
// without this the trees, type maps and sources of all of them stay alive together.
func (l *goLoader) release(p *goPkg) {
	for _, n := range p.names {
		delete(l.src, n)
	}
	p.files, p.names, p.info = nil, nil, nil
}

// registerFields remembers which named struct owns each field, so a selector like s.name can point at Type#name.
func (l *goLoader) registerFields(p *types.Package) {
	for _, name := range p.Scope().Names() {
		tn, ok := p.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		if st, ok := tn.Type().Underlying().(*types.Struct); ok {
			for i := 0; i < st.NumFields(); i++ {
				l.fieldOwner[st.Field(i)] = goID(p.Path(), name) + "#" + st.Field(i).Name()
			}
		}
	}
}

func goID(pkgPath, name string) string { return "go:" + pkgPath + "." + name }

func namedOf(t types.Type) *types.Named {
	for {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Alias:
			t = types.Unalias(x)
		case *types.Named:
			return x
		default:
			return nil
		}
	}
}

// objID is the stable ID of a declared object, or "" for things that have none (locals, builtins, anonymous types).
func (l *goLoader) objID(obj types.Object) string {
	switch o := obj.(type) {
	case *types.Func:
		o = o.Origin()
		if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
			if n := namedOf(sig.Recv().Type()); n != nil && n.Obj().Pkg() != nil {
				return goID(n.Obj().Pkg().Path(), n.Obj().Name()) + "#" + o.Name()
			} else if n != nil { // universe: error.Error
				return "go:builtin." + n.Obj().Name() + "#" + o.Name()
			}
			return ""
		}
		if o.Pkg() == nil || o.Parent() != o.Pkg().Scope() || o.Name() == "init" || o.Name() == "_" {
			return ""
		}
		return goID(o.Pkg().Path(), o.Name())
	case *types.TypeName:
		if o.Pkg() == nil || o.Parent() != o.Pkg().Scope() {
			return ""
		}
		return goID(o.Pkg().Path(), o.Name())
	case *types.Var:
		if o.IsField() {
			return l.fieldOwner[o.Origin()]
		}
		if o.Pkg() == nil || o.Parent() != o.Pkg().Scope() || o.Name() == "_" {
			return ""
		}
		return goID(o.Pkg().Path(), o.Name())
	case *types.Const:
		if o.Pkg() == nil || o.Parent() != o.Pkg().Scope() || o.Name() == "_" {
			return ""
		}
		return goID(o.Pkg().Path(), o.Name())
	}
	return ""
}

// repoPackages lists the import paths of every package directory in the module.
func (l *goLoader) repoPackages() []string {
	var out []string
	filepath.WalkDir(l.mod.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != l.mod.dir && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "vendor" || name == "testdata" || name == "node_modules" || fileExists(filepath.Join(p, "go.mod"))) {
			return filepath.SkipDir
		}
		if ms, _ := filepath.Glob(filepath.Join(p, "*.go")); len(ms) > 0 {
			rel, _ := filepath.Rel(l.mod.dir, p)
			out = append(out, strings.TrimSuffix(l.mod.path+"/"+filepath.ToSlash(rel), "/."))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// goAdapter analyzes one Go module and returns IR for the files in emit (paths relative to root).
func goAdapter(root, modDir string, emit []string) ([]irLine, error) {
	l, err := newGoLoader(root, modDir)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, f := range emit {
		want[filepath.Join(root, filepath.FromSlash(f))] = true
	}
	e := &goEmitter{l: l, want: want, uses: map[string]bool{}}
	pkgs := l.repoPackages()
	dirs := map[string]string{}
	for _, p := range pkgs {
		dirs[p], _ = l.dirFor(p)
	}
	wantedIn := func(dir string, test bool) bool {
		for f := range want {
			if filepath.Dir(f) == dir && strings.HasSuffix(f, "_test.go") == test {
				return true
			}
		}
		return false
	}
	l.targets = map[string]bool{}
	for _, p := range pkgs {
		l.targets[p] = wantedIn(dirs[p], false)
	}
	// Every package of the module is loaded before anything is emitted, so "which interfaces does this type satisfy"
	// sees the whole module whatever the order, and whether the scan is full or incremental.
	for _, p := range pkgs {
		l.load(p)
	}
	for _, p := range pkgs {
		if pkg := l.pkgs[p]; l.targets[p] && pkg != nil && pkg.types != nil {
			e.pkg(pkg, false)
			l.release(pkg)
		}
		if !wantedIn(dirs[p], true) {
			continue
		}
		// tests: the package again with its _test.go files, and the external test package
		bp, _ := l.ctx.ImportDir(dirs[p], 0)
		if bp == nil {
			continue
		}
		if len(bp.TestGoFiles) > 0 {
			tp := &goPkg{path: p, dir: dirs[p], repo: true}
			l.check(tp, append(append([]string{}, bp.GoFiles...), bp.TestGoFiles...), p, true)
			e.pkg(tp, true) // the non-test files are emitted from the importable package
			l.release(tp)
		}
		if len(bp.XTestGoFiles) > 0 {
			xp := &goPkg{path: p + "_test", dir: dirs[p], repo: true}
			l.check(xp, bp.XTestGoFiles, p+"_test", true)
			e.pkg(xp, true)
			l.release(xp)
		}
	}
	return e.out, nil
}

type goEmitter struct {
	l    *goLoader
	want map[string]bool
	out  []irLine
	uses map[string]bool // from|to pairs already written
	done map[string]bool
	// interfaces a type could satisfy, listed once per package instead of once per type
	ifaces map[*types.Package][]goIface
	scopes map[*goPkg][]*types.Package
}

type goIface struct {
	tn    *types.TypeName
	iface *types.Interface
	id    string
}

// interfacesOf lists the non-generic, non-empty interfaces a package declares, in name order.
func (e *goEmitter) interfacesOf(pkg *types.Package) []goIface {
	if list, ok := e.ifaces[pkg]; ok {
		return list
	}
	list := []goIface{}
	for _, name := range pkg.Scope().Names() {
		tn, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok {
			continue
		}
		iface, ok := tn.Type().Underlying().(*types.Interface)
		if !ok || iface.NumMethods() == 0 {
			continue
		}
		if n, ok := tn.Type().(*types.Named); ok && n.TypeParams().Len() > 0 {
			continue
		}
		list = append(list, goIface{tn, iface, goID(pkg.Path(), name)})
	}
	if e.ifaces == nil {
		e.ifaces = map[*types.Package][]goIface{}
	}
	e.ifaces[pkg] = list
	return list
}

func (e *goEmitter) rel(abs string) string {
	r, _ := filepath.Rel(e.l.root, abs)
	return filepath.ToSlash(r)
}

func (e *goEmitter) rng(from, to token.Pos) []int {
	a, b := e.l.fset.Position(from), e.l.fset.Position(to)
	return []int{a.Line, a.Column, b.Line, b.Column}
}

func (e *goEmitter) symbol(id, typ, parent, file string, from, to token.Pos, attrs map[string]any) {
	if e.done == nil {
		e.done = map[string]bool{}
	}
	if e.done[id] {
		return
	}
	e.done[id] = true
	b, _ := json.Marshal(attrs)
	e.out = append(e.out, irLine{Kind: "symbol", ID: id, Type: typ, Parent: parent, File: file, Range: e.rng(from, to), Attrs: b})
}

func (e *goEmitter) ref(from, to, via, relation, evidence, file string, pos token.Pos) {
	if from == "" || to == "" {
		return
	}
	e.out = append(e.out, irLine{Kind: "ref", From: from, To: to, Via: via, Relation: relation, Evidence: evidence, File: file, Range: e.rng(pos, pos)})
}

func visibility(name string) []string {
	if ast.IsExported(name) {
		return []string{"public"}
	}
	return []string{"private"}
}

func recvName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	t := fd.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.ParenExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// pkg emits symbols and references for the wanted files of one checked package.
func (e *goEmitter) pkg(p *goPkg, testsOnly bool) {
	if p.types == nil || p.info == nil {
		return
	}
	emits := func(name string) bool { return e.want[name] && (!testsOnly || strings.HasSuffix(name, "_test.go")) }
	wanted := false
	for _, n := range p.names {
		wanted = wanted || emits(n)
	}
	if !wanted {
		return
	}
	qual := func(q *types.Package) string { return q.Name() }
	typeStr := func(x ast.Expr) string {
		if tv, ok := p.info.Types[x]; ok && tv.Type != nil {
			return types.TypeString(tv.Type, qual)
		}
		return ""
	}
	for i, f := range p.files {
		abs := p.names[i]
		if !emits(abs) {
			continue
		}
		file := e.rel(abs)
		lines, code, comment, blank := goLineCounts(e.l.src[abs])
		e.out = append(e.out, irLine{Kind: "file", File: file, Lines: lines, Code: code, Comment: comment, Blank: blank})
		container := goID(p.path, strings.ReplaceAll(filepath.Base(abs), ".", "_"))
		e.symbol(container, "class", "", file, f.Pos(), f.End(), map[string]any{"kind": "file", "modifiers": []string{}})
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == "_" {
					continue
				}
				id, parent := goID(p.path, d.Name.Name), container
				if r := recvName(d); r != "" {
					id, parent = goID(p.path, r)+"#"+d.Name.Name, goID(p.path, r)
				} else if d.Name.Name == "init" {
					id = container + "#init" // a package may have many
				}
				params := []map[string]string{}
				if d.Type.Params != nil {
					for _, fl := range d.Type.Params.List {
						names := fl.Names
						if len(names) == 0 {
							names = []*ast.Ident{{Name: "_"}}
						}
						for _, n := range names {
							params = append(params, map[string]string{"name": n.Name, "type": typeStr(fl.Type)})
						}
					}
				}
				attrs := map[string]any{"modifiers": visibility(d.Name.Name), "params": params}
				if d.Body != nil {
					c := &goComplexity{cc: 1}
					c.block(d.Body, 0)
					attrs["m"] = map[string]int{"cc": c.cc, "nest": c.max, "stmts": c.stmts}
				}
				e.symbol(id, "method", parent, file, d.Pos(), d.End(), attrs)
				if d.Body != nil {
					e.refs(p, d.Body, id, file)
				}
				e.typeRefs(p, d.Type, id, file)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						e.typeDecl(p, s, d, file)
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.Name == "_" {
								continue
							}
							kind := "var"
							if d.Tok == token.CONST {
								kind = "const"
							}
							t := ""
							if obj := p.info.Defs[n]; obj != nil {
								t = types.TypeString(obj.Type(), qual)
							}
							e.symbol(goID(p.path, n.Name), "field", container, file, n.Pos(), n.End(), map[string]any{"kind": kind, "type": t, "modifiers": visibility(n.Name)})
						}
						for _, v := range s.Values {
							e.refs(p, v, container, file)
						}
						if s.Type != nil {
							e.typeRefs(p, s.Type, container, file)
						}
					}
				}
			}
		}
	}
}

func (e *goEmitter) typeDecl(p *goPkg, s *ast.TypeSpec, d *ast.GenDecl, file string) {
	if s.Name.Name == "_" {
		return
	}
	id := goID(p.path, s.Name.Name)
	kind := "type"
	from, to := s.Pos(), s.End()
	if len(d.Specs) == 1 {
		from = d.Pos()
	}
	switch t := s.Type.(type) {
	case *ast.StructType:
		kind = "struct"
		e.symbol(id, "class", "", file, from, to, map[string]any{"kind": kind, "modifiers": visibility(s.Name.Name)})
		for _, fl := range t.Fields.List {
			if len(fl.Names) == 0 { // embedding is the closest Go has to extends
				if tv, ok := p.info.Types[fl.Type]; ok {
					if n := namedOf(tv.Type); n != nil {
						e.ref(id, e.l.objID(n.Obj()), "", "extends", "declared", file, fl.Pos())
					}
				}
				continue
			}
			for _, n := range fl.Names {
				if n.Name == "_" {
					continue
				}
				ft := ""
				if tv, ok := p.info.Types[fl.Type]; ok && tv.Type != nil {
					ft = types.TypeString(tv.Type, func(q *types.Package) string { return q.Name() })
				}
				e.symbol(id+"#"+n.Name, "field", id, file, n.Pos(), n.End(), map[string]any{"type": ft, "modifiers": visibility(n.Name)})
			}
		}
	case *ast.InterfaceType:
		kind = "interface"
		e.symbol(id, "class", "", file, from, to, map[string]any{"kind": kind, "modifiers": visibility(s.Name.Name)})
		for _, m := range t.Methods.List {
			if len(m.Names) == 0 {
				if tv, ok := p.info.Types[m.Type]; ok {
					if n := namedOf(tv.Type); n != nil {
						e.ref(id, e.l.objID(n.Obj()), "", "extends", "declared", file, m.Pos())
					}
				}
				continue
			}
			for _, n := range m.Names {
				e.symbol(id+"#"+n.Name, "method", id, file, m.Pos(), m.End(), map[string]any{"modifiers": append(visibility(n.Name), "abstract"), "params": []any{}})
			}
		}
	default:
		e.symbol(id, "class", "", file, from, to, map[string]any{"kind": kind, "modifiers": visibility(s.Name.Name)})
	}
	e.typeRefs(p, s.Type, id, file)
	e.implements(p, s, id, file)
}

// implements links a concrete type to every interface it satisfies among the repository's and its direct imports'.
// Go satisfaction is structural, so this is computed, not read: resolved, not declared.
func (e *goEmitter) implements(p *goPkg, s *ast.TypeSpec, id, file string) {
	tn, ok := p.info.Defs[s.Name].(*types.TypeName)
	if !ok || types.IsInterface(tn.Type()) {
		return
	}
	named, ok := tn.Type().(*types.Named)
	if !ok || named.TypeParams().Len() > 0 {
		return
	}
	ptr := types.NewPointer(named)
	if types.NewMethodSet(ptr).Len() == 0 {
		return
	}
	scopes, ok := e.scopes[p]
	if !ok { // the package itself, what it imports, and every package of the repository
		scopes = []*types.Package{p.types}
		seen := map[string]bool{p.path: true}
		add := func(path string) {
			if q, ok := e.l.pkgs[path]; ok && q.types != nil && !seen[path] {
				seen[path] = true
				scopes = append(scopes, q.types)
			}
		}
		for _, im := range p.imports {
			add(im)
		}
		for path, q := range e.l.pkgs {
			if q.repo {
				add(path)
			}
		}
		sort.Slice(scopes[1:], func(i, j int) bool { return scopes[1+i].Path() < scopes[1+j].Path() })
		if e.scopes == nil {
			e.scopes = map[*goPkg][]*types.Package{}
		}
		e.scopes[p] = scopes
	}
	mset := types.NewMethodSet(ptr)
	has := make(map[string]bool, mset.Len())
	for i := 0; i < mset.Len(); i++ {
		has[mset.At(i).Obj().Name()] = true
	}
	try := func(itn *types.TypeName, iface *types.Interface, iid string) {
		if itn.Type() == tn.Type() {
			return
		}
		for i := 0; i < iface.NumMethods(); i++ { // cheap rejection by method names before the real check
			if !has[iface.Method(i).Name()] {
				return
			}
		}
		if !types.Implements(named, iface) && !types.Implements(ptr, iface) {
			return
		}
		e.ref(id, iid, "", "implements", "resolved", file, s.Pos())
		for i := 0; i < iface.NumMethods(); i++ {
			m := iface.Method(i)
			if obj, _, _ := types.LookupFieldOrMethod(ptr, true, tn.Pkg(), m.Name()); obj != nil {
				if fn, ok := obj.(*types.Func); ok {
					e.ref(e.l.objID(fn), e.l.objID(m), "", "overrides", "resolved", file, s.Pos())
				}
			}
		}
	}
	for _, sc := range scopes {
		for _, it := range e.interfacesOf(sc) {
			if sc == p.types || it.tn.Exported() {
				try(it.tn, it.iface, it.id)
			}
		}
	}
	if et, ok := types.Universe.Lookup("error").(*types.TypeName); ok {
		try(et, et.Type().Underlying().(*types.Interface), "go:builtin.error")
	}
}

// typeRefs records the named types a declaration mentions (parameter, result and field types).
func (e *goEmitter) typeRefs(p *goPkg, x ast.Node, from, file string) {
	ast.Inspect(x, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if tn, ok := p.info.Uses[id].(*types.TypeName); ok {
				e.use(from, e.l.objID(tn), file, id.Pos())
			}
		}
		return true
	})
}

func (e *goEmitter) use(from, to, file string, pos token.Pos) {
	if to == "" || to == from || strings.HasPrefix(to, from+"#") || e.uses[from+"|"+to] {
		return
	}
	e.uses[from+"|"+to] = true
	e.ref(from, to, "", "uses", "resolved", file, pos)
}

func (e *goEmitter) unresolved(from string, x ast.Node, reason, file string) {
	start, end := e.l.fset.Position(x.Pos()), e.l.fset.Position(x.End())
	text := ""
	if src := e.l.src[start.Filename]; start.Offset < end.Offset && end.Offset <= len(src) {
		text = strings.Join(strings.Fields(string(src[start.Offset:end.Offset])), " ")
	}
	if len(text) > 120 {
		text = text[:120]
	}
	e.out = append(e.out, irLine{Kind: "unresolved", From: from, Text: text, Reason: reason, File: file, Range: e.rng(x.Pos(), x.Pos())})
}

var routerPackages = map[string]bool{"net/http": true, "github.com/gin-gonic/gin": true, "github.com/labstack/echo/v4": true, "github.com/go-chi/chi/v5": true,
	"github.com/gorilla/mux": true, "github.com/gofiber/fiber/v2": true, "github.com/julienschmidt/httprouter": true}
var routeVerbs = map[string]string{"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE", "PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
	"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE", "Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
	"Handle": "ANY", "HandleFunc": "ANY", "Any": "ANY"}

// refs walks a function body or an initializer: calls, references to declared things, and HTTP route registrations.
func (e *goEmitter) refs(p *goPkg, body ast.Node, from, file string) {
	callee := map[*ast.Ident]bool{}
	inline := map[*ast.FuncLit]string{} // route handlers written in place, analyzed as symbols of their own
	ast.Inspect(body, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok && inline[fl] != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := ast.Unparen(call.Fun)
		for {
			if ix, ok := fun.(*ast.IndexExpr); ok {
				fun = ix.X
			} else if ix, ok := fun.(*ast.IndexListExpr); ok {
				fun = ix.X
			} else {
				break
			}
		}
		var id *ast.Ident
		switch f := fun.(type) {
		case *ast.Ident:
			id = f
		case *ast.SelectorExpr:
			id = f.Sel
		}
		tv := p.info.Types[fun]
		switch obj := p.info.Uses[id].(type) {
		case *types.Func:
			callee[id] = true
			to := e.l.objID(obj)
			if to == "" { // a method of an anonymous interface or a local function: nothing to name
				return true
			}
			e.ref(from, to, "", "calls", "resolved", file, call.Pos())
			if fl, id := e.route(p, call, obj, from, file); fl != nil {
				inline[fl] = id
			}
		case *types.Builtin, *types.TypeName, *types.PkgName:
			// built-in or a conversion: not a call into code
		default:
			if id != nil {
				callee[id] = true
			}
			if _, isLit := fun.(*ast.FuncLit); isLit {
				return true
			}
			// A local variable holding a function (a closure defined a few lines up, a cancel func) stays inside this
			// function: its body is already attributed here, so the call hides nothing from the graph.
			if v, ok := p.info.Uses[id].(*types.Var); ok && v.Kind() == types.LocalVar {
				return true
			}
			if tv.IsType() {
				return true // conversion to an unnamed or generic type
			}
			if tv.Type == nil || tv.Type == types.Typ[types.Invalid] {
				e.unresolved(from, call, "missing-module", file)
			} else if _, ok := tv.Type.Underlying().(*types.Signature); ok {
				e.unresolved(from, call, "dynamic-call", file) // a function value: the target is only known at run time
			}
		}
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok && inline[fl] != "" {
			return false
		}
		id, ok := n.(*ast.Ident)
		if !ok || callee[id] {
			return true
		}
		switch obj := p.info.Uses[id].(type) {
		case *types.TypeName, *types.Const, *types.Func:
			e.use(from, e.l.objID(obj), file, id.Pos())
		case *types.Var:
			if obj.IsField() || (obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope()) {
				e.use(from, e.l.objID(obj), file, id.Pos())
			}
		}
		return true
	})
}

// route recognizes mux.HandleFunc("GET /x", h), r.GET("/x", h) and the like on known router types.
// A handler given as a function literal becomes a symbol of its own (returned so the caller analyzes its body under
// that symbol), which gives every endpoint its own call graph.
func (e *goEmitter) route(p *goPkg, call *ast.CallExpr, fn *types.Func, from, file string) (*ast.FuncLit, string) {
	verb, ok := routeVerbs[fn.Name()]
	if !ok || fn.Pkg() == nil || !routerPackages[fn.Pkg().Path()] || len(call.Args) < 2 {
		return nil, ""
	}
	tv, ok := p.info.Types[call.Args[0]]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return nil, ""
	}
	pattern := constant.StringVal(tv.Value)
	if m, rest, found := strings.Cut(pattern, " "); found && routeVerbs[m] != "" && strings.HasPrefix(rest, "/") { // net/http "GET /path"
		verb, pattern = m, rest
	}
	if !strings.HasPrefix(pattern, "/") {
		return nil, ""
	}
	handler := from
	last := ast.Unparen(call.Args[len(call.Args)-1])
	via := verb + " " + path.Clean(pattern)
	if fl, ok := last.(*ast.FuncLit); ok {
		sep := "#"
		if strings.Contains(from, "#") {
			sep = "~"
		}
		id := from + sep + strings.TrimRight(routeIDChars.ReplaceAllString(via, "_"), "_")
		c := &goComplexity{cc: 1}
		c.block(fl.Body, 0)
		container := goID(p.path, strings.ReplaceAll(path.Base(file), ".", "_"))
		e.symbol(id, "method", container, file, fl.Pos(), fl.End(), map[string]any{"kind": "route", "modifiers": []string{}, "params": []any{},
			"annotations": []map[string]any{{"name": "route", "args": map[string]any{}}}, "m": map[string]int{"cc": c.cc, "nest": c.max, "stmts": c.stmts}})
		e.ref(from, id, via, "routes", "declared", file, call.Pos())
		e.refs(p, fl.Body, id, file)
		return fl, id
	}
	var id *ast.Ident
	switch h := last.(type) {
	case *ast.Ident:
		id = h
	case *ast.SelectorExpr:
		id = h.Sel
	}
	if f, ok := p.info.Uses[id].(*types.Func); ok && id != nil {
		if hid := e.l.objID(f); hid != "" {
			handler = hid
		}
	}
	e.ref(from, handler, via, "routes", "declared", file, call.Pos())
	return nil, ""
}

var routeIDChars = regexp.MustCompile(`[^A-Za-z0-9]+`)

// goComplexity mirrors the Java adapter's measure: 1 + if, for, case, select case, && and ||.
type goComplexity struct{ cc, max, stmts int }

func (c *goComplexity) block(n ast.Node, depth int) {
	if n == nil {
		return
	}
	c.max = max(c.max, depth)
	ast.Inspect(n, func(x ast.Node) bool {
		if x == n {
			return true
		}
		switch s := x.(type) {
		case *ast.IfStmt:
			c.cc++
			c.stmts++
			c.expr(s.Init)
			c.expr(s.Cond)
			c.block(s.Body, depth+1)
			if _, elseIf := s.Else.(*ast.IfStmt); elseIf {
				c.block(&ast.BlockStmt{List: []ast.Stmt{s.Else}}, depth)
			} else {
				c.block(s.Else, depth+1)
			}
			return false
		case *ast.ForStmt:
			c.cc++
			c.stmts++
			c.expr(s.Cond)
			c.block(s.Body, depth+1)
			return false
		case *ast.RangeStmt:
			c.cc++
			c.stmts++
			c.expr(s.X)
			c.block(s.Body, depth+1)
			return false
		case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			c.stmts++
			var body *ast.BlockStmt
			switch sw := s.(type) {
			case *ast.SwitchStmt:
				c.expr(sw.Tag)
				body = sw.Body
			case *ast.TypeSwitchStmt:
				body = sw.Body
			case *ast.SelectStmt:
				body = sw.Body
			}
			c.max = max(c.max, depth+1)
			for _, cl := range body.List {
				switch cl := cl.(type) {
				case *ast.CaseClause:
					if len(cl.List) > 0 {
						c.cc++
					}
					c.block(&ast.BlockStmt{List: cl.Body}, depth+1)
				case *ast.CommClause:
					if cl.Comm != nil {
						c.cc++
					}
					c.block(&ast.BlockStmt{List: cl.Body}, depth+1)
				}
			}
			return false
		case *ast.FuncLit:
			c.block(s.Body, depth+1)
			return false
		case *ast.BinaryExpr:
			if s.Op == token.LAND || s.Op == token.LOR {
				c.cc++
			}
		case ast.Stmt:
			if _, isBlock := s.(*ast.BlockStmt); !isBlock {
				c.stmts++
			}
		}
		return true
	})
}

func (c *goComplexity) expr(n ast.Node) {
	if n == nil {
		return
	}
	ast.Inspect(n, func(x ast.Node) bool {
		if b, ok := x.(*ast.BinaryExpr); ok && (b.Op == token.LAND || b.Op == token.LOR) {
			c.cc++
		}
		if fl, ok := x.(*ast.FuncLit); ok {
			c.block(fl.Body, 1)
			return false
		}
		return true
	})
}

// goLineCounts classifies lines the same way the Java adapter does; a line with code and a comment is code.
func goLineCounts(src []byte) (lines, code, comment, blank int) {
	inBlock, inRaw := false, false
	for _, line := range strings.Split(string(src), "\n") {
		lines++
		hasCode, hasComment := inRaw, inBlock
		var str byte
		for i := 0; i < len(line); i++ {
			ch := line[i]
			var nx byte
			if i+1 < len(line) {
				nx = line[i+1]
			}
			switch {
			case inBlock:
				if ch == '*' && nx == '/' {
					inBlock = false
					i++
				}
			case inRaw:
				if ch == '`' {
					inRaw = false
				}
			case str != 0:
				if ch == '\\' {
					i++
				} else if ch == str {
					str = 0
				}
			case ch == '/' && nx == '/':
				hasComment = true
				i = len(line)
			case ch == '/' && nx == '*':
				inBlock, hasComment = true, true
				i++
			case ch == '`':
				inRaw, hasCode = true, true
			case ch == '"' || ch == '\'':
				str, hasCode = ch, true
			case ch != ' ' && ch != '\t' && ch != '\r':
				hasCode = true
			}
		}
		switch {
		case hasCode:
			code++
		case hasComment:
			comment++
		default:
			blank++
		}
	}
	if n := len(src); n > 0 && src[n-1] == '\n' { // the final newline does not start a line
		lines--
		blank--
	}
	return
}
