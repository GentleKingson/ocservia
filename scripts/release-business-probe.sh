#!/usr/bin/env bash
# Disposable hosted runner only. This probe cannot close the full T07 gate.
# shellcheck disable=SC2024 # sudo reads protected state; redirects are runner-owned.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${ARTIFACT_DIR:?}" "${VERSION:?}" "${CANDIDATE_SHA:?}"
[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted ]]
[[ "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && "${CANDIDATE_SHA}" =~ ^[0-9a-f]{40}$ ]]
[[ "${GITHUB_SHA:?}" == "${CANDIDATE_SHA}" && "$(git -C "${ROOT}" rev-parse HEAD)" == "${CANDIDATE_SHA}" ]]
[[ -z "$(git -C "${ROOT}" status --porcelain)" && "$(ps -p 1 -o comm=)" == systemd ]]
[[ ! -e /etc/ocservia-agent && ! -e /usr/libexec/ocservia && ! -e /etc/ocservia ]]
if getent passwd ocserv-agent >/dev/null; then
  echo 'refusing an existing Agent account' >&2
  exit 2
fi
[[ -z "$(docker ps -aq --filter label=com.docker.compose.project=ocservia-production)" ]]
[[ -z "$(docker volume ls -q --filter label=com.docker.compose.project=ocservia-production)" ]]
umask 077
mkdir -m 700 "${ARTIFACT_DIR}"
export T07_WORK
T07_WORK="$(mktemp -d "${HOME}/.t07-XXXXXX")"
work="${T07_WORK}"
stage=preflight
started="$(date -u +%FT%TZ)"
export SOURCE_COMMIT="${CANDIDATE_SHA}" PACKAGE_ARCH=amd64 CONTROLLER_ARCH=amd64
export SOURCE_DATE_EPOCH OUTPUT_DIR="${work}/products" AGENT_SIGNING_KEY="${work}/signing.key"
SOURCE_DATE_EPOCH="$(git -C "${ROOT}" log -1 --format=%ct)"
export BUILDX_BUILDER="business-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}"
registry="${BUILDX_BUILDER}-registry"
export T07_RELAY_CONTAINER="${BUILDX_BUILDER}-relay"
export OCSERV_SECRET_DIR="${work}/secrets" OCSERV_BACKUP_DIR="${work}/backup"
export OCSERV_CONTROLLER_STATE_ROOT="${work}/state"
export OCSERV_PUBLIC_HOST=localhost OCSERV_HTTPS_ADDRESS=127.0.0.1
export OCSERV_CONTROLLER_PUBLIC_URL=https://localhost OCSERV_PUBLIC_ORIGIN=https://localhost
export OCSERV_LOCAL_AUTH_ENABLED=true OCSERV_AUDIT_EVENT_KEY_ID=t07 OCSERV_BACKUP_INTERVAL_SECONDS=86400
export OCSERV_CERTIFICATE_SIGNER_URL=https://signer.unavailable.invalid
unset OCSERV_OIDC_ISSUER OCSERV_OIDC_CLIENT_ID OCSERV_OIDC_REDIRECT_URL OCSERV_OTEL_BACKEND_ENDPOINT
unset OCSERV_CONTROLLER_COMPOSE_SH OCSERV_CONTROLLER_SMOKE_SH OCSERV_MANAGED_NODE_SYSROOT OCSERV_MANAGED_NODE_OS_RELEASE
compose() { "${ROOT}/deploy/production/compose.sh" "$@"; }
record() { printf '%s\n' "$1" >>"${ARTIFACT_DIR}/checkpoints.txt"; }
cleanup() {
  local code=$?
  trap - EXIT
  set +e
  if [[ -f "${work}/private.log" ]]; then
    compose logs --no-color --tail 100 >>"${work}/private.log" 2>&1
    sudo journalctl --no-pager -n 100 -u ocservia-agent -u ocservia-privd -u ocserv >>"${work}/private.log" 2>&1
  fi
  # Never export environment, cookies, raw databases, passwords or raw logs.
  if [[ -f "${work}/private.log" ]]; then
    sudo "$(command -v node)" "${ROOT}/scripts/redact-release-upgrade-log.mjs" \
      "${work}/private.log" "${work}/private" "${work}/sanitized.log" &&
      sudo install -o "$(id -u)" -g "$(id -g)" -m 600 "${work}/sanitized.log" "${ARTIFACT_DIR}/probe.log"
  fi
  jq -n --arg sha "${CANDIDATE_SHA}" --arg version "${VERSION}" --arg start "${started}" \
    --arg end "$(date -u +%FT%TZ)" --arg stage "${stage}" --argjson code "${code}" \
    --arg run "${GITHUB_RUN_ID}" --arg attempt "${GITHUB_RUN_ATTEMPT}" \
    '{candidate_sha:$sha,candidate_version:$version,run_id:$run,run_attempt:$attempt,
      started_at:$start,finished_at:$end,exit_code:$code,last_stage:$stage,
      probe_status:(if $code == 0 then "PASS" else "FAIL" end),t07_status:"BLOCKED",
      planned_topology:{hosts:1,architecture:"amd64",native_systemd_node:true,relays:1,relay_redundancy:false},
      blockers:["unpublished candidate: Release download/bootstrap path not exercised",
        "private Relay CA adaptations are not unchanged production launchers",
        "independent human operators not provisioned; separate real Local identities only",
        "external OIDC and certificate signer/CA unavailable",
        "positive configuration apply and certificate/P12 lifecycle not exercised",
        "browser UI not exercised; HTTPS API evidence only"],
      not_applicable:["T08 independent failure domains and formal SLO"]}' >"${ARTIFACT_DIR}/result.json"
  sudo systemctl stop ocservia-agent ocservia-privd ocserv >/dev/null 2>&1
  sudo ip netns pids t07-client 2>/dev/null | xargs -r sudo kill
  sudo ip netns del t07-client >/dev/null 2>&1
  compose down --volumes --remove-orphans >/dev/null 2>&1
  docker rm -f "${registry}" "${T07_RELAY_CONTAINER}" >/dev/null 2>&1
  docker buildx rm "${BUILDX_BUILDER}" >/dev/null 2>&1
  # Host installation is confined to this ephemeral GitHub runner, destroyed
  # by Actions after the job. Remove this task's private material now.
  sudo rm -rf -- "${work}"
  exit "${code}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'printf "probe failed at line %s, stage %s\n" "${LINENO}" "${stage}" >&2' ERR
mkdir -m 700 "${work}/private" "${work}/secrets" "${work}/state" "${work}/backup" "${work}/bundle"
# Unlike shared BuildServer, this explicitly authorized runner is disposable.
if [[ -f /proc/sys/fs/binfmt_misc/status ]]; then
  printf '%s\n' -1 | sudo tee /proc/sys/fs/binfmt_misc/status >/dev/null
fi
bash "${ROOT}/scripts/release-upgrade-native.sh" amd64 >"${ARTIFACT_DIR}/native.json"
{ uname -a; cat /etc/os-release; docker version; docker compose version; node --version; } >"${ARTIFACT_DIR}/environment.txt"
stage=build
bash "${ROOT}/scripts/bootstrap.sh" native-packages
openssl genpkey -algorithm ED25519 -out "${AGENT_SIGNING_KEY}"
openssl pkey -in "${AGENT_SIGNING_KEY}" -pubout -out "${work}/trusted-release.pub.pem"
export OCSERV_CONTROLLER_RELEASE_PUBLIC_KEY="${work}/trusted-release.pub.pem"
bash "${ROOT}/scripts/build-release-agent.sh" >"${ARTIFACT_DIR}/agent-build.log" 2>&1
bash "${ROOT}/scripts/build-release-controller.sh" >"${ARTIFACT_DIR}/controller-build.log" 2>&1
bash "${ROOT}/scripts/g6-buildx-cache.sh" relay-business-amd64 true business-relay \
  --builder "${BUILDX_BUILDER}" --platform linux/amd64 --provenance=false --load \
  --label "org.opencontainers.image.revision=${CANDIDATE_SHA}" \
  -t "${BUILDX_BUILDER}-relay" -f "${ROOT}/deploy/production/relay.Dockerfile" "${ROOT}" \
  >"${ARTIFACT_DIR}/relay-build.log" 2>&1
docker run --rm --entrypoint /usr/local/bin/iroh-relay "${BUILDX_BUILDER}-relay" --version >"${ARTIFACT_DIR}/relay-version.txt"
docker image inspect --format '{{.Id}} {{.Architecture}}' "${BUILDX_BUILDER}-relay" >"${ARTIFACT_DIR}/relay-image.txt"
find "${OUTPUT_DIR}" -maxdepth 1 -type f -print0 | sort -z | xargs -0 sha256sum >"${ARTIFACT_DIR}/product-digests.txt"
record candidate_built
stage=provision
sudo apt-get update -qq
sudo apt-get install -y --no-install-recommends ocserv openconnect vpnc-scripts sqlite3 iputils-ping
sudo systemctl stop ocserv
{ dpkg-query -W ocserv openconnect libgnutls30t64 openssl systemd; ocserv --version; } >>"${ARTIFACT_DIR}/environment.txt" 2>&1
# Subsequent output can contain task-only secret data. Keep it private until
# the exact-value redactor runs; the uploaded results never contain cookies.
exec >"${work}/private.log" 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=t07-ca \
  -addext basicConstraints=critical,CA:TRUE -keyout "${work}/private/ca.key" -out "${work}/ca.crt"
chmod 444 "${work}/ca.crt"
gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
openssl req -new -newkey rsa:2048 -nodes -subj /CN=localhost \
  -keyout "${work}/private/tls.key" -out "${work}/tls.csr"
printf 'basicConstraints=critical,CA:FALSE\nsubjectAltName=DNS:localhost,IP:127.0.0.1,IP:%s,IP:10.207.0.1\nextendedKeyUsage=serverAuth\n' "${gateway}" >"${work}/leaf.ext"
openssl x509 -req -days 1 -in "${work}/tls.csr" -CA "${work}/ca.crt" -CAkey "${work}/private/ca.key" \
  -CAcreateserial -extfile "${work}/leaf.ext" -out "${OCSERV_SECRET_DIR}/tls.crt"
cp "${work}/private/tls.key" "${OCSERV_SECRET_DIR}/tls.key"
export CURL_CA_BUNDLE="${work}/ca.crt" OCSERV_RELAY_URL_A="https://${gateway}:3443" OCSERV_RELAY_URL_B=
for name in postgres-owner-password postgres-app-password postgres-backup-password session-key audit-checkpoint-key \
  audit-event-key certificate-signer-token relay-access-token; do
  openssl rand -hex 32 >"${work}/private/${name}"
  cp "${work}/private/${name}" "${OCSERV_SECRET_DIR}/${name}"
done
openssl genpkey -algorithm ED25519 -out "${work}/private/controller-command-signing-key.pem"
cp "${work}/private/controller-command-signing-key.pem" "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem"
openssl pkey -in "${work}/private/controller-command-signing-key.pem" -pubout -out "${work}/command.pub.pem"
openssl pkey -in "${work}/private/controller-command-signing-key.pem" -outform DER | tail -c 32 | \
  od -An -v -tx1 | tr -d ' \n' >"${OCSERV_SECRET_DIR}/controller-iroh.key"
OCSERV_CONTROLLER_ENDPOINT_ID="$(openssl pkey -pubin -in "${work}/command.pub.pem" -outform DER | tail -c 32 | od -An -v -tx1 | tr -d ' \n')"
export OCSERV_CONTROLLER_ENDPOINT_ID
for role in owner app; do
  printf 'postgres://ocservia_%s:%s@postgres:5432/ocservia?sslmode=disable\n' "${role}" \
    "$(cat "${work}/private/postgres-${role}-password")" >"${OCSERV_SECRET_DIR}/database-${role}-url"
done
printf 'postgres:5432:*:ocservia_backup:%s\n' "$(cat "${work}/private/postgres-backup-password")" >"${OCSERV_SECRET_DIR}/postgres.pgpass"
chmod 444 "${OCSERV_SECRET_DIR}/"*
sudo chown 65534:65532 "${OCSERV_SECRET_DIR}/audit-event-key" "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem"
sudo chown 65532:65532 "${OCSERV_SECRET_DIR}/controller-iroh.key" "${OCSERV_SECRET_DIR}/relay-access-token"
sudo chmod 400 "${OCSERV_SECRET_DIR}/audit-event-key" "${OCSERV_SECRET_DIR}/controller-command-signing-key.pem" \
  "${OCSERV_SECRET_DIR}/controller-iroh.key" "${OCSERV_SECRET_DIR}/relay-access-token"
sudo chown 999:999 "${work}/backup"
docker run -d --name "${registry}" -p 127.0.0.1:5000:5000 registry:2
args=()
for name in gateway control transport backup; do
  docker load -i "${OUTPUT_DIR}/${name}-linux-amd64.tar"
  docker tag "ghcr.io/gentlekingson/ocservia/${name}:${VERSION}-linux-amd64" "localhost:5000/${name}:${VERSION}"
  docker push "localhost:5000/${name}:${VERSION}"
  ref="$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "localhost:5000/${name}:${VERSION}" | grep "^localhost:5000/${name}@sha256:")"
  args+=(--image "${name}=${ref}")
  export "OCSERV_${name^^}_IMAGE=${ref}"
done
# The frozen existing production database/image rows, not a new support matrix.
export OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE
OCSERV_POSTGRES_IMAGE=docker.io/library/postgres@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f
OCSERV_OTEL_IMAGE=docker.io/otel/opentelemetry-collector@sha256:0c066d4388070dad8dc9961d9f23649e85a226620e6b359334e4a6c7f9d73b23
manifest="${work}/bundle/controller-release-amd64.json"
node "${ROOT}/scripts/generate-controller-release-manifest.mjs" --output "${manifest}" \
  --release-version "${VERSION}" --release-tag "v${VERSION}" --source-commit "${CANDIDATE_SHA}" \
  --migration-dir "${ROOT}/control-plane/migrations" --platform linux/amd64 \
  "${args[@]}" --image "postgres=${OCSERV_POSTGRES_IMAGE}" --image "otel=${OCSERV_OTEL_IMAGE}"
(cd "${work}/bundle" && sha256sum controller-release-amd64.json >SHA256SUMS)
openssl pkeyutl -sign -rawin -inkey "${AGENT_SIGNING_KEY}" -in "${work}/bundle/SHA256SUMS" -out "${work}/bundle/SHA256SUMS.sig"
cp "${manifest}" "${ARTIFACT_DIR}/candidate-manifest.json"
stage=controller_install
"${ROOT}/deploy/production/controller.sh" install --release-file "${manifest}"
curl --fail --silent --show-error https://localhost/api/v1/version | \
  jq -e --arg sha "${CANDIDATE_SHA}" --arg v "${VERSION}" '.commit == $sha and .version == $v' >/dev/null
record signed_controller_lifecycle
export T07_WORKSPACE=00000000-0000-7000-8000-000000000071
compose exec -T postgres psql -XAt -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia <<SQL
INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES
 ('${T07_WORKSPACE}','T07','t07',now(),now()),
 ('00000000-0000-7000-8000-000000000072','Other','t07-other',now(),now());
SQL
for role in requester approver; do
  openssl rand -hex 32 >"${work}/private/${role}-password"
  sudo install -o 65534 -g 65532 -m 400 "${work}/private/${role}-password" "${OCSERV_SECRET_DIR}/${role}-password"
done
compose run --rm --no-deps -e OCSERV_LOCAL_BOOTSTRAP_USERNAME=t07-requester \
  -e "OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID=${T07_WORKSPACE}" \
  -e OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/secrets/requester-password \
  -e OCSERV_LOCAL_BOOTSTRAP_APPROVER_USERNAME=t07-approver \
  -e OCSERV_LOCAL_BOOTSTRAP_APPROVER_PASSWORD_FILE=/run/secrets/approver-password \
  -v "${OCSERV_SECRET_DIR}/requester-password:/run/secrets/requester-password:ro" \
  -v "${OCSERV_SECRET_DIR}/approver-password:/run/secrets/approver-password:ro" \
  control-plane --bootstrap-local-admin
stage=local_auth
python3 "${ROOT}/scripts/release-business-api.py" local
record real_local_auth
stage=native_node
# The candidate is not a published Release. Verify the real signed package
# locally, then exercise the official managed-node convergence/preparation.
deb="${OUTPUT_DIR}/ocservia-agent_${VERSION}-1_amd64.deb"
sha256sum "${deb}" >"${work}/package.sha256"
openssl pkeyutl -sign -rawin -inkey "${AGENT_SIGNING_KEY}" -in "${work}/package.sha256" -out "${work}/package.sig"
openssl pkeyutl -verify -rawin -pubin -inkey "${work}/trusted-release.pub.pem" \
  -in "${work}/package.sha256" -sigfile "${work}/package.sig"
cp "${work}/package.sha256" "${work}/tampered.sha256"
printf '\ntampered\n' >>"${work}/tampered.sha256"
if openssl pkeyutl -verify -rawin -pubin -inkey "${work}/trusted-release.pub.pem" \
  -in "${work}/tampered.sha256" -sigfile "${work}/package.sig"; then
  echo 'tampered package manifest unexpectedly verified' >&2
  exit 1
fi
sha256sum -c "${work}/package.sha256"
record real_signature_and_tamper_rejection
sudo install -d -m 755 /etc/ocservia
sudo install -m 644 "${work}/trusted-release.pub.pem" /etc/ocservia/release-signing.pub.pem
export EXPECTED_RELEASE_KEY_SHA256
EXPECTED_RELEASE_KEY_SHA256="$(openssl pkey -pubin -in "${work}/trusted-release.pub.pem" -outform DER | sha256sum | cut -d' ' -f1)"
printf '%s\n' "${EXPECTED_RELEASE_KEY_SHA256}" | sudo tee /etc/ocservia/trusted-release-key.sha256 >/dev/null
sudo chmod 644 /etc/ocservia/trusted-release-key.sha256
sudo touch /etc/ocservia/agent-install-production-relays
sudo dpkg -i "${deb}"
export CONTROLLER_ENDPOINT_ID="${OCSERV_CONTROLLER_ENDPOINT_ID}" RELAY_URL_A="${OCSERV_RELAY_URL_A}" RELAY_URL_B=
export RELAY_ACCESS_TOKEN_SOURCE="${OCSERV_SECRET_DIR}/relay-access-token"
export CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE="${work}/command.pub.pem"
export TRUSTED_RELEASE_KEY=/etc/ocservia/release-signing.pub.pem
export USER_PASSWORD_SEAL_KEY_ID=t07-user P12_PASSWORD_SEAL_KEY_ID=t07-p12 ENROLLMENT_ENVIRONMENT=production
bash "${ROOT}/deploy/managed-node/install.sh" --version "v${VERSION}" >"${ARTIFACT_DIR}/managed-prepare.log"
grep -q ENROLLMENT_READY "${ARTIFACT_DIR}/managed-prepare.log"
record signed_native_package_and_managed_prepare

mkdir -m 700 "${work}/relay"
cp "${OCSERV_SECRET_DIR}/tls.crt" "${work}/relay/relay.crt"
cp "${work}/private/tls.key" "${work}/relay/relay.key"
cp "${work}/private/relay-access-token" "${work}/relay/relay-token"
sudo chown -R 65532:65532 "${work}/relay"
docker run -d --name "${T07_RELAY_CONTAINER}" --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true -p "${gateway}:3443:3443" \
  -e IROH_RELAY_ACCESS_TOKEN_FILE=/run/relay-secrets/relay-token \
  -e RUST_LOG=info,iroh_relay::server::clients=debug \
  -v "${work}/relay:/run/relay-secrets:ro" \
  -v "${ROOT}/deploy/g6-readiness/relay.toml:/etc/iroh-relay/relay.toml:ro" \
  "${BUILDX_BUILDER}-relay" --config-path /etc/iroh-relay/relay.toml
# Keep the original single-relay private-CA limitation explicit. Recreate only
# transportd from the rendered production descriptor with its explicit CA flag;
# do not change the checkout, signed images, authorization, or TLS verification.
compose config --format json >"${work}/compose.json"
python3 - "${work}" <<'PY'
import json, pathlib, sys
work = pathlib.Path(sys.argv[1])
config = json.loads((work / 'compose.json').read_text())
service = config['services']['transportd']
service['command'] += ['--relay-ca-file', '/run/t07-ca.crt']
service['volumes'].append({'type': 'bind', 'source': str(work / 'ca.crt'), 'target': '/run/t07-ca.crt', 'read_only': True})
(work / 'relay-compose.json').write_text(json.dumps(config))
PY
docker compose -f "${work}/relay-compose.json" up -d --no-deps transportd
export T07_TRANSPORT_CONTAINER
T07_TRANSPORT_CONTAINER="$(compose ps -q transportd)"
sudo install -m 644 "${work}/ca.crt" /etc/ocservia-agent/t07-relay-ca.crt
sudo openssl pkey -in /etc/ocservia-agent/user-password-seal-private.pem -pubout >"${work}/user.pub.pem"
export T07_ENDPOINT T07_USER_HASH T07_P12_HASH
T07_ENDPOINT="$(sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent --controller "${CONTROLLER_ENDPOINT_ID}" --prepare-enrollment)"
T07_USER_HASH="$(openssl pkey -pubin -in "${work}/user.pub.pem" -outform DER | sha256sum | cut -d' ' -f1)"
T07_P12_HASH="$(sudo openssl pkey -in /etc/ocservia-agent/p12-password-seal-private.pem -pubout -outform DER | sha256sum | cut -d' ' -f1)"
python3 "${ROOT}/scripts/release-business-api.py" token
sudo install -o root -g ocserv-agent -m 640 "${work}/private/enrollment-token" /etc/ocservia-agent/enrollment-token
sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent --controller "${CONTROLLER_ENDPOINT_ID}" \
  --enrollment-token-file /etc/ocservia-agent/enrollment-token --enrollment-environment production \
  --user-password-seal-key-id t07-user --user-password-seal-public-key-sha256 "${T07_USER_HASH}" \
  --p12-password-seal-key-id t07-p12 --p12-password-seal-public-key-sha256 "${T07_P12_HASH}" \
  --relay-mode custom --relay-url "${RELAY_URL_A}" --relay-token-file /etc/ocservia-agent/relay-access-token \
  --relay-ca-file /etc/ocservia-agent/t07-relay-ca.crt >"${work}/enrollment.log"
export T07_NODE
T07_NODE="$(sed -nE '/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/p' "${work}/enrollment.log")"
[[ "${T07_NODE}" =~ ^[0-9a-f-]{36}$ ]]
sudo rm /etc/ocservia-agent/enrollment-token
cat >"${work}/agent.env" <<EOF
CONTROLLER_ENDPOINT_ID=${CONTROLLER_ENDPOINT_ID}
NODE_ID=${T07_NODE}
AGENT_ENDPOINT_ID=${T07_ENDPOINT}
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem
USER_PASSWORD_SEAL_KEY_ID=t07-user
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${T07_USER_HASH}
P12_PASSWORD_SEAL_KEY_ID=t07-p12
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${T07_P12_HASH}
EOF
sudo install -o root -g ocserv-agent -m 640 "${work}/agent.env" /etc/ocservia-agent/agent.env
sed 's|exec /usr/libexec/ocservia/ocservia-agent "\$@"|exec /usr/libexec/ocservia/ocservia-agent "$@" --relay-ca-file /etc/ocservia-agent/t07-relay-ca.crt|' \
  "${ROOT}/deploy/production/systemd/agent-relays.sh" >"${work}/agent-relays"
sudo install -m 755 "${work}/agent-relays" /usr/libexec/ocservia/t07-agent-relays
sudo install -d -m 755 /etc/systemd/system/ocservia-agent.service.d
printf '[Service]\nExecStart=\nExecStart=/usr/libexec/ocservia/t07-agent-relays\n' | \
  sudo tee /etc/systemd/system/ocservia-agent.service.d/99-t07-ca.conf >/dev/null
# Prevent direct UDP connectivity from masking the single-Relay outage.
sudo iptables -I OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
sudo install -m 600 "${work}/private/tls.key" /etc/ocserv/t07.key
sudo install -m 644 "${OCSERV_SECRET_DIR}/tls.crt" /etc/ocserv/t07.crt
sudo install -m 600 /dev/null /etc/ocserv/ocpasswd
cat >"${work}/ocserv.conf" <<'EOF'
auth = "plain[passwd=/etc/ocserv/ocpasswd]"
tcp-port = 44443
udp-port = 0
run-as-user = nobody
run-as-group = nogroup
socket-file = /run/ocserv.socket
server-cert = /etc/ocserv/t07.crt
server-key = /etc/ocserv/t07.key
isolate-workers = false
max-clients = 4
max-same-clients = 2
max-ban-score = 1000
device = vpns
ipv4-network = 10.208.0.0
ipv4-netmask = 255.255.255.0
use-occtl = true
EOF
sudo install -m 600 "${work}/ocserv.conf" /etc/ocserv/ocserv.conf
sudo ocserv --test-config -c /etc/ocserv/ocserv.conf
sudo systemctl daemon-reload
sudo systemctl start ocserv ocservia-privd
stage=node_approval
python3 "${ROOT}/scripts/release-business-api.py" approve
sudo systemctl enable --now ocservia-privd ocservia-agent
bash "${ROOT}/deploy/managed-node/install.sh" --version "v${VERSION}" >"${ARTIFACT_DIR}/managed-active.log"
grep -q SERVICES_ACTIVE "${ARTIFACT_DIR}/managed-active.log"
record native_node_services
sudo ip netns add t07-client
sudo ip link add t07-host type veth peer name t07-peer
sudo ip link set t07-peer netns t07-client
sudo ip addr add 10.207.0.1/30 dev t07-host
sudo ip link set t07-host up
sudo ip netns exec t07-client ip addr add 10.207.0.2/30 dev t07-peer
sudo ip netns exec t07-client ip link set t07-peer up
sudo ip netns exec t07-client ip link set lo up
cat >"${work}/vpn-script" <<'EOF'
#!/bin/sh
set -eu
if [ "$reason" = connect ]; then
  ip link set "$TUNDEV" up
  ip addr add "$INTERNAL_IP4_ADDRESS/24" dev "$TUNDEV"
fi
EOF
chmod 700 "${work}/vpn-script"
stage=business
python3 "${ROOT}/scripts/release-business-api.py" business
record real_vpn_business
stage=complete
