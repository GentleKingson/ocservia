#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
test -z "$(gofmt -l "${ROOT}/control-plane")"
(cd "${ROOT}/control-plane" && go vet ./...)
(cd "${ROOT}/control-plane" && \
  HOME="${ROOT}/.cache/staticcheck-home" staticcheck ./...)
(cd "${ROOT}/control-plane" && go test ./...)
(cd "${ROOT}/control-plane" && go test -race ./...)
(cd "${ROOT}/control-plane" && govulncheck ./...)
