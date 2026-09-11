#!/usr/bin/env bash
# 게이트 단일 진입점. 사람도 git 훅도, 나중에 CI 가 생기면 CI 도 이것만 부른다.
# 게이트 로직이 훅과 CI 에 이중으로 흩어지는 것을 막는다.
#
#   scripts/verify.sh --fast   빠른 등급(L1, L2). 훅이 부른다
#   scripts/verify.sh          위 전부 + E2E(통합 등급). 미구현 시나리오가 있으면 실패한다
#   scripts/verify.sh --deep   위 전부 + 실환경 등급(E2E_DEEP=1)
#
# 순서가 중요하다. 게이트 자기검증이 게이트 실행보다 앞이다.
# 검증되지 않은 게이트는 아무것도 막지 못한다.
#
# 이 스크립트가 지키는 것. 하나라도 어기면 "잘못된 녹색" 이 생긴다.
#
#   1. 모든 명령의 실제 종료 코드를 본다. 출력 문자열을 grep 해서 판정하지 않는다.
#      `--- FAIL` 을 찾는 방식은 TestMain 의 os.Exit(2) 나 초기화 실패를 놓쳤다
#   2. t.Skip 은 go test 를 성공으로 끝낸다. 그래서 go test -json 을 testreport 로 읽어
#      미구현(SCENARIO_TODO)과 환경 부족을 가르고, full 이상에서는 미구현을 실패로 친다
#   3. 도구가 없거나 오류를 냈으면 실패다. gofmt 의 구문 오류를 버리면 다른 플랫폼용 파일이
#      깨진 채로 통과한다
#   4. 계층 경로와 테스트 패키지 목록은 archgate.json 한 곳에만 있다. 여기 하드코딩하지 않는다
#   5. 시나리오는 "파일에 있다" 가 아니라 "실행됐고 통과했다" 로 판정한다. archgate 의 정적 대응만 보면
#      tests.e2e 가 다른 곳을 가리키거나 파일이 빌드 태그로 배제돼도 녹색이다. 둘 다 실제로 뚫렸다.
#      그래서 문서의 시나리오 목록(archgate -print scenarios)을 testreport 에 넘겨 실행 결과와 대조한다
#   6. 테스트가 0 개 돈 것은 통과가 아니다. fast 에도 -require-tests 다. 패턴이 빈 곳을 가리키면 실패다
#
# 훅 범위에 대한 결정 (known-holes #5). 세 선택지 중 두 번째를 골랐다.
#   훅은 --fast 만 돈다. E2E 는 오케스트레이터가 커밋 전에 `verify.sh`(full) 를 돌린 결과로
#   완료 판정을 내린다(SKILL.md 도입 절차 7). 근거: E2E 가 훅에 들어가면 커밋 한 번이 분 단위가
#   되고, 그러면 에이전트가 --no-verify 를 쓰기 시작한다. 그 대신 "full 통과 없이는 커밋하지
#   않는다" 를 오케스트레이터 절차로 못 박는다. 훅이 못 보는 것: E2E 깨짐. 이것을 아는 상태로 둔다.
#   훅에서 E2E 까지 돌리고 싶으면 .githooks/pre-commit 의 --fast 를 지우면 된다.
#
# -race 에 대한 결정. full 이상에서만 켠다. 훅에서는 끈다.
#   근거: -race 는 빌드가 수 배 느리고 gcc 가 필요하다. 데이터 경쟁은 커밋 단위가 아니라
#   완료 판정 단위로 잡아도 늦지 않다. gcc 가 없는 환경이면 full 이 실패하는데, 그것은
#   "도구가 없으면 실패" 원칙과 같다. 조용히 빼지 않는다.

set -uo pipefail

cd "$(dirname "$0")/.."

MODE="full"
case "${1:-}" in
  --fast) MODE="fast" ;;
  --deep) MODE="deep" ;;
  "")     MODE="full" ;;
  *) echo "알 수 없는 인자: $1" >&2; exit 2 ;;
esac

CONFIG="archgate.json"
MIN_GO="1.22"   # archgate 가 for range n 과 types.Unalias 를 쓴다

# 설정에서 읽는 값. 여기 적으면 정의가 두 곳이 된다. 설정을 못 읽으면 판정 불능(2)이다.
cfgval() { go run ./tools/archgate -config "$CONFIG" -print "$1"; }

fail=0
step() { printf '\n== %s\n' "$1"; }
run() {
  if ! "$@"; then
    echo "실패: $*" >&2
    fail=1
  fi
}

step "도구"
if ! command -v go >/dev/null 2>&1; then
  echo "go 가 없다" >&2
  exit 2
fi
gover="$(go env GOVERSION | sed 's/^go//')"
if [ "$(printf '%s\n%s\n' "$MIN_GO" "$gover" | sort -V | head -1)" != "$MIN_GO" ]; then
  echo "Go $MIN_GO 이상이 필요하다. 현재 $gover" >&2
  exit 2
fi
echo "go $gover"

