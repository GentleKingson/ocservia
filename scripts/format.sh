#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
(cd "${ROOT}/proto" && buf format -w)
(cd "${ROOT}/control-plane" && gofmt -w ./gen ./internal)
(cd "${ROOT}/rust" && cargo fmt --all)
(cd "${ROOT}/web" && npm run format)
