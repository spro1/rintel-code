# Repository Intelligence (`rintel`)

[English](README.md) · 한국어

코드 저장소를 정적 분석해 "누가 무엇을 호출하고 참조하는가", 구조와 의존성, 코드 지표를 **근거 등급과 함께** 보여주는 로컬 도구입니다. **Java, Go, TypeScript/JavaScript(React, Vue), Python**을 분석합니다.

- 소스는 PC 밖으로 나가지 않습니다. 네트워크를 쓰지 않고, 저장소의 코드나 빌드 도구를 실행하지 않습니다.
- 확정된 사실과 추정을 구분합니다. 추정에는 항상 사용한 규칙 이름이 붙고, 해석하지 못한 지점은 숨기지 않고 따로 기록합니다.
- 터미널(CLI), 코딩 에이전트(MCP), 브라우저(HTML 보고서) 세 가지로 씁니다.

![구조 화면: 메서드를 호출하는 곳을 파일별로 묶어 보여 줌](docs/img/structure.png)

## 빌드와 실행

빌드에는 Go 1.27.4+가 필요합니다. 분석 대상 언어에 따라 다음이 필요합니다(`rintel doctor`로 확인).

- **Java 저장소**: JDK 17+(`javac` 포함). Java 어댑터는 바이너리에 내장되어 첫 실행 때 한 번 컴파일됩니다.
- **Go 저장소**: Go 툴체인(표준 라이브러리 소스를 읽습니다)과 `go.mod`.
- **TypeScript/JavaScript/Vue 저장소**: Node.js 18+. 컴파일러(TypeScript 5.9.3)는 rintel에 내장되어 있어, 분석 대상 저장소의 `node_modules` 코드를 실행하지 않습니다.
- **Python 저장소**: `python3` 3.8+.

한 저장소에 여러 언어가 섞여 있어도 됩니다. 툴체인이 없는 언어는 경고와 함께 건너뛰고 나머지는 그대로 분석합니다.

```sh
go install github.com/spro1/rintel/cmd/rintel@latest   # 또는 소스에서: git clone https://github.com/spro1/rintel && cd rintel && make build
cd <분석할 저장소>
rintel callers UserService.createUser --depth 2   # 호출하는 곳 (트리)
rintel callees UserService.createUser             # 호출되는 대상
rintel refs UserDto                               # 참조하는 모든 곳: 호출, 타입·필드 사용, 주입, 오버라이드
rintel find User                                  # 심볼 검색
rintel api --prefix /api                          # REST 엔드포인트
rintel stats                                      # 해석률, 미해석 사유, 분석 제외 언어
rintel metrics [class|method|package|file]        # 복잡도·크기·결합도·응집도 (--top N, --tests, --format csv|json)
rintel deps                                       # 패키지 의존과 순환 (--format dot|mermaid)
rintel check                                      # 품질 게이트: 경고가 있으면 종료 코드 1 (--format sarif)
rintel impact --base origin/main                  # 변경의 영향: 호출자, 엔드포인트, 돌릴 테스트
rintel report --open                              # 분석 결과를 HTML 한 장으로 보기 (브라우저로 열림)
rintel scan [--full]                              # 인덱스 갱신 (조회 명령이 자동으로 수행). --full은 라이브러리 선택도 처음부터 다시
rintel doctor                                     # 이 PC에서 분석 가능한 언어 점검
```

- 저장소 안 어느 폴더에서 실행해도 됩니다(git처럼 루트를 찾아 올라갑니다).
- 인덱스는 `.ri/cache/`에 저장되고, rintel이 `.ri/.gitignore`를 직접 써서 git에서 제외합니다. `git add -A`를 해도 커밋되지 않습니다.
- 조회 명령은 인덱스가 없거나 낡았으면 먼저 증분 스캔을 합니다(`--no-scan`으로 끔).
- 심볼은 rintel이 출력하는 형태 그대로 다시 입력할 수 있습니다: `UserService.createUser`, `"UserService.createUser(String)"`, `"new User(String)"`, 전체 ID. 이름이 틀리면 가까운 이름을 제안합니다.
- 클래스에 `callers`/`callees`를 쓰면 그 클래스의 메서드·생성자 기준으로 보여줍니다.
- 종료 코드: 0 정상, 1 심볼 없음 또는 점검 실패, 2 사용법 오류, 3 분석 오류. 플래그는 명령 앞뒤 어디에나 둘 수 있습니다(`rintel -C repo callers X`).
- 출력 언어는 영어/한국어입니다. `--lang en|ko` 또는 `RINTEL_LANG`, 없으면 시스템 로캘(`LANG`)을 따릅니다.
- `--format json`은 MCP 응답과 같은 스키마이고 전체 심볼 ID를 담습니다. 텍스트 출력은 읽기 쉽게 패키지를 생략합니다.
- 추정 관계에만 `~추정: <규칙>` 표시가 붙습니다. 표시가 없으면 정적 분석으로 확정된 관계입니다.

