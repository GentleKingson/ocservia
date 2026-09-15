#!/usr/bin/env bash
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${FROZEN_FILE:?}" "${CONTROLLER_ARCH:?}" "${VERSION:?}" "${IMAGES_DIR:?}" "${ARTIFACT_DIR:?}" "${UPGRADE_SCENARIOS_FILE:?}"
umask 077
work="$(mktemp -d "${HOME}/.ocservia-controller-upgrade.XXXXXX")"
baseline_commit="$(jq -er '.baseline_commit' "${FROZEN_FILE}")"
candidate_commit="$(jq -er '.candidate_sha' "${FROZEN_FILE}")"
baseline_version="$(jq -er '.baseline_tag | ltrimstr("v")' "${FROZEN_FILE}")"
registry="ocservia-upgrade-registry-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
restore="${registry}-restore"
oidc="${registry}-oidc"
active="${work}/baseline"
export OCSERV_SECRET_DIR="${work}/secrets" OCSERV_BACKUP_DIR="${work}/backup"
export OCSERV_CONTROLLER_STATE_ROOT="${work}/state"
export OCSERV_PUBLIC_HOST=localhost OCSERV_HTTPS_ADDRESS=127.0.0.1
export OCSERV_CONTROLLER_PUBLIC_URL=https://localhost
export OCSERV_OIDC_ISSUER=https://172.30.240.3:19443 OCSERV_OIDC_CLIENT_ID=upgrade
export OCSERV_OIDC_REDIRECT_URL=https://localhost/api/v1/auth/callback
export OCSERV_CERTIFICATE_SIGNER_URL=https://172.30.240.1:19443/signer
export OCSERV_RELAY_URL_A=https://172.30.240.1:19443/relay-a OCSERV_RELAY_URL_B=https://172.30.240.1:19443/relay-b
export OCSERV_OTEL_BACKEND_ENDPOINT=172.30.240.1:19443
# The explicit post-data backup must not race the worker's periodic cycle.
export OCSERV_AUDIT_EVENT_KEY_ID=upgrade-test OCSERV_BACKUP_INTERVAL_SECONDS=86400
unset OCSERV_CONTROLLER_COMPOSE_SH OCSERV_CONTROLLER_SMOKE_SH OCSERV_LOCAL_AUTH_ENABLED
unset OCSERV_DATABASE_BACKEND OCSERV_DATABASE_DEPLOYMENT
record() { printf '%s\n' "$@" >>"${UPGRADE_SCENARIOS_FILE}"; }
compose() { "${active}/deploy/production/compose.sh" "$@"; }
controller() { "${active}/deploy/production/controller.sh" "$@"; }
map_images() {
  local name variable
  for name in gateway control transport backup postgres otel; do
    variable="OCSERV_${name^^}_IMAGE"
    export "${variable}=$(jq -er --arg name "${name}" '.images[$name]' "$1")"
  done
}
cleanup() {
  local status=$?
  trap - EXIT
  # Only sanitized state/health is exported, never container environment or DB dumps.
  if [[ -d "${OCSERV_CONTROLLER_STATE_ROOT}" ]]; then
    mkdir -p "${ARTIFACT_DIR}/lifecycle"
    cp "${OCSERV_CONTROLLER_STATE_ROOT}/"*-release.json "${ARTIFACT_DIR}/lifecycle/" 2>/dev/null || true
  fi
  if [[ -x "${active}/deploy/production/compose.sh" ]]; then
    compose ps --format json >"${ARTIFACT_DIR}/services.json" 2>/dev/null || true
    if compose logs --no-color --tail 80 >"${work}/private-services.log" 2>&1; then
      sudo "$(command -v node)" "${ROOT}/scripts/redact-release-upgrade-log.mjs" \
        "${work}/private-services.log" "${OCSERV_SECRET_DIR}" "${work}/redacted-services.log" &&
        sudo install -o "$(id -u)" -g "$(id -g)" -m 600 "${work}/redacted-services.log" "${ARTIFACT_DIR}/services.log" || true
    fi
  fi
  docker rm -f "${oidc}" >/dev/null 2>&1 || true
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  docker rm -f "${registry}" "${restore}" "${registry}-elf-gateway" "${registry}-elf-control" \
    "${registry}-elf-transport" "${registry}-elf-backup" >/dev/null 2>&1 || true
  sudo rm -rf -- "${work}" || true
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Production fixes the Compose project name. Never collide with another install.
if [[ -n "$(docker ps -aq --filter label=com.docker.compose.project=ocservia-production)" ||
  -n "$(docker volume ls -q --filter label=com.docker.compose.project=ocservia-production)" ]]; then
  echo 'refusing to touch an existing ocservia-production project; use an isolated daemon' >&2
  exit 2
fi
mkdir -m 700 "${work}/secrets" "${work}/backup" "${work}/state" "${work}/candidate-bundle" "${work}/restore"
(umask 022; git clone --quiet --no-hardlinks "${ROOT}" "${work}/baseline")
(umask 022; git -C "${work}/baseline" checkout --quiet --detach "${baseline_commit}")
(umask 022; git clone --quiet --no-hardlinks "${ROOT}" "${work}/candidate")
(umask 022; git -C "${work}/candidate" checkout --quiet --detach "${candidate_commit}")
bash "${ROOT}/scripts/release-upgrade-fetch.sh" "${FROZEN_FILE}" "${work}/published"
baseline_manifest="${work}/published/bundle/controller-release-${CONTROLLER_ARCH}.json"
export OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${work}/published/trust/release-signing.pub.pem"
map_images "${baseline_manifest}"
check_elf() {
  local image="$1" name="$2" binary container machine
  case "${name}" in
    gateway) binary=/usr/bin/caddy ;;
    control) binary=/usr/local/bin/ocserv-control ;;
    transport) binary=/usr/local/bin/ocservia-transportd ;;
    backup) binary=/usr/lib/postgresql/17/bin/psql ;;
  esac
  container="$(docker create --name "${registry}-elf-${name}" "${image}")"
  docker cp "${container}:${binary}" "${work}/elf-${name}"
  docker rm "${container}" >/dev/null
  case "${CONTROLLER_ARCH}" in amd64) machine='Advanced Micro Devices X86-64' ;; arm64) machine=AArch64 ;; esac
  readelf -h "${work}/elf-${name}" | grep -F "${machine}"
  sha256sum "${work}/elf-${name}" >>"${ARTIFACT_DIR}/elf-digests.txt"
}
for image in "${OCSERV_GATEWAY_IMAGE}" "${OCSERV_CONTROL_IMAGE}" "${OCSERV_TRANSPORT_IMAGE}" "${OCSERV_BACKUP_IMAGE}" "${OCSERV_POSTGRES_IMAGE}" "${OCSERV_OTEL_IMAGE}"; do
  docker pull "${image}"
  [[ "$(docker image inspect --format '{{.Architecture}}' "${image}")" == "${CONTROLLER_ARCH}" ]]
