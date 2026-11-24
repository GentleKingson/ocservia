#!/usr/bin/env bash
# Disposable hosted runner only: Quick install end to end with shared
# release image artifacts and a Pebble ACME directory. Run as root through sudo.
#
# Checks the default shared hostname, lifecycle, native Agent execution
# through Relay, and QUIC after a real ACME renewal.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/release-common.sh disable=SC1091
source "${ROOT}/scripts/release-common.sh"

: "${VERSION:?}" "${GITHUB_RUN_ID:?}" "${GITHUB_RUN_ATTEMPT:?}" "${IMAGES_DIR:?}" "${PRODUCTS_DIR:?}" "${RELAY_NETWORK_PROBE:?}"
SOURCE_COMMIT="$(git -C "${ROOT}" rev-parse HEAD)"
CONTROLLER_ARCH="${CONTROLLER_ARCH:-amd64}"
export SOURCE_COMMIT CONTROLLER_ARCH
[[ "${EUID}" == 0 && -n "${SUDO_UID:-}" && "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc[.][1-9][0-9]*)?$ ]]
[[ -z "$(git -C "${ROOT}" status --porcelain)" ]]
[[ ! -e /etc/ocservia && ! -e /var/lib/ocservia-controller && ! -e /root/ocservia-initial-credentials ]]
[[ ! -e /etc/ocservia-agent && ! -e /usr/libexec/ocservia ]]
if getent passwd ocserv-agent >/dev/null; then
  echo 'Quick acceptance requires a runner without the ocserv-agent account' >&2
  exit 1
fi
# As in install.sh --quick: the lifecycle belongs to root; SUDO_UID keeps git's
# trust in the runner-owned checkout.
unset SUDO_USER
umask 077

PEBBLE_IMAGE=ghcr.io/letsencrypt/pebble@sha256:ddf230642b1a584f519f32e347de1b05a6e4c1f6c35c1863b33effeab5f78199
CREDENTIALS_DIR=/root/ocservia-initial-credentials
# controller.sh refuses a release file under a group/world-writable ancestor
# such as /tmp, so the work directory lives under root-only /root.
work="$(mktemp -d /root/quick-acceptance.XXXXXX)"
run_prefix="quick-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
registry="${run_prefix}-registry" pebble="${run_prefix}-pebble"
gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
controller="controller.${gateway}.sslip.io"
origin="https://${controller}"
host_trust=/usr/local/share/ca-certificates/ocservia-quick-acceptance-pebble.crt
stage=preflight
native_services=false
udp_filters=()

cleanup() {
  local code=$?
  trap - EXIT
  set +e
  if ((code)); then
    echo "Quick acceptance failed at stage ${stage}" >&2
    local container
    for container in $(docker ps -aq --filter label=com.docker.compose.project=ocservia-production); do
      docker inspect --format '== {{.Name}} {{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "${container}"
    done >&2
  fi
  if [[ "${native_services}" == true ]]; then
    systemctl stop ocservia-agent ocservia-privd ocserv
  fi
  for filter in "${udp_filters[@]}"; do
    "${filter}" -D OUTPUT -m owner --uid-owner ocserv-agent -p udp ! --dport 53 -j REJECT
  done
  docker rm -f "${registry}" "${pebble}" >/dev/null 2>&1
  rm -f -- "${host_trust}" && update-ca-certificates --fresh >/dev/null 2>&1
  rm -rf -- "${work}"
  exit "${code}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'echo "Quick acceptance failed at line ${LINENO}" >&2' ERR
pass() { echo "PASS: $1"; }
# errexit ignores negated commands, so absence is asserted explicitly.
absent() { if grep -qF -- "$@"; then echo "unexpected: $1" >&2; return 1; fi; }

release_verify_native_runner "${CONTROLLER_ARCH}" >/dev/null
test -s "${IMAGES_DIR}/product-identity.json"
test -s "${PRODUCTS_DIR}/product-identity.json"
test -x "${RELAY_NETWORK_PROBE}"
deb="${PRODUCTS_DIR}/ocservia-agent_${VERSION}-1_${CONTROLLER_ARCH}.deb"
test -s "${deb}"

stage=native_dependencies
apt-get -o Acquire::http::Timeout=15 -o Acquire::Retries=3 update -qq >"${work}/native-dependencies.log" 2>&1
apt-get -o Acquire::http::Timeout=15 -o Acquire::Retries=3 install -y --no-install-recommends \
  ocserv sqlite3 >>"${work}/native-dependencies.log" 2>&1
native_services=true
systemctl stop ocserv

stage=publish
docker pull -q "${PEBBLE_IMAGE}" >/dev/null &
pebble_pull_pid=$!
docker pull -q registry:2 >/dev/null &
registry_pull_pid=$!
docker pull -q "${OCSERV_POSTGRES_IMAGE}" >/dev/null &
postgres_pull_pid=$!
release_load_controller_images "${IMAGES_DIR}" "${VERSION}" "${CONTROLLER_ARCH}"
registry_pull_status=0
wait "${registry_pull_pid}" || registry_pull_status=$?
((registry_pull_status == 0))
docker run -d --restart unless-stopped --name "${registry}" -p 127.0.0.1:5000:5000 registry:2
manifest="${work}/controller-release-${CONTROLLER_ARCH}.json"
release_publish_local_manifest "${ROOT}" "localhost:5000" "" "${VERSION}" "${CONTROLLER_ARCH}" "${SOURCE_COMMIT}" "${manifest}"
# The upgrade target names the same images through a second digest-pinned
# repository, preserving a real configuration transition without mutable tags.
upgrade_manifest="${work}/controller-release-digests.json"
release_publish_local_manifest "${ROOT}" "localhost:5000" "upgrade-" "${VERSION}" "${CONTROLLER_ARCH}" "${SOURCE_COMMIT}" "${upgrade_manifest}"
pg_pull_status=0
wait "${postgres_pull_pid}" || pg_pull_status=$?
((pg_pull_status == 0))

stage=pebble
pebble_pull_status=0
wait "${pebble_pull_pid}" || pebble_pull_status=$?
((pebble_pull_status == 0))
# Pebble runs on the host network: Controller containers reach its directory
# on the bridge gateway address, and it validates TLS-ALPN-01 on public 443.
pebble_dir="${work}/pebble"
mkdir -m 0755 "${pebble_dir}"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=quick-pebble-ca \
  -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign \
  -keyout "${work}/pebble-ca.key" -out "${pebble_dir}/ca.pem" 2>/dev/null
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=pebble \
  -keyout "${pebble_dir}/key.pem" -out "${work}/pebble.csr" 2>/dev/null
printf 'subjectAltName=IP:%s,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' "${gateway}" >"${work}/pebble.ext"
openssl x509 -req -days 1 -in "${work}/pebble.csr" -CA "${pebble_dir}/ca.pem" -CAkey "${work}/pebble-ca.key" \
  -CAcreateserial -extfile "${work}/pebble.ext" -out "${pebble_dir}/cert.pem" 2>/dev/null
jq -n '{pebble: {listenAddress: "0.0.0.0:14000", managementListenAddress: "0.0.0.0:15000",
  certificate: "/pebble/cert.pem", privateKey: "/pebble/key.pem", httpPort: 5002, tlsPort: 443,
  ocspResponderURL: "", externalAccountBindingRequired: false,
  profiles: {default: {description: "Quick renewal fixture", validityPeriod: 900}}}}' >"${pebble_dir}/config.json"
chmod 0444 "${pebble_dir}"/*
docker run -d --restart unless-stopped --name "${pebble}" --network host \
  -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 -v "${pebble_dir}:/pebble:ro" \
  "${PEBBLE_IMAGE}" -config /pebble/config.json
for _ in $(seq 120); do
  curl -fsS --cacert "${pebble_dir}/ca.pem" "https://${gateway}:14000/dir" >/dev/null 2>&1 && break
  sleep 0.25
done
curl -fsS --cacert "${pebble_dir}/ca.pem" "https://${gateway}:14000/dir" >/dev/null
# Pebble's issuing root stands in for a public CA: the install's release smoke
# verifies the Controller against the host trust store.
curl -fsS --cacert "${pebble_dir}/ca.pem" "https://${gateway}:15000/roots/0" >"${work}/acme-root.pem"
install -m 0644 -- "${work}/acme-root.pem" "${host_trust}"
update-ca-certificates >/dev/null

stage=first_install
passphrase="${work}/root-ca-passphrase" export_dir="${work}/root-ca-export"
openssl rand -hex 24 >"${passphrase}"
quick=("${ROOT}/deploy/production/install.sh" --quick --controller-domain "${controller}"
  --acme-email quick-acceptance@ocservia.test --root-ca-passphrase-file "${passphrase}"
  --root-ca-export-dir "${export_dir}" --release-file "${manifest}")
OCSERV_ACME_DIRECTORY_URL="https://${gateway}:14000/dir" OCSERV_ACME_CA_FILE="${pebble_dir}/ca.pem" \
  "${quick[@]}" >"${work}/run-first.log" 2>&1
grep -qxF "Quick install complete: https://${controller}" "${work}/run-first.log"

stage=certificates
capture_leaf() {
  timeout 10 openssl s_client -connect "${gateway}:443" -servername "${controller}" \
    -CAfile "${work}/acme-root.pem" -verify_return_error -verify_hostname "${controller}" </dev/null 2>/dev/null |
    openssl x509 -out "$1" 2>/dev/null
}
serve_before=$((SECONDS + 180))
until capture_leaf "${work}/first-leaf.pem"; do
  ((SECONDS < serve_before))
  sleep 1
done
# Fail early if Pebble ignored the short certificate profile.
if openssl x509 -in "${work}/first-leaf.pem" -noout -checkend 1200 >/dev/null; then
  echo 'Pebble issued a certificate outside the renewal test window' >&2
  exit 1
fi
grep -qxF "OCSERV_RELAY_PUBLIC_HOST=${controller}" /etc/ocservia/install.env
relay_http_ready() {
  local status
  status="$(curl -fsS --max-time 10 --cacert "${work}/acme-root.pem" \
    --output /dev/null --write-out '%{http_code}' "${origin}/generate_204")" &&
    [[ "${status}" == 204 ]]
}
relay_http_ready
check_original_certificate() {
  capture_leaf "${work}/current-leaf.pem"
  cmp --silent "${work}/first-leaf.pem" "${work}/current-leaf.pem"
}
pass "Gateway and Relay respond on the shared HTTPS hostname"

stage=accounts
login() {
  local account="$1" status attempt retry
  # Local login admits 5 attempts per source per minute; the stages log in
  # more often than that, so wait out the window once.
  for attempt in 1 2; do
    rm -f -- "${work}/${account}.cookies"
    status="$(sed -n 's/^Password: //p' "${CREDENTIALS_DIR}/${account}.txt" |
      jq -Rc --arg username "${account}" '{username: $username, password: .}' |
      curl -sS -o /dev/null -w '%{http_code}' --cacert "${work}/acme-root.pem" -c "${work}/${account}.cookies" \
        -D "${work}/login.headers" -H "Origin: https://${controller}" -H 'Content-Type: application/json' \
        --data-binary @- "https://${controller}/api/v1/auth/login")"
    [[ "${status}" == 429 && "${attempt}" == 1 ]] || break
    retry="$(sed -n 's/^retry-after: *\([0-9]*\).*/\1/Ip' "${work}/login.headers")"
    sleep "${retry:-60}"
  done
  [[ "${status}" == 204 ]] && grep -qF __Host-ocservia_session "${work}/${account}.cookies"
}
accounts() {
  grep -qxF 'Username: admin' "${CREDENTIALS_DIR}/admin.txt"
  grep -qxE 'Password: [A-Za-z0-9+/]{8}' "${CREDENTIALS_DIR}/admin.txt"
  login admin
  [[ "$(stat -c '%u:%a' "${CREDENTIALS_DIR}" "${CREDENTIALS_DIR}/admin.txt" | sort -u | paste -sd' ')" == "0:400 0:700" ]]
  [[ -e "${CREDENTIALS_DIR}/complete" && ! -e /etc/ocservia/secrets/local-bootstrap-admin-password ]]
}
accounts
pass "admin logs in with the generated eight-character password"

