import com.sun.source.tree.*;
import com.sun.source.util.*;
import java.io.*;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.*;
import javax.lang.model.element.*;
import javax.lang.model.type.*;
import javax.lang.model.util.Elements;
import javax.lang.model.util.Types;
import javax.tools.*;

/**
 * rintel Java adapter (protocol 1). Reads one JSON request on stdin, writes JSON Lines IR on stdout.
 * Type resolution is done by javac itself; nothing from the repository is executed (-proc:none).
 * Lombok is handled by handing javac sources with member stubs added in memory (see Delombok).
 */
public class Adapter {
    static final PrintStream OUT = new PrintStream(new FileOutputStream(FileDescriptor.out), false, StandardCharsets.UTF_8);
    static JavaCompiler compiler;
    static StandardJavaFileManager std;
    static Trees trees;
    static Types types;
    static Elements els;
    static String base; // repo root; emitted paths are relative to it
    static final Set<String> notFound = new TreeSet<>();
    static final Set<String> missing = new TreeSet<>(); // packages javac could not find: the core maps them to jars

    public static void main(String[] a) throws Exception {
        Map<String, Object> req = (Map<String, Object>) new Json(new String(System.in.readAllBytes(), StandardCharsets.UTF_8)).value();
        base = (String) req.get("root");
        List<String> emit = (List<String>) req.get("emit");
        OUT.println("{\"kind\":\"header\",\"protocol\":1,\"adapter\":\"java\"}");

        compiler = ToolProvider.getSystemJavaCompiler();
        std = compiler.getStandardFileManager(d -> {}, null, StandardCharsets.UTF_8);
        List<String> opts = new ArrayList<>(List.of("-proc:none", "-implicit:none", "-Xlint:none",
                // keep attributing after errors: missing classpath must degrade, not abort
                "-XDshould-stop.ifError=FLOW", "-XDshould-stop.at=FLOW",
                "-sourcepath", String.join(File.pathSeparator, (List<String>) req.get("source_roots"))));
        List<String> cp = (List<String>) req.get("classpath");
        if (cp != null && !cp.isEmpty()) { opts.add("-classpath"); opts.add(String.join(File.pathSeparator, cp)); }
        List<JavaFileObject> units = new ArrayList<>();
        for (String f : emit) for (JavaFileObject o : std.getJavaFileObjects(new File(base, f))) units.add(new Source(o));
        java.util.regex.Pattern noClass = java.util.regex.Pattern.compile("class file for ([\\w.$]+) not found");
        JavacTask task = (JavacTask) compiler.getTask(null, new Sources(std), d -> {
            java.util.regex.Matcher m = noClass.matcher(d.getMessage(Locale.ENGLISH)); // a supertype living in a jar nobody imports
            if (m.find()) notFound.add(m.group(1)); // looked up after analysis: Elements is unusable while javac is still entering
        }, opts, null, units);
        task.setLocale(Locale.ENGLISH);
        trees = Trees.instance(task);
        types = task.getTypes();
        els = task.getElements();
        List<CompilationUnitTree> cus = new ArrayList<>();
        for (CompilationUnitTree cu : task.parse()) cus.add(cu);
        try { task.analyze(); } catch (Throwable t) { System.err.println("analyze: " + t); }
        for (String cls : notFound) missingClass(cls);
        for (CompilationUnitTree cu : cus) {
            try {
                Scan sc = new Scan(cu);
                sc.scan(cu, null);
                JavaFileObject f = cu.getSourceFile();
                OUT.println("{\"kind\":\"file\",\"file\":" + Scan.q(sc.file) + "," + lineCounts((f instanceof Source s ? s.raw() : f).getCharContent(true)) + "}");
            } catch (Throwable t) { System.err.println(cu.getSourceFile().getName() + ": " + t); }
            for (ImportTree i : cu.getImports()) {
                String q = i.getQualifiedIdentifier().toString();
                boolean star = q.endsWith(".*");
                if (star) q = q.substring(0, q.length() - 2);
                if (i.isStatic() && !star) q = q.substring(0, q.lastIndexOf('.'));
                if (star && !i.isStatic()) { if (els.getPackageElement(q) == null && els.getTypeElement(q) == null) missing.add(q); }
                else if (els.getTypeElement(q) == null) missingClass(q);
            }
        }
        for (String pkg : missing) OUT.println("{\"kind\":\"missing\",\"text\":" + Scan.q(pkg) + "}");
        OUT.flush();
    }

