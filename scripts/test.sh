#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
(cd "${ROOT}/control-plane" && go test ./...)
(cd "${ROOT}/rust" && cargo test --workspace --all-features)
(cd "${ROOT}/web" && npm test)
