#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

(cd "${ROOT}/control-plane" && go test ./internal/useroperations ./internal/api ./internal/telemetry -count=1)

grep -Fq 'b8f59026c4d879f40c1da43dc00d97e34f9790bc' "${ROOT}/docs/upstream/v4.9-post1.md"
grep -Fq '4d25478580d899b77460bdf0cf0a590cfdd26030' "${ROOT}/docs/upstream/v4.9-post1.md"
grep -Fq 'mmtaee/ocserv-dashboard' "${ROOT}/THIRD_PARTY_NOTICES.md"
grep -Fq '/user-batches' "${ROOT}/openapi/openapi.yaml"
grep -Fq 'DefaultGlobalConcurrency = 50' "${ROOT}/control-plane/internal/useroperations/service.go"

if rg -n 'occtl|systemctl|docker[.]sock|/etc/ocserv|/proc|/sys|privileged' \
  "${ROOT}/control-plane/internal/useroperations" \
  "${ROOT}/web/src/upstream" \
  "${ROOT}/web/src/adapters"; then
  echo "I14 imported a rejected local-execution boundary" >&2
  exit 1
fi