    /** Lines of a source file by kind. A line with both code and a comment counts as code. */
    static String lineCounts(CharSequence src) {
        int lines = 0, code = 0, comment = 0, blank = 0;
        boolean inBlock = false;
        for (String line : src.toString().split("\n", -1)) {
            lines++;
            boolean hasCode = false, hasComment = inBlock;
            char str = 0;
            for (int i = 0; i < line.length(); i++) {
                char ch = line.charAt(i), nx = i + 1 < line.length() ? line.charAt(i + 1) : 0;
                if (inBlock) { if (ch == '*' && nx == '/') { inBlock = false; i++; } continue; }
                if (str != 0) { if (ch == '\\') i++; else if (ch == str) str = 0; continue; }
                if (ch == '/' && nx == '/') { hasComment = true; break; }
                if (ch == '/' && nx == '*') { inBlock = true; hasComment = true; i++; continue; }
                if (ch == '"' || ch == '\'') { str = ch; hasCode = true; continue; }
                if (!Character.isWhitespace(ch)) hasCode = true;
            }
            if (hasCode) code++; else if (hasComment) comment++; else blank++;
        }
        if (src.length() > 0 && src.charAt(src.length() - 1) == '\n') { lines--; blank--; } // the final newline does not start a line
        return "\"lines\":" + lines + ",\"code\":" + code + ",\"comment\":" + comment + ",\"blank\":" + blank;
    }

    /** Cyclomatic complexity, deepest nesting and statement count of one method body. Nested classes are not part of it. */
    static class Complexity extends TreeScanner<Void, Void> {
        int cc = 1, nest, max, stmts;
        void nested(Tree t) { nest++; max = Math.max(max, nest); scan(t, null); nest--; }
        @Override public Void scan(Tree t, Void v) { if (t instanceof StatementTree && !(t instanceof BlockTree)) stmts++; return super.scan(t, v); }
        @Override public Void visitClass(ClassTree t, Void v) { return null; }
        @Override public Void visitIf(IfTree t, Void v) {
            cc++; scan(t.getCondition(), null); nested(t.getThenStatement());
            if (t.getElseStatement() instanceof IfTree) scan(t.getElseStatement(), null); // else-if is a sibling branch, not a deeper one
            else if (t.getElseStatement() != null) nested(t.getElseStatement());
            return null;
        }
        @Override public Void visitForLoop(ForLoopTree t, Void v) { cc++; scan(t.getInitializer(), null); scan(t.getCondition(), null); scan(t.getUpdate(), null); nested(t.getStatement()); return null; }
        @Override public Void visitEnhancedForLoop(EnhancedForLoopTree t, Void v) { cc++; scan(t.getExpression(), null); nested(t.getStatement()); return null; }
        @Override public Void visitWhileLoop(WhileLoopTree t, Void v) { cc++; scan(t.getCondition(), null); nested(t.getStatement()); return null; }
        @Override public Void visitDoWhileLoop(DoWhileLoopTree t, Void v) { cc++; nested(t.getStatement()); scan(t.getCondition(), null); return null; }
        @Override public Void visitCase(CaseTree t, Void v) { if (!t.toString().trim().startsWith("default")) cc++; return super.visitCase(t, v); }
        @Override public Void visitSwitch(SwitchTree t, Void v) { scan(t.getExpression(), null); nest++; max = Math.max(max, nest); scan(t.getCases(), null); nest--; return null; }
        @Override public Void visitCatch(CatchTree t, Void v) { cc++; nested(t.getBlock()); return null; }
        @Override public Void visitConditionalExpression(ConditionalExpressionTree t, Void v) { cc++; return super.visitConditionalExpression(t, v); }
        @Override public Void visitBinary(BinaryTree t, Void v) { if (t.getKind() == Tree.Kind.CONDITIONAL_AND || t.getKind() == Tree.Kind.CONDITIONAL_OR) cc++; return super.visitBinary(t, v); }
        @Override public Void visitLambdaExpression(LambdaExpressionTree t, Void v) { scan(t.getParameters(), null); nested(t.getBody()); return null; }
    }

    /**
     * a.b.Outer.Inner is ambiguous: report the longest lowercase-led prefix as the package.
     * The package existing on the classpath proves nothing: another jar may hold the same package without this class.
     */
    static void missingClass(String q) {
        for (int i = q.lastIndexOf('.'); i > 0; i = q.lastIndexOf('.', i - 1)) {
            String pkg = q.substring(0, i);
            if (Character.isLowerCase(pkg.charAt(pkg.lastIndexOf('.') + 1))) { missing.add(pkg); break; }
        }
    }

    // ---- sources as javac sees them: the file on disk plus Lombok member stubs ----

    static class Source extends ForwardingJavaFileObject<JavaFileObject> {
        String text;
        Source(JavaFileObject o) { super(o); }
        JavaFileObject raw() { return fileObject; }
        @Override public CharSequence getCharContent(boolean ignore) throws IOException {
            if (text == null) text = Delombok.apply(fileObject.getCharContent(ignore).toString());
            return text;
        }
    }

