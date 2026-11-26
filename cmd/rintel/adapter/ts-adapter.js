// rintel TypeScript / JavaScript / Vue adapter (protocol 1).
// Reads one JSON request on stdin, writes JSON Lines IR on stdout. It runs the TypeScript compiler that ships with
// rintel (./typescript), never one from the analyzed repository, and executes nothing from the repository.
"use strict";
const ts = require("./typescript/lib/typescript.js");
const path = require("path");
const fs = require("fs");

const req = JSON.parse(fs.readFileSync(0, "utf8"));
const root = req.root;
const emit = new Set(req.emit.map(f => path.join(root, f)));
const out = [];
const put = o => out.push(JSON.stringify(o));
put({ kind: "header", protocol: 1, adapter: "typescript", version: ts.version });

// ---- Vue single-file components: only the <script> blocks are code. Everything else is blanked out in place,
// so line and column numbers stay those of the .vue file.
const vueReal = f => (f.endsWith(".vue.ts") ? f.slice(0, -3) : null);
function vueScript(text) {
  let res = "", last = 0;
  const re = /<script\b[^>]*>([\s\S]*?)<\/script>/g;
  for (let m; (m = re.exec(text)); ) {
    const start = m.index + m[0].indexOf(">") + 1, end = start + m[1].length;
    res += text.slice(last, start).replace(/[^\n]/g, " ") + text.slice(start, end);
    last = end;
  }
  return res + text.slice(last).replace(/[^\n]/g, " ");
}
const realFile = sf => vueReal(sf.fileName) || sf.fileName;

// ---- program
let options = { allowJs: true, jsx: ts.JsxEmit.Preserve, target: ts.ScriptTarget.ESNext, module: ts.ModuleKind.ESNext, moduleResolution: ts.ModuleResolutionKind.Bundler };
const cfg = ts.findConfigFile(req.module, ts.sys.fileExists, "tsconfig.json") || ts.findConfigFile(req.module, ts.sys.fileExists, "jsconfig.json");
if (cfg && !path.relative(root, cfg).startsWith("..")) {
  try {
    const parsed = ts.getParsedCommandLineOfConfigFile(cfg, {}, { ...ts.sys, onUnRecoverableConfigFileDiagnostic() {} });
    if (parsed) options = { ...options, ...parsed.options };
  } catch { /* a config this compiler cannot read: analyze with defaults */ }
}
Object.assign(options, { allowJs: true, checkJs: false, noEmit: true, skipLibCheck: true, incremental: false, composite: false, tsBuildInfoFile: undefined, allowImportingTsExtensions: true });
delete options.outDir; delete options.rootDir; delete options.declaration; delete options.declarationDir;

const host = ts.createCompilerHost(options, true);
const hostExists = host.fileExists.bind(host), hostRead = host.readFile.bind(host), hostGet = host.getSourceFile.bind(host);
host.fileExists = f => (vueReal(f) ? fs.existsSync(vueReal(f)) : hostExists(f));
host.readFile = f => (vueReal(f) ? vueScript(fs.readFileSync(vueReal(f), "utf8")) : hostRead(f));
host.getSourceFile = (f, v, onErr, create) => (vueReal(f) ? ts.createSourceFile(f, host.readFile(f), v, true, ts.ScriptKind.TS) : hostGet(f, v, onErr, create));
host.resolveModuleNames = (names, containing) => names.map(n => {
  let r = ts.resolveModuleName(n, containing, options, host).resolvedModule;
  if (!r && n.endsWith(".vue")) {
    r = ts.resolveModuleName(n + ".ts", containing, options, host).resolvedModule;
    const p = n.startsWith(".") ? path.resolve(path.dirname(vueReal(containing) || containing), n) : null;
    if (!r && p && fs.existsSync(p)) r = { resolvedFileName: p + ".ts", extension: ".ts" };
  }
  return r;
});
const program = ts.createProgram(req.files.map(f => path.join(root, f)).map(f => (f.endsWith(".vue") ? f + ".ts" : f)), options, host);
const checker = program.getTypeChecker();

