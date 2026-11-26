# Adapters

One adapter per language family. Each reads source files and writes the same JSON Lines IR; the Go core knows no language.

| File | Language | Runs with |
|---|---|---|
| `Adapter.java` | Java | the user's JDK (`javac` API); compiled once into the user cache |
| `../goadapter.go` | Go | in-process (`go/parser`, `go/types`) |
| `ts-adapter.js` | TypeScript, JavaScript, Vue | the user's Node.js and the bundled TypeScript compiler |
| `py_adapter.py` | Python | the user's `python3` (`ast` only, run with `-I`) |

## Bundled third-party code: TypeScript 5.9.3

`typescript-5.9.3.tar.gz` contains `lib/typescript.js`, the `lib/lib*.d.ts` standard library declarations, `LICENSE.txt` and
`ThirdPartyNoticeText.txt` from the npm package `typescript@5.9.3` (Apache-2.0, © Microsoft Corporation). Nothing in it is modified.

- Source: `https://registry.npmjs.org/typescript/-/typescript-5.9.3.tgz`
- SHA-256 of that tarball: `10e108c9cf7d5f2879053dff18515fb405abf2ccef63eaaf017d9c571687a1d3`
- Vulnerability check: `osv-scanner` on `typescript@5.9.3` reports no known issues (checked 2026-11-26).

It is bundled so that analysis never executes anything from the analyzed repository's `node_modules`, and so that a project on
any TypeScript version (including 7, which no longer has this compiler API) is analyzed the same way.

To rebuild the archive from a fresh download:

```sh
npm pack typescript@5.9.3 && shasum -a 256 typescript-5.9.3.tgz   # compare with the hash above
tar xzf typescript-5.9.3.tgz
mkdir -p typescript/lib && cp package/lib/typescript.js package/lib/lib*.d.ts typescript/lib/
cp package/LICENSE.txt package/ThirdPartyNoticeText.txt typescript/
node -e "const p=require('./package/package.json');require('fs').writeFileSync('typescript/package.json',JSON.stringify({name:p.name,version:p.version,license:p.license,main:'lib/typescript.js'},null,1))"
COPYFILE_DISABLE=1 tar -czf typescript-5.9.3.tar.gz typescript
```

Upgrading means replacing the archive, the `//go:embed` line in `../adapters.go`, and re-running the vulnerability check.