    /** Wraps every source javac loads (compilation units and sourcepath lookups alike). */
    static class Sources extends ForwardingJavaFileManager<StandardJavaFileManager> {
        Sources(StandardJavaFileManager fm) { super(fm); }
        static JavaFileObject wrap(JavaFileObject o) { return o != null && o.getKind() == JavaFileObject.Kind.SOURCE && !(o instanceof Source) ? new Source(o) : o; }
        static FileObject raw(FileObject o) { return o instanceof Source s ? s.raw() : o; }
        @Override public Iterable<JavaFileObject> list(Location l, String pkg, Set<JavaFileObject.Kind> kinds, boolean recurse) throws IOException {
            List<JavaFileObject> out = new ArrayList<>();
            for (JavaFileObject o : super.list(l, pkg, kinds, recurse)) out.add(wrap(o));
            return out;
        }
        @Override public JavaFileObject getJavaFileForInput(Location l, String cls, JavaFileObject.Kind k) throws IOException { return wrap(super.getJavaFileForInput(l, cls, k)); }
        @Override public String inferBinaryName(Location l, JavaFileObject f) { return super.inferBinaryName(l, (JavaFileObject) raw(f)); }
        @Override public boolean isSameFile(FileObject a, FileObject b) { return super.isSameFile(raw(a), raw(b)); }
        @Override public boolean contains(Location l, FileObject f) throws IOException { return super.contains(l, raw(f)); }
    }

    /**
     * Adds the members Lombok would generate, as stubs, right before each class's closing brace.
     * Same line as the brace, so no line number moves. Bodies are irrelevant: only signatures feed attribution.
     * shortcut: @Singular, @With, @Delegate, @Accessors(fluent) and @Builder on methods/constructors are not expanded; calls to those stay unresolved.
     */
    static class Delombok {
        static final Map<String, String> LOGS = Map.of("Slf4j", "org.slf4j.Logger", "XSlf4j", "org.slf4j.ext.XLogger", "Log4j2", "org.apache.logging.log4j.Logger",
                "Log4j", "org.apache.log4j.Logger", "Log", "java.util.logging.Logger", "CommonsLog", "org.apache.commons.logging.Log", "JBossLog", "org.jboss.logging.Logger");
        static final String G = "@lombok.Generated ";

        static String apply(String src) {
            if (!src.contains("lombok")) return src;
            try {
                JavaFileObject so = new SimpleJavaFileObject(URI.create("string:///Delombok.java"), JavaFileObject.Kind.SOURCE) {
                    @Override public CharSequence getCharContent(boolean ignore) { return src; }
                };
                JavacTask t = (JavacTask) compiler.getTask(null, std, d -> {}, List.of("-proc:none"), null, List.of(so));
                CompilationUnitTree cu = t.parse().iterator().next();
                SourcePositions sp = Trees.instance(t).getSourcePositions();
                TreeMap<Integer, String> inserts = new TreeMap<>(Comparator.reverseOrder());
                new TreeScanner<Void, Void>() {
                    @Override public Void visitClass(ClassTree ct, Void v) {
                        String stubs = stubs(ct);
                        int at = (int) sp.getEndPosition(cu, ct) - 1;
                        if (!stubs.isEmpty() && at > 0 && at < src.length() && src.charAt(at) == '}') inserts.put(at, stubs);
                        return super.visitClass(ct, v);
                    }
                }.scan(cu, null);
                StringBuilder sb = new StringBuilder(src);
                for (Map.Entry<Integer, String> e : inserts.entrySet()) sb.insert(e.getKey(), e.getValue()); // last position first
                return sb.toString();
            } catch (Throwable t) {
                return src; // a file we cannot pre-parse is analyzed as written
            }
        }

        static String ann(ModifiersTree m, String... names) {
            for (AnnotationTree a : m.getAnnotations()) {
                String n = a.getAnnotationType().toString();
                n = n.substring(n.lastIndexOf('.') + 1);
                for (String x : names) if (n.equals(x)) return a.toString();
            }
            return null;
        }
        static String cap(String n) { return Character.toUpperCase(n.charAt(0)) + n.substring(1); }
        static boolean isPrefixed(String n) { return n.startsWith("is") && n.length() > 2 && Character.isUpperCase(n.charAt(2)); }

