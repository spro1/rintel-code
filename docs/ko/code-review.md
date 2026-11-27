# 변경 검토

푸시하기 전이나 다른 사람의 브랜치를 리뷰할 때, 변경이 무엇을 건드리고 무엇이 거기에 의존하며 어떤 테스트가 그것을
덮는지 확인합니다.

[English](../code-review.md) · [전체 가이드](../README.md)

## 커밋하지 않은 변경이 미치는 범위

```
$ rintel impact
HEAD 대비: 파일 1개 변경, 심볼 1개 변경, 영향받는 심볼 4개, 엔드포인트 3개, 닿는 테스트 13개.

변경된 심볼 (1)
  Owner.addPet(Pet)  owner/Owner.java:100

영향받는 심볼 (4)
  PetController.initCreationForm(Owner,ModelMap)     1단계  owner/PetController.java:100
  PetController.processCreationForm(Owner,Pet,…)     1단계  owner/PetController.java:107
  PetController.updatePetDetails(Owner,Pet)          1단계  owner/PetController.java:186
  PetController.processUpdateForm(Owner,Pet,…)       2단계  owner/PetController.java:144

엔드포인트 (3)
  GET  /owners/{ownerId}/pets/new
  POST /owners/{ownerId}/pets/new
  POST /owners/{ownerId}/pets/{petId}/edit

변경에 닿는 테스트 (13)
  OwnerTests.addPetAddsPersistedPet()                1단계
  OwnerTests.addPetDoesNotAddDuplicatePet()          1단계
  ...
```

위에서부터 읽습니다.

1. **변경된 심볼**: diff가 건드린 메서드와 클래스.
2. **영향받는 심볼**: 그것에 의존하는 운영 코드. 가까운 것부터 나옵니다.
3. **엔드포인트**: 요청이 변경된 코드까지 닿을 수 있는 REST 엔드포인트. 직접 눌러 볼 대상입니다.
4. **변경에 닿는 테스트**: 돌려야 할 테스트. 이 목록이 비어 있는데 "영향받는 심볼"이 있으면 테스트되지 않는 변경입니다.
   리뷰에서 가장 먼저 짚을 점입니다.

## 브랜치 전체

```sh
rintel impact --base origin/main            # 이 브랜치가 바꾼 것 전부
rintel impact --base origin/main --depth 5  # 더 멀리 따라가기 (기본 3, 최대 6)
```

## PR 코멘트로 남기기

```sh
rintel impact --base origin/main --format md > impact.md
gh pr comment --body-file impact.md
```

## 지우거나 이름을 바꾸기 전에

```sh
rintel callers PetValidator.validate --depth 2   # 누가 호출하나
rintel refs PetValidator                         # 모든 언급: 타입, 필드, 주입, 오버라이드
```

"호출자 없음"은 정적 분석이 볼 수 있는 범위에서 없다는 뜻입니다. 리플렉션, 템플릿, rintel이 모델링하지 않는
프레임워크를 통한 호출은 보이지 않습니다. 공개 코드를 지우기 전에 `rintel stats`의 미해석 비율을 확인하세요.

## 새로 생긴 문제만 보기

기존 코드베이스에서는 `rintel check`가 오래된 발견 항목을 수백 건 낼 수 있습니다. 브랜치가 건드린 파일만 보려면:

```sh
rintel check --changed-since origin/main
```
