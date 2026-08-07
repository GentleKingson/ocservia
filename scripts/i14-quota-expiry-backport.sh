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
  .branch == "master" and
  .imported_at == "2026-08-07T16:41:52Z" and
  .license == "MIT" and
  .license_blob == "ce7caf5a71c20fd589b9e5c251554afc6efa3681" and
  .old.commit == "b8f59026c4d879f40c1da43dc00d97e34f9790bc" and
  .new.ref == "master" and
  .new.commit == "4d25478580d899b77460bdf0cf0a590cfdd26030" and
  .diff.ahead_by == 2 and .diff.total_commits == 2 and
  .diff.commits == ["b84969abac926c0ddc0a560ca06ebf85cbe5c787", "4d25478580d899b77460bdf0cf0a590cfdd26030"] and
  .diff.patch_file == "docs/upstream/v4.9-post1.patch" and
  .diff.patch_sha256 == "be9b113b2c2d5f32acf146f361bd2edfd32464708343db910fc1174eba7cc25a" and
  .diff.files == ["web/src/components/auth/SetupForm.vue"] and
  .diff.dependencies == [] and .diff.migrations == [] and
  .diff.security_sensitive_files == ["web/src/components/auth/SetupForm.vue"] and
  .classification.A == [] and
  .classification.B == ["web/src/components/auth/SetupForm.vue"] and
  (.classification.D | length) == 4 and
  .adaptation.upstream_view == "web/src/upstream/UserPolicyFields.vue" and
  .adaptation.node_adapter == "web/src/adapters/user-policy.ts" and
  .publication.review_pr == "https://github.com/GentleKingson/ocservia/pull/15" and
  .publication.implementation_pr == "https://github.com/GentleKingson/ocservia/pull/14" and
  .adaptation.verbatim_upstream_files == []
' "${manifest}" >/dev/null
printf '%s  %s\n' \
  "$(jq -r '.diff.patch_sha256' "${manifest}")" \
  "${ROOT}/$(jq -r '.diff.patch_file' "${manifest}")" | shasum -a 256 -c -
test -f "${ROOT}/web/src/upstream/UserPolicyFields.vue"
test -f "${ROOT}/web/src/adapters/user-policy.ts"

rejected_boundary_pattern='occtl|systemctl|docker[.]sock|/etc/ocserv|/proc|/sys|privileged'
if command -v rg >/dev/null 2>&1; then
  boundary_scanner='rg'
  boundary_matches() {
    rg -n --glob '!**/*_test.go' --glob '!api/generated/**' "${rejected_boundary_pattern}" "$@"
  }
else
  boundary_scanner='grep'
  boundary_matches() {
    grep -REnE --exclude='*_test.go' --exclude-dir=generated "${rejected_boundary_pattern}" "$@"
  }
fi

if boundary_matches \
  "${ROOT}/control-plane/internal" \
  "${ROOT}/web/src"; then
  echo "I14 imported a rejected local-execution boundary" >&2
  exit 1
fi

printf 'I14 rejected-boundary scan passed with %s\n' "${boundary_scanner}"
