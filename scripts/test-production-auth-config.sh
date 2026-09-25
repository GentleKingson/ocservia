#!/usr/bin/env bash
# Static production auth regression: no containers, real accounts or secrets.
set -euo pipefail
if (( EUID != 0 )); then
  exec sudo -- bash "$0" "$@"
fi
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d "${HOME}/ocservia-auth-config.XXXXXX")"
trap '"${owner[@]}" rm -rf -- "${work}"' EXIT
owner=()
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
if (( EUID != 0 )); then owner=(sudo); fi
"${owner[@]}" install -o 0 -g 65532 -m 440 /dev/null "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem"
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
    (.services.transportd.command | index("--controller-verification-key-file") != null) and
    any(.services.transportd.secrets[]; .source == "controller_command_verification_key" and .uid == "0" and .gid == "65532") and
    all(.services.transportd.secrets[]; .source != "controller_command_signing_key") and
    .secrets.controller_command_verification_key.file == ($dir + "/controller-command-verification-key.pem") and
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
"${owner[@]}" chmod 0444 "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
"${owner[@]}" chmod 0440 "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem"
mv "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem" "${OCSERV_SECRET_DIR}/verification-saved.pem"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
ln -s verification-saved.pem "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
rm "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem"
mv "${OCSERV_SECRET_DIR}/verification-saved.pem" "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem"
ln "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem" "${OCSERV_SECRET_DIR}/verification-hardlink.pem"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
rm "${OCSERV_SECRET_DIR}/verification-hardlink.pem"
"${ROOT}/deploy/production/compose.sh" config --format json |
  jq -e '.secrets | has("relay_ca") | not' >/dev/null
"${owner[@]}" install -o 0 -g 0 -m 444 "${OCSERV_SECRET_DIR}/tls.crt" "${OCSERV_SECRET_DIR}/relay-ca.pem"
"${ROOT}/deploy/production/compose.sh" config --format json |
  jq -e --arg dir "${OCSERV_SECRET_DIR}" '
    .secrets.relay_ca.file == ($dir + "/relay-ca.pem") and
    any(.services.transportd.secrets[]; .source == "relay_ca") and
    all(.services | to_entries[] | select(.key != "transportd") | .value.secrets[]?; .source != "relay_ca")' >/dev/null
"${owner[@]}" chmod 644 "${OCSERV_SECRET_DIR}/relay-ca.pem"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
"${owner[@]}" chmod 444 "${OCSERV_SECRET_DIR}/relay-ca.pem"
"${owner[@]}" chown 65532:65532 "${OCSERV_SECRET_DIR}/relay-ca.pem"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
"${owner[@]}" chown 0:0 "${OCSERV_SECRET_DIR}/relay-ca.pem"
mv "${OCSERV_SECRET_DIR}/relay-ca.pem" "${OCSERV_SECRET_DIR}/relay-ca.saved"
ln -s relay-ca.saved "${OCSERV_SECRET_DIR}/relay-ca.pem"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
rm "${OCSERV_SECRET_DIR}/relay-ca.pem"
mv "${OCSERV_SECRET_DIR}/relay-ca.saved" "${OCSERV_SECRET_DIR}/relay-ca.pem"
ln "${OCSERV_SECRET_DIR}/relay-ca.pem" "${OCSERV_SECRET_DIR}/relay-ca.link"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
rm "${OCSERV_SECRET_DIR}/relay-ca.link" "${OCSERV_SECRET_DIR}/relay-ca.pem"
"${owner[@]}" install -o 0 -g 0 -m 444 /dev/null "${OCSERV_SECRET_DIR}/relay-ca.pem"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
rm "${OCSERV_SECRET_DIR}/relay-ca.pem"
chmod 0770 "${work}"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
chmod 0700 "${work}"
chmod 0600 "${OCSERV_SECRET_DIR}/oidc-client-secret"
expect_failure env OCSERV_OIDC_ISSUER=https://id.example.test OCSERV_OIDC_CLIENT_ID=test \
  "${ROOT}/deploy/production/compose.sh" config --quiet
rm "${OCSERV_SECRET_DIR}/oidc-client-secret"
expect_failure env OCSERV_OIDC_ISSUER=https://id.example.test OCSERV_OIDC_CLIENT_ID=test \
  "${ROOT}/deploy/production/compose.sh" config --quiet
# Integrated shares the existing auth and database overlays and must preserve
# their private networks while adding the isolated, TLS-verified Signer.
export OCSERV_DEPLOYMENT_MODE=integrated OCSERV_RELAY_PUBLIC_HOST=relay.example.com
export OCSERV_RELAY_SECRET_DIR="${work}/relay"
export OCSERV_SIGNER_SECRET_DIR="${work}/signer-secrets" OCSERV_SIGNER_STATE_DIR="${work}/signer-state"
export OCSERV_EDGE_IMAGE="${image}" OCSERV_RELAY_IMAGE="${image}" OCSERV_SIGNER_IMAGE="${image}"
unset OCSERV_RELAY_URL_A OCSERV_RELAY_URL_B OCSERV_CERTIFICATE_SIGNER_URL
mkdir -m 700 "${OCSERV_RELAY_SECRET_DIR}" "${OCSERV_SIGNER_SECRET_DIR}" "${OCSERV_SIGNER_STATE_DIR}"
for name in tls.crt tls.key; do
  cp "${OCSERV_SECRET_DIR}/${name}" "${OCSERV_RELAY_SECRET_DIR}/${name}"