done
for name in gateway control transport backup; do
  check_elf "$(jq -r --arg name "${name}" '.images[$name]' "${baseline_manifest}")" "${name}"
done
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=upgrade-fixture \
  -addext 'subjectAltName=DNS:localhost,IP:172.30.240.3' -addext 'basicConstraints=critical,CA:TRUE' \
  -keyout "${OCSERV_SECRET_DIR}/tls.key" -out "${OCSERV_SECRET_DIR}/tls.crt" >/dev/null 2>&1
export CURL_CA_BUNDLE="${OCSERV_SECRET_DIR}/tls.crt"
for name in postgres-owner-password postgres-app-password postgres-backup-password oidc-client-secret \
  session-key audit-checkpoint-key audit-event-key certificate-signer-token relay-access-token controller-iroh.key; do
  openssl rand -hex 32 >"${OCSERV_SECRET_DIR}/${name}"
done
openssl genpkey -algorithm ED25519 -out "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem" >/dev/null 2>&1
openssl pkey -in "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem" -outform DER | tail -c 32 | \
  od -An -v -tx1 | tr -d ' \n' >"${OCSERV_SECRET_DIR}/controller-iroh.key"
OCSERV_CONTROLLER_ENDPOINT_ID="$(openssl pkey -in "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem" -pubout -outform DER | tail -c 32 | od -An -v -tx1 | tr -d ' \n')"
export OCSERV_CONTROLLER_ENDPOINT_ID
for role in owner app; do
  printf 'postgres://ocservia_%s:%s@postgres:5432/ocservia?sslmode=disable\n' "${role}" \
    "$(cat "${OCSERV_SECRET_DIR}/postgres-${role}-password")" >"${OCSERV_SECRET_DIR}/database-${role}-url"