// ---- identifiers
const rel = f => path.relative(root, f).split(path.sep).join("/");
const noExt = r => r.replace(/\.(d\.ts|d\.mts|d\.cts|tsx?|jsx?|mts|cts|mjs|cjs|vue)$/, "");
const isRepo = f => !path.relative(root, f).startsWith("..") && !f.includes("/node_modules/");
const libDir = path.dirname(ts.getDefaultLibFilePath(options));
function pkgOf(file) { // library group of a declaration outside the repository
  if (file.startsWith(libDir)) return "lib/std";
  const i = file.lastIndexOf("/node_modules/");
  if (i < 0) return "lib/unknown";
  const parts = file.slice(i + 14).split("/");
  return "pkg/" + (parts[0].startsWith("@") ? parts[0] + "/" + parts[1] : parts[0]);
}
function declName(d) {
  if (ts.isConstructorDeclaration(d)) return "constructor";
  if (ts.isExportAssignment(d)) return "default";
  const n = d.name;
  if (!n) return d.modifiers && d.modifiers.some(m => m.kind === ts.SyntaxKind.DefaultKeyword) ? "default" : "";
  if (ts.isIdentifier(n) || ts.isPrivateIdentifier(n) || ts.isStringLiteralLike(n) || ts.isNumericLiteral(n)) return n.text;
  return "";
}
const isTop = d => ts.isSourceFile(d.parent) ||
  (ts.isVariableDeclaration(d) && d.parent && d.parent.parent && ts.isVariableStatement(d.parent.parent) && ts.isSourceFile(d.parent.parent.parent));
const isOwner = p => ts.isClassLike(p) || ts.isInterfaceDeclaration(p) || ts.isEnumDeclaration(p);
/** Stable ID of a declaration, or "" for locals, parameters and anonymous things. */
function declId(d) {
  if (!d || !d.getSourceFile) return "";
  const name = declName(d);
  if (!name) return "";
  const file = realFile(d.getSourceFile());
  let owner = d.parent;
  if (owner && ts.isTypeLiteralNode(owner) && owner.parent && ts.isTypeAliasDeclaration(owner.parent)) owner = owner.parent;
  const base = isRepo(file) ? "ts:" + noExt(rel(file)) : "ts:" + pkgOf(file);
  if (isRepo(file) ? isTop(d) : !(owner && (isOwner(owner) || ts.isTypeAliasDeclaration(owner)))) {
    if (!isRepo(file) && !(ts.isSourceFile(d.parent) || ts.isModuleBlock(d.parent) || ts.isVariableDeclarationList(d.parent))) return "";
    return base + "." + name;
  }
  if (owner && (isOwner(owner) || ts.isTypeAliasDeclaration(owner))) {
    const on = declName(owner);
    if (on && (!isRepo(file) || isTop(owner))) return base + "." + on + "#" + name;
  }
  return "";
}
const isLocal = d => { for (let p = d.parent; p; p = p.parent) if (ts.isFunctionLike(p) || ts.isBlock(p)) return true; return false; };
const inParameter = d => { for (let p = d.parent; p; p = p.parent) { if (ts.isParameter(p)) return true; if (ts.isVariableDeclaration(p)) return false; } return false; };
/** "missing-module" when the callee could not be typed because an import did not resolve, "dynamic-call" when it is `any`. */
function anyTyped(expr) {
  const t = checker.getTypeAtLocation(expr);
  return t && t.flags & ts.TypeFlags.Any ? (t.intrinsicName === "error" ? "missing-module" : "dynamic-call") : "";
}
function resolve(sym) {
  if (!sym) return undefined;
  if (sym.flags & ts.SymbolFlags.Alias) { try { sym = checker.getAliasedSymbol(sym); } catch { return undefined; } }
  return sym && sym.declarations && (sym.valueDeclaration || sym.declarations[0]);
}