stage=custody
# The root CA private key exists only, encrypted, in the export directory.
[[ -s "${export_dir}/root-ca.key.enc" ]]
root_public="$(openssl x509 -in "${export_dir}/root-ca.crt" -noout -pubkey)"
while IFS= read -r -d '' file; do
  grep -qF 'PRIVATE KEY-----' "${file}" || continue
  absent 'ENCRYPTED PRIVATE KEY' "${file}"
  [[ "$(openssl pkey -in "${file}" -pubout 2>/dev/null)" != "${root_public}" ]]
done < <(find /etc/ocservia /var/lib/ocservia-signer /var/lib/ocservia-controller /var/backups/ocservia -type f -print0)
pass "the root CA key is only in the export directory"

stage=ports
published="$(docker ps --filter label=com.docker.compose.project=ocservia-production --format '{{.Ports}}' |
  grep -oE ':[0-9]+->[0-9]+/(tcp|udp)' | sed -E 's/^:([0-9]+)->[0-9]+/\1/' | sort -u | paste -sd' ')"
[[ "${published}" == "443/tcp 7842/udp" ]]
pass "only 443/tcp and 7842/udp are published"

# The candidate stack uses the usual private Relay CA override. The next
# lifecycle activation mounts the test root for transportd.
install -o root -g root -m 0444 "${work}/acme-root.pem" /etc/ocservia/secrets/relay-ca.pem
stage=rerun
cp -a -- /etc/ocservia "${work}/config.before"
cp -a -- "${CREDENTIALS_DIR}" "${work}/credentials.before"
check_materials() {
  diff -qr -- "${work}/config.before" /etc/ocservia
  diff -qr --exclude=complete -- "${work}/credentials.before" "${CREDENTIALS_DIR}"
}
"${quick[@]}" >"${work}/run-rerun.log" 2>&1
grep -qF "initial Local administrator already exists" "${work}/run-rerun.log"
check_materials
check_original_certificate
accounts
pass "a rerun keeps material and certificates"

