#!/usr/bin/env bash
# Static production auth regression: no containers, real accounts or secrets.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d "${HOME}/ocservia-auth-config.XXXXXX")"
trap 'rm -rf -- "${work}"' EXIT
mkdir -m 0700 "${work}/secrets"
export OCSERV_SECRET_DIR="${work}/secrets" OCSERV_BACKUP_DIR="${work}/backups"
for name in tls.crt tls.key postgres-owner-password postgres-app-password \
  postgres-backup-password postgres.pgpass database-owner-url database-app-url \
  session-key audit-checkpoint-key certificate-signer-token; do
  printf 'test-only-not-a-production-secret\n' >"${OCSERV_SECRET_DIR}/${name}"
  chmod 0444 "${OCSERV_SECRET_DIR}/${name}"
done
for name in audit-event-key controller-command-signing-key.pem relay-access-token controller-iroh.key; do
  printf 'test-only-not-a-production-secret\n' >"${OCSERV_SECRET_DIR}/${name}"
  chmod 0400 "${OCSERV_SECRET_DIR}/${name}"
done
owner=()
if (( EUID != 0 )); then owner=(sudo); fi
"${owner[@]}" chown 65534:65532 "${OCSERV_SECRET_DIR}/audit-event-key" \
  "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem"
"${owner[@]}" chown 65532:65532 "${OCSERV_SECRET_DIR}/relay-access-token" \
  "${OCSERV_SECRET_DIR}/controller-iroh.key"
image="example.invalid/test@sha256:$(printf '%064d' 0)"
export OCSERV_GATEWAY_IMAGE="${image}" OCSERV_CONTROL_IMAGE="${image}"
export OCSERV_TRANSPORT_IMAGE="${image}" OCSERV_BACKUP_IMAGE="${image}"
export OCSERV_POSTGRES_IMAGE="${image}" OCSERV_OTEL_IMAGE="${image}"
export OCSERV_PUBLIC_HOST=controller.example.com OCSERV_AUDIT_EVENT_KEY_ID=test-only
export OCSERV_CONTROLLER_ENDPOINT_ID="$(printf '%064d' 1)"
export OCSERV_CERTIFICATE_SIGNER_URL=https://pki.example.test
export OCSERV_RELAY_URL_A=https://relay-a.example.test OCSERV_RELAY_URL_B=https://relay-b.example.test
unset OCSERV_OTEL_BACKEND_ENDPOINT

# Exercise the published mode examples through each real installer allowlist.
awk -v dir="${work}" '
  /^```dotenv$/ { block++; active=1; next }
  /^```$/ { active=0 }
  active { print > (dir "/mode-" block ".env") }