// ---- output helpers
function rng(sf, node) {
  const a = sf.getLineAndCharacterOfPosition(node.getStart(sf)), b = sf.getLineAndCharacterOfPosition(node.getEnd());
  return [a.line + 1, a.character + 1, b.line + 1, b.character + 1];
}
function typeText(n) { try { return n ? n.getText().replace(/\s+/g, " ").slice(0, 80) : ""; } catch { return ""; } }
const mods = n => {
  const m = ts.canHaveModifiers(n) && ts.getModifiers(n) || [];
  const has = k => m.some(x => x.kind === k);
  const out = [];
  if (has(ts.SyntaxKind.ExportKeyword)) out.push("public");
  for (const [k, v] of [[ts.SyntaxKind.AbstractKeyword, "abstract"], [ts.SyntaxKind.StaticKeyword, "static"], [ts.SyntaxKind.PrivateKeyword, "private"], [ts.SyntaxKind.ProtectedKeyword, "protected"], [ts.SyntaxKind.ReadonlyKeyword, "final"], [ts.SyntaxKind.AsyncKeyword, "async"]]) if (has(k)) out.push(v);
  return out;
};
const annotations = n => (ts.canHaveDecorators(n) && ts.getDecorators(n) || []).map(d => ({ name: typeText(ts.isCallExpression(d.expression) ? d.expression.expression : d.expression), args: {} }));
const params = fn => (fn.parameters || []).map(p => ({ name: typeText(p.name), type: typeText(p.type) }));

/** Cyclomatic complexity, deepest nesting and statement count, the same measure as the other adapters. */
function complexity(body) {
  let cc = 1, max = 0, stmts = 0;
  const K = ts.SyntaxKind;
  const walk = (n, depth) => {
    if (!n) return;
    max = Math.max(max, depth);
    const nest = x => walk(x, depth + 1);
    switch (n.kind) {
      case K.IfStatement:
        cc++; stmts++; walk(n.expression, depth); nest(n.thenStatement);
        if (n.elseStatement) (n.elseStatement.kind === K.IfStatement ? walk(n.elseStatement, depth) : nest(n.elseStatement));
        return;
      case K.ForStatement: case K.ForInStatement: case K.ForOfStatement: case K.WhileStatement: case K.DoStatement:
        cc++; stmts++; ts.forEachChild(n, c => (c === n.statement ? nest(c) : walk(c, depth))); return;
      case K.SwitchStatement: stmts++; walk(n.expression, depth); n.caseBlock.clauses.forEach(c => { if (c.kind === K.CaseClause) cc++; c.statements.forEach(s => nest(s)); }); return;
      case K.CatchClause: cc++; nest(n.block); return;
      case K.TryStatement: stmts++; nest(n.tryBlock); walk(n.catchClause, depth); if (n.finallyBlock) nest(n.finallyBlock); return;
      case K.ConditionalExpression: cc++; break;
      case K.BinaryExpression: { const o = n.operatorToken.kind; if (o === K.AmpersandAmpersandToken || o === K.BarBarToken || o === K.QuestionQuestionToken) cc++; break; }
      case K.ArrowFunction: case K.FunctionExpression: case K.FunctionDeclaration: case K.MethodDeclaration: nest(n.body); return;
      case K.ClassDeclaration: case K.ClassExpression: return;
      case K.Block: break;
      default: if (n.kind >= K.FirstStatement && n.kind <= K.LastStatement) stmts++;
    }
    ts.forEachChild(n, c => walk(c, depth));
  };
  if (body) { if (body.kind === ts.SyntaxKind.Block) body.statements.forEach(s => walk(s, 0)); else walk(body, 0); }
  return { cc, nest: max, stmts };
}

