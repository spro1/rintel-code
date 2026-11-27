# Security

## Reporting a vulnerability

Please report privately through GitHub's
[private vulnerability reporting](https://github.com/spro1/rintel/security/advisories/new), not a public issue.
Include the input that triggers the problem and the rintel version (`rintel version`).

## What rintel promises

rintel is built to be run on a repository you do not trust. These are the properties it is designed and tested to
keep. It has not had an independent security audit.

- **Nothing from the repository is executed.** No build tool, no annotation processor, no package script, no
  `go list`, no Python import. Git is called with hooks and the file-system monitor switched off.
- **Symbolic links are not followed.** A link in the repository to a file or directory elsewhere on the machine is
  skipped, for source files, settings files and dependency manifests alike.
- **What is read outside the repository is limited to resolving types.** That is: the toolchains' standard
  libraries, the local Go module, Gradle and Maven caches, jars listed in `.ri/classpath`, and locations the
  repository's own build files point to (a `replace` directory in `go.mod`, a path mapping or relative import in
  `tsconfig.json`). From those, rintel records names and locations; it does not copy their contents into its output.
- **Nothing is written outside `.ri/`** in the repository, apart from rintel's own cache in the user cache directory
  and a report written to a path you name with `--out`.
- **No network**, except `check --vulns`, which you switch on yourself. It sends the names and versions of pinned
  dependencies to osv.dev and says so first. Go modules matching `GOPRIVATE` and npm packages from a private
  registry are not sent.
- **Output cannot drive your terminal.** Control characters and text-direction overrides in file names, symbol
  names, strings, error messages and dependency names are replaced before they are stored or printed.
- **The HTML report runs no repository content as code.** It is one static file with a content security policy that
  allows no network access; repository text is only ever inserted as text.
- **Security findings do not quote source lines**, so a report or a CI log does not spread a secret that was found.
  The report contains symbol names and file paths, and source code only when you pass `--source`.

Breaking any of these is a vulnerability; please report it.

## What you still need to watch

- **Build files can point outside the repository.** As listed above, a `go.mod` `replace`, a `tsconfig.json` path
  or `.ri/classpath` can make rintel read code elsewhere on the machine to resolve types. On a shared CI runner,
  treat those files in a pull request as you would treat a change to the build itself.

- **Coding agents and untrusted repositories.** Symbol names, file paths and route strings are copied from the
  repository into what an agent reads. The skills and the MCP server tell the agent to treat them as data, but a
  repository can still contain text written to mislead an agent. Review what an agent does in a repository you do
  not trust.
- **`.ri/config.json` is part of the repository.** It can exclude paths and switch rules off. `rintel check` lists
  the rules that were switched off, and a change to this file deserves the same review as a change to CI.
- **The index is a cache, not evidence.** `.ri/cache/` is git-ignored automatically. Do not commit it, and in CI
  do not reuse an index that arrived with a pull request: let the run build its own.
- **Security rules are text patterns.** They find candidates, miss what needs data-flow analysis, and say nothing
  about authorization or business logic. A clean result is not a security review.
- **`check --vulns` reports affected versions**, not whether the vulnerable code is reached, and does not cover
  transitive Java dependencies or yarn and pnpm lock files.

## Dependencies

Dependencies are kept at versions with no known vulnerabilities: `make vuln` (govulncheck) runs in CI on every
push, CI actions are pinned to commits, and the licence texts of everything compiled into the binary are in
`THIRD_PARTY_NOTICES`.