done
printf 'postgres:5432:*:ocservia_backup:%s\n' "$(cat "${OCSERV_SECRET_DIR}/postgres-backup-password")" >"${OCSERV_SECRET_DIR}/postgres.pgpass"
cp "${OCSERV_SECRET_DIR}/tls.crt" "${OCSERV_SECRET_DIR}/otel-client.crt"
cp "${OCSERV_SECRET_DIR}/tls.key" "${OCSERV_SECRET_DIR}/otel-client.key"
cp "${OCSERV_SECRET_DIR}/tls.crt" "${OCSERV_SECRET_DIR}/otel-ca.crt"
chmod 444 "${OCSERV_SECRET_DIR}/"*
sudo chown 65534:65532 "${OCSERV_SECRET_DIR}/audit-event-key" "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem"
sudo chown 65532:65532 "${OCSERV_SECRET_DIR}/controller-iroh.key" "${OCSERV_SECRET_DIR}/relay-access-token"
sudo chmod 400 "${OCSERV_SECRET_DIR}/audit-event-key" "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem" \
  "${OCSERV_SECRET_DIR}/controller-iroh.key" "${OCSERV_SECRET_DIR}/relay-access-token"
sudo chown 999:999 "${work}/backup" "${work}/restore"
oidc_image=node:24.18.1-bookworm-slim@sha256:235600a8101ab264e117b1768e925532262668dc9b581ef1dd7d96ced463b8e7
docker pull "${oidc_image}" >/dev/null
[[ "$(docker image inspect --format '{{.Architecture}}' "${oidc_image}")" == "${CONTROLLER_ARCH}" ]]
docker run -d --name "${oidc}" --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -p 127.0.0.1:19443:19443 \
  -v "${ROOT}/scripts/release-upgrade-oidc-fixture.mjs:/fixture.mjs:ro" \
  -v "${OCSERV_SECRET_DIR}/tls.key:/fixture/tls.key:ro" \
  -v "${OCSERV_SECRET_DIR}/tls.crt:/fixture/tls.crt:ro" \
  -v "${OCSERV_SECRET_DIR}/oidc-client-secret:/fixture/oidc-client-secret:ro" \
  "${oidc_image}" node /fixture.mjs /fixture "${OCSERV_OIDC_ISSUER}" >/dev/null
controller install --release-file "${baseline_manifest}"
# Only the fixture joins the unchanged internal production network.
docker network connect --ip 172.30.240.3 ocservia-production_application "${oidc}"
check_version() {
  curl --fail --silent --show-error https://localhost/api/v1/readyz | jq -e '.status == "ok"' >/dev/null
  curl --fail --silent --show-error https://localhost/api/v1/version >"${ARTIFACT_DIR}/version-$1.json"
  jq -e --arg v "$1" --arg sha "$2" '.version == $v and .commit == $sha' "${ARTIFACT_DIR}/version-$1.json" >/dev/null
}
check_version "${baseline_version}" "${baseline_commit}"
record baseline_ready
# Provision the isolated test CA into the running container's trust store.
# This is a bind mount in that container namespace, not a rebuilt image, an
# insecure TLS setting, an authentication bypass, or a host trust-store change.
trust_oidc() {
  local container pid
  container="$(compose ps -q control-plane)"
  pid="$(docker inspect --format '{{.State.Pid}}' "${container}")"
  docker exec --user 0 -i "${container}" sh -c 'cat > /tmp/upgrade-ca.crt; chmod 444 /tmp/upgrade-ca.crt' <"${OCSERV_SECRET_DIR}/tls.crt"
  sudo nsenter --target "${pid}" --mount --root --wd=/ \
    mount --bind /tmp/upgrade-ca.crt /etc/ssl/certs/ca-certificates.crt
}
trust_oidc
cookie="${work}/cookie"
curl --fail --silent --show-error -c "${cookie}" -D "${work}/login.headers" https://localhost/api/v1/auth/login -o /dev/null
location="$(awk 'tolower($1)=="location:" {sub(/\r$/, "", $2); print $2}' "${work}/login.headers")"
[[ "${location}" == "${OCSERV_OIDC_ISSUER}/authorize?"* ]]
curl --fail --silent --show-error --connect-to 172.30.240.3:19443:127.0.0.1:19443 \
  -u "upgrade:$(cat "${OCSERV_SECRET_DIR}/oidc-client-secret")" \
  -D "${work}/authorize.headers" "${location}" -o /dev/null