        static String stubs(ClassTree ct) {
            Tree.Kind kind = ct.getKind();
            String name = ct.getSimpleName().toString();
            if (name.isEmpty() || (kind != Tree.Kind.CLASS && kind != Tree.Kind.ENUM && kind != Tree.Kind.RECORD)) return "";
            ModifiersTree cm = ct.getModifiers();
            StringJoiner decl = new StringJoiner(",", "<", ">"), use = new StringJoiner(",", "<", ">");
            for (TypeParameterTree tp : ct.getTypeParameters()) { decl.add(tp.toString()); use.add(tp.getName()); }
            boolean generic = !ct.getTypeParameters().isEmpty();
            String tpDecl = generic ? decl.toString() : "", self = name + (generic ? use.toString() : "");

            List<VariableTree> fields = new ArrayList<>();
            Set<String> declared = new HashSet<>(); // name/arity of explicit members; a stub must not collide with them
            boolean explicitCtor = false;
            for (Tree m : ct.getMembers()) {
                if (m instanceof VariableTree f) {
                    if (!f.getModifiers().getFlags().contains(Modifier.STATIC)) fields.add(f);
                    declared.add(f.getName().toString());
                } else if (m instanceof MethodTree mt) {
                    declared.add(mt.getName() + "/" + mt.getParameters().size());
                    if (mt.getName().contentEquals("<init>")) explicitCtor = true;
                } else if (m instanceof ClassTree c) declared.add(c.getSimpleName().toString());
            }
            StringBuilder sb = new StringBuilder();
            String pub = G + "public ";
            boolean value = ann(cm, "Value") != null, data = ann(cm, "Data") != null;
            String accessors = ann(cm, "Accessors");
            boolean chain = accessors != null && accessors.contains("chain") && accessors.contains("true");

            if (kind != Tree.Kind.RECORD) {
                for (VariableTree f : fields) {
                    String n = f.getName().toString(), type = f.getType().toString();
                    boolean bool = type.equals("boolean"), fin = f.getModifiers().getFlags().contains(Modifier.FINAL) || value;
                    String g = ann(f.getModifiers(), "Getter"), s = ann(f.getModifiers(), "Setter");
                    if ((g != null || data || value || ann(cm, "Getter") != null) && (g == null || !g.contains("NONE"))) {
                        String gn = bool ? (isPrefixed(n) ? n : "is" + cap(n)) : "get" + cap(n);
                        if (declared.add(gn + "/0")) sb.append(pub).append(type).append(' ').append(gn).append("(){return this.").append(n).append(";}");
                    }
                    if ((s != null || data || ann(cm, "Setter") != null) && !fin && (s == null || !s.contains("NONE"))) {
                        String sn = "set" + (bool && isPrefixed(n) ? n.substring(2) : cap(n));
                        if (declared.add(sn + "/1")) sb.append(pub).append(chain ? self : "void").append(' ').append(sn).append('(').append(type).append(' ').append(n)
                                .append(chain ? "){return this;}" : "){}");
                    }
                }
                StringJoiner all = new StringJoiner(","), req = new StringJoiner(",");
                int nAll = 0, nReq = 0;
                for (VariableTree f : fields) {
                    boolean fin = f.getModifiers().getFlags().contains(Modifier.FINAL) || value, init = f.getInitializer() != null;
                    String p = f.getType() + " " + f.getName();
                    if (!(fin && init)) { all.add(p); nAll++; }
                    if ((fin || ann(f.getModifiers(), "NonNull") != null) && !init) { req.add(p); nReq++; }
                }
                boolean noArgs = ann(cm, "NoArgsConstructor") != null, allArgs = ann(cm, "AllArgsConstructor") != null, reqArgs = ann(cm, "RequiredArgsConstructor") != null;
                boolean builder = ann(cm, "Builder", "SuperBuilder") != null;
                String cpub = G + (kind == Tree.Kind.ENUM ? "" : "public ");
                if (noArgs && declared.add("<init>/0")) sb.append(cpub).append(name).append("(){}");
                if ((allArgs || ((value || builder) && !explicitCtor && !noArgs && !reqArgs)) && declared.add("<init>/" + nAll))
                    sb.append(cpub).append(name).append('(').append(all).append("){}");
                if ((reqArgs || (data && !explicitCtor && !noArgs && !allArgs && !value)) && declared.add("<init>/" + nReq))
                    sb.append(cpub).append(name).append('(').append(req).append("){}");
            }
            String b = ann(cm, "Builder", "SuperBuilder");
            if (b != null) {
                String bn = name + "Builder", bself = bn + (generic ? use.toString() : "");
                if (declared.add("builder/0")) sb.append(pub).append("static ").append(tpDecl).append(' ').append(bself).append(" builder(){return null;}");
                if (b.contains("toBuilder") && declared.add("toBuilder/0")) sb.append(pub).append(bself).append(" toBuilder(){return null;}");
                if (declared.add(bn)) { // a hand-written XBuilder class is left alone
                    sb.append(pub).append("static class ").append(bn).append(tpDecl).append('{');
                    for (VariableTree f : fields)
                        sb.append(pub).append(bself).append(' ').append(f.getName()).append('(').append(f.getType()).append(' ').append(f.getName()).append("){return this;}");
                    sb.append(pub).append(self).append(" build(){return null;}}");
                }
            }
            if (kind != Tree.Kind.ENUM || !fields.isEmpty())
                for (Map.Entry<String, String> l : LOGS.entrySet())
                    if (ann(cm, l.getKey()) != null && declared.add("log")) sb.append(G).append("private static final ").append(l.getValue()).append(" log = null;");
            return sb.toString();
        }
    }

