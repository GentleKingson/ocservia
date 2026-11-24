#!/usr/bin/env bash
# Disposable hosted runner only: Integrated Business Smoke with production Signer,
# real browser flow, OpenConnect VPN, single-instance recovery, and terminal revocation.
# shellcheck disable=SC2024 # sudo reads protected state; redirects are runner-owned.
set -Eeuo pipefail
# Imported acceptance helpers must not dirty the exact-source lifecycle checkout.
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${ARTIFACT_DIR:?}" "${VERSION:?}" "${PRODUCTS_DIR:?}" "${IMAGES_DIR:?}"
SOURCE_COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
export SOURCE_COMMIT
# shellcheck source=scripts/env.sh disable=SC1091
source "$ROOT/scripts/env.sh"
# shellcheck source=scripts/release-common.sh disable=SC1091
source "$ROOT/scripts/release-common.sh"

export PRODUCTION_SIGNER_ACCEPTANCE=true
resilience_result=FAIL
relay_ipv4_rule=false
relay_ipv6_rule=false

[[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" == github-hosted && "$EUID" == 0 ]]
[[ "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc[.][1-9][0-9]*)?$ && "${SOURCE_COMMIT}" =~ ^[0-9a-f]{40}$ ]]
[[ -z "$(git -C "${ROOT}" status --porcelain)" && "$(ps -p 1 -o comm=)" == systemd ]]
[[ ! -e /etc/ocservia-agent && ! -e /usr/libexec/ocservia && ! -e /etc/ocservia ]]
if getent passwd ocserv-agent >/dev/null; then
  echo 'refusing an existing Agent account' >&2
  exit 2
fi
[[ -z "$(docker ps -aq --filter label=com.docker.compose.project=ocservia-production)" ]]
[[ -z "$(docker volume ls -q --filter label=com.docker.compose.project=ocservia-production)" ]]
[[ -s "$PRODUCTS_DIR/product-identity.json" && -s "$IMAGES_DIR/product-identity.json" ]]

umask 077
mkdir -m 700 "${ARTIFACT_DIR}"
: >"${ARTIFACT_DIR}/scenario-timings.jsonl"
export OCSERVIA_BUSINESS_TEST_WORK_DIR
OCSERVIA_BUSINESS_TEST_WORK_DIR="$(mktemp -d "${HOME}/.ocservia-business-test-XXXXXX")"
work="${OCSERVIA_BUSINESS_TEST_WORK_DIR}"
prefetch_pids=()
stage=preflight
started="$(date -u +%FT%TZ)"
stage_started="$(date +%s)"
CONTROLLER_ARCH="${CONTROLLER_ARCH:-amd64}"
[[ "${CONTROLLER_ARCH}" == amd64 || "${CONTROLLER_ARCH}" == arm64 ]]
export PACKAGE_ARCH="${CONTROLLER_ARCH}" CONTROLLER_ARCH
export DEPLOYMENT_CONFIG_DIR="${work}/bundle"
export OCSERVIA_BUSINESS_TEST_WORKSPACE_ID=00000000-0000-7000-8000-000000000071
export OCSERV_SECRET_DIR="${work}/secrets" OCSERV_BACKUP_DIR="${work}/backup"
export OCSERV_CONTROLLER_STATE_ROOT="${work}/state"
export OCSERV_LOCAL_AUTH_ENABLED=true OCSERV_AUDIT_EVENT_KEY_ID=business-test OCSERV_BACKUP_INTERVAL_SECONDS=86400

gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
export OCSERV_DEPLOYMENT_MODE=integrated OCSERV_HTTPS_ADDRESS=0.0.0.0
export OCSERV_PUBLIC_HOST="controller.${gateway}.sslip.io" OCSERV_RELAY_PUBLIC_HOST="relay.${gateway}.sslip.io"
export OCSERV_CONTROLLER_PUBLIC_URL="https://${OCSERV_PUBLIC_HOST}" OCSERV_PUBLIC_ORIGIN="https://${OCSERV_PUBLIC_HOST}"
export OCSERV_RELAY_URL_A="https://${OCSERV_RELAY_PUBLIC_HOST}"
export OCSERV_CERTIFICATE_SIGNER_URL=https://signer:9443/sign
export OCSERV_RELAY_SECRET_DIR="${work}/relay-secrets"
export OCSERV_SIGNER_SECRET_DIR="${work}/production-signer/secrets" OCSERV_SIGNER_STATE_DIR="${work}/production-signer/data"
unset OCSERV_OTEL_BACKEND_ENDPOINT OCSERV_CONTROLLER_COMPOSE_SH OCSERV_CONTROLLER_SMOKE_SH

registry="business-${GITHUB_RUN_ID:?}-${GITHUB_RUN_ATTEMPT:?}-registry"
export OCSERVIA_BUSINESS_TEST_RELAY_CONTAINER="business-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}-relay"
export OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER="business-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}-signer"

compose() { "${ROOT}/deploy/production/compose.sh" "$@"; }
scenario() { python3 "${ROOT}/scripts/release_business_scenarios.py" "$@"; }
record() { printf '%s\n' "$1" >>"${ARTIFACT_DIR}/checkpoints.txt"; }
next_stage() {
  local now
  now="$(date +%s)"
  printf '%s\t%s\n' "${stage}" "$((now - stage_started))" >>"${ARTIFACT_DIR}/timings.tsv"
  stage="$1"
  stage_started="${now}"
}

cleanup() {
  local code=$?
  trap - EXIT ERR
  set +e
  local failed_stage="${stage}"
  next_stage cleanup
  jq -Rn '[inputs | split("\t") | {stage:.[0],seconds:(.[1]|tonumber)}]' \
    <"${ARTIFACT_DIR}/timings.tsv" >"${ARTIFACT_DIR}/timings.json"
  if [[ -f "${work}/private.log" ]]; then
    {
      compose logs --no-color
      if [[ -n "$(compose ps -q postgres 2>/dev/null)" ]]; then
        echo '=== transport_event_quarantine ==='
        compose exec -T postgres psql -XAt -U ocservia_owner -d ocservia -c \
          "SELECT observed_at, event_id, node_id, event_type, reason_code, reason_detail FROM transport_event_quarantine ORDER BY observed_at DESC LIMIT 50"
      fi
      for service in control-plane transportd; do
        container="$(compose ps -a -q "${service}")"
        if [[ -n "${container}" ]]; then
          docker inspect --format '{{json .State}}' "${container}"
        fi
      done
      sudo journalctl --no-pager -o short-iso-precise -u ocservia-agent -u ocservia-privd -u ocserv
      docker logs "$OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER"
    } >>"${work}/private.log" 2>&1
    # Never export environment, cookies, raw databases, passwords or raw logs.
    sudo "$(command -v node)" "${ROOT}/scripts/redact-release-upgrade-log.mjs" \
      "${work}/private.log" "${work}/private" "${work}/sanitized.log" &&
      sudo install -o "$(id -u)" -g "$(id -g)" -m 600 "${work}/sanitized.log" "${ARTIFACT_DIR}/probe.log"
  fi
  [[ -f "${ARTIFACT_DIR}/checkpoints.txt" ]] || : >"${ARTIFACT_DIR}/checkpoints.txt"
  jq -n --arg version "${VERSION}" --arg start "${started}" \
    --arg end "$(date -u +%FT%TZ)" --arg stage "${failed_stage}" --argjson code "${code}" \
    --arg source "$SOURCE_COMMIT" --arg resilience "$resilience_result" \
    --argjson resilience_scenarios "$(if [[ -f "${ARTIFACT_DIR}/api-checkpoints.jsonl" ]]; then jq -s '[.[] | select(.name | startswith("resilience_")) | {name,status,time}]' "${ARTIFACT_DIR}/api-checkpoints.jsonl"; else printf '[]'; fi)" \
    --arg arch "$CONTROLLER_ARCH" \
    --arg run "${GITHUB_RUN_ID}" --arg attempt "${GITHUB_RUN_ATTEMPT}" \
    --rawfile checkpoints "${ARTIFACT_DIR}/checkpoints.txt" \
    --slurpfile timings "${ARTIFACT_DIR}/timings.json" \
    --slurpfile scenario_timings "${ARTIFACT_DIR}/scenario-timings.jsonl" \
    '{source_commit:$source,version:$version,run_id:$run,run_attempt:$attempt,profile:"smoke",terminal:"revocation",
      ocserv_fixture:"distribution",execution_environment:"GitHub-hosted runner",
      started_at:$start,finished_at:$end,exit_code:$code,last_stage:$stage,
      timings:$timings[0],scenario_timings:$scenario_timings,
      passed_checkpoints:($checkpoints | split("\n") | map(select(length > 0))),
      probe_status:(if $code == 0 then "PASS" else "FAIL" end),
      scope:"business-smoke",resilience_requested:true,
      resilience_result:(if $code != 0 and $resilience == "PASS" then "FAIL" else $resilience end),
      resilience_scenarios:$resilience_scenarios,
      planned_topology:{hosts:1,architecture:$arch,native_systemd_node:true,relays:1,relay_redundancy:false},
      operator_mode:"single_admin",
      limitations:[],
      not_applicable:["cross-host resilience and performance assessment"]}' >"${ARTIFACT_DIR}/result.json"
  sudo systemctl stop ocservia-agent ocservia-privd ocserv >/dev/null 2>&1
  for pid in "${prefetch_pids[@]:-}"; do
    [[ -n "${pid}" ]] || continue
    kill "${pid}" >/dev/null 2>&1 || true
    wait "${pid}" >/dev/null 2>&1 || true
  done
  if [[ "$relay_ipv4_rule" == true ]]; then
    sudo iptables -D OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
  fi
  if [[ "$relay_ipv6_rule" == true ]]; then
    sudo ip6tables -D OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
  fi
  docker rm -f "$OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER" >/dev/null 2>&1
  sudo ip netns pids business-test-client 2>/dev/null | xargs -r sudo kill
  sudo ip netns del business-test-client >/dev/null 2>&1
  compose down --volumes --remove-orphans >/dev/null 2>&1
  docker rm -f "${registry}" "${OCSERVIA_BUSINESS_TEST_RELAY_CONTAINER}" >/dev/null 2>&1
  sudo rm -rf -- "${work}"
  exit "${code}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'printf "probe failed at line %s, stage %s\n" "${LINENO}" "${stage}" >&2' ERR

mkdir -m 700 "${work}/private" "${work}/secrets" "${work}/state" "${work}/backup" "${work}/bundle"
release_verify_native_runner "$CONTROLLER_ARCH" >"${ARTIFACT_DIR}/native.json"
{ uname -a; cat /etc/os-release; docker version; docker compose version; node --version; } >"${ARTIFACT_DIR}/environment.txt"

pw_version="$(jq -r '.devDependencies["@playwright/test"]' "${ROOT}/web/package.json")"
pw_version="${pw_version#^}"
[[ "${pw_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
pw_image="mcr.microsoft.com/playwright:v${pw_version}-noble"

# Overlap image pulls and web npm ci with product loading, apt-get, and Controller setup.
(
  docker pull -q registry:2 >/dev/null
  docker pull -q "${OCSERV_POSTGRES_IMAGE}" >/dev/null
) >"${work}/prefetch-infra.log" 2>&1 &
prefetch_infra_pid=$!
(
  pw_pulled=false
  for attempt in 1 2 3; do
    if docker pull -q "${pw_image}"; then
      pw_pulled=true
      break
    fi
    if ((attempt < 3)); then sleep $((attempt * 2)); fi
  done
  [[ "${pw_pulled}" == true ]]
) >"${work}/prefetch-playwright.log" 2>&1 &
prefetch_pw_pid=$!
npm --prefix "${ROOT}/web" ci --ignore-scripts >"${work}/prefetch-npm.log" 2>&1 &
prefetch_npm_pid=$!
prefetch_pids=("${prefetch_infra_pid}" "${prefetch_pw_pid}" "${prefetch_npm_pid}")

next_stage product_consumption
cp "$PRODUCTS_DIR/product-identity.json" "$ARTIFACT_DIR/agent-product-identity.json"
cp "$IMAGES_DIR/product-identity.json" "$ARTIFACT_DIR/controller-product-identity.json"
release_load_controller_images "$IMAGES_DIR" "$VERSION" "$CONTROLLER_ARCH"
export OCSERVIA_BUSINESS_TEST_SIGNER_IMAGE="ghcr.io/gentlekingson/ocservia/signer:$VERSION-linux-$CONTROLLER_ARCH"
docker run --rm --entrypoint /usr/local/bin/iroh-relay \
  "ghcr.io/gentlekingson/ocservia/relay:$VERSION-linux-$CONTROLLER_ARCH" --version >"${ARTIFACT_DIR}/relay-version.txt"
record products_ready

next_stage dependency_install
sudo apt-get -o Acquire::http::Timeout=15 -o Acquire::Retries=3 update -qq
sudo apt-get -o Acquire::http::Timeout=15 -o Acquire::Retries=3 install -y --no-install-recommends ocserv openconnect vpnc-scripts sqlite3 iputils-ping libnss3-tools
sudo systemctl stop ocserv
{ dpkg-query -W ocserv openconnect libgnutls30t64 openssl systemd; ocserv --version; } >>"${ARTIFACT_DIR}/environment.txt" 2>&1

# Subsequent output can contain task-only secret data. Keep it private until
# the exact-value redactor runs; the uploaded results never contain cookies.
exec >"${work}/private.log" 2>&1

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=business-test-ca \
  -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign \
  -keyout "${work}/private/ca.key" -out "${work}/ca.crt"
chmod 444 "${work}/ca.crt"
openssl req -new -newkey rsa:2048 -nodes -subj /CN=localhost \
  -keyout "${work}/private/tls.key" -out "${work}/tls.csr"
printf 'basicConstraints=critical,CA:FALSE\nsubjectAltName=DNS:localhost,DNS:%s,IP:127.0.0.1,IP:%s,IP:10.207.0.1,IP:172.30.240.1,IP:172.30.240.3\nextendedKeyUsage=serverAuth\n' \
  "$OCSERV_PUBLIC_HOST" "${gateway}" >"${work}/leaf.ext"
openssl x509 -req -days 1 -in "${work}/tls.csr" -CA "${work}/ca.crt" -CAkey "${work}/private/ca.key" \
  -CAcreateserial -extfile "${work}/leaf.ext" -out "${OCSERV_SECRET_DIR}/tls.crt"
cp "${work}/private/tls.key" "${OCSERV_SECRET_DIR}/tls.key"
export CURL_CA_BUNDLE="${work}/ca.crt"

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
  "${OCSERV_SECRET_DIR}/controller-iroh.key"
sudo chmod 440 "${OCSERV_SECRET_DIR}/relay-access-token"
sudo install -o root -g 65532 -m 440 "${work}/command.pub.pem" "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem"
sudo install -o root -g root -m 444 "${work}/ca.crt" "${OCSERV_SECRET_DIR}/relay-ca.pem"
sudo chown 999:999 "${work}/backup"

mkdir -m 700 "$OCSERV_RELAY_SECRET_DIR"
openssl req -new -newkey rsa:2048 -nodes -subj "/CN=$OCSERV_RELAY_PUBLIC_HOST" \
  -keyout "$work/private/relay-tls.key" -out "$work/relay.csr"
printf 'subjectAltName=DNS:%s\nextendedKeyUsage=serverAuth\n' "$OCSERV_RELAY_PUBLIC_HOST" >"$work/relay.ext"
openssl x509 -req -days 1 -in "$work/relay.csr" -CA "$work/ca.crt" -CAkey "$work/private/ca.key" \
  -CAcreateserial -extfile "$work/relay.ext" -out "$OCSERV_RELAY_SECRET_DIR/tls.crt"
install -m 444 "$work/private/relay-tls.key" "$OCSERV_RELAY_SECRET_DIR/tls.key"
chmod 444 "$OCSERV_RELAY_SECRET_DIR/tls.crt"
scenario signer_prepare

infra_status=0
wait "${prefetch_infra_pid}" || infra_status=$?
cat "${work}/prefetch-infra.log"
((infra_status == 0))

docker run -d --name "${registry}" -p 127.0.0.1:5000:5000 registry:2
manifest="${DEPLOYMENT_CONFIG_DIR}/controller-release-${CONTROLLER_ARCH}.json"
release_publish_local_manifest "${ROOT}" "localhost:5000" "" "${VERSION}" "${CONTROLLER_ARCH}" "${SOURCE_COMMIT}" "${manifest}"
export OCSERVIA_BUSINESS_TEST_SIGNER_IMAGE="${OCSERV_SIGNER_IMAGE:?}"

next_stage controller_install
"${ROOT}/deploy/production/controller.sh" install --release-file "$manifest"
OCSERVIA_BUSINESS_TEST_SIGNER_CONTAINER="$(compose ps -q signer)"
OCSERVIA_BUSINESS_TEST_RELAY_CONTAINER="$(compose ps -q relay)"
curl --fail --silent --show-error "$OCSERV_CONTROLLER_PUBLIC_URL/api/v1/version" | \
  jq -e --arg sha "${SOURCE_COMMIT}" --arg v "${VERSION}" '.commit == $sha and .version == $v' >/dev/null
record controller_lifecycle
scenario integrated_entry
record integrated_entry_and_internal_tls

compose exec -T postgres psql -XAt -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia <<SQL
INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES
 ('${OCSERVIA_BUSINESS_TEST_WORKSPACE_ID}','Business Test','business-test',now(),now()),
 ('00000000-0000-7000-8000-000000000072','Other','business-test-other',now(),now());
SQL
openssl rand -hex 32 >"${work}/private/requester-password"
sudo install -o 65534 -g 65532 -m 400 "${work}/private/requester-password" "${OCSERV_SECRET_DIR}/requester-password"
compose run --rm --no-deps -e OCSERV_LOCAL_BOOTSTRAP_USERNAME=business-test-requester \
  -e "OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID=${OCSERVIA_BUSINESS_TEST_WORKSPACE_ID}" \
  -e OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/secrets/requester-password \
  -v "${OCSERV_SECRET_DIR}/requester-password:/run/secrets/requester-password:ro" \
  control-plane --bootstrap-local-admin

next_stage local_auth
scenario local
record real_local_auth
scenario transport_ready

next_stage native_node
deb="${PRODUCTS_DIR}/ocservia-agent_${VERSION}-1_${CONTROLLER_ARCH}.deb"
sudo dpkg -i "${deb}"
sudo install -o root -g root -m 444 "${work}/ca.crt" /etc/ocservia-agent/relay-ca.pem
export CONTROLLER_ENDPOINT_ID="${OCSERV_CONTROLLER_ENDPOINT_ID}" RELAY_URL_A="${OCSERV_RELAY_URL_A}"
export RELAY_ACCESS_TOKEN_SOURCE="${OCSERV_SECRET_DIR}/relay-access-token"
export CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE="${work}/command.pub.pem"
export USER_PASSWORD_SEAL_KEY_ID=business-test-user-seal P12_PASSWORD_SEAL_KEY_ID=business-test-p12-seal ENROLLMENT_ENVIRONMENT=production
managed_options=(--version "v${VERSION}" --root-lifecycle)

sudo install -m 600 "${work}/private/tls.key" /etc/ocserv/business-test.key
sudo install -m 644 "${OCSERV_SECRET_DIR}/tls.crt" /etc/ocserv/business-test.crt
sudo install -m 600 /dev/null /etc/ocserv/ocpasswd
sudo groupadd --system ocservia-vpn
sudo useradd --system --no-create-home --gid ocservia-vpn --shell /usr/sbin/nologin ocservia-vpn
write_node_config() {
  cat >"${work}/ocserv.conf" <<EOF
auth = "plain[passwd=/etc/ocserv/ocpasswd]"
tcp-port = 44443
udp-port = 0
run-as-user = ocservia-vpn
run-as-group = ocservia-vpn
socket-file = /run/ocserv.socket
server-cert = $1
server-key = $2
max-clients = 4
max-same-clients = 2
cookie-timeout = 300
device = vpns
ipv4-network = 10.208.0.0/24
dns = 1.1.1.1
route = default
use-occtl = true
EOF
  sudo install -m 600 "${work}/ocserv.conf" /etc/ocserv/ocserv.conf
}
write_node_config /etc/ocserv/business-test.crt /etc/ocserv/business-test.key
sudo ocserv --test-config -c /etc/ocserv/ocserv.conf
sudo systemctl daemon-reload
sudo systemctl restart ocserv
sudo /usr/libexec/ocservia/ocservia-privd --host-preflight
sudo install -m 644 "${work}/ca.crt" /usr/local/share/ca-certificates/ocservia-business-test-ca.crt
sudo update-ca-certificates

scenario bootstrap_token
sudo env USER_PASSWORD_SEAL_KEY_ID="${USER_PASSWORD_SEAL_KEY_ID}" P12_PASSWORD_SEAL_KEY_ID="${P12_PASSWORD_SEAL_KEY_ID}" \
  bash "${ROOT}/deploy/managed-node/install.sh" --non-interactive \
  --join-token-file "${work}/bootstrap/join-token" "${managed_options[@]}" >"${ARTIFACT_DIR}/managed-enrollment.log"
rm -f -- "${work}/bootstrap/join-token"
grep -Fxq ONLINE "${ARTIFACT_DIR}/managed-enrollment.log"
export OCSERVIA_BUSINESS_TEST_NODE_ID
OCSERVIA_BUSINESS_TEST_NODE_ID="$(sed -nE 's/^NODE_ID: ([0-9a-f-]{36})$/\1/p' "${ARTIFACT_DIR}/managed-enrollment.log")"
[[ "${OCSERVIA_BUSINESS_TEST_NODE_ID}" =~ ^[0-9a-f-]{36}$ ]]
sudo test ! -e /etc/ocservia-agent/enrollment-token
export OCSERVIA_BUSINESS_TEST_AGENT_ENDPOINT_ID
OCSERVIA_BUSINESS_TEST_AGENT_ENDPOINT_ID="$(sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent --controller "${CONTROLLER_ENDPOINT_ID}" --prepare-enrollment)"
record signed_native_package_and_managed_prepare

export OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER
OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER="$(compose ps -q transportd)"
[[ -n "${OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER}" ]]
sudo openssl pkey -pubin -in "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem" -noout
[[ "$(sudo stat -c '%u:%g:%a:%h' "${OCSERV_SECRET_DIR}/controller-command-verification-key.pem")" == 0:65532:440:1 ]]
docker inspect "${OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER}" | jq -e '.[0].Mounts | all(.[]; .Destination != "/run/secrets/controller_command_signing_key")' >/dev/null
docker exec "${OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER}" test ! -e /run/secrets/controller_command_signing_key
printf 'SPKI public key, regular one-link root:65532 0440; no Controller signing-key mount\n' >"${ARTIFACT_DIR}/transport-key-boundary.txt"
[[ "$(sudo stat -c '%u:%g:%a:%h' "${OCSERV_SECRET_DIR}/relay-ca.pem")" == 0:0:444:1 ]]
[[ "$(sudo stat -c '%u:%g:%a:%h' /etc/ocservia-agent/relay-ca.pem)" == 0:0:444:1 ]]
docker exec "${OCSERVIA_BUSINESS_TEST_TRANSPORT_CONTAINER}" sh -c '
  found=false
  for executable in /proc/[0-9]*/exe; do
    if [ "$(readlink "$executable")" = /usr/local/bin/ocservia-transportd ]; then
      cat "${executable%exe}cmdline"
      found=true
    fi
  done
  [ "$found" = true ]
' | tr '\0' '\n' >"${ARTIFACT_DIR}/transport-argv.txt"
grep -Fx /run/secrets/relay_ca "${ARTIFACT_DIR}/transport-argv.txt"
printf 'Additional public Relay CA: one-link root:root 0444 on Controller and node; official transport launcher flag present\n' >"${ARTIFACT_DIR}/relay-ca-boundary.txt"
cmp "${ROOT}/deploy/production/systemd/agent-relays.sh" /usr/libexec/ocservia/ocservia-agent-relays
[[ ! -e /etc/systemd/system/ocservia-agent.service.d/99-t07-ca.conf ]]

identity_dir=/var/lib/ocservia-agent/identity
sudo install -d -m 0700 "${work}/unchanged"
sudo cp -- "${identity_dir}/endpoint.key" "${identity_dir}/controller.endpoint" "${work}/unchanged/"
scenario bootstrap_verify
bash "${ROOT}/deploy/managed-node/install.sh" --non-interactive --resume-install "${managed_options[@]}" >"${ARTIFACT_DIR}/managed-rerun.log"
grep -Fxq ONLINE "${ARTIFACT_DIR}/managed-rerun.log"
grep -Fxq "NODE_ID: ${OCSERVIA_BUSINESS_TEST_NODE_ID}" "${ARTIFACT_DIR}/managed-rerun.log"
scenario check_enrolled_identity
record bootstrap_token_enrollment_and_rerun_identity
record official_managed_enrollment_and_unchanged_launchers

# Prevent direct UDP connectivity from masking the single-Relay outage.
sudo iptables -I OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
relay_ipv4_rule=true
if [[ -n "$(cat /proc/net/if_inet6)" ]]; then
  sudo ip6tables -I OUTPUT -m owner --uid-owner "$(id -u ocserv-agent)" -p udp ! --dport 53 -j REJECT
  relay_ipv6_rule=true
fi

next_stage config_tls
scenario config_prepare
config_ref="$(jq -r .id "${work}/config-reference.json")"
write_node_config "/etc/ocservia-agent/config-tls/${config_ref}/v1/server-cert.pem" "/etc/ocservia-agent/config-tls/${config_ref}/v1/server-key.pem"
sudo ocserv --test-config -c /etc/ocserv/ocserv.conf
sudo systemctl restart ocserv
scenario signer_import
record production_signer_active_binding
bash "${ROOT}/deploy/managed-node/install.sh" --non-interactive --resume-install "${managed_options[@]}" >"${ARTIFACT_DIR}/managed-active.log"
grep -Fxq ONLINE "${ARTIFACT_DIR}/managed-active.log"
grep -Fxq "NODE_ID: ${OCSERVIA_BUSINESS_TEST_NODE_ID}" "${ARTIFACT_DIR}/managed-active.log"
scenario check_enrolled_identity
record native_node_services

next_stage browser
npm_status=0; pw_status=0
wait "${prefetch_npm_pid}" || npm_status=$?
wait "${prefetch_pw_pid}" || pw_status=$?
cat "${work}/prefetch-npm.log" "${work}/prefetch-playwright.log"
((npm_status == 0 && pw_status == 0))
mkdir -p "${HOME}/.pki/nssdb"
if [[ ! -f "${HOME}/.pki/nssdb/cert9.db" ]]; then certutil -N --empty-password -d "sql:${HOME}/.pki/nssdb"; fi
certutil -A -d "sql:${HOME}/.pki/nssdb" -n ocservia-business-test-ca -t C,, -i "${work}/ca.crt"
scenario browser_prepare
docker run --rm --init --ipc=host --network=host \
  --security-opt seccomp=unconfined \
  --security-opt apparmor=unconfined \
  -v "${ROOT}:${ROOT}:ro" \
  -v "${work}:${work}" \
  -v "${ARTIFACT_DIR}:${ARTIFACT_DIR}" \
  -v "${HOME}/.pki/nssdb:${HOME}/.pki/nssdb" \
  -w "${ROOT}" \
  -e "PATH=${ROOT}/.tools/node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  -e "HOME=${HOME}" \
  -e "NODE_EXTRA_CA_CERTS=${work}/ca.crt" \
  -e "OCSERVIA_BUSINESS_TEST_WORK_DIR=${work}" \
  -e "OCSERVIA_BUSINESS_TEST_NODE_ID=${OCSERVIA_BUSINESS_TEST_NODE_ID}" \
  -e "OCSERVIA_BUSINESS_TEST_WORKSPACE_ID=${OCSERVIA_BUSINESS_TEST_WORKSPACE_ID}" \
  -e "OCSERV_CONTROLLER_PUBLIC_URL=${OCSERV_CONTROLLER_PUBLIC_URL}" \
  -e "ARTIFACT_DIR=${ARTIFACT_DIR}" \
  "${pw_image}" \
  node "${ROOT}/scripts/release-business-browser.mjs"
scenario browser_verify
record real_browser_subset

next_stage signer_vpn_user
scenario smoke_user
record config_apply_and_vpn_user

next_stage vpn_client_setup
sudo ip netns add business-test-client
sudo ip link add biztest-host type veth peer name biztest-peer
sudo ip link set biztest-peer netns business-test-client
sudo ip addr add 10.207.0.1/30 dev biztest-host
sudo ip link set biztest-host up
sudo ip netns exec business-test-client ip addr add 10.207.0.2/30 dev biztest-peer
sudo ip netns exec business-test-client ip link set biztest-peer up
sudo ip netns exec business-test-client ip link set lo up
cat >"${work}/vpn-script" <<'EOF'
#!/bin/sh
set -eu
if [ "$reason" = connect ]; then
  ip link set dev "$TUNDEV" mtu "$INTERNAL_IP4_MTU" up
  ip addr add "$INTERNAL_IP4_ADDRESS/24" dev "$TUNDEV"
fi
EOF
chmod 700 "${work}/vpn-script"

next_stage business_vpn
scenario vpn_after_config_apply
record real_vpn_after_config_apply

next_stage single_instance_recovery
scenario resilience
resilience_result=PASS
record single_instance_recovery

next_stage node_revocation
scenario revocation
record real_node_revocation_and_disconnect
next_stage complete