### 보고서 페이지

`rintel report`는 최신 스캔을 정적 HTML 파일 하나(`.ri/cache/report.html`, `--out`으로 변경)로 만듭니다.

- **개요**: 패키지별 클래스·메서드 수와 호출 해석률, 패키지 의존 그래프, 가장 많이 의존받는 클래스, 외부 라이브러리 사용 현황
- **지표**: 트리맵(면적 = 코드 줄 수, 색 = 최대 복잡도)과 클래스·메서드·패키지·파일별 지표 표(열 제목을 눌러 정렬)
- **의존성**: 패키지 의존 그래프(패키지나 화살표를 누르면 그 뒤의 클래스 관계 표시), 의존성 매트릭스, 패키지 결합도(의존받음·의존함·불안정도), 계층 그래프(controller → service → repository → entity)
- **구조**: 패키지 트리 · 멤버 목록 · 상세의 3단 화면. 메서드는 호출 계층(호출자를 계속 펼칠 수 있음)과 양방향 호출 그래프(깊이 1~3), 클래스는 의존 그래프와 상속 계층. 각 심볼의 지표가 함께 표시됨
- **API**: 컨트롤러별 엔드포인트와, 핸들러에서 호출을 따라 도달하는 저장소
- **점검**: 지표 기준 초과(복잡도 10 초과, 60줄 초과, 중첩 4 초과, 매개변수 5개 초과, WMC 50 또는 500줄 초과, CBO 14 초과, LCOM4 1 초과), 패키지·클래스 순환 의존, 계층 역방향 의존, 컨트롤러의 저장소 직접 참조, 참조 없는 메서드 후보, 테스트에서만 쓰는 코드, 테스트가 닿지 않는 클래스
- **미해석**: 분석이 해석하지 못한 지점을 파일별로

`rintel report --source`를 쓰면 소스 코드를 함께 담아, 심볼의 소스 탭과 호출 지점의 코드 한 줄 미리보기가 생깁니다. 기본값은 꺼져 있습니다(보고서에 코드 본문이 들어가므로 공유 범위를 확인해야 합니다).

그래프는 외부 라이브러리 없이 페이지 안에서 직접 그립니다. 외부 리소스를 전혀 참조하지 않아 오프라인에서 열리고, 페이지 안에서 영어/한국어를 전환할 수 있습니다. 미해석 코드 조각의 문자열 리터럴 내용은 `"…"`로 가려서 넣습니다. 다만 심볼 이름과 파일 경로는 그대로 들어가므로 외부 공유 전에 확인하세요.

점검 목록은 정적 분석으로 보이는 관계만으로 계산합니다. 프레임워크·리플렉션·템플릿에서의 호출은 보이지 않으므로 "참조 없는 메서드"는 후보로 취급해야 합니다. 지표는 CLI(`rintel metrics`)와 MCP(`get_metrics`)로도 제공되지만, 의존 그래프·순환 탐지·점검 목록은 아직 보고서 안에서만 계산됩니다.

### 지표 정의

| 지표 | 정의 |
|---|---|
| CC | 순환 복잡도: 1 + if, 반복문, case, catch, `?:`, `&&`, `||`의 수 |
| 중첩 | 제어 구조가 가장 깊게 겹친 단계 |
| WMC | 클래스의 메서드 CC 합 |
| CBO | 이 클래스가 참조하거나 이 클래스를 참조하는 저장소 내 다른 클래스 수 |
| RFC | 클래스의 메서드 수 + 그 메서드들이 호출하는 서로 다른 메서드 수 |
| DIT / NOC | 상속 깊이 / 직접 하위 클래스·구현체 수 |
| LCOM | LCOM4: 필드를 공유하지도 서로 호출하지도 않는 메서드 묶음 수(getter/setter 제외). 1이면 응집, 2 이상이면 무관한 일을 함께 함 |
| Ca / Ce | 패키지 밖에서 의존해 오는 클래스 수 / 패키지가 의존하는 바깥 클래스 수 |
| 불안정도 | Ce / (Ca + Ce) |
| 추상도 / 거리 | (인터페이스 + 추상 클래스) / 클래스, 그리고 \|추상도 + 불안정도 − 1\| |

