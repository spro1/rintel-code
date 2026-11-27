# 시작하기

설치부터 첫 답을 얻기까지 10분이면 됩니다. 예시는
[spring-petclinic](https://github.com/spring-projects/spring-petclinic)을 쓰지만, Java·Go·TypeScript/JavaScript·
Python 저장소라면 똑같이 동작합니다.

[English](../getting-started.md) · [전체 가이드](../README.md)

## 1. 설치하고 PC 점검하기

```sh
go install github.com/spro1/rintel-code/cmd/rintel@latest
rintel doctor
```

`rintel doctor`는 이 PC에서 어떤 언어를 분석할 수 있는지 알려 줍니다. Java는 JDK, TypeScript는 Node.js, Python은
`python3`, Go는 Go 툴체인이 필요합니다. 툴체인이 없는 언어는 건너뛰고 나머지는 그대로 분석합니다.

셸이 `rintel`을 찾지 못하면 `$(go env GOPATH)/bin`을 `PATH`에 추가하세요.

## 2. 분석이 얼마나 볼 수 있는지 확인하기

```
$ cd spring-petclinic
$ rintel stats
스캔              #1 · 500158f · 파일 50개 · 심볼 364개 · 엔드포인트 17개
호출 해석률       100.0%  (호출 1707건 중 1707건)
미해석            -
관계              declared 17 · heuristic 4 · resolved 2637
언어              java 50개
클래스패스        JAR 38개
```

첫 명령이 저장소를 스캔합니다. 그다음부터는 바뀐 파일만 다시 읽습니다.

가장 먼저 볼 숫자는 **호출 해석률**입니다. 낮으면 `stats`가 이 PC에 없는 라이브러리 이름을 알려 줍니다. rintel은
아무것도 내려받지 않으므로, 프로젝트를 한 번 빌드(`./gradlew build`, `mvn package`, `npm install`,
`go mod download`)한 뒤 `rintel stats`를 다시 실행하세요.

## 3. 누가 호출하는지 물어보기

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

- 정확한 이름을 모르면 `rintel find Owner`로 클래스·메서드·필드를 검색합니다.
- `rintel refs Owner`는 호출뿐 아니라 타입이나 필드를 언급하는 모든 곳을 보여 줍니다.
- `rintel callees <심볼>`은 반대 방향입니다.
- `--depth 2`는 호출자의 호출자까지 따라갑니다.

`~추정` 표시가 붙은 줄은 추정이며(예: 주입 지점에 맞는 Spring 빈이 여럿) 사용한 규칙 이름이 함께 나옵니다. 표시가
없는 줄은 코드에서 읽은 사실입니다.

## 4. 보고서 열기

```sh
rintel report --open
```

패키지 그래프, 지표, 탐색할 수 있는 구조 화면, 엔드포인트, 점검 결과가 HTML 파일 하나에 들어 있습니다. 서버도
네트워크도 필요 없습니다.

## 5. 점검 돌리기

```
$ rintel check
high-coupling  3  Class coupled to many others
  수정: 의존하는 클래스를 줄이세요. 인터페이스를 사이에 두거나 동작을 데이터가 있는 쪽으로 옮기세요.
  model/NamedEntity.java:30  NamedEntity is coupled to 18 classes (limit 14)
  ...
규칙 22개: 통과 18개 · 경고 9건 · 참고(후보) 5건
```

규칙마다 발견한 것을 어떻게 고치는지 함께 출력합니다. `rintel check --category security`는 보안 규칙만 보여 줍니다.

## 다음

- [변경 검토](code-review.md): 변경이 미치는 범위와 돌릴 테스트
- [CI에서 쓰기](ci.md): 품질·보안 게이트
- [Claude Code에서 쓰기](claude-code.md): 스킬과 MCP 서버