done
for name in issuer-chain.pem issuer-key.pem tls-cert.pem tls-key.pem api-token tls-ca.pem; do
  cp "${OCSERV_SECRET_DIR}/certificate-signer-token" "${OCSERV_SIGNER_SECRET_DIR}/${name}"
  "${owner[@]}" chown 65532:65532 "${OCSERV_SIGNER_SECRET_DIR}/${name}"
  chmod 400 "${OCSERV_SIGNER_SECRET_DIR}/${name}"
done
chmod 444 "${OCSERV_SIGNER_SECRET_DIR}/tls-ca.pem"
"${owner[@]}" chown 65532:65532 "${OCSERV_SIGNER_SECRET_DIR}" "${OCSERV_SIGNER_STATE_DIR}"
for name in database-ca.pem database-backup.cnf oidc-client-secret; do
  cp "${OCSERV_SECRET_DIR}/certificate-signer-token" "${OCSERV_SECRET_DIR}/${name}"
done
export OCSERV_DATABASE_BACKUP_HOST=database.example.com OCSERV_DATABASE_BACKUP_IMAGE="${image}"
for backend in postgres:bundled postgres:external mysql:external mariadb:external; do
  export OCSERV_DATABASE_BACKEND="${backend%:*}" OCSERV_DATABASE_DEPLOYMENT="${backend#*:}"
  for auth in local oidc combined; do
    export OCSERV_LOCAL_AUTH_ENABLED=true OCSERV_OIDC_ISSUER='' OCSERV_OIDC_CLIENT_ID='' OCSERV_OIDC_REDIRECT_URL=''
    if [[ "${auth}" != local ]]; then
      export OCSERV_OIDC_ISSUER=https://id.example.com OCSERV_OIDC_CLIENT_ID=test
      export OCSERV_OIDC_REDIRECT_URL=https://controller.example.com/api/v1/auth/callback
      [[ "${auth}" != oidc ]] || export OCSERV_LOCAL_AUTH_ENABLED=false
    fi
    "${ROOT}/deploy/production/compose.sh" config --format json >"${work}/integrated.json"
    jq -e --arg backend "${backend}" '
      (.services["control-plane"].networks | has("application") and has("signer") and
        has(if $backend == "postgres:bundled" then "database" else "database-egress" end)) and
      .services["control-plane"].environment.OCSERV_CERTIFICATE_SIGNER_URL == "https://signer:9443/sign" and
      .services["control-plane"].environment.OCSERV_CERTIFICATE_SIGNER_CA_FILE == "/run/secrets/certificate_signer_ca" and
      .services.transportd.environment.OCSERV_RELAY_URL_A == "https://relay.example.com" and
      (.services.signer.networks | keys == ["signer"]) and
      (.services.signer.ports // [] | length == 0) and .networks.signer.internal
    ' "${work}/integrated.json" >/dev/null
  done
done
expect_failure env OCSERV_RELAY_PUBLIC_HOST=controller.example.com "${ROOT}/deploy/production/compose.sh" config --quiet
expect_failure env OCSERV_RELAY_URL_B=https://second.example.com "${ROOT}/deploy/production/compose.sh" config --quiet
expect_failure env OCSERV_AUTH_TRUSTED_PROXY_CIDRS= "${ROOT}/deploy/production/compose.sh" config --quiet
expect_failure env OCSERV_CERTIFICATE_SIGNER_URL=https://elsewhere.test/sign "${ROOT}/deploy/production/compose.sh" config --quiet
expect_failure env OCSERV_SIGNER_IMAGE=signer:latest "${ROOT}/deploy/production/compose.sh" config --quiet
expect_failure "${ROOT}/deploy/production/compose.sh" up --build
"${owner[@]}" chmod 444 "${OCSERV_SIGNER_SECRET_DIR}/issuer-key.pem"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
"${owner[@]}" chmod 400 "${OCSERV_SIGNER_SECRET_DIR}/issuer-key.pem"
# Ordinary partial Compose activation must not stop an unselected Signer.
# Lifecycle transactions own its exclusive stop/inspect/restart sequence.
mkdir -m 700 "${work}/bin" "${OCSERV_BACKUP_DIR}"
"${owner[@]}" chown 999:999 "${OCSERV_BACKUP_DIR}"
CONFIG_TEST_DOCKER="$(command -v docker)"
export CONFIG_TEST_DOCKER CONFIG_TEST_LOG="${work}/commands.log"
cat >"${work}/bin/docker" <<'EOF'
#!/usr/bin/env bash
case " $* " in
  *" config "*|*" version "*) exec "${CONFIG_TEST_DOCKER}" "$@" ;;
  *) printf '%s\n' "$*" >>"${CONFIG_TEST_LOG}" ;;
esac
EOF
chmod 700 "${work}/bin/docker"
PATH="${work}/bin:${PATH}" "${ROOT}/deploy/production/compose.sh" up -d --wait --no-deps control-plane
grep -Fq ' stop control-plane transportd' "${CONFIG_TEST_LOG}"
if grep -Fq ' stop signer' "${CONFIG_TEST_LOG}"; then
  echo "partial Controller activation stopped Signer without restarting it" >&2; exit 1
fi
rm "${OCSERV_SECRET_DIR}/session-key"
expect_failure "${ROOT}/deploy/production/compose.sh" config --quiet
echo "Production auth configuration: all three documented modes, installer allowlists, secret mounts and rejection checks passed"
