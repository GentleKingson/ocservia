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

if [[ "${MODE}" != "race" ]]; then
  test -z "$(gofmt -l "${ROOT}/control-plane")"
  (cd "${ROOT}/control-plane" && go vet ./...)
  (cd "${ROOT}/control-plane" && \
    HOME="${ROOT}/.cache/staticcheck-home" staticcheck ./...)
  (cd "${ROOT}/control-plane" && go test ./...)
  (cd "${ROOT}/control-plane" && govulncheck ./...)
fi

if [[ "${MODE}" != "standard" ]]; then
  (cd "${ROOT}/control-plane" && go test -race ./...)
fi
