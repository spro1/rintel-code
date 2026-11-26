package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type noIn struct{}

type searchIn struct {
	Query  string `json:"query" jsonschema:"substring of a class, method or field name"`
	Kind   string `json:"kind,omitempty" jsonschema:"class, method, constructor or field"`
	Module string `json:"module,omitempty"`
	Limit  int    `json:"limit,omitempty" jsonschema:"default 50, max 200"`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page"`
}

type symbolIn struct {
	Symbol string `json:"symbol" jsonschema:"symbol ID (java:com.acme.UserService#createUser(UserDto)) or a dotted name such as UserService.createUser"`
}

type neighborsIn struct {
	Symbol    string `json:"symbol" jsonschema:"symbol ID or dotted name"`
	Direction string `json:"direction" jsonschema:"in = who depends on this symbol (callers), out = what it depends on (callees)"`
	Depth     int    `json:"depth,omitempty" jsonschema:"default 1, max 3"`
	Relation  string `json:"relation,omitempty" jsonschema:"calls, uses (type or field mentioned), extends, implements, overrides or injects; empty for all"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}

type apiIn struct {
	PathPrefix string `json:"path_prefix,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}

type metricsIn struct {
	Scope        string `json:"scope,omitempty" jsonschema:"class (default), method, package or file; the other lists come back empty"`
	Limit        int    `json:"limit,omitempty" jsonschema:"rows to return, highest first (class by WMC, method by cyclomatic complexity, file by code lines); default 30"`
	IncludeTests bool   `json:"include_tests,omitempty"`
}

type depsIn struct {
	IncludeTests bool `json:"include_tests,omitempty"`
	Detail       bool `json:"detail,omitempty" jsonschema:"also list the class pairs behind each package dependency (larger response)"`
}

type checksIn struct {
	Rule            string `json:"rule,omitempty" jsonschema:"only this rule's findings, e.g. sql-concatenation or package-cycle"`
	Category        string `json:"category,omitempty" jsonschema:"security or quality; empty for both"`
	Vulnerabilities bool   `json:"vulnerabilities,omitempty" jsonschema:"also look up known vulnerabilities in the pinned dependencies at osv.dev. Uses the network and sends package names and versions; off by default. If dependency_check.error is set in the response the lookup did not run (no network)."`
	Limit           int    `json:"limit,omitempty" jsonschema:"findings to return, worst first within each rule; default 100"`
}

type impactIn struct {
	Base  string `json:"base,omitempty" jsonschema:"git revision to compare the working tree with; default HEAD (uncommitted changes). Use origin/main for a branch."`
	Depth int    `json:"depth,omitempty" jsonschema:"how many steps of dependents to follow; default 3, max 6"`
}

type scanIn struct {
	Full bool `json:"full,omitempty" jsonschema:"re-analyze everything instead of only what changed"`
}

// tool registers a typed tool; the SDK derives input and output schemas from the Go types.
func tool[In, Out any](s *mcp.Server, name, desc string, readOnly bool, fn func(In) (Out, error)) {
	closed := false
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, IdempotentHint: true, OpenWorldHint: &closed, DestructiveHint: &closed}},
		func(_ context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
			out, err := fn(in)
			return nil, out, err
		})
}

