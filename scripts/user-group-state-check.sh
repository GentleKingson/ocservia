#!/usr/bin/env bash
set -euo pipefail
ulimit -c 0

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

(cd "${ROOT}/control-plane" && go test ./internal/userstate ./internal/telemetry ./internal/api ./internal/semanticpayload ./internal/operations -count=1)
(cd "${ROOT}/rust" && cargo test -p ocservia-agent-protocol -p ocservia-ocserv-adapter -p ocservia-privd -p ocservia-agent --all-features)
"${ROOT}/scripts/agent-boundary-check.sh"
"${ROOT}/scripts/transport-boundary-check.sh"

if grep -R -n -E --include='*.go' --include='*.rs' --include='*.ts' --include='*.vue' \
  'json:"password|json:"password_hash|tracing::(info|warn|error)!.*password|slog\..*password' \
  "${ROOT}/control-plane" "${ROOT}/rust/crates" "${ROOT}/web/src"; then
  echo "password material entered an ordinary serialization or log surface" >&2
  exit 1
fi

grep -q "writeOnly: true" "${ROOT}/openapi/openapi.yaml"
grep -q "rsa_padding_mode:oaep" "${ROOT}/rust/crates/ocserv-adapter/src/lib.rs"
grep -q "atomic_replace" "${ROOT}/rust/crates/ocserv-adapter/src/lib.rs"
