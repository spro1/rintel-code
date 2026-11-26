# rintel Python adapter (protocol 1). Reads one JSON request on stdin, writes JSON Lines IR on stdout.
# Standard library only (ast); nothing from the repository is imported or executed.
#
# Python has no static types, so every reference gets the strongest evidence that can be justified:
#   resolved   the name is bound by a def, class or import, or the receiver's class is known from a type hint,
#              a constructor call, or self
#   heuristic  the receiver is unknown but only a few classes in the repository define a method of that name
#              (rule "method-name")
#   unresolved a call through a parameter or variable holding a function, or too many candidates ("dynamic-call")
import ast
import builtins
import json
import os
import re
import sys

req = json.load(sys.stdin)
ROOT, EMIT = req["root"], set(req["emit"])
out = []


def put(**o):
    out.append(json.dumps(o, ensure_ascii=False))


put(kind="header", protocol=1, adapter="python", version="%d.%d" % sys.version_info[:2])

BUILTINS = set(dir(builtins))
STDLIB = set(getattr(sys, "stdlib_module_names", ())) | {"os", "sys", "re", "json", "typing", "collections", "itertools", "functools", "pathlib", "datetime", "logging", "subprocess", "asyncio", "abc", "dataclasses", "enum", "math", "time", "io", "unittest"}
# method names of the built-in containers: with an unknown receiver these almost never mean repository code
COMMON = set(dir(str)) | set(dir(list)) | set(dir(dict)) | set(dir(set)) | set(dir(bytes)) | {"get", "close", "read", "write", "open", "send", "run", "start", "stop"}
ROUTE_VERBS = {"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE", "patch": "PATCH", "head": "HEAD", "options": "OPTIONS", "route": "ANY", "api_route": "ANY", "websocket": "WS"}


def noext(rel):
    return re.sub(r"\.py$", "", rel)


class Cls:
    def __init__(self, cid, node, mod):
        self.id, self.node, self.mod = cid, node, mod
        self.methods, self.fields, self.ftypes = {}, {}, {}  # name -> id; field name -> annotation/constructor expr

    def bases(self):
        res = []
        for b in self.node.bases:
            t = self.mod.resolve_expr(b)
            if t and t[0] == "class":
                res.append(t[1])
        return res

    def mro(self, seen=None):
        seen = seen if seen is not None else set()
        if self.id in seen:
            return []
        seen.add(self.id)
        res = [self]
        for b in self.bases():
            res += b.mro(seen)
        return res

    def method(self, name, skip_self=False):
        for c in self.mro()[1 if skip_self else 0:]:
            if name in c.methods:
                return c.methods[name]
        return None

    def field(self, name):
        for c in self.mro():
            if name in c.fields:
                return c.fields[name], c
        return None, None


