---
name: security
description: >
  Find and triage security problems in a Java, Go, TypeScript/JavaScript or
  Python repository: hardcoded secrets, SQL built from strings, disabled TLS
  verification, unsafe deserialization, eval, weak crypto, command execution,
  HTML injection, and optionally known-vulnerable dependency versions. Use
  when asked to "find vulnerabilities", "security review", "security audit",
  "is this code safe", "check for secrets", or before making a repository
  public. Each finding is checked against the code before it is reported.
argument-hint: "[path or 'deps' to include vulnerable dependencies]"
---

# Security review

rintel finds candidates by matching source lines; your job is to decide which are real.

1. Run `rintel check --category security --format json` (MCP: `get_checks` with `category: "security"`).
   Each rule carries `cwe` and `fix`; each finding has `file`, `line` and the enclosing `symbol`.
2. **Triage every warning** before reporting it. Read the line and trace where the value comes from:
   - `sql-concatenation`: safe when the joined parts are fixed text or `$n`/`?` placeholders and the values are
     bound; real when request data, a parameter or a field reaches the statement text.
   - `hardcoded-secret`: real unless it is a placeholder, a test value or a public identifier. Never print the value.
   - `tls-verification-disabled`, `unsafe-deserialization`, `dynamic-code`: real when the peer or the input can
     come from outside; say who controls it.
   Use `rintel callers <symbol> --depth 2` to see whether the code is reachable from an endpoint (`rintel api`).
3. **Notes** (`weak-crypto`, `command-execution`, `html-injection`) are places to review. Report only the ones
   where outside input or a security purpose is involved.
4. Dependencies: only when the user asked for them (argument `deps`) or agrees, run `rintel check --vulns`.
   It sends package names and versions to osv.dev; say so first. If it reports that the lookup was not done
   (no network), say that the dependency check is missing, not that it is clean.
5. Report in three groups: **Confirmed** (what, where, how it can be reached, the fix), **Judged safe** (one line
   each with the reason), **Could not decide** (what you would need to know). Most severe first.
6. Change code only if asked. When fixing, follow the rule's `fix`, then rerun the check to confirm it is gone.

What this does not find: flaws that need data-flow or business-logic understanding (missing authorization, a
`window.open` of stored data, path traversal). Say that the review covers the patterns above, not everything.
