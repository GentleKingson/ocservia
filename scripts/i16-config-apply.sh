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
  (cd "${ROOT}/control-plane" && go test ./internal/configplan ./internal/operations ./internal/localslice ./internal/api -count=1)
  (cd "${ROOT}/rust" && cargo test -p ocservia-agent-protocol -p ocservia-ocserv-adapter -p ocservia-privd -p ocservia-agent)
fi

grep -Fq '/config-plans/{plan_id}/apply' "${ROOT}/openapi/openapi.yaml"
grep -Fq 'ConfigApplyResult' "${ROOT}/proto/ocserv/platform/agent/v1/agent.proto"
grep -Fq 'automation_locked' "${ROOT}/control-plane/migrations/000015_config_apply_rollback.up.sql"
grep -Fq 'atomic_replace(&stage_path, &self.resources.config)' "${ROOT}/rust/crates/ocserv-adapter/src/lib.rs"
grep -Fq 'sync_directory(parent).await' "${ROOT}/rust/crates/ocserv-adapter/src/lib.rs"
grep -Fq 'config_apply.rollback_failed' "${ROOT}/control-plane/internal/localslice/service.go"

if grep -REnE 'caller_path|target_path|shell[.]exec|command[.]run|occtl[.]raw|systemctl[.]raw' \
  "${ROOT}/control-plane/internal/configplan" \
  "${ROOT}/control-plane/internal/api/configplans.go" \
  "${ROOT}/rust/crates/agent-protocol/src/lib.rs"; then
  echo "I16 exposed a caller-selected path or arbitrary execution surface" >&2
  exit 1
fi

printf 'I16 atomic apply, rollback, lockout, and typed-boundary checks passed\n'