stage=bootstrap_resume
# An interruption after Local bootstrap committed but before completion was
# recorded: the rerun reuses the kept passwords and records completion.
rm -- "${CREDENTIALS_DIR}/complete"
"${quick[@]}" >"${work}/run-resume.log" 2>&1
grep -qF "Local authentication was already initialized by an earlier run" "${work}/run-resume.log"
check_materials
accounts
pass "a rerun resumes an unrecorded Local bootstrap"

stage=lifecycle
config_keys=(OCSERV_DEPLOYMENT_MODE OCSERV_DATABASE_BACKEND OCSERV_DATABASE_DEPLOYMENT OCSERV_TLS_MODE
  OCSERV_ACME_EMAIL OCSERV_ACME_DIRECTORY_URL OCSERV_ACME_CA_FILE OCSERV_PUBLIC_HOST OCSERV_RELAY_PUBLIC_HOST
  OCSERV_LOCAL_AUTH_ENABLED OCSERV_SECRET_DIR OCSERV_RELAY_SECRET_DIR OCSERV_SIGNER_SECRET_DIR
  OCSERV_SIGNER_STATE_DIR OCSERV_BACKUP_DIR OCSERV_AUDIT_EVENT_KEY_ID OCSERV_CONTROLLER_ENDPOINT_ID)
