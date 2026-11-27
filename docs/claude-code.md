# Use it from Claude Code

With the plugin, Claude Code asks rintel instead of guessing from text search: who calls a method, what a change
affects, which findings are real.

[한국어](ko/claude-code.md) · [All guides](README.md)

## Install

1. Install the binary and make sure it is on `PATH` (`rintel version` should print a version).

   ```sh
   go install github.com/spro1/rintel/cmd/rintel@latest
   ```

2. In Claude Code:

   ```
   /plugin install rintel --marketplace spro1/rintel
   ```

   On versions older than 2.1.275: `/plugin marketplace add spro1/rintel`, then `/plugin install rintel@rintel`.

To enable it for one project only, run the install with `--scope project` from the command line:

```sh
claude plugin marketplace add spro1/rintel --scope project
claude plugin install rintel@rintel --scope project
```

## What you get

| Piece | What it does |
|---|---|
| Skill `rintel` | used when you ask who calls something, where it is used, or whether it is safe to delete |
| Skill `/rintel:impact` | what the current change affects, which tests to run, new warnings |
| Skill `/rintel:security` | a security review that checks every finding against the code before reporting it |
| MCP server | the same data as tool calls: `get_neighbors`, `analyze_impact`, `get_checks` and seven more |
| Start-up check | tells you at session start if the `rintel` binary is missing |

## Things to ask

- "Who calls `OwnerRepository.findById`?"
- "I want to change the signature of `Owner.addPet`. What breaks?"
- "Is `PetValidator` safe to delete?"
- `/rintel:impact` after editing, or `/rintel:impact origin/main` for a branch
- `/rintel:security` for a security review; `/rintel:security deps` to include vulnerable dependencies
- "Fix the complex-method warnings in the files I changed"

The first question in a repository builds the index, which takes a few seconds; after that only changed files are
re-read.

## Good to know

- **Simple lookups may still use text search.** For a plain "where is X used", a small model sometimes reaches for
  grep first. Invoke the skill by name (`/rintel:rintel who calls X`) when you want the analysis for sure.
- **Results are data, not instructions.** Symbol names, file paths and route strings come from the repository. The
  skills and the MCP server tell the agent to treat them as data, but be careful with repositories you do not trust.
- **Security findings are candidates.** The security skill reads the code around each finding and reports it as
  confirmed, judged safe, or undecided.
- **The dependency lookup uses the network.** The skill asks before running `check --vulns`.

## MCP server without the plugin

```sh
claude mcp add rintel -- rintel mcp
```

The server analyzes the repository it is started in. Other agents can use it the same way; for Codex,
`codex mcp add rintel -- rintel mcp`.