class Mod:
    def __init__(self, rel, tree, src):
        self.rel, self.tree, self.src = rel, tree, src
        self.base = "py:" + noext(rel)
        self.container = self.base + ".py"
        self.names = {}  # top-level name -> ("func", id, node) | ("class", Cls) | ("var", id) | ("mod", Mod) | ("ext", id) | ("extmod", dotted)
        self.imports = []

    def collect(self):
        for st in self.tree.body:
            if isinstance(st, (ast.FunctionDef, ast.AsyncFunctionDef)):
                self.names[st.name] = ("func", self.base + "." + st.name, st)
            elif isinstance(st, ast.ClassDef):
                c = Cls(self.base + "." + st.name, st, self)
                self.names[st.name] = ("class", c)
                for m in st.body:
                    if isinstance(m, (ast.FunctionDef, ast.AsyncFunctionDef)):
                        c.methods[m.name] = c.id + "#" + m.name
                        for n in ast.walk(m):  # self.x = ... declares a field
                            targets = n.targets if isinstance(n, ast.Assign) else [n.target] if isinstance(n, (ast.AnnAssign, ast.AugAssign)) else []
                            for t in targets:
                                if isinstance(t, ast.Attribute) and isinstance(t.value, ast.Name) and t.value.id == "self":
                                    c.fields.setdefault(t.attr, c.id + "#" + t.attr)
                                    v = n.annotation if isinstance(n, ast.AnnAssign) else getattr(n, "value", None)
                                    if v is not None:
                                        c.ftypes.setdefault(t.attr, (v, m))
                    else:
                        for t in (m.targets if isinstance(m, ast.Assign) else [m.target] if isinstance(m, ast.AnnAssign) else []):
                            if isinstance(t, ast.Name):
                                c.fields.setdefault(t.id, c.id + "#" + t.id)
                                if isinstance(m, ast.AnnAssign):
                                    c.ftypes.setdefault(t.id, (m.annotation, None))
            elif isinstance(st, (ast.Assign, ast.AnnAssign)):
                for t in (st.targets if isinstance(st, ast.Assign) else [st.target]):
                    if isinstance(t, ast.Name):
                        self.names.setdefault(t.id, ("var", self.base + "." + t.id))
                        v = st.annotation if isinstance(st, ast.AnnAssign) else st.value
                        VTYPES.setdefault(self.base + "." + t.id, (v, self))  # service = UserService(...) at module level

    def bind_imports(self):
        for st in ast.walk(self.tree):
            if isinstance(st, ast.Import):
                for a in st.names:
                    m = find_module(a.name)
                    top = a.name.split(".")[0]
                    if a.asname:
                        self.names.setdefault(a.asname, ("mod", m) if m else ("extmod", a.name))
                    else:
                        tm = find_module(top)
                        self.names.setdefault(top, ("mod", tm) if tm and "." not in a.name else ("extmod", top) if not m else ("pkg", top))
            elif isinstance(st, ast.ImportFrom):
                dotted = st.module or ""
                if st.level:  # relative import: resolve against this file's directory
                    parts = os.path.dirname(self.rel).split("/") if os.path.dirname(self.rel) else []
                    parts = parts[: len(parts) - (st.level - 1)] if st.level > 1 else parts
                    dotted = ".".join([p for p in parts if p] + ([st.module] if st.module else []))
                m = find_module(dotted, exact=bool(st.level))
                for a in st.names:
                    name = a.asname or a.name
                    sub = find_module((dotted + "." if dotted else "") + a.name, exact=bool(st.level))
                    if sub:
                        self.names.setdefault(name, ("mod", sub))
                    elif m and a.name in m.names:
                        self.names.setdefault(name, m.names[a.name])
                    elif m:
                        self.names.setdefault(name, ("var", m.base + "." + a.name))
                    else:
                        self.names.setdefault(name, ("ext", ext_id(dotted, a.name)))

    def resolve_expr(self, e, env=None):
        """What a Name or dotted attribute chain is bound to, or None."""
        if isinstance(e, ast.Name):
            if env and e.id in env:
                return env[e.id]
            return self.names.get(e.id)
        if isinstance(e, ast.Attribute):
            b = self.resolve_expr(e.value, env)
            if not b:
                return None
            if b[0] == "mod":
                return b[1].names.get(e.attr) or ("var", b[1].base + "." + e.attr)
            if b[0] in ("extmod", "pkg"):
                return ("extmod", b[1] + "." + e.attr)
            if b[0] == "ext":
                return ("ext", b[1] + "." + e.attr)
            if b[0] == "class":
                mid = b[1].method(e.attr)
                if mid:
                    return ("func", mid, None)
                fid, _ = b[1].field(e.attr)
                return ("var", fid) if fid else None
        if isinstance(e, ast.Subscript):  # Optional[X], list[X]: take the first class mentioned
            for n in ast.walk(e.slice):
                t = self.resolve_expr(n, env) if isinstance(n, (ast.Name, ast.Attribute)) else None
                if t and t[0] == "class":
                    return t
        if isinstance(e, ast.Constant) and isinstance(e.value, str):  # a quoted annotation
            try:
                return self.resolve_expr(ast.parse(e.value, mode="eval").body, env)
            except SyntaxError:
                return None
        return None


def ext_id(module, name=None):
    top = (module or "").split(".")[0]
    return "py:%s/%s%s" % ("std" if top in STDLIB else "lib", module or "unknown", "." + name if name else "")


MODS, BY_DOTTED, VTYPES = {}, {}, {}
for rel in req["files"]:
    try:
        with open(os.path.join(ROOT, rel), encoding="utf-8", errors="replace") as fh:
            src = fh.read()
        MODS[rel] = Mod(rel, ast.parse(src, filename=rel), src)
    except (SyntaxError, ValueError, OSError) as e:
        sys.stderr.write("%s: %s\n" % (rel, e))