Lombok이 생성한 멤버와 컴파일러가 만든 암시적 멤버는 지표에서 제외합니다.

### Go 저장소

`go.mod`의 의존 모듈을 **로컬 모듈 캐시**(`GOMODCACHE`)의 소스에서 읽어 타입을 해석합니다. `go build`나 `go list`를 실행하지 않고 내려받지도 않습니다.

- 캐시에 없는 모듈을 부르는 코드는 `missing-module` 미해석으로 기록됩니다. `go mod download` 후 다시 스캔하면 해소됩니다.
- 함수 값(필드·매개변수에 담긴 함수)을 통한 호출은 대상을 정적으로 알 수 없어 `dynamic-call`로 기록됩니다.
- 인터페이스 메서드 호출은 인터페이스 메서드로 연결하고(확정), 구현체가 5개 이하이면 구현 메서드로도 연결합니다(추정 `class-hierarchy`).
- 패키지 수준 선언(함수·변수)은 파일 단위로 묶여 보입니다(`web/server.go`). 타입(struct·interface)은 그 자체가 단위입니다.
- HTTP 라우트는 `net/http`, gin, echo, chi, gorilla/mux, fiber, httprouter의 등록 호출에서 읽습니다. 라우트 그룹의 경로 접두어는 합쳐지지 않습니다.
- `vendor/`, `testdata/`는 분석하지 않습니다. `_test.go`는 테스트 코드로 분류합니다.

### TypeScript / JavaScript / Vue 저장소

내장된 TypeScript 5.9.3 컴파일러로 타입을 해석합니다. 프로젝트가 쓰는 TypeScript 버전과 무관하며(7 포함), 이 컴파일러가 모르는 `tsconfig.json` 옵션은 무시합니다.

- 외부 패키지의 타입은 저장소의 `node_modules`에 있는 선언 파일에서 읽습니다. 없으면 `missing-module` 미해석이 되고, `npm install` 후 다시 스캔하면 해소됩니다.
- JSX에서 컴포넌트를 쓰는 것(`<Editor />`)은 호출로 기록합니다.
- 콜백 매개변수와 `any` 타입 값을 통한 호출은 `dynamic-call`입니다.
- `.vue` 파일은 `<script>` 블록만 분석합니다. 템플릿에서 참조하는 것은 보이지 않습니다.
- HTTP 라우트는 Hono, Express, Fastify, Koa 라우터의 등록 호출에서 읽습니다.
- `.d.ts`, `*.min.js`, `dist/`, `node_modules/`는 분석하지 않습니다. `*.test.ts`, `*.spec.ts`, `__tests__/`, `e2e/`는 테스트 코드로 분류합니다.

### Python 저장소

Python에는 정적 타입이 없으므로 근거 등급을 다음과 같이 매깁니다.

- **확정**: 이름이 `def`·`class`·`import`로 묶여 있거나, 수신 객체의 클래스를 타입 힌트·생성자 호출·`self`로 알 수 있는 호출
- **추정**(`method-name` 규칙): 수신 객체의 타입을 모르지만 그 이름의 메서드를 가진 클래스가 저장소에 3개 이하인 호출
- **미해석**(`dynamic-call`): 매개변수·변수에 담긴 함수 호출, 후보가 너무 많은 호출

FastAPI/Flask의 라우트 데코레이터(`@app.get("/x")`)는 모양으로 인식하므로 추정으로 표시됩니다. 데코레이터가 함수를 바꿔치기하는 경우, 메타클래스, `getattr`는 다루지 않습니다. `test_*.py`, `*_test.py`, `tests/`는 테스트 코드로 분류합니다.

### Java 클래스패스

외부 라이브러리 호출을 해석하려면 JAR이 필요합니다. rintel은 javac가 찾지 못한 패키지를 **로컬 Gradle/Maven 캐시**(`~/.gradle`, `~/.m2`)에서 찾아 자동으로 붙입니다. 다운로드도, 빌드 도구 실행도 하지 않습니다. 프로젝트를 그 PC에서 한 번이라도 빌드했다면 별도 설정이 필요 없습니다.

