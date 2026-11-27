# Use it in CI

`rintel check` exits with 1 when a rule is broken, so it can gate a build. This guide shows a GitHub Actions setup
and how to introduce the gate on a code base that already has findings.

[한국어](ko/ci.md) · [All guides](README.md)

## Exit codes

| Code | Meaning |
|---|---|
| 0 | no warnings (notes do not fail the build) |
| 1 | at least one warning |
| 3 | analysis could not run, or `--vulns` was asked for and the lookup could not be done |

`--fail-on note` also fails on notes; `--fail-on never` only reports.

## GitHub Actions

```yaml
name: rintel
on: [pull_request]
permissions:
  contents: read
  security-events: write   # only for the SARIF upload
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # --changed-since needs the base branch
      - uses: actions/setup-go@v5
        with:
          go-version: stable
      - run: go install github.com/spro1/rintel/cmd/rintel@v0.1.0

      # Java, TypeScript: build once so the libraries are on the machine (rintel downloads nothing)
      # - run: ./gradlew assemble
      # - run: npm ci

      - run: rintel stats
      - run: rintel check --changed-since origin/${{ github.base_ref }}

      - if: always()
        run: rintel check --fail-on never --format sarif > rintel.sarif
      - if: always()
        uses: github/codeql-action/upload-sarif@v3
        with:
          sarif_file: rintel.sarif
```

Pin `rintel` to a version, as above, so a new release cannot change your gate without a commit.

## Introducing the gate on existing code

A first run on an older repository usually reports many findings. Three ways to start, strictest last:

1. **Report only.** `rintel check --fail-on never` and read the output for a week.
2. **Gate new code.** `rintel check --changed-since origin/main` holds only the files a branch touched to the rules.
3. **Gate everything**, after adjusting the limits to what the team accepts.

Limits and switches live in `.ri/config.json`, committed with the code:

```json
{
  "exclude": ["legacy/**", "**/*.pb.go"],
  "thresholds": { "cc": 15, "method_loc": 80 },
  "disable": ["untested-class"]
}
```

When rules are switched off, `rintel check` says so at the end of its output, so a disabled gate does not look like a
passed one. Review changes to this file like code.

## Security rules in CI

```sh
rintel check --category security
```

The security rules match source text. A statement built from fixed fragments with bound parameters is reported the
same way as an injectable one, so expect to confirm findings by hand. If one rule is too noisy for a repository,
add it to `disable` rather than ignoring the whole run.

## Known vulnerabilities in dependencies

```sh
rintel check --vulns
```

Off by default because it is the only feature that uses the network: package names and versions go to
[osv.dev](https://osv.dev). When the lookup cannot be done, the command exits with 3 instead of passing.

## Keep the index out of the repository

The index under `.ri/cache/` is ignored by git automatically. Do not commit it: a CI run should build its own index
from the checked-out code, not trust one that came with a pull request.
