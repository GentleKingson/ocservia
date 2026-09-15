#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
log_args=()
if [[ "$#" == 1 && "$1" == --candidate-history ]]; then
  if [[ "$(git -C "${ROOT}" rev-parse --is-shallow-repository)" != false ]]; then
    echo "candidate secret scan requires complete history" >&2
    exit 2
  fi
  # Keep Gitleaks' traversal defaults, but exclude unrelated release branches.
  log_args+=(--log-opts="--full-history --diff-filter=tuxdb HEAD")
  echo "scanning complete candidate history: $(git -C "${ROOT}" rev-parse HEAD)"
elif [[ "$#" != 0 ]]; then
  echo "usage: security-check.sh [--candidate-history]" >&2
  exit 2
fi
gitleaks git --no-banner --redact --no-color --config "${ROOT}/scripts/g6-secret-scan.toml" "${log_args[@]}" "${ROOT}"
"${ROOT}/scripts/test-g6-secret-scan-config-runtime.sh"
