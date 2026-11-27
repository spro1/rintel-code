# Getting started

Ten minutes from install to your first answers. The examples use
[spring-petclinic](https://github.com/spring-projects/spring-petclinic); any Java, Go, TypeScript/JavaScript or
Python repository works the same way.

[한국어](ko/getting-started.md) · [All guides](README.md)

## 1. Install and check the machine

```sh
go install github.com/spro1/rintel-code/cmd/rintel@latest
rintel doctor
```

`rintel doctor` lists which languages can be analyzed here. Java needs a JDK, TypeScript needs Node.js, Python needs
`python3`, Go needs the Go toolchain. A language without its toolchain is skipped, the rest still works.

If the shell cannot find `rintel`, add `$(go env GOPATH)/bin` to your `PATH`.

## 2. Look at how much rintel can see

```
$ cd spring-petclinic
$ rintel stats
Scan              #1 · 500158f · 50 files · 364 symbols · 17 endpoints
Resolved calls    100.0%  (1707 of 1707 call sites)
Unresolved        -
Edges             declared 17 · heuristic 4 · resolved 2637
Languages         java 50 files
Classpath         38 jars
```

The first command scans the repository; later commands only re-read files that changed.

**Resolved calls** is the number to look at first. If it is low, `stats` names the libraries that are missing from
this machine. rintel downloads nothing, so build the project once (`./gradlew build`, `mvn package`, `npm install`,
`go mod download`) and run `rintel stats` again.

## 3. Ask who calls something

```
$ rintel callers OwnerRepository.findById
OwnerRepository.findById(Integer)             owner/OwnerRepository.java:60
  ← OwnerController.findOwner(Integer)        owner/OwnerController.java:67
  ← OwnerController.showOwner(int)            owner/OwnerController.java:181
  ← PetController.findOwner(int)              owner/PetController.java:69
  ← PetController.findPet(int,Integer)        owner/PetController.java:83
  ← VisitController.loadPetWithVisit(int,int,Map)   owner/VisitController.java:66
  ...
```

- Do not know the exact name? `rintel find Owner` searches classes, methods and fields.
- `rintel refs Owner` lists everything that mentions a type or field, not only calls.
- `rintel callees <symbol>` goes the other way.
- `--depth 2` follows callers of callers.

A line marked `~inferred` is a guess (for example, several Spring beans fit an injection point) and names the rule
that made it. Everything else is a fact read from the code.

## 4. Open the report

```sh
rintel report --open
```

One HTML file with the package graph, metrics, a browsable structure view, endpoints and the check results. It needs
no server and no network.

## 5. Run the checks

```
$ rintel check
high-coupling  3  Class coupled to many others
  fix: Depend on fewer classes: put an interface in between or move the behavior to where its data is.
  model/NamedEntity.java:30  NamedEntity is coupled to 18 classes (limit 14)
  ...
22 rules: 18 passed · 9 warnings · 5 notes (candidates)
```

Every rule prints how to fix what it found. `rintel check --category security` shows only the security rules.

## Next

- [Review a change](code-review.md): what a diff affects and which tests to run
- [Use it in CI](ci.md): a quality and security gate
- [Use it from Claude Code](claude-code.md): skills and the MCP server
