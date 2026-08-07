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

manifest="${ROOT}/docs/upstream/v4.9-post1.manifest.json"
jq -e '
  .schema_version == 1 and
  .repository == "https://github.com/mmtaee/ocserv-dashboard" and
  .license == "MIT" and
  .old.commit == "b8f59026c4d879f40c1da43dc00d97e34f9790bc" and
  .new.commit == "4d25478580d899b77460bdf0cf0a590cfdd26030" and
  .diff.files == ["web/src/components/auth/SetupForm.vue"] and
  .diff.dependencies == [] and .diff.migrations == [] and
  .diff.security_sensitive_files == [] and
  .classification.A == [] and
  .classification.B == ["web/src/components/auth/SetupForm.vue"] and
  (.classification.D | length) == 4 and
  .adaptation.verbatim_upstream_files == []
' "${manifest}" >/dev/null
test -f "${ROOT}/web/src/upstream/UserPolicyFields.vue"
test -f "${ROOT}/web/src/adapters/user-policy.ts"

rejected_boundary_pattern='occtl|systemctl|docker[.]sock|/etc/ocserv|/proc|/sys|privileged'
if command -v rg >/dev/null 2>&1; then
  boundary_scanner='rg'
  boundary_matches() {
    rg -n "${rejected_boundary_pattern}" "$@"
  }
else
  boundary_scanner='grep'
  boundary_matches() {
    grep -REnE "${rejected_boundary_pattern}" "$@"
  }
fi

if boundary_matches \
  "${ROOT}/control-plane/internal/useroperations" \
  "${ROOT}/web/src/upstream" \
  "${ROOT}/web/src/adapters"; then
  echo "I14 imported a rejected local-execution boundary" >&2
  exit 1
fi

printf 'I14 rejected-boundary scan passed with %s\n' "${boundary_scanner}"