    // ---- IR extraction ----

    static class Scan extends TreePathScanner<Void, Void> {
        final CompilationUnitTree cu;
        final String file;
        final Deque<String> owner = new ArrayDeque<>();        // enclosing named symbol id
        final Deque<TypeElement> classes = new ArrayDeque<>();
        final Set<String> emitted = new HashSet<>();           // symbol ids and from|to pairs of `uses` edges already written

        Scan(CompilationUnitTree cu) {
            this.cu = cu;
            this.file = new File(base).toURI().relativize(cu.getSourceFile().toUri()).getPath();
        }

        // ---- names ----
        String qname(TypeMirror t) {
            if (t == null) return "";
            if (t.getKind() == TypeKind.ERROR) return viaImports(t.toString());
            t = types.erasure(t);
            if (t.getKind() == TypeKind.DECLARED) return ((TypeElement) ((DeclaredType) t).asElement()).getQualifiedName().toString();
            return t.toString();
        }
        /** Missing-classpath types only have the name as written; recover the FQN from the imports. */
        String viaImports(String name) {
            name = name.replaceAll("<.*", "");
            if (name.contains(".")) return name;
            for (ImportTree i : cu.getImports()) {
                String q = i.getQualifiedIdentifier().toString();
                if (q.endsWith("." + name)) return q;
            }
            return name;
        }
        String typeName(Tree typeTree, TreePath parent) {
            TypeMirror t = trees.getTypeMirror(new TreePath(parent, typeTree));
            return t == null || t.getKind() == TypeKind.ERROR ? viaImports(typeTree.toString()) : qname(t);
        }
        static String simple(String q) { return q.replaceAll("[\\w$]+\\.", ""); }
        String typeId(TypeElement e) { return "java:" + e.getQualifiedName(); }
        String methodId(ExecutableElement m) {
            Element o = m.getEnclosingElement();
            if (!(o instanceof TypeElement te) || te.getQualifiedName().isEmpty()) return null;
            StringJoiner sig = new StringJoiner(",", "(", ")");
            for (VariableElement p : m.getParameters()) sig.add(simple(qname(p.asType())));
            return typeId(te) + "#" + m.getSimpleName() + sig;
        }

        // ---- output ----
        String rangeJson(Tree t) {
            SourcePositions sp = trees.getSourcePositions();
            long s = sp.getStartPosition(cu, t), e = sp.getEndPosition(cu, t);
            if (s < 0) return "[0,0,0,0]";
            if (e < 0) e = s;
            LineMap lm = cu.getLineMap();
            return "[" + lm.getLineNumber(s) + "," + lm.getColumnNumber(s) + "," + lm.getLineNumber(e) + "," + lm.getColumnNumber(e) + "]";
        }
        static String q(String s) {
            StringBuilder b = new StringBuilder("\"");
            for (char ch : s.toCharArray()) {
                switch (ch) {
                    case '"' -> b.append("\\\"");
                    case '\\' -> b.append("\\\\");
                    case '\n' -> b.append("\\n");
                    case '\r' -> b.append("\\r");
                    case '\t' -> b.append("\\t");
                    default -> { if (ch < 0x20) b.append(String.format("\\u%04x", (int) ch)); else b.append(ch); }
                }
            }
            return b.append('"').toString();
        }
        static String arr(Collection<String> xs) {
            StringJoiner j = new StringJoiner(",", "[", "]");
            for (String x : xs) j.add(q(x));
            return j.toString();
        }
        void symbol(String id, String type, Tree t, String attrs) {
            if (!emitted.add(id)) return;
            if (attrs.contains("\"synthetic\":\"lombok\"")) { // the stub sits on the closing brace; the annotation that causes it is on the class
                for (TreePath p = getCurrentPath(); p != null; p = p.getParentPath())
                    if (p.getLeaf() instanceof ClassTree c && !(c == t) && !generated(c.getModifiers())) { t = c; break; }
            }
            OUT.println("{\"kind\":\"symbol\",\"id\":" + q(id) + ",\"type\":" + q(type) + ",\"parent\":" + (owner.isEmpty() ? "null" : q(owner.peek()))
                    + ",\"file\":" + q(file) + ",\"range\":" + rangeJson(t) + ",\"attrs\":{" + attrs + "}}");
        }
        void ref(String to, String via, String relation, String evidence, Tree at) {
            if (owner.isEmpty() || to == null) return;
            OUT.println("{\"kind\":\"ref\",\"from\":" + q(owner.peek()) + ",\"to\":" + q(to) + (via != null && !via.isEmpty() ? ",\"via\":" + q(via) : "")
                    + ",\"relation\":" + q(relation) + ",\"evidence\":" + q(evidence) + ",\"file\":" + q(file) + ",\"range\":" + rangeJson(at) + "}");
        }
        void unresolved(Tree t, String reason) {
            if (owner.isEmpty()) return;
            String text = t.toString();
            if (text.length() > 120) text = text.substring(0, 120);
            OUT.println("{\"kind\":\"unresolved\",\"from\":" + q(owner.peek()) + ",\"text\":" + q(text) + ",\"reason\":" + q(reason)
                    + ",\"file\":" + q(file) + ",\"range\":" + rangeJson(t) + "}");
        }

