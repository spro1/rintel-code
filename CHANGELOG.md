# Changelog

Notable changes to rintel. Versions follow [semantic versioning](https://semver.org); before 1.0, a minor version
may change behavior.

## 0.1.0 (2026-11-27)

First public version.

### Analysis

- Java (Spring, Lombok), Go, TypeScript/JavaScript (React, Vue) and Python: calls, type and field uses,
  inheritance, overrides, dependency injection and REST endpoints, each with an evidence level (declared,
  resolved, heuristic) and unresolved references kept with a reason.
- Incremental scans that equal a full scan after every edit.
- Java libraries are found in the local Gradle and Maven caches in the versions the build declares.

### Commands

- `callers`, `callees`, `refs`, `find`, `api`, `stats`, `metrics`, `deps`.
- `impact`: what a change affects, the endpoints involved and the tests that reach it; Markdown output for
  pull-request comments.
- `check`: 8 security rules with CWE ids and 14 quality rules, each with a fix; `--category`,
  `--changed-since`, SARIF output; `.ri/config.json` for excludes, limits and disabled rules.
- `check --vulns`: known vulnerabilities in pinned dependencies through osv.dev, off by default.
- `report`: one self-contained HTML page with overview, metrics, dependencies, structure, API, checks and
  unresolved references, in English and Korean.

### Coding agents

- MCP server (`rintel mcp`) with ten tools.
- Claude Code plugin with three skills: `rintel`, `impact`, `security`.

### Security

- Nothing from the analyzed repository is executed and symbolic links are not followed.
- Control characters in names and strings are replaced before output.
- Findings do not quote source lines.