for rel, m in MODS.items():
    parts = noext(rel).split("/")
    if parts[-1] == "__init__":
        parts = parts[:-1]
    for i in range(len(parts)):  # a file is importable by every suffix of its path, shortest prefix-stripping last
        BY_DOTTED.setdefault(".".join(parts[i:]), []).append((i, m))


def find_module(dotted, exact=False):
    c = BY_DOTTED.get(dotted)
    if not c:
        return None
    c = sorted(c, key=lambda x: x[0])
    return c[0][1] if not exact or c[0][0] == 0 else c[0][1]


for m in MODS.values():
    m.collect()
for m in MODS.values():
    m.bind_imports()
METHODS = {}  # method name -> ids across the repository, for the method-name rule
for m in MODS.values():
    for b in m.names.values():
        if b[0] == "class" and b[1].mod is m:
            for name, mid in b[1].methods.items():
                METHODS.setdefault(name, []).append(mid)


def complexity(fn):
    st = {"cc": 1, "max": 0, "stmts": 0}

    def walk(n, depth):
        st["max"] = max(st["max"], depth)
        if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef, ast.Lambda)) and n is not fn:
            if not isinstance(n, ast.ClassDef):
                for c in (n.body if isinstance(n.body, list) else [n.body]):
                    walk(c, depth + 1)
            return
        if isinstance(n, ast.stmt) and n is not fn:
            st["stmts"] += 1
        if isinstance(n, ast.If):
            st["cc"] += 1
            walk(n.test, depth)
            for c in n.body:
                walk(c, depth + 1)
            if len(n.orelse) == 1 and isinstance(n.orelse[0], ast.If):  # elif: a sibling branch
                walk(n.orelse[0], depth)
            else:
                for c in n.orelse:
                    walk(c, depth + 1)
            return
        if isinstance(n, (ast.For, ast.AsyncFor, ast.While)):
            st["cc"] += 1
            for c in ast.iter_child_nodes(n):
                walk(c, depth + 1 if isinstance(c, ast.stmt) else depth)
            return
        if isinstance(n, (ast.With, ast.AsyncWith, ast.Try)) or n.__class__.__name__ in ("TryStar", "Match"):
            for c in ast.iter_child_nodes(n):
                walk(c, depth + 1 if isinstance(c, (ast.stmt, ast.ExceptHandler)) or c.__class__.__name__ == "match_case" else depth)
            return
        if isinstance(n, ast.ExceptHandler) or n.__class__.__name__ == "match_case":
            st["cc"] += 1
            for c in ast.iter_child_nodes(n):
                walk(c, depth)
            return
        if isinstance(n, ast.BoolOp):
            st["cc"] += len(n.values) - 1
        elif isinstance(n, ast.IfExp):
            st["cc"] += 1
        elif isinstance(n, ast.comprehension):
            st["cc"] += 1 + len(n.ifs)
        for c in ast.iter_child_nodes(n):
            walk(c, depth)

    for c in fn.body:
        walk(c, 0)
    return {"cc": st["cc"], "nest": st["max"], "stmts": st["stmts"]}


def rng(n):
    start = min([n.lineno] + [d.lineno for d in getattr(n, "decorator_list", [])])
    return [start, n.col_offset + 1, getattr(n, "end_lineno", n.lineno) or n.lineno, (getattr(n, "end_col_offset", 0) or 0) + 1]


def text(mod, n):
    try:
        s = ast.get_source_segment(mod.src, n) or ""
    except Exception:
        s = ""
    return " ".join(s.split())[:120]


def dname(d):
    while isinstance(d, ast.Call):
        d = d.func
    parts = []
    while isinstance(d, ast.Attribute):
        parts.append(d.attr)
        d = d.value
    if isinstance(d, ast.Name):
        parts.append(d.id)
    return ".".join(reversed(parts))


