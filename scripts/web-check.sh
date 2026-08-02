#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
(cd "${ROOT}/web" && npm run format:check)
(cd "${ROOT}/web" && npm run lint)
(cd "${ROOT}/web" && npm run typecheck)
(cd "${ROOT}/web" && npm test)
(cd "${ROOT}/web" && npm run build)
(cd "${ROOT}/web/src/api/generated" && npm run build)
(cd "${ROOT}/web" && npm run test:generated-auth)
(cd "${ROOT}/web" && npm audit --audit-level=high)