location="$(awk 'tolower($1)=="location:" {sub(/\r$/, "", $2); print $2}' "${work}/authorize.headers")"
[[ "${location}" == https://localhost/api/v1/auth/callback\?* ]]
curl --fail --silent --show-error -b "${cookie}" -c "${cookie}" "${location}" -o /dev/null
grep -q __Host-ocservia_session "${cookie}"
sql() { compose exec -T postgres psql -XAt -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia "$@"; }
workspace=00000000-0000-7000-8000-000000000001
sql <<SQL
INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES ('${workspace}','Upgrade','upgrade',now(),now());
INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES ('00000000-0000-7000-8000-000000000002','Restricted','restricted',now(),now());
INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,created_at)
 SELECT '00000000-0000-7000-8000-000000000003',id,'${workspace}','PlatformAdmin',now() FROM identities
 WHERE issuer='${OCSERV_OIDC_ISSUER}' AND subject='upgrade-operator';
INSERT INTO nodes(id,workspace_id,name,status,created_at,updated_at)
 VALUES ('00000000-0000-7000-8000-000000000004','${workspace}','old-node','pending',now(),now());
SQL
api_read() {
  curl --fail --silent --show-error -b "${cookie}" -H "X-Workspace-ID: ${workspace}" "https://localhost/api/v1/$1"
}
api_write() {
  local status
  status="$(curl --silent --show-error -b "${cookie}" -H "Origin: https://localhost" -H 'Content-Type: application/json' \
    -H "X-Workspace-ID: ${workspace}" -o "${work}/write-response" -w '%{http_code}' \
    --data "{\"workspace_id\":\"${workspace}\",\"environment\":\"production\",\"reason\":\"upgrade validation\"}" \
    https://localhost/api/v1/node-bootstrap-tokens)"
  [[ "${status}" == 201 ]]
}
api_write
api_read workspaces | jq -S . >"${work}/workspaces.before"
api_read nodes | jq -e '.. | objects | select(.name? == "old-node")' >/dev/null
data_snapshot() {
  sql -c "SELECT jsonb_build_object('workspaces',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM workspaces t),
    'bindings',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM role_bindings t),
    'identity',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM identities t),
    'nodes',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM nodes t),
    'audit',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM audit_events t));" | jq -S .
}
data_snapshot >"${work}/data.before"
# shellcheck disable=SC2024 # runner-owned evidence, root only reads protected keys
sudo sha256sum "${OCSERV_SECRET_DIR}/"* >"${work}/identity.before"
record baseline_data
compose run --rm --no-deps backup --once >"${ARTIFACT_DIR}/baseline-backup.log" 2>&1
backup_id="$(sudo cat "${OCSERV_BACKUP_DIR}/LATEST")"
[[ "${backup_id}" =~ ^[a-zA-Z0-9._-]+$ ]]
sudo cp -a "${OCSERV_BACKUP_DIR}/base/${backup_id}/." "${work}/restore/"
# Production appends this bookkeeping marker after pg_basebackup's manifest.
# Check it before removing it only from the disposable PostgreSQL restore copy.
sudo test -f "${work}/restore/OCSERVIA_BACKUP_ID"
sudo test ! -L "${work}/restore/OCSERVIA_BACKUP_ID"
[[ "$(sudo cat "${work}/restore/OCSERVIA_BACKUP_ID")" == "${backup_id}" ]]
printf '%s\n' "${backup_id}" >"${ARTIFACT_DIR}/backup-id.txt"
sudo rm -- "${work}/restore/OCSERVIA_BACKUP_ID"