' "${ROOT}/docs/operations/authentication.md"
source "${ROOT}/deploy/lib/install-env.sh"
for mode in 1 2 3; do
  if (( mode == 2 )); then
    printf 'test-only-not-a-production-secret\n' >"${OCSERV_SECRET_DIR}/oidc-client-secret"
    chmod 0444 "${OCSERV_SECRET_DIR}/oidc-client-secret"
  fi
  for list in bootstrap root installer; do
    case "${list}" in
      bootstrap) section="$(sed -n '/^INSTALL_ENV_NAMES=(/,/^)/p' "${ROOT}/deploy/production/controller-bootstrap.sh")" ;;
      root) section="$(sed -n '/^ROOT_LIFECYCLE_ENV_NAMES=(/,/^)/p' "${ROOT}/deploy/production/install.sh")" ;;
      installer) section="$(sed -n '/^  install_env_load /,/^fi$/p' "${ROOT}/deploy/production/install.sh")" ;;
    esac
    mapfile -t names < <(printf '%s\n' "${section}" | grep -oE 'OCSERV_[A-Z0-9_]+')
    (
      unset OCSERV_LOCAL_AUTH_ENABLED OCSERV_PUBLIC_ORIGIN OCSERV_SESSION_TTL
      unset OCSERV_OIDC_ISSUER OCSERV_OIDC_CLIENT_ID OCSERV_OIDC_REDIRECT_URL
      install_env_load "${work}/mode-${mode}.env" "${names[@]}"
      "${ROOT}/deploy/production/compose.sh" config --format json >"${work}/mode-${mode}.json"
    )
  done
  jq -e --argjson mode "${mode}" --arg dir "${OCSERV_SECRET_DIR}" '
    . as $config |
    all(.services.migrate, .services["control-plane"];
      . as $service | .environment as $env |
      $env.OCSERV_LOCAL_AUTH_ENABLED == (if $mode == 2 then "false" else "true" end) and
      $env.OCSERV_PUBLIC_ORIGIN == "https://controller.example.com" and
      $env.OCSERV_SESSION_TTL == "8h" and
      $env.OCSERV_SESSION_KEY_FILE == "/run/secrets/session_key" and
      any($service.secrets[]; .source == "session_key" and
        (.target == "session_key" or .target == "/run/secrets/session_key")) and
      ($env | has("OCSERV_SESSION_KEY") | not) and
      ($env | has("OCSERV_LOCAL_BOOTSTRAP_PASSWORD") | not) and
      ($env | has("OCSERV_OIDC_CLIENT_SECRET") | not) and
      if $mode == 1 then
        ($env | keys | all(.[]; startswith("OCSERV_OIDC_") | not)) and
        ($config.secrets | has("oidc_client_secret") | not) and
        all($service.secrets[]; .source != "oidc_client_secret")
      else
        $env.OCSERV_OIDC_ISSUER == "https://id.example.com" and
        $env.OCSERV_OIDC_CLIENT_ID == "ocservia" and
        $env.OCSERV_OIDC_REDIRECT_URL == "https://controller.example.com/api/v1/auth/callback" and
        $env.OCSERV_OIDC_CLIENT_SECRET_FILE == "/run/secrets/oidc_client_secret" and
        any($service.secrets[]; .source == "oidc_client_secret" and
          (.target == "oidc_client_secret" or .target == "/run/secrets/oidc_client_secret")) and
        $config.secrets.oidc_client_secret.file == ($dir + "/oidc-client-secret")
      end
    ) and .secrets.session_key.file == ($dir + "/session-key") and
    all(.services[].environment // {} | keys[]; endswith("_PASSWORD") | not)
  ' "${work}/mode-${mode}.json" >/dev/null
done

OCSERV_LOCAL_AUTH_ENABLED=true OCSERV_PUBLIC_ORIGIN=https://custom.example.com \
  OCSERV_SESSION_TTL=30m OCSERV_OIDC_ISSUER=https://id.example.com \
  OCSERV_OIDC_CLIENT_ID=ocservia \
  OCSERV_OIDC_REDIRECT_URL=https://custom.example.com/api/v1/auth/callback \
  "${ROOT}/deploy/production/compose.sh" config --format json |
  jq -e 'all(.services.migrate, .services["control-plane"]; .environment |
    .OCSERV_PUBLIC_ORIGIN == "https://custom.example.com" and
    .OCSERV_SESSION_TTL == "30m" and
    .OCSERV_OIDC_REDIRECT_URL == "https://custom.example.com/api/v1/auth/callback")' >/dev/null

export OCSERV_LOCAL_AUTH_ENABLED=true OCSERV_PUBLIC_ORIGIN=https://controller.example.com
export OCSERV_OIDC_ISSUER= OCSERV_OIDC_CLIENT_ID= OCSERV_OIDC_REDIRECT_URL=
expect_failure() {
  if "$@" >"${work}/failure.log" 2>&1; then
    echo "unexpected success: $*" >&2
    exit 1
  fi
}
expect_failure env OCSERV_LOCAL_AUTH_ENABLED=false "${ROOT}/deploy/production/compose.sh" config --quiet
expect_failure env OCSERV_OIDC_CLIENT_ID=partial "${ROOT}/deploy/production/compose.sh" config --quiet
chmod 0600 "${OCSERV_SECRET_DIR}/oidc-client-secret"
expect_failure env OCSERV_OIDC_ISSUER=https://id.example.test OCSERV_OIDC_CLIENT_ID=test \
  "${ROOT}/deploy/production/compose.sh" config --quiet
rm "${OCSERV_SECRET_DIR}/oidc-client-secret"
expect_failure env OCSERV_OIDC_ISSUER=https://id.example.test OCSERV_OIDC_CLIENT_ID=test \
  "${ROOT}/deploy/production/compose.sh" config --quiet
rm "${OCSERV_SECRET_DIR}/session-key"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
echo "Production auth configuration: all three documented modes, installer allowlists, secret mounts and rejection checks passed"
