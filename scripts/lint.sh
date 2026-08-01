#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
shellcheck -x "${ROOT}"/scripts/*.sh
(cd "${ROOT}/proto" && buf format --diff --exit-code && buf lint)
(cd "${ROOT}/web" && npx --no-install redocly lint \
  --config ../openapi/.redocly.yaml ../openapi/openapi.yaml)
(cd "${ROOT}/control-plane" && go vet ./...)
(cd "${ROOT}/rust" && cargo clippy --workspace --all-targets --all-features -- -D warnings)
(cd "${ROOT}/web" && npm run format:check && npm run lint && npm run typecheck)
"${ROOT}/scripts/check-public-repository.sh"
"${ROOT}/scripts/docs-check.sh"