class Scan:
    def __init__(self, mod):
        self.m, self.rel, self.used, self.local = mod, mod.rel, set(), set()

    def symbol(self, sid, typ, parent, node, attrs):
        put(kind="symbol", id=sid, type=typ, parent=parent, file=self.rel, range=rng(node) if node is not None else [1, 1, 1, 1], attrs=attrs)

    def ref(self, frm, to, relation, node, evidence="resolved", rule=None, via=None):
        if frm and to:
            o = dict(kind="ref", to=to, relation=relation, evidence=evidence, file=self.rel, range=rng(node))
            o["from"] = frm
            if rule:
                o["rule"] = rule
            if via:
                o["via"] = via
            out.append(json.dumps(o, ensure_ascii=False))

    def use(self, frm, to, node):
        if to and to != frm and not to.startswith(frm + "#") and (frm, to) not in self.used:
            self.used.add((frm, to))
            self.ref(frm, to, "uses", node)

    def unresolved(self, frm, node, reason):
        put(**{"kind": "unresolved", "from": frm, "text": text(self.m, node), "reason": reason, "file": self.rel, "range": rng(node)})

    def run(self):
        m = self.m
        lines = m.src.split("\n")
        n = len(lines) - (1 if m.src.endswith("\n") else 0)
        blank = sum(1 for l in lines[:n] if not l.strip())
        comment = sum(1 for l in lines[:n] if l.strip().startswith("#"))
        put(kind="file", file=self.rel, lines=n, code=n - blank - comment, comment=comment, blank=blank)
        put(kind="symbol", id=m.container, type="class", parent="", file=self.rel, range=[1, 1, max(n, 1), 1], attrs={"kind": "file", "modifiers": []})
        for st in m.tree.body:
            if isinstance(st, (ast.FunctionDef, ast.AsyncFunctionDef)):
                self.func(st, m.base + "." + st.name, m.container, None)
            elif isinstance(st, ast.ClassDef):
                self.cls(st)
            else:
                if isinstance(st, (ast.Assign, ast.AnnAssign)):
                    for t in (st.targets if isinstance(st, ast.Assign) else [st.target]):
                        if isinstance(t, ast.Name):
                            self.symbol(m.base + "." + t.id, "field", m.container, t, {"kind": "var", "type": "", "modifiers": vis(t.id)})
                self.body([st], m.container, None, {})

    def cls(self, node):
        m = self.m
        c = m.names[node.name][1] if m.names.get(node.name, ("",))[0] == "class" and m.names[node.name][1].node is node else Cls(m.base + "." + node.name, node, m)
        self.symbol(c.id, "class", "", node, {"kind": "class", "modifiers": vis(node.name), "annotations": [{"name": dname(d), "args": {}} for d in node.decorator_list]})
        for b in node.bases:
            t = m.resolve_expr(b)
            to = t[1].id if t and t[0] == "class" else t[1] if t and t[0] in ("ext", "var") else ext_id(*t[1].rsplit(".", 1)) if t and t[0] == "extmod" and "." in t[1] else None
            if to:
                self.ref(c.id, to, "extends", b, evidence="declared")
        for name, fid in sorted(c.fields.items()):
            if not any(isinstance(x, (ast.FunctionDef, ast.AsyncFunctionDef)) and x.name == name for x in node.body):
                self.symbol(fid, "field", c.id, node, {"type": "", "modifiers": vis(name)})
        for st in node.body:
            if isinstance(st, (ast.FunctionDef, ast.AsyncFunctionDef)):
                mid = c.id + "#" + st.name
                self.func(st, mid, c.id, c)
                sup = c.method(st.name, skip_self=True)
                if sup:
                    self.ref(mid, sup, "overrides", st)
            elif not isinstance(st, ast.ClassDef):
                self.body([st], c.id, c, {})

    def func(self, node, fid, parent, cls):
        m = self.m
        a = node.args
        ps = [{"name": p.arg, "type": text(m, p.annotation) if p.annotation else ""} for p in a.posonlyargs + a.args + a.kwonlyargs if p.arg not in ("self", "cls")]
        anns = [{"name": dname(d), "args": {}} for d in node.decorator_list]
        self.symbol(fid, "constructor" if node.name == "__init__" and cls else "method", parent, node,
                    {"modifiers": vis(node.name), "annotations": anns, "params": ps, "m": complexity(node)})
        for d in node.decorator_list:  # @app.get("/path"), @router.post("/x"), @app.route("/x", methods=[...])
            if isinstance(d, ast.Call) and isinstance(d.func, ast.Attribute) and d.func.attr in ROUTE_VERBS and d.args and isinstance(d.args[0], ast.Constant) and isinstance(d.args[0].value, str) and d.args[0].value.startswith("/"):
                verbs = [ROUTE_VERBS[d.func.attr]]
                for kw in d.keywords:
                    if kw.arg == "methods" and isinstance(kw.value, (ast.List, ast.Tuple)):
                        verbs = [str(e.value).upper() for e in kw.value.elts if isinstance(e, ast.Constant)] or verbs
                for v in verbs:
                    self.ref(m.container, fid, "routes", d, evidence="heuristic", rule="route-decorator", via=v + " " + d.args[0].value)
        env = {}
        for p in a.posonlyargs + a.args + a.kwonlyargs + ([a.vararg] if a.vararg else []) + ([a.kwarg] if a.kwarg else []):
            t = m.resolve_expr(p.annotation) if p.annotation else None
            env[p.arg] = ("inst", t[1]) if t and t[0] == "class" else ("param",)
        if cls and a.args and not any(dname(d) == "staticmethod" for d in node.decorator_list):
            env[a.args[0].arg] = ("class", cls) if any(dname(d) == "classmethod" for d in node.decorator_list) else ("inst", cls)
        for ann in [p.annotation for p in a.posonlyargs + a.args + a.kwonlyargs if p.annotation] + ([node.returns] if node.returns else []):
            self.types(ann, fid)
        self.body(node.body, fid, cls, env)

    def types(self, ann, frm):
        for n in ast.walk(ann):
            if isinstance(n, (ast.Name, ast.Attribute)):
                t = self.m.resolve_expr(n)
                if t and t[0] == "class":
                    self.use(frm, t[1].id, n)

    def typeof(self, e, env, cls):
        """The repository class an expression is an instance of, when that can be justified."""
        m = self.m
        if isinstance(e, ast.Name):
            b = env.get(e.id)
            if b:
                return b[1] if b[0] == "inst" else None
            b = m.names.get(e.id) if e.id not in self.local else None
            if b and b[0] == "var" and b[1] in VTYPES:  # a module-level object: its class is what it was constructed from
                v, vm = VTYPES[b[1]]
                t = vm.resolve_expr(v.func if isinstance(v, ast.Call) else v)
                return t[1] if t and t[0] == "class" else None
            return None
        if isinstance(e, ast.Call):
            if isinstance(e.func, ast.Name) and e.func.id == "super" and cls:
                bases = cls.bases()
                return bases[0] if bases else None
            t = m.resolve_expr(e.func, {k: v for k, v in env.items() if v[0] == "class"})
            if t and t[0] == "class":
                return t[1]
            if t and t[0] == "func" and t[2] is not None and t[2].returns is not None:  # the callee's return annotation
                r = m.resolve_expr(t[2].returns)
                return r[1] if r and r[0] == "class" else None
            return None
        if isinstance(e, ast.Attribute):
            owner = self.typeof(e.value, env, cls)
            if owner:
                for c in owner.mro():
                    if e.attr in c.ftypes:
                        v, meth = c.ftypes[e.attr]
                        if isinstance(v, ast.Call):
                            t = c.mod.resolve_expr(v.func)
                            return t[1] if t and t[0] == "class" else None
                        t = c.mod.resolve_expr(v)
                        if t and t[0] == "class":
                            return t[1]
                        if isinstance(v, ast.Name) and meth is not None:  # self.repo = repo, where repo: Repo is a parameter
                            for p in meth.args.args + meth.args.kwonlyargs:
                                if p.arg == v.id and p.annotation is not None:
                                    t = c.mod.resolve_expr(p.annotation)
                                    return t[1] if t and t[0] == "class" else None
                        return None
        return None

    def body(self, stmts, frm, cls, env):
        m = self.m
        env = dict(env)
        local = set()
        for st in stmts:
            for n in ast.walk(st):  # locals: assignment targets, nested defs, loop and with variables
                if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)) and n not in stmts:
                    local.add(n.name)
                elif isinstance(n, ast.Name) and isinstance(n.ctx, ast.Store):
                    local.add(n.id)
                if isinstance(n, ast.Assign) and len(n.targets) == 1 and isinstance(n.targets[0], ast.Name):
                    t = self.typeof(n.value, env, cls)
                    if t:
                        env[n.targets[0].id] = ("inst", t)
                elif isinstance(n, ast.AnnAssign) and isinstance(n.target, ast.Name):
                    t = m.resolve_expr(n.annotation)
                    if t and t[0] == "class":
                        env[n.target.id] = ("inst", t[1])
        outer, self.local = self.local, local
        callee = set()
        for st in stmts:
            for n in ast.walk(st):
                if isinstance(n, ast.Call):
                    callee.add(id(n.func))
                    self.call(n, frm, cls, env, local)
        for st in stmts:
            for n in ast.walk(st):
                if id(n) in callee:
                    continue
                if isinstance(n, ast.Name) and isinstance(n.ctx, ast.Load) and n.id not in env and n.id not in local:
                    b = m.names.get(n.id)
                    if b and b[0] in ("func", "var", "ext"):
                        self.use(frm, b[1], n)
                    elif b and b[0] == "class":
                        self.use(frm, b[1].id, n)
                elif isinstance(n, ast.Attribute):
                    owner = self.typeof(n.value, env, cls)
                    if owner:
                        fid, _ = owner.field(n.attr)
                        self.use(frm, fid or owner.method(n.attr), n)
                    else:
                        b = m.resolve_expr(n)
                        if b and b[0] in ("func", "var"):
                            self.use(frm, b[1], n)
        self.local = outer

    def call(self, n, frm, cls, env, local):
        m, f = self.m, n.func
        if isinstance(f, ast.Name):
            if f.id in env:
                b = env[f.id]
                if b[0] == "param":
                    self.unresolved(frm, n, "dynamic-call")  # a callback
                return
            if f.id in local:
                return  # a function defined or assigned inside this one: nothing is hidden
            b = m.names.get(f.id)
            if not b:
                return  # a builtin, or a name we know nothing about
            if b[0] == "func" or b[0] == "ext":
                self.ref(frm, b[1], "calls", n)
            elif b[0] == "class":
                init = b[1].method("__init__")
                self.ref(frm, init, "calls", n) if init else self.use(frm, b[1].id, n)
            elif b[0] == "var":
                self.unresolved(frm, n, "dynamic-call")  # a module-level variable holding a callable
            return
        if not isinstance(f, ast.Attribute):
            return
        owner = self.typeof(f.value, env, cls)
        if isinstance(f.value, ast.Name) and env.get(f.value.id, ("",))[0] == "class":
            owner = env[f.value.id][1]
        if owner:
            mid = owner.method(f.attr)
            if mid:
                self.ref(frm, mid, "calls", n)
            elif owner.field(f.attr)[0]:
                self.unresolved(frm, n, "dynamic-call")
            return  # else inherited from a base class outside the repository
        b = m.resolve_expr(f, {k: v for k, v in env.items() if v[0] == "class"})
        if b:
            if b[0] == "func":
                self.ref(frm, b[1], "calls", n)
            elif b[0] == "class":
                init = b[1].method("__init__")
                self.ref(frm, init, "calls", n) if init else self.use(frm, b[1].id, n)
            elif b[0] == "ext":
                self.ref(frm, b[1], "calls", n)
            elif b[0] == "extmod":
                self.ref(frm, ext_id(*b[1].rsplit(".", 1)) if "." in b[1] else ext_id(b[1]), "calls", n)
            elif b[0] == "var":
                self.unresolved(frm, n, "dynamic-call")
            return
        # unknown receiver: only a method name to go on
        cands = METHODS.get(f.attr, [])
        if not cands or f.attr in COMMON or f.attr.startswith("__"):
            return  # no class here defines it, or the name is too common to mean anything
        if len(cands) <= 3:
            for c in cands:
                self.ref(frm, c, "calls", n, evidence="heuristic", rule="method-name")
        else:
            self.unresolved(frm, n, "dynamic-call")


def vis(name):
    return ["private"] if name.startswith("_") and not name.startswith("__") else ["public"]


for rel in sorted(EMIT):
    if rel in MODS:
        try:
            Scan(MODS[rel]).run()
        except RecursionError as e:
            sys.stderr.write("%s: %s\n" % (rel, e))
sys.stdout.write("\n".join(out) + "\n")
