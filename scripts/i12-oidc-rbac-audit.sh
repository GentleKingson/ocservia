#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

(cd "${ROOT}/control-plane" && go test ./internal/auth ./internal/rbac ./internal/approvals ./internal/audit ./internal/api ./internal/operations -count=1)

if grep -R -n -E --include='*.go' --include='*.ts' --include='*.vue' \
  'localStorage.*(token|Token)|SameSiteNoneMode|Secure:[[:space:]]*false|HttpOnly:[[:space:]]*false' \
  "${ROOT}/control-plane" "${ROOT}/web/src"; then
  echo "unsafe authentication storage or cookie setting found" >&2
  exit 1
fi

grep -q 'oauth2.S256ChallengeOption' "${ROOT}/control-plane/internal/auth/service.go"
grep -q 'subtle.ConstantTimeCompare.*attempt.Nonce' "${ROOT}/control-plane/internal/auth/service.go"
grep -q 'requester_id IS DISTINCT FROM approver_id' "${ROOT}/control-plane/migrations/000011_oidc_rbac_approvals_audit.up.sql"