lifecycle() {
  (
    # shellcheck source=deploy/lib/install-env.sh disable=SC1091
    source "${ROOT}/deploy/lib/install-env.sh"
    install_env_load /etc/ocservia/install.env "${config_keys[@]}"
    "${ROOT}/deploy/production/controller.sh" "$@"
  ) >"${work}/run-lifecycle.log" 2>&1
  curl -fsS --cacert "${work}/acme-root.pem" "https://${controller}/api/v1/version" |
    jq -e --arg commit "${SOURCE_COMMIT}" --arg version "${VERSION}" '.commit == $commit and .version == $version' >/dev/null
  check_materials
  check_original_certificate
  accounts
}
lifecycle upgrade --release-file "${upgrade_manifest}"
pass "upgrade keeps material, certificates and accounts"
lifecycle rollback
pass "rollback keeps material, certificates and accounts"
lifecycle upgrade --release-file "${upgrade_manifest}"
pass "upgrade after rollback keeps material, certificates and accounts"

stage=native_agent
dpkg -i "${deb}" >"${work}/run-native-package.log" 2>&1
install -o root -g root -m 0444 "${work}/acme-root.pem" /etc/ocservia-agent/relay-ca.pem
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=quick-test-node \
  -keyout /etc/ocserv/quick-test.key -out /etc/ocserv/quick-test.crt 2>/dev/null