        // ---- annotations ----
        List<String> annValue(ExpressionTree e, TreePath parent) {
            List<String> out = new ArrayList<>();
            if (e instanceof NewArrayTree na) {
                if (na.getInitializers() != null) for (ExpressionTree x : na.getInitializers()) out.addAll(annValue(x, parent));
            } else if (e instanceof LiteralTree l) {
                out.add(String.valueOf(l.getValue()));
            } else {
                Element el = trees.getElement(new TreePath(parent, e));
                if (el instanceof VariableElement v && v.getConstantValue() != null) out.add(String.valueOf(v.getConstantValue()));
                else if (e instanceof MemberSelectTree ms) out.add(ms.getIdentifier().toString()); // enum constant, X.class
                else out.add(e.toString());
            }
            return out;
        }
        /** Annotations of the declaration at `decl` (class, method, field or parameter path). */
        String annotations(ModifiersTree mods, TreePath decl) {
            StringJoiner j = new StringJoiner(",", "[", "]");
            for (AnnotationTree a : mods.getAnnotations()) {
                TreePath ap = new TreePath(new TreePath(decl, mods), a);
                StringJoiner args = new StringJoiner(",", "{", "}");
                for (ExpressionTree arg : a.getArguments()) {
                    if (arg instanceof AssignmentTree as) args.add(q(as.getVariable().toString()) + ":" + arr(annValue(as.getExpression(), new TreePath(ap, as))));
                    else args.add("\"value\":" + arr(annValue(arg, ap)));
                }
                j.add("{\"name\":" + q(typeName(a.getAnnotationType(), ap)) + ",\"args\":" + args + "}");
            }
            return j.toString();
        }
        static boolean generated(ModifiersTree m) {
            for (AnnotationTree a : m.getAnnotations()) if (a.getAnnotationType().toString().equals("lombok.Generated")) return true;
            return false;
        }
        static String mods(ModifiersTree m) {
            List<String> xs = new ArrayList<>();
            for (Modifier x : m.getFlags()) xs.add(x.toString());
            return arr(xs) + (generated(m) ? ",\"synthetic\":\"lombok\"" : "");
        }

        // ---- declarations ----
        @Override public Void visitClass(ClassTree ct, Void v) {
            Element e = trees.getElement(getCurrentPath());
            if (!(e instanceof TypeElement te) || te.getQualifiedName().isEmpty()) return super.visitClass(ct, v); // anonymous/local: body belongs to the enclosing symbol
            String id = typeId(te);
            symbol(id, "class", ct, "\"kind\":" + q(ct.getKind().toString().toLowerCase()) + ",\"modifiers\":" + mods(ct.getModifiers())
                    + ",\"annotations\":" + annotations(ct.getModifiers(), getCurrentPath()));
            owner.push(id);
            classes.push(te);
            if (ct.getExtendsClause() != null) ref("java:" + typeName(ct.getExtendsClause(), getCurrentPath()), null, "extends", "declared", ct.getExtendsClause());
            for (Tree i : ct.getImplementsClause())
                ref("java:" + typeName(i, getCurrentPath()), null, ct.getKind() == Tree.Kind.INTERFACE ? "extends" : "implements", "declared", i);
            for (RecordComponentElement rc : te.getRecordComponents()) // accessors exist without a declaration
                if (ct.getMembers().stream().noneMatch(m -> m instanceof MethodTree mt && mt.getName().contentEquals(rc.getSimpleName())
                        && mt.getParameters().isEmpty() && trees.getSourcePositions().getEndPosition(cu, mt) >= 0))
                    symbol(id + "#" + rc.getSimpleName() + "()", "method", ct, "\"synthetic\":\"implicit\"");
            super.visitClass(ct, v);
            classes.pop();
            owner.pop();
            return null;
        }

