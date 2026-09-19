#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
mode="${1:-full}"
case "${mode}" in basic|full) ;; *) echo 'usage: web-check.sh [basic|full]' >&2; exit 2 ;; esac
(cd "${ROOT}/web/src/api/generated" && npm run build)
(cd "${ROOT}/web" && npm run format:check)
(cd "${ROOT}/web" && npm --ignore-scripts run lint)
(cd "${ROOT}/web" && npx --no-install vue-tsc --noEmit)
(cd "${ROOT}/web" && npm test)
(cd "${ROOT}/web" && npm run build)
(cd "${ROOT}/web" && npm run test:generated-auth)
if [[ "${mode}" == full ]]; then
  (cd "${ROOT}/web" && node test/run-auth-browser.mjs)
fi