function lineCounts(text) { // a line with code and a comment is code
  let lines = 0, code = 0, comment = 0, blank = 0, block = false, tpl = false;
  for (const line of text.split("\n")) {
    lines++;
    let hasCode = tpl, hasComment = block, str = "";
    for (let i = 0; i < line.length; i++) {
      const ch = line[i], nx = line[i + 1];
      if (block) { if (ch === "*" && nx === "/") { block = false; i++; } }
      else if (tpl) { if (ch === "\\") i++; else if (ch === "`") tpl = false; }
      else if (str) { if (ch === "\\") i++; else if (ch === str) str = ""; }
      else if (ch === "/" && nx === "/") { hasComment = true; break; }
      else if (ch === "/" && nx === "*") { block = true; hasComment = true; i++; }
      else if (ch === "`") { tpl = true; hasCode = true; }
      else if (ch === '"' || ch === "'") { str = ch; hasCode = true; }
      else if (ch !== " " && ch !== "\t" && ch !== "\r") hasCode = true;
    }
    if (hasCode) code++; else if (hasComment) comment++; else blank++;
  }
  if (text.endsWith("\n")) { lines--; blank--; }
  return { lines, code, comment, blank };
}

const ROUTERS = /^pkg\/(hono|express|@types\/express|@types\/express-serve-static-core|fastify|koa-router|@koa\/router|@hono\/[\w-]+)$/;
const VERBS = { get: "GET", post: "POST", put: "PUT", delete: "DELETE", patch: "PATCH", head: "HEAD", options: "OPTIONS", all: "ANY" };