        @Override public Void visitMethod(MethodTree mt, Void v) {
            Element e = trees.getElement(getCurrentPath());
            String id = e instanceof ExecutableElement ee ? methodId(ee) : null;
            if (id == null) return super.visitMethod(mt, v);
            boolean ctor = mt.getName().contentEquals("<init>");
            if (trees.getSourcePositions().getEndPosition(cu, mt) < 0) { // compiler-generated: default constructor, record members
                symbol(id, ctor ? "constructor" : "method", getCurrentPath().getParentPath().getLeaf(), "\"synthetic\":\"implicit\"");
                return null;
            }
            StringJoiner ps = new StringJoiner(",", "[", "]");
            for (VariableTree p : mt.getParameters()) {
                TreePath pp = new TreePath(getCurrentPath(), p);
                ps.add("{\"name\":" + q(p.getName().toString()) + ",\"type\":" + q(typeName(p.getType(), pp)) + ",\"annotations\":" + annotations(p.getModifiers(), pp) + "}");
            }
            symbol(id, ctor ? "constructor" : "method", mt, "\"modifiers\":" + mods(mt.getModifiers()) + ",\"annotations\":" + annotations(mt.getModifiers(), getCurrentPath())
                    + ",\"params\":" + ps + ",\"returns\":" + q(ctor ? "void" : typeName(mt.getReturnType(), getCurrentPath())) + metrics(mt));
            owner.push(id);
            if (!ctor && !e.getModifiers().contains(Modifier.STATIC) && !e.getModifiers().contains(Modifier.PRIVATE)) overrides((ExecutableElement) e, mt);
            super.visitMethod(mt, v);
            owner.pop();
            return null;
        }

        static String metrics(MethodTree mt) {
            if (mt.getBody() == null || generated(mt.getModifiers())) return "";
            Complexity c = new Complexity();
            c.scan(mt.getBody(), null);
            return ",\"m\":{\"cc\":" + c.cc + ",\"nest\":" + c.max + ",\"stmts\":" + c.stmts + "}";
        }

        void overrides(ExecutableElement m, Tree at) {
            TypeElement own = (TypeElement) m.getEnclosingElement();
            Deque<TypeMirror> todo = new ArrayDeque<>(types.directSupertypes(own.asType()));
            Set<String> seen = new HashSet<>();
            while (!todo.isEmpty()) {
                TypeMirror t = todo.pop();
                if (t.getKind() != TypeKind.DECLARED || !seen.add(qname(t))) continue;
                TypeElement st = (TypeElement) ((DeclaredType) t).asElement();
                for (Element x : st.getEnclosedElements())
                    if (x instanceof ExecutableElement sm && sm.getSimpleName().equals(m.getSimpleName()) && els.overrides(m, sm, own))
                        ref(methodId(sm), null, "overrides", "resolved", at);
                todo.addAll(types.directSupertypes(t));
            }
        }

        @Override public Void visitVariable(VariableTree vt, Void v) {
            if (getCurrentPath().getParentPath().getLeaf() instanceof ClassTree && !owner.isEmpty() && !classes.isEmpty()
                    && owner.peek().equals(typeId(classes.peek()))) {
                symbol(owner.peek() + "#" + vt.getName(), "field", vt, "\"type\":" + q(typeName(vt.getType(), getCurrentPath()))
                        + ",\"modifiers\":" + mods(vt.getModifiers()) + ",\"annotations\":" + annotations(vt.getModifiers(), getCurrentPath())
                        + ",\"initialized\":" + (vt.getInitializer() != null));
            }
            return super.visitVariable(vt, v);
        }

        // ---- references ----
        /** `uses`: which symbols mention a type or touch a field. One edge per (from, to). */
        void use(Tree t) {
            Element e = trees.getElement(getCurrentPath());
            String to = null;
            if (e instanceof TypeElement te && !te.getQualifiedName().isEmpty()) {
                if (te.asType().getKind() != TypeKind.DECLARED) {
                    // a name javac could not resolve. Recorded so that an incremental scan revisits this file once the type exists.
                    // Qualifiers and method selects are skipped: the enclosing name or call is reported instead.
                    Tree parent = getCurrentPath().getParentPath().getLeaf();
                    // `value`, `field`… of an annotation whose own type is missing: the annotation is reported, not each argument name
                    if (parent instanceof AssignmentTree as && as.getVariable() == t
                            && getCurrentPath().getParentPath().getParentPath().getLeaf() instanceof AnnotationTree) return;
                    if (!(parent instanceof MemberSelectTree) && !(parent instanceof MethodInvocationTree mi && mi.getMethodSelect() == t)
                            && !t.toString().startsWith("lombok.") && !owner.isEmpty() && emitted.add(owner.peek() + "|?" + t)) {
                        unresolved(t, "missing-type");
                        if (t.toString().contains(".")) missingClass(t.toString()); // written fully qualified, so no import names its package (e.g. the logger of @Slf4j)
                    }
                    return;
                }
                if (classes.contains(te)) return;
                to = typeId(te);
            } else if (e instanceof VariableElement ve && (e.getKind() == ElementKind.FIELD || e.getKind() == ElementKind.ENUM_CONSTANT)
                    && e.getEnclosingElement() instanceof TypeElement te && !te.getQualifiedName().isEmpty()
                    && !ve.getSimpleName().contentEquals("this") && !ve.getSimpleName().contentEquals("super")
                    && !ve.getSimpleName().contentEquals("class")) { // X.class is a mention of the type, already recorded, not a field
                to = typeId(te) + "#" + ve.getSimpleName();
            }
            if (to == null || to.startsWith("java:java.") || owner.isEmpty() || !emitted.add(owner.peek() + "|" + to)) return;
            ref(to, null, "uses", "resolved", t);
        }
        @Override public Void visitIdentifier(IdentifierTree t, Void v) { use(t); return super.visitIdentifier(t, v); }
        @Override public Void visitMemberSelect(MemberSelectTree t, Void v) { use(t); return super.visitMemberSelect(t, v); }