# Load the exact four built archives once; Docker push yields registry manifest
# digests. An image config ID is never used as a release digest.
docker run -d --name "${registry}" -p 127.0.0.1:5000:5000 registry:2 >/dev/null
candidate_manifest="${work}/candidate-bundle/controller-release-${CONTROLLER_ARCH}.json"
args=()
for name in gateway control transport backup; do
  docker load -i "${IMAGES_DIR}/${name}-linux-${CONTROLLER_ARCH}.tar" >/dev/null
  image="ghcr.io/gentlekingson/ocservia/${name}:${VERSION}-linux-${CONTROLLER_ARCH}"
  [[ "$(docker image inspect --format '{{.Architecture}}' "${image}")" == "${CONTROLLER_ARCH}" ]]
  check_elf "${image}" "${name}"
  docker tag "${image}" "localhost:5000/${name}:${VERSION}"
  docker push "localhost:5000/${name}:${VERSION}" >"${ARTIFACT_DIR}/push-${name}.log"
  ref="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "localhost:5000/${name}:${VERSION}" | grep "^localhost:5000/${name}@sha256:")"
  [[ "${ref}" =~ ^localhost:5000/${name}@sha256:[0-9a-f]{64}$ ]]
  args+=(--image "${name}=${ref}")
done
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" --output "${candidate_manifest}" \
  --release-version "${VERSION}" --release-tag "v${VERSION}" --source-commit "${candidate_commit}" \
  --migration-dir "${ROOT}/control-plane/migrations" --platform "linux/${CONTROLLER_ARCH}" \
  "${args[@]}" --image "postgres=${OCSERV_POSTGRES_IMAGE}" --image "otel=${OCSERV_OTEL_IMAGE}"
openssl genpkey -algorithm ED25519 -out "${work}/candidate.key" >/dev/null 2>&1
openssl pkey -in "${work}/candidate.key" -pubout -out "${work}/candidate.pub" >/dev/null 2>&1
(cd "${work}/candidate-bundle" && sha256sum "$(basename "${candidate_manifest}")") >"${candidate_manifest}.sha256"
cp "${candidate_manifest}.sha256" "${work}/candidate-bundle/SHA256SUMS"
openssl pkeyutl -sign -rawin -inkey "${work}/candidate.key" -in "${work}/candidate-bundle/SHA256SUMS" -out "${work}/candidate-bundle/SHA256SUMS.sig"
cp "${candidate_manifest}" "${ARTIFACT_DIR}/candidate-manifest.json"
record candidate_images
active="${work}/candidate"
export OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${work}/candidate.pub"
docker stop "${registry}" >/dev/null
if controller upgrade --release-file "${candidate_manifest}" >"${ARTIFACT_DIR}/expected-failure.log" 2>&1; then
  echo 'upgrade unexpectedly confirmed while candidate registry was unavailable' >&2; exit 1
fi
grep -F 'target image pull failed' "${ARTIFACT_DIR}/expected-failure.log"
cmp "${baseline_manifest}" "${OCSERV_CONTROLLER_STATE_ROOT}/current-release.json"
jq -e --arg sha "${candidate_commit}" '.phase == "failed" and .manifest.source_commit == $sha' "${OCSERV_CONTROLLER_STATE_ROOT}/pending-release.json" >/dev/null
cp "${OCSERV_CONTROLLER_STATE_ROOT}/pending-release.json" "${ARTIFACT_DIR}/failed-pending.json"
record failure_pending
docker start "${registry}" >/dev/null
controller upgrade --release-file "${candidate_manifest}"
record same_target_recovery
map_images "${candidate_manifest}"
"${active}/deploy/production/controller-release-smoke.sh" --release-file "${candidate_manifest}"
check_version "${VERSION}" "${candidate_commit}"
record production_smoke
data_snapshot >"${work}/data.after"
cmp "${work}/data.before" "${work}/data.after"
sudo sha256sum -c "${work}/identity.before" >/dev/null
api_read workspaces | jq -S . >"${work}/workspaces.after"
cmp "${work}/workspaces.before" "${work}/workspaces.after"
api_read nodes | jq -e '.. | objects | select(.name? == "old-node")' >/dev/null
denied="$(curl --silent --show-error -b "${cookie}" -H 'X-Workspace-ID: 00000000-0000-7000-8000-000000000002' \
  -o /dev/null -w '%{http_code}' https://localhost/api/v1/nodes)"