func serveMCP(q *Q) error {
	s := mcp.NewServer(&mcp.Implementation{Name: "rintel", Version: "0.1.0"}, nil)
	korean = false // agents get one stable language
	const evidence = " Every edge carries an evidence level: declared/resolved are facts from static analysis, heuristic is an inference (rule spring-di-multiple-candidates: several beans fit; class-hierarchy: any override may be the target). Responses give locations, not source text. For impact analysis use direction=in with relation empty: it returns callers and every symbol that mentions the type or field."
	tool(s, "get_overview", "Start here. Modules, size, endpoints count, reference resolution rate and which languages are not analyzed for this repository.", true,
		func(noIn) (*Overview, error) { return q.Overview() })
	tool(s, "search_symbols", "Find classes, methods and fields by name.", true,
		func(in searchIn) (*SearchResult, error) {
			return q.Search(in.Query, in.Kind, in.Module, in.Limit, in.Cursor)
		})
	tool(s, "get_symbol", "Location of one symbol and how many edges enter and leave it.", true,
		func(in symbolIn) (*SymbolResult, error) { return q.GetSymbol(in.Symbol) })
	tool(s, "get_neighbors", "Callers/dependents (direction=in) or callees/dependencies (direction=out) of a symbol across the whole repository, up to depth 3."+evidence, true,
		func(in neighborsIn) (*NeighborsResult, error) {
			return q.Neighbors(in.Symbol, in.Direction, in.Depth, in.Relation, in.Limit, in.Cursor)
		})
	tool(s, "get_api_spec", "REST endpoints (Spring MVC) with their handler methods.", true,
		func(in apiIn) (*APIResult, error) { return q.APISpec(in.PathPrefix, in.Limit, in.Cursor) })
	tool(s, "get_metrics", "Code metrics: cyclomatic complexity, size, coupling (CBO, fan-in/out, Ca/Ce, instability) and cohesion (LCOM4). Use it to find the most complex methods, the largest or most coupled classes, or package-level structure.", true,
		func(in metricsIn) (*Metrics, error) {
			m, err := q.Metrics()
			if err != nil {
				return nil, err
			}
			if in.Limit <= 0 {
				in.Limit = 30
			}
			m = m.limit(in.Limit, in.IncludeTests)
			switch in.Scope { // one scope per call keeps the response small
			case "method":
				m.Classes, m.Packages, m.Files = []ClassMetric{}, []PackageMetric{}, []FileMetric{}
			case "package":
				m.Classes, m.Methods, m.Files = []ClassMetric{}, []MethodMetric{}, []FileMetric{}
			case "file":
				m.Classes, m.Methods, m.Packages = []ClassMetric{}, []MethodMetric{}, []PackageMetric{}
			default:
				m.Methods, m.Packages, m.Files = []MethodMetric{}, []PackageMetric{}, []FileMetric{}
			}
			return m, nil
		})
	tool(s, "get_dependencies", "Package-level dependency graph with reference counts, and dependency cycles between packages and between classes. Use it for architecture questions.", true,
		func(in depsIn) (*DepsResult, error) { return q.Deps(in.IncludeTests, in.Detail) })
	tool(s, "get_checks", "Security and quality findings, each rule with a fix: hardcoded secrets, SQL built from strings, disabled TLS verification, unsafe deserialization, eval, weak crypto, command execution and HTML injection (matched on source text: confirm before changing); complex or long methods, large or tightly coupled classes, dependency cycles, layering problems, and candidates for unused or untested code. Warnings are rule violations; notes are candidates found with static relations only.", true,
		func(in checksIn) (*CheckResult, error) {
			c, err := q.Checks()
			if err != nil {
				return nil, err
			}
			if in.Vulnerabilities {
				q.addVulns(c)
			}
			if in.Limit <= 0 {
				in.Limit = 100
			}
			kept := []Finding{}
			security := map[string]bool{}
			for _, r := range c.Rules {
				security[r.ID] = r.Category == "security"
			}
			for _, f := range c.Findings {
				if (in.Rule == "" || f.Rule == in.Rule) && (in.Category == "" || (in.Category == "security") == security[f.Rule]) && len(kept) < in.Limit {
					kept = append(kept, f)
				}
			}
			c.Findings = kept
			return c, nil
		})
	tool(s, "analyze_impact", "What a code change affects: the changed symbols (from git diff against base), the production symbols that depend on them, the endpoints involved, and the tests that reach the change. Call scan_repository first if the index is stale.", true,
		func(in impactIn) (*ImpactResult, error) {
			if in.Depth <= 0 {
				in.Depth = 3
			}
			return q.Impact(in.Base, in.Depth)
		})
	tool(s, "scan_repository", "Refresh the index after code changes (incremental). Only writes the local cache under .ri/cache. Call it when a response says scan.stale is true.", false,
		func(in scanIn) (*ScanResult, error) { return scan(q.root, q.db, in.Full) })
	return s.Run(context.Background(), &mcp.StdioTransport{})
}