- 캐시에 없는 라이브러리의 호출은 `missing-classpath` 미해석으로 기록되고 해석률에 반영됩니다.
- 정확한 버전을 쓰고 싶으면 `.ri/classpath`에 JAR 경로를 적습니다(자동 구성보다 우선). `mvn dependency:build-classpath -Dmdep.outputFile=.ri/classpath` 출력을 그대로 쓸 수 있습니다.
- `RINTEL_NO_AUTO_CLASSPATH=1`로 자동 구성을 끕니다.

### CI 점검

`rintel check`는 규칙 14개를 봅니다. 10개는 **경고**로 빌드를 실패시킵니다: 복잡한·긴·중첩이 깊은·매개변수가 많은 메서드, 큰·결합도가 높은·응집도가 낮은 클래스, 패키지·클래스 순환 의존, 계층 역방향 의존. 4개는 **참고**로 실패시키지 않습니다: 서비스 계층을 건너뛴 컨트롤러, 참조 없는 코드·테스트에서만 쓰는 코드·테스트가 닿지 않는 클래스 후보. 참고는 정적 관계만으로 찾은 것이라 프레임워크·리플렉션·템플릿 호출이 있으면 틀릴 수 있습니다.

```sh
rintel check                                  # 경고가 하나라도 있으면 종료 코드 1
rintel check --changed-since origin/main      # 이 브랜치가 건드린 파일의 결과만 (기존 코드베이스에 도입할 때)
rintel check --fail-on never --format sarif > rintel.sarif    # 코드 스캐닝 대시보드용
rintel impact --base origin/main --format md  # PR 코멘트용 Markdown
```

`rintel impact`는 작업 트리를 git 리비전(기본 `HEAD`)과 비교해 줄이 바뀐 심볼을 찾고, 그것에 의존하는 심볼을 `--depth` 단계(기본 3, 최대 6)까지 따라갑니다. 추정 관계를 거친 경로는 추정이라고 표시합니다.

### 설정

선택 사항이며 `.ri/config.json`에 둡니다. 모르는 키는 무시하지 않고 오류로 처리합니다.

```json
{
  "exclude": ["**/*.pb.go", "legacy/**"],
  "thresholds": { "cc": 15, "method_loc": 80, "nesting": 4, "params": 5, "wmc": 50, "class_loc": 500, "cbo": 14, "lcom": 1 },
  "disable": ["untested-class"]
}
```

### MCP (Claude Code)

```sh
claude mcp add rintel -- /path/to/rintel mcp -C /path/to/repo
```

도구: `get_overview`, `search_symbols`, `get_symbol`, `get_neighbors`, `get_api_spec`, `get_metrics`, `get_dependencies`, `get_checks`, `analyze_impact`, `scan_repository`.
모든 응답의 `scan.stale`이 true이면 작업 트리가 스캔 이후 바뀐 것이므로 `scan_repository`로 갱신합니다.

## 분석 범위