[[ "${denied}" == 403 ]]
record data_preserved
api_write
api_read audit/events | jq -e '.. | objects | select(.action? == "node_bootstrap_token.create")' >/dev/null
record authenticated_read_write
compose ps -a --format json migrate | jq -s -e 'length == 1 and .[0].State == "exited" and .[0].ExitCode == 0' >/dev/null
compose run --rm --no-deps migrate "--schema-compatibility-check=$(jq -r .database_migration "${candidate_manifest}")"
record migration
cmp "${candidate_manifest}" "${OCSERV_CONTROLLER_STATE_ROOT}/current-release.json"
cmp "${baseline_manifest}" "${OCSERV_CONTROLLER_STATE_ROOT}/previous-release.json"
test ! -e "${OCSERV_CONTROLLER_STATE_ROOT}/pending-release.json"
sha256sum "${OCSERV_CONTROLLER_STATE_ROOT}/"*-release.json >"${work}/state.before-retry"
controller upgrade --release-file "${candidate_manifest}"
sha256sum -c "${work}/state.before-retry"
record idempotence release_state
# The production lifecycle itself decides rollback compatibility. Only the
# specifically proven changed-descriptor refusal is accepted as forward-only.
if controller rollback >"${ARTIFACT_DIR}/rollback.log" 2>&1; then
  check_version "${baseline_version}" "${baseline_commit}"
  cmp "${baseline_manifest}" "${OCSERV_CONTROLLER_STATE_ROOT}/current-release.json"
  api_read nodes | jq -e '.. | objects | select(.name? == "old-node")' >/dev/null
else
  grep -F 'production deployment descriptor changed since previous release:' "${ARTIFACT_DIR}/rollback.log"
  descriptor="$(sed -n 's/.*production deployment descriptor changed since previous release: //p' "${ARTIFACT_DIR}/rollback.log")"
  [[ "${descriptor}" == deploy/production/* ]]
  if git -C "${ROOT}" diff --quiet "${baseline_commit}" "${candidate_commit}" -- "${descriptor}"; then exit 1; fi
  sha256sum -c "${work}/state.before-retry"
  test ! -e "${OCSERV_CONTROLLER_STATE_ROOT}/pending-release.json"
  check_version "${VERSION}" "${candidate_commit}"
fi
record rollback_contract
docker run --rm --user 999:999 -v "${work}/restore:/restore:ro" --entrypoint pg_verifybackup \
  "${OCSERV_POSTGRES_IMAGE}" /restore
sudo rm -f "${work}/restore/standby.signal"
docker run -d --name "${restore}" --network none --user 999:999 \
  -v "${work}/restore:/var/lib/postgresql/data" --entrypoint postgres "${OCSERV_POSTGRES_IMAGE}" \
  -D /var/lib/postgresql/data -c listen_addresses= -c unix_socket_directories=/tmp >/dev/null
restored=false
for _ in $(seq 1 60); do
  if docker exec "${restore}" pg_isready -h /tmp -U ocservia_owner -d ocservia >/dev/null 2>&1; then restored=true; break; fi
  sleep 1
done
[[ "${restored}" == true ]]
[[ "$(docker exec "${restore}" psql -h /tmp -XAt -U ocservia_owner -d ocservia -c 'SELECT count(*) FROM nodes WHERE name = '\''old-node'\''')" == 1 ]]
[[ "$(docker exec "${restore}" psql -h /tmp -XAt -U ocservia_owner -d ocservia -c 'SELECT count(*) FROM role_bindings')" == 1 ]]
record backup_restore
sha256sum "${work}/data.before" "${work}/data.after" >"${ARTIFACT_DIR}/data-digests.txt"
