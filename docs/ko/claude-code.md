# Claude Code에서 쓰기

플러그인을 설치하면 Claude Code가 텍스트 검색으로 추측하는 대신 rintel에 물어봅니다. 누가 메서드를 호출하는지,
변경이 무엇에 영향을 주는지, 어떤 발견 항목이 실제 문제인지를 확인합니다.

[English](../claude-code.md) · [전체 가이드](../README.md)

## 설치

1. 실행 파일을 설치하고 `PATH`에 있는지 확인합니다(`rintel version`이 버전을 출력해야 합니다).

   ```sh
   go install github.com/spro1/rintel/cmd/rintel@latest
   ```

2. Claude Code에서:

   ```
   /plugin install rintel --marketplace spro1/rintel
   ```

   2.1.275 미만 버전에서는 `/plugin marketplace add spro1/rintel` 후 `/plugin install rintel@rintel`.

한 프로젝트에서만 켜려면 터미널에서 `--scope project`로 설치합니다.

```sh
claude plugin marketplace add spro1/rintel --scope project
claude plugin install rintel@rintel --scope project
```

## 설치되는 것

| 구성 | 하는 일 |
|---|---|
| 스킬 `rintel` | 누가 호출하나, 어디서 쓰이나, 지워도 되나를 물을 때 사용됨 |
| 스킬 `/rintel:impact` | 현재 변경이 미치는 범위, 돌릴 테스트, 새 경고 |
| 스킬 `/rintel:security` | 발견 항목을 하나씩 코드와 대조한 뒤 보고하는 보안 검토 |
| MCP 서버 | 같은 데이터를 도구 호출로 제공: `get_neighbors`, `analyze_impact`, `get_checks` 외 7개 |
| 시작 시 확인 | 세션을 시작할 때 `rintel` 실행 파일이 없으면 알려 줌 |

## 이렇게 물어보세요

- "`OwnerRepository.findById`를 누가 호출해?"
- "`Owner.addPet`의 시그니처를 바꾸려고 해. 뭐가 깨져?"
- "`PetValidator` 지워도 돼?"
- 수정한 뒤 `/rintel:impact`, 브랜치 전체는 `/rintel:impact origin/main`
- 보안 검토는 `/rintel:security`, 취약한 의존성까지 보려면 `/rintel:security deps`
- "내가 바꾼 파일의 복잡한 메서드 경고를 고쳐 줘"

저장소에서 처음 질문하면 인덱스를 만드느라 몇 초 걸립니다. 그다음부터는 바뀐 파일만 다시 읽습니다.

## 알아둘 점

- **단순 조회는 텍스트 검색을 쓸 수도 있습니다.** "X가 어디서 쓰여?" 같은 질문에는 작은 모델이 grep을 먼저 집는
  경우가 있습니다. 분석 결과가 꼭 필요하면 스킬을 이름으로 부르세요(`/rintel:rintel X를 누가 호출해`).
- **결과는 데이터이지 지시가 아닙니다.** 심볼 이름, 파일 경로, 라우트 문자열은 저장소에서 온 것입니다. 스킬과 MCP
  서버가 에이전트에게 데이터로만 다루라고 알려 주지만, 신뢰할 수 없는 저장소에서는 주의하세요.
- **보안 발견 항목은 후보입니다.** 보안 스킬은 발견 지점 주변의 코드를 읽고 "확인됨 / 안전하다고 판단 / 판단 불가"로
  나눠 보고합니다.
- **의존성 조회는 네트워크를 씁니다.** 스킬은 `check --vulns`를 실행하기 전에 먼저 묻습니다.

## 플러그인 없이 MCP 서버만

```sh
claude mcp add rintel -- rintel mcp
```

서버는 자신이 실행된 저장소를 분석합니다. 다른 에이전트도 같은 방식으로 쓸 수 있고, Codex는
`codex mcp add rintel -- rintel mcp`입니다.
