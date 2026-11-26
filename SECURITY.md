# Security

rintel is meant to be safe to run on code you do not trust: it does not use the network unless you ask for the dependency lookup (`check --vulns`, which sends package names and versions to osv.dev), and it does not execute anything
from the repository it analyzes (no build tool, no annotation processor, no package scripts, no imports).

If you find a way to break that — code execution, writing outside `.ri/`, reading files outside the repository, script
execution in the HTML report — please report it privately through GitHub's
[private vulnerability reporting](https://github.com/spro1/rintel/security/advisories/new) rather than a public issue.
Include the input that triggers it and the rintel version (`rintel version`).

Dependencies are kept at versions with no known vulnerabilities; `make vuln` (govulncheck) runs in CI.
