#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:-${RUNNER_TEMP:?RUNNER_TEMP is required}/artifacts/real-e2e-node}"
WORK="${RUNNER_TEMP:-/tmp}/ocservia-real-e2e-node-${RUN_ID}"
TARGET="${RUNNER_TEMP:-/tmp}/ocservia-real-e2e-target-${RUN_ID}"
PACKAGE_DIR="${WORK}/packages"
SECRETS="${WORK}/secrets"
OUTBOX="${WORK}/outbox"
VERSION=0.0.0

case "${RUN_ID}" in
  *[!a-zA-Z0-9._-]*) echo "RUN_ID contains unsafe characters" >&2; exit 2 ;;
esac

require_endpoint() {
  [[ "${1:-}" =~ ^[0-9a-f]{64}$ ]] || { echo "invalid Controller EndpointID" >&2; return 1; }
}

runner_instance_id() {
  local boot_id
  boot_id="$(< /proc/sys/kernel/random/boot_id)"
  [[ "${boot_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
    echo "runner boot ID is unavailable or invalid" >&2
    return 1
  }
  printf '%s\n' "${boot_id}"
}

prepare_endpoint() {
  local ready_dir="${1:?controller-ready directory is required}"
  local controller endpoint
  controller="$(<"${ready_dir}/controller-endpoint-id")"
  require_endpoint "${controller}"
  [[ "$(<"${ready_dir}/runner-instance")" != "$(runner_instance_id)" ]] || {
    echo "Controller and Agent resolved to the same runner instance" >&2
    return 1
  }
  mkdir -p "${SECRETS}" "${OUTBOX}/agent-endpoint" "${ARTIFACT_DIR}"
  chmod 0700 "${WORK}" "${SECRETS}"
  mkdir -p "${WORK}/controller-ready"
  cp "${ready_dir}/command.pub.pem" "${WORK}/controller-ready/command.pub.pem"
  [[ -s "${PACKAGE_DIR}/ocservia-agent_${VERSION}-1_amd64.deb" ]]
  [[ ! -e /etc/ocservia-agent && ! -e /usr/libexec/ocservia && ! -e /etc/ocservia \
    && ! -e /var/lib/ocservia-agent && ! -e /var/lib/ocservia-privd ]]
  ! getent passwd ocserv-agent >/dev/null
  ! getent passwd ocservia-vpn >/dev/null
  ! dpkg-query -W -f='${Status}' ocservia-agent 2>/dev/null | grep -Fxq 'install ok installed'
  ! dpkg-query -W -f='${Status}' ocserv 2>/dev/null | grep -Fxq 'install ok installed'
  touch "${WORK}/native-owned"
  sudo apt-get update -qq
  sudo apt-get install -y --no-install-recommends ocserv openconnect vpnc-scripts sqlite3 iputils-ping
  sudo systemctl stop ocserv || true
  sudo install -d -m 0755 /etc/ocservia
  sudo touch /etc/ocservia/agent-install-production-relays
  sudo dpkg -i "${PACKAGE_DIR}/ocservia-agent_${VERSION}-1_amd64.deb"
  sudo install -m 0444 "${WORK}/relay/ca.pem" /etc/ocservia-agent/relay-ca.pem
  sudo install -m 0600 /dev/null /etc/ocserv/ocpasswd
  sudo groupadd --system ocservia-vpn
  sudo useradd --system --no-create-home --gid ocservia-vpn --shell /usr/sbin/nologin ocservia-vpn
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=real-e2e-vpn-ca \
    -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -keyout "${SECRETS}/vpn-ca.key" -out "${WORK}/vpn-ca.crt" >/dev/null 2>&1
  openssl req -new -newkey rsa:2048 -nodes -subj /CN=10.207.0.1 \
    -keyout "${SECRETS}/vpn.key" -out "${SECRETS}/vpn.csr" >/dev/null 2>&1
  printf 'basicConstraints=critical,CA:FALSE\nsubjectAltName=IP:10.207.0.1\nextendedKeyUsage=serverAuth\n' \
    >"${SECRETS}/vpn.ext"
  openssl x509 -req -days 1 -in "${SECRETS}/vpn.csr" -CA "${WORK}/vpn-ca.crt" \
    -CAkey "${SECRETS}/vpn-ca.key" -CAcreateserial -extfile "${SECRETS}/vpn.ext" \
    -out "${WORK}/vpn.crt" >/dev/null 2>&1
  sudo install -m 0600 "${SECRETS}/vpn.key" /etc/ocserv/t07.key
  sudo install -m 0644 "${WORK}/vpn.crt" /etc/ocserv/t07.crt
  cat >"${WORK}/ocserv.conf" <<'EOF'
auth = "plain[passwd=/etc/ocserv/ocpasswd]"
tcp-port = 44443
udp-port = 0
run-as-user = ocservia-vpn
run-as-group = ocservia-vpn
socket-file = /run/ocserv.socket
server-cert = /etc/ocserv/t07.crt
server-key = /etc/ocserv/t07.key
max-clients = 4
max-same-clients = 2
cookie-timeout = 300
device = vpns
ipv4-network = 10.208.0.0/24
dns = 1.1.1.1
route = default
use-occtl = true
EOF
  sudo install -m 0600 "${WORK}/ocserv.conf" /etc/ocserv/ocserv.conf
  sudo ocserv --test-config -c /etc/ocserv/ocserv.conf
  sudo systemctl restart ocserv
  export CONTROLLER_ENDPOINT_ID="${controller}" RELAY_URL_A="$(<"${WORK}/relay/url")" RELAY_URL_B=
  export RELAY_ACCESS_TOKEN_SOURCE="${WORK}/relay/token"
  export CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE="${WORK}/controller-ready/command.pub.pem"
  export USER_PASSWORD_SEAL_KEY_ID=t07-user P12_PASSWORD_SEAL_KEY_ID=t07-p12 ENROLLMENT_ENVIRONMENT=development
  if [[ "${NODE_INSTALL_E2E:-false}" == true ]]; then
    install -m 0600 "${ready_dir}/install.env" "${WORK}/install.env"
    # Validate literals before the native loader reads them; never source the file.
    python3 - "${WORK}" "${controller}" <<'CONFIG'
import pathlib, sys
work = pathlib.Path(sys.argv[1])
expected = (f"# Node: github-hosted-node\n# Workspace: 019d0000-0000-7000-8000-000000000001\n"
            f"# Release: v0.0.0\nCONTROLLER_ENDPOINT_ID={sys.argv[2]}\n"
            f"ENROLLMENT_ENVIRONMENT=development\nRELAY_URL_A={(work/'relay/url').read_text().strip()}\n"
            f"RELAY_URL_B=\nBOOTSTRAP_TOKEN_SOURCE={work}/secrets/bootstrap-token\n"
            f"RELAY_ACCESS_TOKEN_SOURCE={work}/relay/token\n"
            f"CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE={work}/controller-ready/command.pub.pem\n")
assert (work/'install.env').read_text() == expected
CONFIG
    # Explicit empty source delays issuance until host preparation is confirmed.
    (unset CONTROLLER_ENDPOINT_ID RELAY_URL_A RELAY_URL_B RELAY_ACCESS_TOKEN_SOURCE \
      CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE ENROLLMENT_ENVIRONMENT
     cd "${WORK}"
     BOOTSTRAP_TOKEN_SOURCE= bash "${ROOT}/deploy/managed-node/install.sh" --version "v${VERSION}") \
      >"${SECRETS}/managed-prepare.log"
    assert_not_started
    sudo sha256sum /var/lib/ocservia-agent/identity/endpoint.key \
      /var/lib/ocservia-agent/identity/controller.endpoint >"${SECRETS}/identity.sha256"
  else
    bash "${ROOT}/deploy/managed-node/install.sh" --version "v${VERSION}" >"${SECRETS}/managed-prepare.log"
  fi
  grep -Fxq ENROLLMENT_READY "${SECRETS}/managed-prepare.log"
  endpoint="$(sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent \
    --controller "${controller}" --prepare-enrollment)"
  require_endpoint "${endpoint}"
  printf '%s\n' "${endpoint}" >"${OUTBOX}/agent-endpoint/agent-endpoint-id"
  runner_instance_id >"${OUTBOX}/agent-endpoint/runner-instance"
  printf '%s\n' "${controller}" >"${WORK}/controller-endpoint-id"
  sudo openssl pkey -in /etc/ocservia-agent/user-password-seal-private.pem -pubout >"${OUTBOX}/agent-endpoint/user.pub.pem"
  openssl x509 -in "${WORK}/vpn.crt" -outform DER | sha256sum | cut -d' ' -f1 >"${OUTBOX}/agent-endpoint/vpn-cert-sha256"
  openssl x509 -in "${WORK}/vpn.crt" -pubkey -noout | openssl pkey -pubin -outform DER \
    | sha256sum | cut -d' ' -f1 >"${OUTBOX}/agent-endpoint/vpn-spki-sha256"
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "${WORK}/exchange.key" \
    2>"${SECRETS}/exchange-keygen.log"
  openssl pkey -in "${WORK}/exchange.key" -pubout \
    -out "${OUTBOX}/agent-endpoint/exchange.pub" 2>/dev/null
}

assert_not_started() {
  local unit
  for unit in ocservia-agent ocservia-privd; do
    [[ "$(systemctl show "$unit" -p ActiveState --value)" == inactive ]]
    [[ "$(systemctl show "$unit" -p MainPID --value)" == 0 ]]
  done
}

build_agent() {
  mkdir -p "${PACKAGE_DIR}"
  VERSION="${VERSION}" PACKAGE_ARCH=amd64 OUTPUT_DIR="${PACKAGE_DIR}" \
    SOURCE_DATE_EPOCH="$(git -C "${ROOT}" log -1 --format=%ct)" \
    bash "${ROOT}/scripts/build-release-agent.sh"
  (cd "${ROOT}/rust" && CARGO_TARGET_DIR="${TARGET}" \
    cargo build --locked --release --package ocservia-relay-network-probe)
}

enroll() {
  local token_dir="${1:?enrollment token directory is required}"
  local controller enrollment_output node_id
  local -a node_ids
  controller="$(<"${WORK}/controller-endpoint-id")"
  require_endpoint "${controller}"
  if [[ "${NODE_INSTALL_E2E:-false}" == true ]]; then
    install -m 0600 "${token_dir}/enrollment-token" "${SECRETS}/bootstrap-token"
  else
    sudo install -o root -g ocserv-agent -m 0640 "${token_dir}/enrollment-token" /etc/ocservia-agent/enrollment-token
  fi
  export CONTROLLER_ENDPOINT_ID="${controller}" RELAY_URL_A="$(<"${WORK}/relay/url")" RELAY_URL_B=
  export RELAY_ACCESS_TOKEN_SOURCE="${WORK}/relay/token"
  export CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE="${WORK}/controller-ready/command.pub.pem"
  export USER_PASSWORD_SEAL_KEY_ID=t07-user P12_PASSWORD_SEAL_KEY_ID=t07-p12 ENROLLMENT_ENVIRONMENT=development
  if [[ "${NODE_INSTALL_E2E:-false}" == true ]]; then
    (unset CONTROLLER_ENDPOINT_ID RELAY_URL_A RELAY_URL_B RELAY_ACCESS_TOKEN_SOURCE \
      CONTROLLER_COMMAND_VERIFICATION_KEY_SOURCE ENROLLMENT_ENVIRONMENT BOOTSTRAP_TOKEN_SOURCE
     cd "${WORK}"
     bash "${ROOT}/deploy/managed-node/install.sh" --version "v${VERSION}") >"${SECRETS}/enrollment-output"
    sudo sha256sum --check --status "${SECRETS}/identity.sha256"
    [[ "$(sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent \
      --controller "${controller}" --prepare-enrollment)" == "$(<"${OUTBOX}/agent-endpoint/agent-endpoint-id")" ]]
    assert_not_started
    [[ ! -e "${SECRETS}/bootstrap-token" ]]
  else
    bash "${ROOT}/deploy/managed-node/install.sh" --version "v${VERSION}" >"${SECRETS}/enrollment-output"
  fi
  enrollment_output="$(<"${SECRETS}/enrollment-output")"
  mapfile -t node_ids < <(sed -nE 's/^NODE_ID: ([0-9a-f-]{36})$/\1/p' <<<"${enrollment_output}")
  [[ ${#node_ids[@]} -eq 1 ]] || {
    echo "enrollment did not return a UUIDv7 node ID" >&2
    return 1
  }
  node_id="${node_ids[0]}"
  grep -Fxq ENROLLED_LOCAL "${SECRETS}/enrollment-output"
  sudo test ! -e /etc/ocservia-agent/enrollment-token
  mkdir -p "${OUTBOX}/enrollment-result"
  printf '%s\n' "${node_id}" >"${OUTBOX}/enrollment-result/node-id"
  runner_instance_id >"${OUTBOX}/enrollment-result/runner-instance"
  if [[ "${NODE_INSTALL_E2E:-false}" == true ]]; then
    printf 'ENROLLMENT_READY=PASS\nENROLLED_LOCAL=PASS\nIDENTITY_UNCHANGED=PASS\nSERVICES_NOT_STARTED=PASS\nRELEASE_ASSET_DOWNLOAD=NOT_TESTED\n' \
      >"${RUNNER_TEMP}/real-e2e-evidence-node/node-install.txt"
  fi
}

activate_native() {
  local authority_dir="${1:?business authority directory is required}"
  local node endpoint ref expected_key observed_key
  node="$(<"${OUTBOX}/enrollment-result/node-id")"
  endpoint="$(<"${OUTBOX}/agent-endpoint/agent-endpoint-id")"
  [[ "$(jq -r .node_id "${authority_dir}/authority.json")" == "${node}" ]]
  [[ "$(jq -r .agent_endpoint_id "${authority_dir}/authority.json")" == "${endpoint}" ]]
  openssl pkeyutl -decrypt -inkey "${WORK}/exchange.key" \
    -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 \
    -in "${authority_dir}/credential.oaep" -out "${SECRETS}/privd-credential"
  sudo /usr/libexec/ocservia/ocservia-privd attestation-registration \
    /var/lib/ocservia-privd/attestation.key "${node}" \
    "$(jq -r .controller_nonce_hex "${authority_dir}/authority.json")" \
    "$(jq -r .credential_context_sha256_hex "${authority_dir}/authority.json")" \
    >"${WORK}/proof.json"
  jq -e 'has("credential") | not' "${WORK}/proof.json" >/dev/null
  ref="$(jq -r .secret_ref_id "${authority_dir}/authority.json")"
  expected_key="$(jq -r .secret_ref_key "${authority_dir}/authority.json")"
  observed_key="$(sudo bash "${ROOT}/deploy/managed-node/provision-config-tls.sh" \
    "${node}" "${ref}" v1 /etc/ocserv/t07.crt /etc/ocserv/t07.key | jq -r .key)"
  [[ "${observed_key}" == "${expected_key}" ]]
  # ConfigPlan cannot change TLS startup paths through a reload.
  sed -e "s|^server-cert = /etc/ocserv/t07.crt$|server-cert = /etc/ocservia-agent/config-tls/${ref}/v1/server-cert.pem|" \
    -e "s|^server-key = /etc/ocserv/t07.key$|server-key = /etc/ocservia-agent/config-tls/${ref}/v1/server-key.pem|" \
    "${WORK}/ocserv.conf" >"${WORK}/bound-ocserv.conf"
  sudo install -m 0600 "${WORK}/bound-ocserv.conf" /etc/ocserv/ocserv.conf
  sudo ocserv --test-config -c /etc/ocserv/ocserv.conf
  sudo systemctl restart ocserv
  mkdir -p "${OUTBOX}/business-node-ready"
  cp "${WORK}/proof.json" "${OUTBOX}/business-node-ready/proof.json"
  jq -n --arg node "${node}" --arg endpoint "${endpoint}" \
    '{node_id:$node,agent_endpoint_id:$endpoint}' \
    >"${OUTBOX}/business-node-ready/ready.json"
}

start_native() {
  local registered_dir="${1:?registered authority directory is required}"
  [[ "$(jq -er .node_id "${registered_dir}/ready.json")" == "$(<"${OUTBOX}/enrollment-result/node-id")" ]]
  [[ "$(jq -er .agent_endpoint_id "${registered_dir}/ready.json")" == "$(<"${OUTBOX}/agent-endpoint/agent-endpoint-id")" ]]
  # Root key registration advances authorization; open the first session after it.
  local uid tool
  uid="$(id -u ocserv-agent)"
  printf '%s\n' "$uid" >"${WORK}/relay-udp-uid"
  for tool in iptables ip6tables; do
    [[ "$tool" != ip6tables || -n "$(cat /proc/net/if_inet6)" ]] || continue
    sudo "$tool" -I OUTPUT -m owner --uid-owner "$uid" -p udp ! --dport 53 -j REJECT
    touch "${WORK}/relay-${tool}-rule"
  done
  sudo systemctl enable --now ocservia-privd ocservia-agent
  for service in ocserv ocservia-privd ocservia-agent; do
    [[ "$(systemctl is-active "${service}")" == active ]]
    [[ "$(systemctl show "${service}" -p MainPID --value)" =~ ^[1-9][0-9]*$ ]]
  done
}

verify_native_business() {
  local done_dir="${1:?business completion directory is required}"
  openssl pkeyutl -decrypt -inkey "${WORK}/exchange.key" \
    -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 \
    -in "${done_dir}/vpn-password.oaep" -out "${SECRETS}/vpn-password"
  python3 "${ROOT}/scripts/real-e2e-native-business.py" "${done_dir}/done.json" \
    "${SECRETS}/vpn-password" "${OUTBOX}/business-node-result/result.json" "${WORK}"
}

collect_diagnostics() {
  local leaked=0 hit
  mkdir -p "${ARTIFACT_DIR}"
  printf 'runner=%s\nidentity_mode=persistent\nagent_uid=%s\n' "$(hostname)" "$(id -u)" \
    >"${ARTIFACT_DIR}/node-summary.txt"
  df -h >"${ARTIFACT_DIR}/disk.txt"
  for secret in "${SECRETS}/enrollment-token" "${SECRETS}/privd-credential" \
    "${SECRETS}/vpn-password" "${SECRETS}/vpn.key" "${WORK}/exchange.key"; do
    [[ -s "${secret}" ]] || continue
    while IFS= read -r hit; do
      rm -f -- "${hit}"
      leaked=1
    done < <(grep -RIlF -f "${secret}" "${ARTIFACT_DIR}" 2>/dev/null || true)
  done
  if grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' "${ARTIFACT_DIR}" >/dev/null 2>&1; then
    while IFS= read -r hit; do rm -f -- "${hit}"; done \
      < <(grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' "${ARTIFACT_DIR}" || true)
    leaked=1
  fi
  ((leaked == 0)) || {
    echo "a node diagnostic file contained secret material and was removed" >&2
    return 1
  }
}

cleanup_node() {
  if [[ -f "${WORK}/native-owned" ]]; then
    local tool
    for tool in iptables ip6tables; do
      if [[ -f "${WORK}/relay-${tool}-rule" ]]; then
        sudo "$tool" -D OUTPUT -m owner --uid-owner "$(<"${WORK}/relay-udp-uid")" -p udp ! --dport 53 -j REJECT
      fi
    done
    sudo ip netns pids real-e2e-client 2>/dev/null | xargs -r sudo kill || true
    sudo ip netns del real-e2e-client 2>/dev/null || true
    sudo ip link del real-e2e-host 2>/dev/null || true
    sudo systemctl disable --now ocservia-agent ocservia-privd 2>/dev/null || true
    if dpkg-query -W -f='${Status}' ocservia-agent 2>/dev/null | grep -Fxq 'install ok installed'; then
      sudo apt-get remove -y ocservia-agent
    fi
    sudo systemctl stop ocserv 2>/dev/null || true
    if dpkg-query -W -f='${Status}' ocserv 2>/dev/null | grep -Fxq 'install ok installed'; then
      sudo apt-get purge -y ocserv
    fi
    sudo rm -rf -- /etc/ocservia-agent /var/lib/ocservia-agent /var/lib/ocservia-privd /etc/ocservia
    sudo rm -f -- /etc/ocserv/t07.crt /etc/ocserv/t07.key /etc/ocserv/ocpasswd
    sudo userdel ocservia-vpn 2>/dev/null || true
    sudo groupdel ocservia-vpn 2>/dev/null || true
    [[ ! -e /etc/ocservia-agent && ! -e /var/lib/ocservia-agent && ! -e /var/lib/ocservia-privd ]]
    ! sudo ip netns list | grep -q '^real-e2e-client '
    ! pgrep -x ocservia-agent >/dev/null
    ! pgrep -x ocservia-privd >/dev/null
    ! pgrep -x ocserv >/dev/null
  fi
  rm -rf -- "${WORK}" "${TARGET}" \
    "${RUNNER_TEMP:-/tmp}/real-e2e-controller-ready" \
    "${RUNNER_TEMP:-/tmp}/real-e2e-enrollment-token"
}

case "${1:-}" in
  build) build_agent ;;
  prepare) prepare_endpoint "${2:-}" ;;
  enroll) enroll "${2:-}" ;;
  activate) activate_native "${2:-}" ;;
  start-native) start_native "${2:-}" ;;
  recovery-baseline|recovery-queued|recovery-result)
    python3 "${ROOT}/scripts/real-e2e-native-business.py" "$1" "${2:?Controller phase directory required}" \
      "${OUTBOX}/recovery-node-${1#recovery-}/result.json" "${WORK}" ;;
  verify-business) verify_native_business "${2:-}" ;;
  diagnostics) collect_diagnostics ;;
  cleanup) cleanup_node ;;
  *) echo "usage: $0 {build|prepare CONTROLLER_DIR|enroll TOKEN_DIR|activate AUTHORITY_DIR|start-native REGISTERED_DIR|verify-business DONE_DIR|recovery-{baseline,queued,result} CONTROLLER_DIR|diagnostics|cleanup}" >&2; exit 2 ;;
esac
