#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
gitleaks git --no-banner --redact --no-color --config "${ROOT}/scripts/g6-secret-scan.toml" "${ROOT}"
"${ROOT}/scripts/test-g6-secret-scan-config-runtime.sh"