install -m 0600 /dev/null /etc/ocserv/ocpasswd
groupadd --system ocservia-vpn
useradd --system --no-create-home --gid ocservia-vpn --shell /usr/sbin/nologin ocservia-vpn
cat >"${work}/ocserv.conf" <<'NODE_CONFIG'
auth = "plain[passwd=/etc/ocserv/ocpasswd]"
tcp-port = 44443
udp-port = 0
run-as-user = ocservia-vpn
run-as-group = ocservia-vpn
socket-file = /run/ocserv.socket
server-cert = /etc/ocserv/quick-test.crt
server-key = /etc/ocserv/quick-test.key
max-clients = 4
max-same-clients = 2
cookie-timeout = 300
device = vpns
ipv4-network = 10.208.0.0/24
dns = 1.1.1.1
route = default
use-occtl = true
NODE_CONFIG
install -o root -g root -m 0600 "${work}/ocserv.conf" /etc/ocserv/ocserv.conf
ocserv --test-config -c /etc/ocserv/ocserv.conf
systemctl restart ocserv
# Apply the same transport isolation as Business Smoke. The separate QUIC
# probe runs as the test runner, so the Agent's UDP restriction stays in force.
iptables -I OUTPUT -m owner --uid-owner ocserv-agent -p udp ! --dport 53 -j REJECT
udp_filters+=(iptables)
if grep -q . /proc/net/if_inet6 2>/dev/null; then
  ip6tables -I OUTPUT -m owner --uid-owner ocserv-agent -p udp ! --dport 53 -j REJECT
  udp_filters+=(ip6tables)
fi

quick_api() {
  local path="$1"
  shift
  curl -fsS --max-time 10 --cacert "${work}/acme-root.pem" -b "${work}/admin.cookies" \
    -H "Origin: ${origin}" -H 'Content-Type: application/json' "$@" "${origin}/api/v1/${path}"
}
workspace="$(quick_api workspaces | jq -er '.items[0].id')"
quick_api node-bootstrap-tokens -H "X-Workspace-ID: ${workspace}" \
  --data-binary "$(jq -nc --arg workspace "${workspace}" '{installation:true,workspace_id:$workspace,
    environment:"production",expected_node_name:"quick-test-agent",reason:"Quick shared Relay acceptance"}')" |
  jq -er '.token | select(startswith("oit1_"))' >"${work}/join-token"
bash "${ROOT}/deploy/managed-node/install.sh" --non-interactive --root-lifecycle \
  --version "v${VERSION}" --join-token-file "${work}/join-token" >"${work}/run-native-enrollment.log" 2>&1
[[ ! -e "${work}/join-token" && ! -e /etc/ocservia-agent/enrollment-token ]]
grep -Fxq ONLINE "${work}/run-native-enrollment.log"
node="$(sed -nE 's/^NODE_ID: ([0-9a-f-]{36})$/\1/p' "${work}/run-native-enrollment.log")"
[[ "${node}" =~ ^[0-9a-f-]{36}$ ]]
relay_node() {
  quick_api "nodes/${node}" | jq -e 'select(.connection_state == "online" and .path.mode == "relay")'
}
connected_by=$((SECONDS + 120))
until observed="$(relay_node)"; do
  ((SECONDS < connected_by))
  sleep 2
