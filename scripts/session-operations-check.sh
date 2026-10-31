#!/usr/bin/env bash
set -euo pipefail
ulimit -c 0

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

(cd "${ROOT}/control-plane" && go test ./internal/api ./internal/operations ./internal/semanticpayload ./internal/telemetry -count=1)
(cd "${ROOT}/rust" && cargo test -p ocservia-ocserv-adapter -p ocservia-agent-protocol -p ocservia-privd --all-features)
(cd "${ROOT}/rust" && cargo test -p ocservia-agent --all-features external_command_is_durable_and_duplicate_delivery_replays)
(cd "${ROOT}/rust" && cargo test -p ocservia-agent --all-features safe_retry_requires_explicit_effect_absence_reconciliation)
(cd "${ROOT}/rust" && cargo test -p ocservia-agent --all-features canonical_semantic_hash_v1_matches_shared_fixture)
"${ROOT}/scripts/agent-boundary-check.sh"
"${ROOT}/scripts/transport-boundary-check.sh"

PATTERN='Command::new\([^)]*(sh|bash)|exec\.Command\([^)]*(sh|bash)|docker\.sock'
if command -v rg >/dev/null 2>&1; then
  MATCHES="$(rg -n --glob '*.go' --glob '*.rs' "${PATTERN}" \
    "${ROOT}/control-plane" "${ROOT}/rust" || true)"
else
  MATCHES="$(grep -R -n -E --include='*.go' --include='*.rs' "${PATTERN}" \
    "${ROOT}/control-plane" "${ROOT}/rust" || true)"
fi
if [[ -n "${MATCHES}" ]]; then
  printf '%s\n' "${MATCHES}"
  echo "forbidden generic execution surface found" >&2
  exit 1
fi
