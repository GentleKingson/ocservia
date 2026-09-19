#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

MODE="${1:-full}"
if (($# > 1)); then
  echo "usage: $0 [full|standard|race]" >&2
  exit 2
fi
case "${MODE}" in
  full | standard | race) ;;
  *)
    echo "unsupported Go check mode: ${MODE}" >&2
    exit 2
    ;;
esac

# shellcheck source=scripts/go-test-environment.sh
source "${ROOT}/scripts/go-test-environment.sh"
require_test_commands go
if [[ "${MODE}" != race ]]; then
  require_test_commands gofmt
fi
if [[ "${MODE}" != standard ]]; then
  require_go_race
fi

GO_MODULES=(control-plane tools/g6-harness)

if [[ "${MODE}" != "race" ]]; then
  test -z "$(gofmt -l "${ROOT}/control-plane" "${ROOT}/tools/g6-harness")"
  for module in "${GO_MODULES[@]}"; do
    (cd "${ROOT}/${module}" && go vet ./...)
    (cd "${ROOT}/${module}" && go test -count=1 ./...)
  done
fi

if [[ "${MODE}" != "standard" ]]; then
  for module in "${GO_MODULES[@]}"; do
    (cd "${ROOT}/${module}" && go test -race -count=1 ./...)
  done
fi
