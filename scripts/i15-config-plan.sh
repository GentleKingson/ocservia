#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

(cd "${ROOT}/control-plane" && go test ./internal/configplan ./internal/api ./internal/operations -count=1)
(cd "${ROOT}/rust" && cargo test -p ocservia-agent-protocol -p ocservia-ocserv-adapter -p ocservia-privd -p ocservia-agent)

grep -Fq '/nodes/{node_id}/config-plans' "${ROOT}/openapi/openapi.yaml"
grep -Fq 'current_unchanged' "${ROOT}/proto/ocserv/platform/agent/v1/agent.proto"
grep -Fq 'create_new(true)' "${ROOT}/rust/crates/ocserv-adapter/src/lib.rs"
grep -Fq 'staging.remove().await?' "${ROOT}/rust/crates/ocserv-adapter/src/lib.rs"

if grep -REnE 'target_path|shell[.]exec|command[.]run|occtl[.]raw|systemctl[.]raw' \
  "${ROOT}/control-plane/internal/configplan" \
  "${ROOT}/control-plane/internal/api/configplans.go" \
  "${ROOT}/proto/ocserv/platform/agent/v1/agent.proto"; then
  echo "I15 exposed a caller-selected path or arbitrary execution surface" >&2
  exit 1
fi

if grep -REnE 'os[.]Rename|tokio::fs::rename|service_reload' \
  "${ROOT}/control-plane/internal/configplan" \
  "${ROOT}/control-plane/internal/api/configplans.go"; then
  echo "I15 planning path contains reload or current-file replacement behavior" >&2
  exit 1
fi

printf 'I15 side-effect boundary and staging-cleanup checks passed\n'
