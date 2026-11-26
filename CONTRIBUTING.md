# Contributing

Issues and pull requests are welcome.

## Before you send a change

```sh
make test     # go vet + go test
make vuln     # govulncheck must print "No vulnerabilities found."
gofmt -l cmd  # must print nothing
```

Tests for a language are skipped when its toolchain is missing, so run them with a JDK 17+, Node.js 18+ and Python 3.8+
installed if your change touches an adapter.

## What a change should keep true

- **Incremental equals full.** After any edit, an incremental scan must give exactly what a full scan gives. The
  `equalAfter` tests check this for each language; add a step there when you add a kind of relation.
- **Nothing from the analyzed repository runs, and nothing is downloaded.**
- **Facts and inferences stay apart.** A relation that is not established by the language's own resolution is `heuristic`
  and names its rule; what cannot be resolved is recorded as unresolved with a reason, not dropped.
- **Dependencies** are added only when the standard library cannot do the job, and only in versions with no known
  vulnerabilities.

## Layout

- `cmd/rintel/` — the CLI, the scanner, the SQLite store, queries, metrics, checks, the MCP server and the report
- `cmd/rintel/adapter/` — language adapters that run as separate processes (Java, TypeScript, Python) and the bundled
  TypeScript compiler; the Go adapter is `cmd/rintel/goadapter.go`
- `cmd/rintel/testdata/` — one small fixture repository per language

A deliberate simplification is marked with a `shortcut:` comment that says what the limit is and when to replace it.
