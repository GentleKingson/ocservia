#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

MODE="${1:-full}"
if (($# > 1)) || [[ "${MODE}" != "full" && "${MODE}" != "--contract-only" ]]; then
  echo "usage: $0 [--contract-only]" >&2
  exit 2
fi
if [[ "${MODE}" == "full" ]]; then
  (cd "${ROOT}/control-plane" && go test ./internal/configplan ./internal/api ./internal/api/configplanhttp ./internal/api/httpx ./internal/operations -count=1)
  (cd "${ROOT}/rust" && cargo test -p ocservia-agent-protocol -p ocservia-ocserv-adapter -p ocservia-privd -p ocservia-agent)
fi

grep -Fq '/nodes/{node_id}/config-plans' "${ROOT}/openapi/openapi.yaml"
grep -Fq 'current_unchanged' "${ROOT}/proto/ocserv/platform/agent/v1/agent.proto"
grep -Fq 'create_new(true)' "${ROOT}/rust/crates/ocserv-adapter/src/lib.rs"
grep -Fq 'staging.remove().await?' "${ROOT}/rust/crates/ocserv-adapter/src/lib.rs"

if grep -REnE 'target_path|shell[.]exec|command[.]run|occtl[.]raw|systemctl[.]raw' \
  "${ROOT}/control-plane/internal/configplan" \
  "${ROOT}/control-plane/internal/api/configplanhttp" \
  "${ROOT}/proto/ocserv/platform/agent/v1/agent.proto"; then
  echo "Configuration plan exposed a caller-selected path or arbitrary execution surface" >&2
  exit 1
fi

if grep -REnE 'os[.]Rename|tokio::fs::rename|service_reload' \
  "${ROOT}/control-plane/internal/configplan" \
  "${ROOT}/control-plane/internal/api/configplanhttp"; then
  echo "Configuration planning path contains reload or current-file replacement behavior" >&2
  exit 1
fi

printf 'Configuration plan side-effect boundary and staging-cleanup checks passed\n'