| 항목 | 상태 |
|---|---|
| 호출 관계(메서드, 생성자, 메서드 참조), 상속·구현·오버라이드 | 구현 |
| 타입·필드 사용 관계(`uses`) — "이 DTO/필드를 바꾸면 어디가 영향받나" | 구현 |
| Lombok: getter/setter, 생성자 3종, `@Builder`, `@Data`/`@Value`, 로그 어노테이션, `@Accessors(chain)` | 구현 (프로세서 실행 없이 스텁 주입) |
| Spring DI: 생성자·필드 주입, `@RequiredArgsConstructor`, `@Qualifier`, `@Primary`, `@Bean` 팩토리 | 구현 |
| 인터페이스 호출 → 구현체 연결 | DI로 확정되면 resolved, 아니면 클래스 계층으로 추정(`class-hierarchy`, 구현 5개 이하) |
| REST 엔드포인트(Spring MVC) | 구현 |
| 클래스패스 자동 구성, `build/generated` 소스 포함 | 구현 — 버전은 빌드 파일·버전 카탈로그(`libs.versions.toml`)·Gradle 변수(`ext`, `gradle.properties`)·Maven 속성에 적힌 것과 Spring Boot BOM이 관리하는 것을 따르고, 선언이 없으면 로컬 캐시의 최신 버전. 내려받지는 않으므로 한 번도 빌드하지 않은 저장소는 `rintel stats`가 없는 라이브러리를 알려 줌 |
| 증분 스캔 + 증분 = 전체 동일성 테스트 | 구현 (`go test ./...`) |
| Lombok `@Singular`·`@With`·`@Delegate`, 커스텀 스테레오타입(메타 어노테이션) | 미구현 — 미해석으로 기록 |
| 변경 영향 분석(`rintel impact`), 의존성·순환(`rintel deps`), 품질 게이트(`rintel check`) | 구현 |
| MyBatis 매퍼 XML ↔ SQL ↔ 테이블, JPA 엔티티, 문서 생성·검증 (M2) | 미착수 |
| Go: 호출·타입 사용·인터페이스 구현·임베딩, HTTP 라우트, 복잡도 지표 | 구현 |
| TypeScript·JavaScript·Vue: 호출(JSX 포함)·타입 사용·상속·오버라이드, Hono/Express 라우트, 복잡도 지표 | 구현 (내장 TypeScript 5.9.3) |
| Python: 함수·클래스·상속·오버라이드, FastAPI/Flask 라우트, 복잡도 지표 | 구현 (타입 힌트 기반, 나머지는 추정·미해석으로 구분) |
| Kotlin, C#, Rust 등 | 미착수 — 파일 수를 세어 "분석하지 않은 언어"로 표시 |

코드 안의 `shortcut:` 주석이 의도적으로 단순화한 지점과 교체 시점을 표시합니다.

## 실측 (설정 없이 첫 실행)

| 저장소 | 언어 | 파일 | 호출 해석률 | 전체 스캔 |
|---|---|---|---|---|
| spring-petclinic | Java | 50 | 100% (한 번 빌드한 뒤; 빌드 전 99.5%) | 0.7초 |
| spring-boot-realworld-example-app (Lombok, MyBatis, DGS) | Java | 144 | 100% (한 번 빌드한 뒤; 빌드 전 76.4%) | 1.0초 |
| k8s.io/client-go | Go | 2,453 | 98.7% | 약 4초 (메모리 약 620MB) |

rintel은 아무것도 내려받지 않습니다. 저장소가 쓰는 라이브러리가 이 PC에 없으면 그 타입을 거치는 호출은 미해석으로 남고, `rintel stats`가 없는 패키지 이름을 알려 줍니다. 프로젝트를 한 번 빌드해 빌드 도구가 라이브러리를 내려받게 한 뒤 다시 스캔하면 해소됩니다(위 표의 "빌드 전" 수치 참고).

client-go 기준으로 변경 없는 재스캔은 0.3초, 파일 하나를 고친 뒤의 증분 스캔은 약 1.5초입니다. macOS에서 개발·검증했고 CI는 Linux에서 돕니다. Windows는 검증하지 않았습니다.

## 의존성 정책

알려진 취약점이 없는 버전만 씁니다. 의존성이나 Go 버전을 바꾸면 반드시 아래 검사를 통과해야 합니다.

```sh
govulncheck ./...        # "No vulnerabilities found." 이어야 함
```

- Go 버전은 `go.mod`에 고정합니다(현재 1.27.4 — 1.27.3의 `net/http` 취약점 GO-2026-6859가 수정된 버전).
- Go 의존성은 둘뿐입니다: `modernc.org/sqlite`(순수 Go SQLite), `github.com/modelcontextprotocol/go-sdk`(공식 MCP SDK).
- TypeScript 컴파일러 5.9.3(Apache-2.0)을 수정 없이 내장합니다. 출처·해시·취약점 검사 결과는 [cmd/rintel/adapter/README.md](cmd/rintel/adapter/README.md)에 있습니다.
- Java·Python 어댑터와 HTML 보고서는 외부 라이브러리를 쓰지 않습니다(각 언어의 표준 API, 바닐라 JS).
- 분석 대상 프로젝트의 JAR은 타입 해석을 위해 읽기만 하고 실행하지 않으며, rintel에 포함되지도 않습니다.

## 기여와 보안

기여 방법은 [CONTRIBUTING.md](CONTRIBUTING.md), 취약점 제보는 [SECURITY.md](SECURITY.md)를 참고하세요.

## 라이선스

Apache-2.0. [LICENSE](LICENSE)와 [NOTICE](NOTICE)를 참고하세요.