        void call(Tree t, Element e, String via) {
            if (e instanceof ExecutableElement ee && methodId(ee) != null) {
                ref(methodId(ee), via, "calls", "resolved", t);
                if (methodId(ee).equals("java:java.lang.reflect.Method#invoke(Object,Object[])")) unresolved(t, "reflection");
            } else unresolved(t, "missing-classpath");
        }

        @Override public Void visitMethodInvocation(MethodInvocationTree mi, Void v) {
            super.visitMethodInvocation(mi, v);
            Element e = trees.getElement(new TreePath(getCurrentPath(), mi.getMethodSelect()));
            String via = null; // static type of the receiver when it differs from the declaring type (inherited methods)
            if (e instanceof ExecutableElement ee && mi.getMethodSelect() instanceof MemberSelectTree ms) {
                TypeMirror rt = trees.getTypeMirror(new TreePath(new TreePath(getCurrentPath(), ms), ms.getExpression()));
                String r = rt == null || rt.getKind() != TypeKind.DECLARED ? "" : qname(rt);
                if (!r.isEmpty() && !r.equals(((TypeElement) ee.getEnclosingElement()).getQualifiedName().toString())) via = "java:" + r;
            }
            call(mi, e, via);
            return null;
        }
        @Override public Void visitNewClass(NewClassTree nc, Void v) {
            super.visitNewClass(nc, v);
            Element e = trees.getElement(getCurrentPath());
            if (nc.getClassBody() == null) call(nc, e, null); // an anonymous class has no constructor of its own to point at; its type is a `uses` edge
            return null;
        }
        @Override public Void visitMemberReference(MemberReferenceTree mr, Void v) {
            super.visitMemberReference(mr, v);
            call(mr, trees.getElement(getCurrentPath()), null);
            return null;
        }
    }

    /** Minimal JSON reader for the request (the JDK has none). */
    static class Json {
        final String s; int i;
        Json(String s) { this.s = s; }
        void ws() { while (i < s.length() && Character.isWhitespace(s.charAt(i))) i++; }
        Object value() {
            ws();
            char c = s.charAt(i);
            if (c == '{') {
                Map<String, Object> m = new LinkedHashMap<>();
                i++; ws();
                if (s.charAt(i) == '}') { i++; return m; }
                while (true) {
                    ws(); String k = str(); ws(); i++; // ':'
                    m.put(k, value()); ws();
                    if (s.charAt(i++) == '}') return m;
                }
            }
            if (c == '[') {
                List<Object> l = new ArrayList<>();
                i++; ws();
                if (s.charAt(i) == ']') { i++; return l; }
                while (true) { l.add(value()); ws(); if (s.charAt(i++) == ']') return l; }
            }
            if (c == '"') return str();
            int st = i;
            while (i < s.length() && ",]} \n\r\t".indexOf(s.charAt(i)) < 0) i++;
            String t = s.substring(st, i);
            return t.equals("null") ? null : t.equals("true") ? Boolean.TRUE : t.equals("false") ? Boolean.FALSE : (Object) Double.valueOf(t);
        }
        String str() {
            StringBuilder b = new StringBuilder();
            i++;
            while (s.charAt(i) != '"') {
                char c = s.charAt(i++);
                if (c != '\\') { b.append(c); continue; }
                c = s.charAt(i++);
                switch (c) {
                    case 'n' -> b.append('\n'); case 't' -> b.append('\t'); case 'r' -> b.append('\r');
                    case 'b' -> b.append('\b'); case 'f' -> b.append('\f');
                    case 'u' -> { b.append((char) Integer.parseInt(s.substring(i, i + 4), 16)); i += 4; }
                    default -> b.append(c);
                }
            }
            i++;
            return b.toString();
        }
    }
}
