<h1 align="center">rintel</h1>

<p align="center">
  <b>Repository Intelligence.</b> Who calls what, what a change breaks, and what to fix,<br>
  answered by static analysis, with the evidence for every answer.
</p>

<p align="center">
  <a href="https://github.com/spro1/rintel-code/actions/workflows/ci.yml"><img src="https://github.com/spro1/rintel-code/actions/workflows/ci.yml/badge.svg" alt="ci"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="license: Apache-2.0"></a>
  <img src="https://img.shields.io/badge/go-1.27%2B-00ADD8.svg" alt="Go 1.27+">
  <img src="https://img.shields.io/badge/analyzes-Java%20%C2%B7%20Go%20%C2%B7%20TypeScript%20%C2%B7%20Python-informational.svg" alt="analyzes Java, Go, TypeScript, Python">
</p>

<p align="center">English · <a href="README.ko.md">한국어</a></p>

![Structure view: callers of a repository method, grouped by file](docs/img/structure.png)

rintel reads a repository once, keeps a local index, and answers questions that text search gets wrong: overloads,
interfaces, dependency injection, and the difference between production code and tests. Use it from the terminal,
from a coding agent, or as a single HTML report.

```
$ rintel callers OwnerRepository.findById
OwnerRepository.findById(Integer)                    owner/OwnerRepository.java:60
  ← OwnerController.findOwner(Integer)               owner/OwnerController.java:67
  ← OwnerController.showOwner(int)                   owner/OwnerController.java:181
  ← PetController.findOwner(int)                     owner/PetController.java:69
  ...
```

**New here?** Start with the [guides](docs/README.md): [getting started](docs/getting-started.md), [reviewing a change](docs/code-review.md), [CI](docs/ci.md), [Claude Code](docs/claude-code.md). Changes are in the [changelog](CHANGELOG.md).

## Contents