// ---- one source file
function scan(sf) {
  const file = realFile(sf), relFile = rel(file), base = "ts:" + noExt(relFile);
  const ext = /\.([a-z]+)$/.exec(relFile)[1];
  const container = base + "." + ext;
  const seen = new Set(), used = new Set();
  const symbol = (id, type, parent, node, attrs) => { if (seen.has(id)) return; seen.add(id); put({ kind: "symbol", id, type, parent, file: relFile, range: rng(sf, node), attrs }); };
  const ref = (from, to, relation, evidence, node, via) => { if (from && to) put({ kind: "ref", from, to, via, relation, evidence, file: relFile, range: rng(sf, node) }); };
  const use = (from, to, node) => { if (!to || to === from || to.startsWith(from + "#") || used.has(from + "|" + to)) return; used.add(from + "|" + to); ref(from, to, "uses", "resolved", node); };
  const unresolved = (from, node, reason) => put({ kind: "unresolved", from, text: node.getText(sf).replace(/\s+/g, " ").slice(0, 120), reason, file: relFile, range: rng(sf, node) });

  put({ kind: "file", file: relFile, ...lineCounts(sf.text) });
  symbol(container, "class", "", sf, { kind: "file", modifiers: [] });

  function refs(node, from) {
    if (!node) return;
    const callee = new Set();
    const visit = n => {
      if (ts.isCallExpression(n) && n.expression.kind === ts.SyntaxKind.ImportKeyword) {
        // import("./x"): loads a module, calls nothing. The names used from it are referenced where they are used.
      } else if (ts.isCallExpression(n) || ts.isNewExpression(n)) {
        let name = n.expression;
        while (ts.isParenthesizedExpression(name) || ts.isNonNullExpression(name)) name = name.expression;
        if (ts.isPropertyAccessExpression(name)) name = name.name;
        callee.add(name);
        // Name the callee by its declaration (function, method, or a function-typed member of a library type).
        const d = resolve(checker.getSymbolAtLocation(name));
        let id = "";
        if (ts.isNewExpression(n)) {
          let ctor;
          try { const sig = checker.getResolvedSignature(n); ctor = sig && sig.declaration; } catch { /* malformed call */ }
          if (ctor && ts.isConstructorDeclaration(ctor)) id = declId(ctor); // a class without a constructor is covered by the `uses` edge
          else if (!d && anyTyped(n.expression)) unresolved(from, n, anyTyped(n.expression));
        } else if (d) {
          const param = ts.isParameter(d) || (ts.isBindingElement(d) && inParameter(d));
          const fnMember = (ts.isPropertySignature(d) || ts.isPropertyDeclaration(d)) && !(d.initializer && ts.isFunctionLike(d.initializer)) && isRepo(realFile(d.getSourceFile()));
          // A callback parameter or a function-typed field of our own types: the target is only known at run time.
          // A local variable holding a function stays inside this function, so nothing is hidden.
          if (param || fnMember) unresolved(from, n, "dynamic-call");
          else if (!isLocal(d) || !isRepo(realFile(d.getSourceFile()))) id = declId(d);
        } else if (anyTyped(n.expression)) unresolved(from, n, anyTyped(n.expression));
        if (id) {
          ref(from, id, "calls", "resolved", n);
          const inline = route(n, id, from);
          if (inline) { // the handler is written in place: it becomes a symbol of its own, so each endpoint has its own call graph
            visit(n.expression);
            n.arguments.slice(0, -1).forEach(visit);
            fn(inline.id, "method", container, inline.fn, inline.fn, { kind: "route", annotations: [{ name: "route", args: {} }] });
            return;
          }
        }
      } else if (ts.isJsxOpeningElement(n) || ts.isJsxSelfClosingElement(n)) {
        const tag = n.tagName, last = ts.isPropertyAccessExpression(tag) ? tag.name : tag;
        if (ts.isIdentifier(last) && /^[A-Z]/.test(last.text)) { // rendering a component invokes it
          callee.add(last);
          const d = resolve(checker.getSymbolAtLocation(last));
          const id = d ? declId(d) : "";
          if (id) ref(from, id, "calls", "resolved", n);
        }
      } else if (ts.isIdentifier(n) && !callee.has(n) && !(n.parent && n.parent.name === n && !ts.isPropertyAccessExpression(n.parent))) {
        const d = resolve(checker.getSymbolAtLocation(n));
        if (d && !ts.isParameter(d)) {
          const id = declId(d);
          if (id && !id.startsWith("ts:lib/")) use(from, id, n); // the standard library is not worth a reference edge
        }
      }
      ts.forEachChild(n, visit);
    };
    visit(node);
  }
  function route(call, calleeId, from) {
    const acc = call.expression;
    if (!ts.isPropertyAccessExpression(acc) || !VERBS[acc.name.text] || call.arguments.length < 2) return null;
    if (!ROUTERS.test(calleeId.slice(3).split(".")[0])) return null;
    const a0 = call.arguments[0];
    if (!ts.isStringLiteralLike(a0) || !a0.text.startsWith("/")) return null;
    let handler = from, inline = null;
    const lastArg = call.arguments[call.arguments.length - 1], via = VERBS[acc.name.text] + " " + a0.text;
    if (ts.isIdentifier(lastArg) || ts.isPropertyAccessExpression(lastArg)) {
      const d = resolve(checker.getSymbolAtLocation(ts.isIdentifier(lastArg) ? lastArg : lastArg.name));
      const id = d ? declId(d) : "";
      if (id && id.startsWith("ts:") && !id.startsWith("ts:pkg/") && !id.startsWith("ts:lib/")) handler = id;
    } else if (ts.isArrowFunction(lastArg) || ts.isFunctionExpression(lastArg)) {
      handler = from + (from.includes("#") ? "~" : "#") + via.replace(/[^A-Za-z0-9]+/g, "_").replace(/_+$/, "");
      inline = { id: handler, fn: lastArg };
    }
    ref(from, handler, "routes", "declared", call, via);
    return inline;
  }
  function fn(id, type, parent, node, f, extra) {
    symbol(id, type, parent, node, { modifiers: mods(node), annotations: annotations(node), params: params(f), ...(f.body ? { m: complexity(f.body) } : {}), ...extra });
    refs(f.body, id);
    for (const p of f.parameters || []) { refs(p.type, id); refs(p.initializer, id); }
    refs(f.type, id);
  }
  function heritage(node, id) {
    for (const h of node.heritageClauses || []) for (const t of h.types) {
      const d = resolve(checker.getSymbolAtLocation(t.expression));
      const to = d ? declId(d) : "";
      if (to) ref(id, to, h.token === ts.SyntaxKind.ImplementsKeyword ? "implements" : "extends", "declared", t);
    }
  }
  function overrides(cls, member, id) {
    const name = declName(member);
    for (const h of cls.heritageClauses || []) for (const t of h.types) {
      const p = checker.getTypeAtLocation(t).getProperty(name);
      const d = p && (p.valueDeclaration || (p.declarations || [])[0]);
      const to = d ? declId(d) : "";
      if (to && to !== id) ref(id, to, "overrides", "resolved", member);
    }
  }
  function members(node, id, list) {
    for (const m of list) {
      const name = declName(m);
      if (!name) continue;
      const mid = id + "#" + name;
      if (ts.isMethodDeclaration(m) || ts.isConstructorDeclaration(m) || ts.isGetAccessor(m) || ts.isSetAccessor(m) || ts.isMethodSignature(m)) {
        fn(mid, ts.isConstructorDeclaration(m) ? "constructor" : "method", id, m, m, ts.isMethodSignature(m) ? { modifiers: ["abstract"] } : {});
        if (ts.isClassLike(node)) overrides(node, m, mid);
      } else if ((ts.isPropertyDeclaration(m) || ts.isPropertySignature(m)) && m.initializer && (ts.isArrowFunction(m.initializer) || ts.isFunctionExpression(m.initializer))) {
        fn(mid, "method", id, m, m.initializer, {});
      } else if (ts.isPropertyDeclaration(m) || ts.isPropertySignature(m) || ts.isEnumMember(m)) {
        symbol(mid, "field", id, m, { type: typeText(m.type), modifiers: mods(m), annotations: annotations(m), initialized: !!m.initializer });
        refs(m.type, id); refs(m.initializer, mid);
      }
    }
  }

  for (const st of sf.statements) {
    if (ts.isFunctionDeclaration(st)) {
      const name = declName(st);
      if (name) fn(base + "." + name, "method", container, st, st, {}); else refs(st, container);
    } else if (ts.isClassDeclaration(st) && declName(st)) {
      const id = base + "." + declName(st);
      symbol(id, "class", "", st, { kind: "class", modifiers: mods(st), annotations: annotations(st) });
      heritage(st, id); members(st, id, st.members);
    } else if (ts.isInterfaceDeclaration(st)) {
      const id = base + "." + st.name.text;
      symbol(id, "class", "", st, { kind: "interface", modifiers: mods(st) });
      heritage(st, id); members(st, id, st.members);
    } else if (ts.isTypeAliasDeclaration(st)) {
      const id = base + "." + st.name.text;
      symbol(id, "class", "", st, { kind: "type", modifiers: mods(st) });
      if (ts.isTypeLiteralNode(st.type)) members(st, id, st.type.members); else refs(st.type, id);
    } else if (ts.isEnumDeclaration(st)) {
      const id = base + "." + st.name.text;
      symbol(id, "class", "", st, { kind: "enum", modifiers: mods(st) });
      members(st, id, st.members);
    } else if (ts.isVariableStatement(st)) {
      for (const d of st.declarationList.declarations) {
        if (!ts.isIdentifier(d.name)) { refs(d.initializer, container); continue; }
        const id = base + "." + d.name.text, init = d.initializer;
        if (init && (ts.isArrowFunction(init) || ts.isFunctionExpression(init))) fn(id, "method", container, st, init, { modifiers: mods(st) });
        else { symbol(id, "field", container, d, { kind: "var", type: typeText(d.type), modifiers: mods(st) }); refs(d.type, id); refs(init, id); }
      }
    } else if (ts.isExportAssignment(st)) {
      if (!ts.isIdentifier(st.expression)) { symbol(base + ".default", "field", container, st, { kind: "var", type: "", modifiers: ["public"] }); refs(st.expression, base + ".default"); }
      else refs(st.expression, container);
    } else if (!ts.isImportDeclaration(st) && !ts.isExportDeclaration(st)) {
      refs(st, container); // top-level statements: route registrations, bootstrapping
    }
  }
}

for (const sf of program.getSourceFiles()) {
  if (!emit.has(realFile(sf))) continue;
  try { scan(sf); } catch (e) { process.stderr.write(`${rel(realFile(sf))}: ${e && e.message}\n`); }
}
process.stdout.write(out.join("\n") + "\n");
