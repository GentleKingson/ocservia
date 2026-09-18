#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
scope="${1-all}"
if (($# > 1)) || [[ "${scope}" != all && "${scope}" != common ]]; then
  echo "usage: $0 [all|common]" >&2
  exit 2
fi
shellcheck -x "${ROOT}"/scripts/*.sh
(cd "${ROOT}/proto" && buf format --diff --exit-code && buf lint)
(cd "${ROOT}/web" && npx --no-install redocly lint \
  --config ../openapi/.redocly.yaml ../openapi/openapi.yaml)
if [[ "${scope}" == all ]]; then
  (cd "${ROOT}/control-plane" && go vet ./...)
  (cd "${ROOT}/tools/g6-harness" && go vet ./...)
  (cd "${ROOT}/rust" && cargo clippy --workspace --all-targets --all-features -- -D warnings)
  (cd "${ROOT}/web" && npm run format:check && npm run lint && npm run typecheck)
fi
"${ROOT}/scripts/check-public-repository.sh"
"${ROOT}/scripts/docs-check.sh"