step "포맷"
# 종료 코드와 표준 오류를 버리지 않는다. gofmt 가 구문 오류를 내면 그것도 실패다.
fmt_out="$(gofmt -l . 2>&1)"
fmt_code=$?
if [ "$fmt_code" -ne 0 ]; then
  echo "gofmt 오류(종료 코드 $fmt_code):" >&2
  echo "$fmt_out" >&2
  fail=1
elif [ -n "$fmt_out" ]; then
  echo "gofmt 미적용:" >&2
  echo "$fmt_out" >&2
  fail=1
fi

# 게이트 자기검증이 먼저다. 설정을 읽는 것도 게이트가 하므로 그 뒤여야 한다.
step "게이트 자기검증"
if ! go test ./tools/archgate/ ./tools/testreport/; then
  echo "게이트 자기검증이 실패했다. 검증되지 않은 게이트의 판정은 쓰지 않으므로 여기서 멈춘다" >&2
  exit 1
fi

# 설정값. buildTags 는 게이트가 보는 세계와 go 명령이 보는 세계를 같게 하기 위해 전부에 전달한다.
FAST_PKGS="$(cfgval tests.fast)"  || { echo "설정을 읽지 못했다" >&2; exit 2; }
E2E_PKGS="$(cfgval tests.e2e)"    || { echo "설정을 읽지 못했다" >&2; exit 2; }
SCENARIOS="$(cfgval scenarios)"   || { echo "시나리오 문서를 읽지 못했다" >&2; exit 2; }
BUILD_TAGS="$(cfgval build.tags)" || { echo "설정을 읽지 못했다" >&2; exit 2; }
TAGS=()
if [ -n "$BUILD_TAGS" ]; then
  TAGS=(-tags "${BUILD_TAGS// /,}")
fi

step "빌드"
run go build "${TAGS[@]}" ./...

step "vet"
run go vet "${TAGS[@]}" ./...

step "계층, 모양, 시나리오 게이트"
run go run ./tools/archgate -config "$CONFIG"

step "린트"
if command -v golangci-lint >/dev/null 2>&1; then
  if [ -n "$BUILD_TAGS" ]; then
    run golangci-lint run --build-tags "${BUILD_TAGS// /,}"
  else
    run golangci-lint run
  fi
else
  # 도구가 없다고 조용히 통과시키지 않는다. 건너뛴 실행과 확인한 실행은 달라야 한다.
  echo "golangci-lint 가 없다. 1차 방어선을 돌리지 못했다" >&2
  fail=1
fi

# tests <testreport 인자...> -- <go test 인자...>
# go test 의 종료 코드(pipefail)와 testreport 의 판정 둘 다 0 이어야 통과다.
# 빌드 오류 같은 비 JSON 출력은 stderr 로 그대로 나가고, 종료 코드가 실패를 알린다.
tests() {
  local report_args=()
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do report_args+=("$1"); shift; done
  [ "${1:-}" = "--" ] && shift
  go test -json "${TAGS[@]}" "$@" | go run ./tools/testreport "${report_args[@]}"
}

# 시나리오 실행 대조 인자. 문서에 시나리오가 없으면(설정에 scenarios 가 없으면) 비어 있다.
SC_ARGS=()
if [ -n "$SCENARIOS" ]; then
  SC_ARGS=(-scenarios "$SCENARIOS")
fi

RACE=()
if [ "$MODE" != "fast" ]; then
  RACE=(-race)
fi

step "빠른 등급 테스트 (L1, L2)"
# 테스트가 0 개 돈 것은 통과가 아니다. 설정은 패턴이 비어 있지 않다는 것만 보장하고,
# 그 패턴 아래 실제로 테스트가 있는지는 여기서만 안다.
# shellcheck disable=SC2086
if ! tests -require-tests -- "${RACE[@]}" $FAST_PKGS; then
  echo "실패: 빠른 등급" >&2
  fail=1
fi

if [ "$MODE" = "full" ]; then
  step "E2E (통합 등급)"
  # 여기서 미구현은 실패다. 완료 판정의 근거가 E2E 통과인데 t.Skip 이 녹색이면 판정이 없다.
  # shellcheck disable=SC2086
  if ! tests -todo-fails -require-tests "${SC_ARGS[@]}" -- "${RACE[@]}" $E2E_PKGS; then
    echo "실패: E2E" >&2
    fail=1
  fi
fi

if [ "$MODE" = "deep" ]; then
  step "실환경 등급 테스트"
  # 실환경 등급은 코드가 아니라 환경 상태를 확인한다. 캐시된 결과를 통과로 보고하면 안 된다(-count=1).
  # deep는 위 full E2E를 반복하지 않고 실제 환경을 켠 E2E를 한 번 실행한다.
  # 시나리오의 환경 부족 skip도 완료 판정에서는 실패하며 이름과 사유를 보고한다.
  # shellcheck disable=SC2086
  if ! E2E_DEEP=1 tests -todo-fails -require-tests "${SC_ARGS[@]}" -- "${RACE[@]}" -count=1 $E2E_PKGS; then
    echo "실패: 실환경 등급" >&2
    fail=1
  fi
fi

if [ "$fail" -ne 0 ]; then
  printf '\n검증 실패 (%s)\n' "$MODE" >&2
  exit 1
fi
printf '\n검증 통과 (%s)\n' "$MODE"