done
revision="$(jq -er '.version' <<<"${observed}")"
operation="$(quick_api "nodes/${node}/service:reload" -H "If-Match: \"revision-${revision}\"" \
  -H "Idempotency-Key: $(openssl rand -hex 16)" --data-binary '{"reason":"Quick shared Relay acceptance","ttl_seconds":300}')"
operation_id="$(jq -er '.id' <<<"${operation}")"
command="$(jq -er '.command_id | gsub("-";"")' <<<"${operation}")"
[[ "${command}" =~ ^[0-9a-f]{32}$ ]]
settled_by=$((SECONDS + 120))
while :; do
  state="$(quick_api "operations/${operation_id}" | jq -er '.state')"
  case "${state}" in
    succeeded) break ;;
    failed|rejected|expired) echo "Reload settled as ${state}" >&2; exit 1 ;;
  esac
  ((SECONDS < settled_by))
  sleep 2
done
[[ "$(sqlite3 -readonly /var/lib/ocservia-agent/agent.db \
  "SELECT state,length(privileged_result_proof)>0 FROM command_journal WHERE hex(command_id)=upper('${command}');")" == 'succeeded|1' ]]
[[ "$(sqlite3 -readonly /var/lib/ocservia-privd/desired-effects.sqlite3 \
  "SELECT state,length(response)>0 FROM authorized_effects WHERE hex(command_id)=upper('${command}');")" == 'applied|1' ]]
pass "native Agent executed reload through the shared Relay"

stage=renewal
renewed_by=$((SECONDS + 22 * 60))
while ! capture_leaf "${work}/renewed-leaf.pem" ||
      cmp --silent "${work}/first-leaf.pem" "${work}/renewed-leaf.pem"; do
  ((SECONDS < renewed_by))
  sleep 5
done
openssl x509 -in "${work}/renewed-leaf.pem" -noout -serial
relay_container="$(docker ps -q --filter label=com.docker.compose.project=ocservia-production \
  --filter label=com.docker.compose.service=relay-acme)"
[[ "${relay_container}" =~ ^[0-9a-f]+$ ]]
heartbeat="$(relay_node | jq -er '.last_heartbeat_at')"
docker stop "${relay_container}" >/dev/null
offline_by=$((SECONDS + 120))
until quick_api "nodes/${node}" | jq -e '.connection_state == "offline"' >/dev/null; do
  ((SECONDS < offline_by))
  sleep 2
done
docker start "${relay_container}" >/dev/null
relay_ready_by=$((SECONDS + 120))
until relay_http_ready; do
  ((SECONDS < relay_ready_by))
  sleep 2
done
timeout 100 "${RELAY_NETWORK_PROBE}" "${origin}" "${work}/acme-root.pem" \
  /etc/ocservia/secrets/relay-access-token --expect-quic-leaf "${work}/renewed-leaf.pem"
connected_by=$((SECONDS + 120))
while ! relay_node | jq -e --arg before "${heartbeat}" \
  '.last_heartbeat_at | type == "string" and . != $before' >/dev/null; do
  ((SECONDS < connected_by))
  sleep 2
done
pass "Gateway renewed its certificate, QUIC serves the same leaf, and the Agent reconnected"

stage=output
password="$(sed -n 's/^Password: //p' "${CREDENTIALS_DIR}/admin.txt")"
[[ -n "${password}" ]]
absent "${password}" "${work}"/run-*.log
for container in $(docker ps -aq --filter label=com.docker.compose.project=ocservia-production); do
  docker logs "${container}" >"${work}/container.log" 2>&1
  absent "${password}" "${work}/container.log"
done

pass "passwords appear in no installer output or container log"
echo "Quick acceptance passed"