- [Why rintel](#why-rintel)
- [Install](#install)
- [Quick start](#quick-start)
- [What you can ask](#what-you-can-ask)
- [Security and quality checks](#security-and-quality-checks)
- [The HTML report](#the-html-report)
- [Use it from a coding agent](#use-it-from-a-coding-agent)
- [Languages](#languages)
- [Configuration](#configuration)
- [How to read the results](#how-to-read-the-results)
- [Scope and limits](#scope-and-limits)
- [Roadmap](#roadmap)
- [Contributing](#contributing)

## Why rintel

- **Local and inert.** Nothing leaves the machine and nothing from the repository runs: no build tool, no
  annotation processor, no `go list`, nothing from `node_modules`, no Python import. Symbolic links are not
  followed. It is built to be pointed at code you do not trust; [SECURITY.md](SECURITY.md) says exactly what that
  covers. The one network feature, `check --vulns`, is off until you ask for it.
- **Evidence on every answer.** A fact read from the code is unmarked. A guess names the rule that made it. A
  place analysis could not see through is recorded as *unresolved* with a reason, so an empty answer can be told
  apart from a blind spot.
- **Findings come with a fix.** Every security and quality rule says what to change, in the terminal, in JSON and
  SARIF, and to an agent.
- **One index, three ways in.** A CLI, an [MCP](https://modelcontextprotocol.io) server with skills for coding
  agents, and a self-contained HTML report.

## Install

```sh
go install github.com/spro1/rintel-code/cmd/rintel@latest
```

This puts `rintel` in `$(go env GOPATH)/bin`; make sure that directory is on your `PATH`.
To build from source (Go 1.27.4 or newer): `git clone https://github.com/spro1/rintel-code && cd rintel && make build`.

## Quick start

```sh
cd your-repository
rintel doctor          # which languages can be analyzed on this machine
rintel stats           # scans, then says how much was resolved
rintel report --open   # the HTML report
```

There is no separate scan step. Every command refreshes the index when files changed. The index lives in
`.ri/cache/`, which rintel git-ignores by itself.

## What you can ask

| Question | Command |
|---|---|
| Who calls this? | `rintel callers UserService.createUser --depth 2` |
| What does it call? | `rintel callees UserService.createUser` |
| Where is this type or field used? | `rintel refs UserDto` |
| What is it called, exactly? | `rintel find User` |
| What does my change affect? Which tests should I run? | `rintel impact --base origin/main` |
| Which REST endpoints exist? | `rintel api --prefix /users` |
| Is anything insecure or badly structured? | `rintel check` |
| Where is the complexity? | `rintel metrics method --top 20` |
| How do the packages depend on each other? | `rintel deps` |
| How much could analysis see? | `rintel stats` |

<details>
<summary>All commands and flags</summary>

| Command | What it does |
|---|---|
| `rintel callers <symbol>` | who calls it, as a tree (`--depth 1..3`) |
| `rintel callees <symbol>` | what it calls, in source order |
| `rintel refs <symbol>` | everything that references it: calls, type and field uses, injection, overrides |
| `rintel find <name>` | search classes, methods and fields |
| `rintel api` | REST endpoints with their handlers (`--prefix`) |
| `rintel metrics [class\|method\|package\|file]` | complexity, size, coupling, cohesion (`--top N`, `--tests`) |
| `rintel deps` | package dependencies and cycles (`--format dot\|mermaid` for a diagram) |
| `rintel check` | security and quality findings; exits 1 on warnings |
| `rintel impact` | what a change affects (`--base REF`, `--depth 1..6`, `--format md`) |
| `rintel stats` | how much was resolved, and what was not |
| `rintel report` | the HTML report (`--open`, `--out file`, `--source`) |
| `rintel scan` | refresh the index by hand (`--full` rebuilds it) |
| `rintel mcp` | MCP server on stdio |
| `rintel doctor` | which languages can be analyzed on this machine |

- **Symbols** are written the way rintel prints them: `UserService.createUser`,
  `"UserService.createUser(String)"`, `web.Server.handle` (Go: package), `service.UserService.create`
  (TypeScript, Python: file), or a full id. A typo gets a suggestion.
- **Output**: `--format json` everywhere, `--format csv` for metrics. JSON and MCP responses share one schema.
- **Language**: `--lang en|ko`, or `RINTEL_LANG`, else the system locale.
- **Switched-off rules** are listed at the end of `rintel check`, so a disabled gate does not look like a passed one.
- **Other flags**: `-C <dir>` sets the repository root; `--no-scan` answers from the existing index.
- **Exit codes**: 0 ok, 1 symbol not found or check failed, 2 usage error, 3 analysis error.

</details>

## Security and quality checks

`rintel check` runs 22 rules and prints a fix under each one that fired.

```sh
rintel check                                   # fail the build on any warning
rintel check --category security               # one group: security or quality
rintel check --changed-since origin/main       # only files this branch touched
rintel check --format sarif > rintel.sarif     # for a code-scanning dashboard
rintel impact --base origin/main --format md   # a pull-request comment
```

**Security rules**

| Rule | Finds | Level | CWE |
|---|---|---|---|
| `hardcoded-secret` | passwords, keys and tokens in source and tracked settings files | warning | 798 |
| `sql-concatenation` | SQL built by joining strings with values | warning | 89 |
| `tls-verification-disabled` | certificate or host name checks switched off | warning | 295 |
| `unsafe-deserialization` | loaders that can run code from their input | warning | 502 |
| `dynamic-code` | `eval`, `exec`, `new Function` | warning | 95 |
| `weak-crypto` | MD5, SHA-1, DES, RC4, ECB | note | 327 |
| `command-execution` | external commands | note | 78 |
| `html-injection` | `innerHTML`, `v-html`, `template.HTML` | note | 79 |

A **warning** fails the build. A **note** is a place to review and does not.

> [!NOTE]
> Security rules match source text line by line (a multi-line template string is read as one piece). They find
> candidates, not proofs, and they do not follow data across functions. Confirm a finding before changing code: SQL assembled
> from fixed fragments with bound parameters is reported the same way as an injectable statement. A finding never
> quotes the line, so a report does not spread the secret it found.

<details>
<summary>Quality rules</summary>

Ten **warnings**: complex, long, deeply nested or many-parameter methods; large, highly coupled or low-cohesion
classes; package and class dependency cycles; a lower layer depending on a higher one.

Four **notes**: controllers that skip the service layer, and candidates for unreferenced, test-only and untested
code. Notes come from static relations only, so a framework, reflection or template call can make them wrong.

Limits are set in [`.ri/config.json`](#configuration).

</details>

<details>
<summary>Vulnerable dependencies: <code>rintel check --vulns</code> (off by default, uses the network)</summary>

Looks up the pinned dependency versions at [osv.dev](https://osv.dev) and reports each known vulnerability with the
version that fixes it.

- **What is sent**: package names and versions, nothing else. rintel says so before it sends anything. Go modules
  matching `GOPRIVATE` and npm packages from a private registry are not sent.
- **Without a network**: the other checks still run, a message says the dependency check was not done, and the exit
  code is 3, so a CI gate does not pass on an unchecked build.
- **What is read**: `go.mod`, `package-lock.json`, `requirements*.txt`, `poetry.lock`, `uv.lock`, and the artifacts
  named in Maven and Gradle build files.
- **Not covered**: yarn and pnpm locks, version ranges, transitive Java dependencies.
- A hit means the version is affected, not that the vulnerable code is reached.

</details>

## The HTML report

`rintel report --open` writes one static HTML file with no external references and no third-party JavaScript. It
opens offline and can be passed around as a file.

![Dependencies view: package graph and dependency matrix](docs/img/dependencies.png)

| Tab | Shows |
|---|---|
| Overview | packages with size, warnings and resolution rate; the package graph; most depended-on classes |
| Metrics | a treemap (area = lines, colour = highest complexity) and sortable tables |
| Dependencies | package graph, dependency matrix, coupling per package |
| Structure | package tree, members, and for each symbol its callers, call graph and inheritance |
| API | endpoints with their handlers |
| Checks | the same findings as `rintel check`, with the fix for each rule |
| Unresolved | every place analysis could not see through |

`--source` also embeds the source code and adds a Source tab. It is off by default because the report then contains
the code itself. Symbol names and file paths are always in the report, so check before sharing it outside.

## Use it from a coding agent

rintel ships **skills** (when to reach for which command, and how to act on the result) and an **MCP server** (the
same data as typed tool calls). Both need the `rintel` binary on `PATH`.

**Claude Code**

```
/plugin install rintel --marketplace spro1/rintel-code
```

| Skill | Use it for |
|---|---|
| `rintel` | who calls this, where is it used, is it safe to delete |
| `/rintel:impact` | what a change affects, which tests to run, new warnings |
| `/rintel:security` | a security review that checks each finding against the code |

The plugin also registers the MCP server and tells you at session start if the binary is missing.
On Claude Code older than 2.1.275, run `/plugin marketplace add spro1/rintel-code` and then
`/plugin install rintel@rintel`.

**MCP only, or another agent**

```sh
claude mcp add rintel -- rintel mcp
```

The server analyzes the repository it is started in, so one registration works for every project (`-C <dir>` pins
it to one). Each skill is a plain Markdown file under [`skills/`](skills/) that only uses the command line, so it
can be given to any agent.

<details>
<summary>MCP tools</summary>

`get_overview`, `search_symbols`, `get_symbol`, `get_neighbors`, `get_api_spec`, `get_metrics`, `get_dependencies`,
`get_checks`, `analyze_impact`, `scan_repository`.

Every response carries a `scan` block. When `scan.stale` is true the working tree changed since the scan, and
`scan_repository` refreshes it. Responses contain identifiers and locations, never source text.

</details>

## Languages

| Language | Needs | Libraries are read from |
|---|---|---|
| Java (Spring, Lombok) | JDK 17+ with `javac` | the local Gradle and Maven caches, found automatically |
| Go | the Go toolchain and a `go.mod` | the module cache (`GOMODCACHE`) |
| TypeScript, JavaScript, Vue | Node.js 18+ | the repository's `node_modules`; the compiler is bundled |
| Python | `python3` 3.8+ | nothing: third-party imports are recorded by name |

A repository may mix languages. A language whose toolchain is missing is skipped with a warning.

> [!TIP]
> rintel downloads nothing. If a library is not on the machine, calls through its types stay unresolved and
> `rintel stats` names the missing packages. **Build the project once**, then scan again.

| Repository | Language | Files | Calls resolved | Full scan |
|---|---|---|---|---|
| spring-petclinic | Java | 50 | 100% (99.5% before the first build) | 0.7 s |
| spring-boot-realworld-example-app | Java | 144 | 100% (76.4% before the first build) | 1.0 s |
| k8s.io/client-go | Go | 2,453 | 98.7% | about 4 s, 620 MB |

On client-go a rescan with no changes takes 0.3 seconds, and about 1.5 seconds after editing one file.

## Configuration

Optional, in `.ri/config.json`. An unknown key is an error, not ignored.

```json
{
  "exclude": ["**/*.pb.go", "legacy/**"],
  "thresholds": {
    "cc": 15, "method_loc": 80, "nesting": 4, "params": 5,
    "wmc": 50, "class_loc": 500, "cbo": 14, "lcom": 1
  },
  "disable": ["untested-class"]
}
```

`exclude` leaves paths out of the analysis, `thresholds` changes the limits of the quality rules, and `disable`
switches rules off by id.

## How to read the results

| Evidence | Meaning | Shown as |
|---|---|---|
| declared | written in the code: an annotation, an `import`, a route registration | no mark |
| resolved | established by type resolution: `repo.save()` → `CrudRepository.save` | no mark |
| heuristic | inferred by a named rule: several beans fit, or any implementation may be the target | `~inferred: rule-name` |

What analysis could not see is kept as an unresolved reference with a reason: `missing-classpath`,
`missing-module`, `missing-type`, `dynamic-call` or `reflection`. `rintel stats` shows the share.

Java, Go and TypeScript are resolved by their own type checkers. Python has no static types, so a call is
*resolved* only when the name is bound by a `def`, `class` or `import`, or the receiver's class is known from a
type hint, a constructor call or `self`. Otherwise it is *heuristic* (when at most three classes define a method
of that name) or unresolved.

<details>
<summary>Metric definitions</summary>

| Metric | Definition here |
|---|---|
| CC | cyclomatic complexity: 1 + `if`, loops, `case`, `catch`, `?:`, `&&`, `\|\|` |
| Nesting | deepest nesting of control structures |
| WMC | sum of the CC of a class's methods |
| CBO | other repository classes the class references or is referenced by |
| RFC | the class's methods plus the distinct methods they call |
| DIT / NOC | inheritance depth / direct subclasses and implementers |
| LCOM | LCOM4: groups of methods (accessors excluded) that share no field and do not call each other; 1 is cohesive |
| Ca / Ce | classes outside a package that depend on it / that it depends on |
| Instability | Ce / (Ca + Ce) |
| Abstractness, Distance | (interfaces + abstract classes) / classes, and \|A + I − 1\| |

Definitions differ between tools (whether CBO counts libraries, what a line is), so compare numbers with that in
mind. Lombok-generated and compiler-generated members are excluded. Outside Java, a "class" is a named type or the
top-level declarations of one file, and a "package" is a Go package or a directory.

</details>

## Scope and limits

<details>
<summary>Java</summary>

- Covered: calls, type and field uses, inheritance, overrides; Lombok accessors, constructors, builders and
  loggers (member stubs are added in memory, the processor is not run); Spring beans, injection (`@Autowired`,
  constructors, `@Qualifier`, `@Primary`, `@Bean`) and MVC endpoints.
- Not yet: MyBatis XML, JPA mapping, custom stereotype annotations, `@Singular`, `@With`, `@Delegate`.
- Library versions follow what the build declares: coordinates in `build.gradle` and `pom.xml`, Gradle version
  catalogs and variables, Maven properties, and what the Spring Boot version manages. Version ranges are not
  followed, and a library with no declared version is taken in the newest cached version.
- For exact versions, write the build tool's classpath to `.ri/classpath`
  (`mvn dependency:build-classpath -Dmdep.outputFile=.ri/classpath` works as is).

</details>

<details>
<summary>Go</summary>

- Covered: calls, type and field uses, interface satisfaction, embedding; routes for `net/http`, gin, echo, chi,
  gorilla/mux, fiber and httprouter.
- An interface call links to the interface method (a fact) and, when five or fewer types implement it, to their
  methods as well (inferred).
- Calls through function values are `dynamic-call`. Route-group prefixes are not joined.
- `vendor/` and `testdata/` are skipped; cgo-only files are not read.

</details>

<details>
<summary>TypeScript, JavaScript, Vue</summary>

- Covered: calls (including JSX component use), type and member uses, `extends` and `implements`, overrides;
  routes for Hono, Express, Fastify and Koa routers.
- Analyzed with the bundled TypeScript 5.9.3 compiler whatever version the project uses; `tsconfig.json` options
  it does not know are ignored.
- In `.vue` files only the `<script>` blocks are analyzed, not what the template references.
- Calls through callback parameters and `any` are `dynamic-call`.

</details>

<details>
<summary>Python</summary>

- Covered: functions, classes, methods, fields assigned on `self`, inheritance, overrides; FastAPI and Flask route
  decorators (matched by shape, so marked as inferred).
- Not modelled: decorators that replace functions, metaclasses, `getattr`.

</details>

<details>
<summary>Everything else</summary>

- **Generated code** (`build/generated*`, `target/generated-*`, `generated/`, `*.pb.go`, `*_gen.go`, `*.gen.ts`,
  `*_pb2.py`) is analyzed so calls into it resolve, but like test code it is left out of metrics, checks and graphs
  unless asked for (`--tests`, or the checkbox in the report).
- **Other languages** (Kotlin, C#, Rust and so on) are counted and reported as not analyzed.
- **Candidate lists** ("unreferenced methods", "no test touches") come from static relations only. Framework,
  reflection and template calls are invisible, so treat them as starting points.
- **Platforms**: developed and tested on macOS; CI runs on Linux. Windows is untested.

</details>

## Roadmap

- [ ] Release binaries, so Go is not needed to install
- [ ] A packaged Codex plugin (today: `codex mcp add rintel -- rintel mcp`, and the skill files by hand)
- [ ] `--vulns`: transitive Java dependencies, yarn and pnpm locks
- [ ] MyBatis mapper XML and JPA entities
- [ ] Windows verification
- [ ] More languages (Kotlin first)

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md); to report a vulnerability, see
[SECURITY.md](SECURITY.md).

```sh
make test     # go vet + go test
make vuln     # govulncheck; must print "No vulnerabilities found."
```

Each language's tests need its toolchain and are skipped without it. The suite's core guarantee is that an
incremental scan equals a full scan after every edit, for every language.

**Dependency policy.** Only versions with no known vulnerabilities. Two direct Go dependencies (`modernc.org/sqlite` and
the official MCP Go SDK; 18 modules in the build with what they pull in, under MIT and BSD-style licences; SQLite itself is public domain) and one bundled compiler (TypeScript 5.9.3, Apache-2.0, unmodified; provenance in
[cmd/rintel/adapter/README.md](cmd/rintel/adapter/README.md)). The Java and Python adapters and the report use no
libraries at all. Places where a simpler approach was chosen on purpose carry a `shortcut:` comment that says when
to replace it.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
