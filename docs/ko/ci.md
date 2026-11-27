# CI에서 쓰기

`rintel check`는 규칙 위반이 있으면 종료 코드 1로 끝나므로 빌드 게이트로 쓸 수 있습니다. GitHub Actions 설정과,
이미 발견 항목이 있는 코드베이스에 게이트를 도입하는 방법을 설명합니다.

[English](../ci.md) · [전체 가이드](../README.md)

## 종료 코드

| 코드 | 의미 |
|---|---|
| 0 | 경고 없음 (참고는 빌드를 실패시키지 않음) |
| 1 | 경고가 하나 이상 |
| 3 | 분석을 못 했거나, `--vulns`를 요청했는데 조회를 못 함 |

`--fail-on note`는 참고에도 실패하고, `--fail-on never`는 보고만 합니다.

## GitHub Actions

```yaml
name: rintel
on: [pull_request]
permissions:
  contents: read
  security-events: write   # SARIF 업로드에만 필요
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # --changed-since 가 기준 브랜치를 필요로 함
      - uses: actions/setup-go@v5
        with:
          go-version: stable
      - run: go install github.com/spro1/rintel-code/cmd/rintel@v0.1.0

      # Java, TypeScript: 라이브러리가 PC에 있도록 한 번 빌드 (rintel은 아무것도 내려받지 않음)
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

위처럼 `rintel`을 버전으로 고정하세요. 새 릴리스가 커밋 없이 게이트를 바꾸지 못하게 됩니다.

## 기존 코드에 게이트 도입하기

오래된 저장소에서 처음 돌리면 보통 발견 항목이 많이 나옵니다. 느슨한 것부터 세 단계입니다.

1. **보고만 하기.** `rintel check --fail-on never`로 일주일쯤 출력을 읽어 봅니다.
2. **새 코드만 막기.** `rintel check --changed-since origin/main`은 브랜치가 건드린 파일에만 규칙을 적용합니다.
3. **전체를 막기.** 팀이 받아들일 수 있는 기준값으로 조정한 뒤 적용합니다.

기준값과 규칙 끄기는 `.ri/config.json`에 적고 코드와 함께 커밋합니다.

```json
{
  "exclude": ["legacy/**", "**/*.pb.go"],
  "thresholds": { "cc": 15, "method_loc": 80 },
  "disable": ["untested-class"]
}
```

끈 규칙이 있으면 `rintel check`가 출력 끝에 알려 줍니다. 꺼진 게이트가 통과한 게이트처럼 보이지 않게 하기
위해서입니다. 이 파일의 변경은 코드처럼 리뷰하세요.

## CI에서 보안 규칙

```sh
rintel check --category security
```

보안 규칙은 소스 텍스트를 대조합니다. 고정된 조각과 바인딩 파라미터로 만든 SQL도 인젝션이 가능한 SQL과 똑같이
보고되므로, 발견 항목은 사람이 확인해야 합니다. 특정 저장소에서 한 규칙이 너무 시끄러우면 전체 결과를 무시하지 말고
그 규칙을 `disable`에 넣으세요.

## 의존성의 알려진 취약점

```sh
rintel check --vulns
```

네트워크를 쓰는 유일한 기능이라 기본은 꺼져 있습니다. 패키지 이름과 버전이 [osv.dev](https://osv.dev)로
전송됩니다. 조회를 못 하면 통과시키지 않고 종료 코드 3으로 끝납니다.

## 인덱스는 저장소에 넣지 마세요

`.ri/cache/` 아래의 인덱스는 자동으로 git에서 제외됩니다. 커밋하지 마세요. CI는 체크아웃한 코드로 자기 인덱스를
만들어야 하고, PR에 딸려 온 인덱스를 믿어서는 안 됩니다.
