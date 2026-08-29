#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
suffix="$(printf '%s' "${RUN_ID}" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-' | cut -c1-48)"
unit="ocservia-f3-${suffix}.service"
config_dir="/etc/ocservia-f3-${suffix}"
binary="/usr/local/libexec/ocservia-privd-f3-${suffix}"
socket="/run/ocserv-platform/privd-f3-${suffix}.sock"
state_dir=/var/lib/ocservia-privd
attestation_key="${state_dir}/attestation-f3-${suffix}.key"
upgrade_state_dir=/var/lib/ocservia-upgrade
upgrade_state_dir_created=no
node_id=019ff100-0000-7000-8000-000000000003
work="${RUNNER_TEMP:-/tmp}/ocservia-f3-${suffix}"

[[ -n "${suffix}" ]] || { echo "RUN_ID is unsafe" >&2; exit 2; }
mkdir -p "${ARTIFACT_DIR}" "${work}"
chmod 0700 "${work}"

cleanup() {
  local status=$?
  sudo systemctl stop "${unit}" >/dev/null 2>&1 || true
  sudo systemctl reset-failed "${unit}" >/dev/null 2>&1 || true
  sudo rm -f -- "${socket}" "${binary}"
  sudo rm -rf -- "${config_dir}"
  if [[ "${upgrade_state_dir_created}" == yes ]]; then
    sudo rm -rf -- "${upgrade_state_dir}"
  fi
  sudo rm -f -- "${attestation_key}" "${state_dir}/desired-effects.sqlite3" \
    "${state_dir}/desired-effects.key"
  rm -rf -- "${work}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

"${ROOT}/scripts/agent-boundary-check.sh" | tee "${ARTIFACT_DIR}/boundary.log"
(cd "${ROOT}/rust" && cargo build --locked --release --package ocservia-agent --package ocservia-privd --package ocservia-upgrader)

sudo install -d -o root -g root -m 0755 /usr/local/libexec
sudo install -o root -g root -m 0755 "${ROOT}/rust/target/release/ocservia-privd" "${binary}"
sudo install -d -o root -g nogroup -m 0750 "${config_dir}"
sudo install -d -o root -g nogroup -m 0700 "${state_dir}"
# The fixture sandbox mirrors the shipped ReadWritePaths contract, and mount
# namespacing fails when the package-owned upgrade state directory is absent.
if ! sudo test -e "${upgrade_state_dir}"; then
  sudo install -d -o root -g root -m 0700 "${upgrade_state_dir}"
  upgrade_state_dir_created=yes
fi
openssl genpkey -algorithm ED25519 -out "${work}/controller-command.key" >/dev/null 2>&1
openssl pkey -in "${work}/controller-command.key" -pubout \
  -out "${work}/controller-command.pub.pem" >/dev/null 2>&1
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${work}/user-seal.pem" >/dev/null 2>&1
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${work}/p12-seal.pem" >/dev/null 2>&1
user_hash="$(openssl pkey -in "${work}/user-seal.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
p12_hash="$(openssl pkey -in "${work}/p12-seal.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
sudo install -o root -g nogroup -m 0640 "${work}/controller-command.pub.pem" \
  "${config_dir}/controller-command.pub.pem"
sudo install -o root -g root -m 0600 "${work}/user-seal.pem" "${config_dir}/user-seal.pem"
sudo install -o root -g root -m 0600 "${work}/p12-seal.pem" "${config_dir}/p12-seal.pem"

start_privd() {
  sudo systemd-run --unit="${unit}" \
    --property=Type=simple \
    --property=User=root \
    --property=Group=nogroup \
    --property=Restart=on-failure \
    --property=RestartSec=1s \
    --property=RuntimeDirectory=ocserv-platform \
    --property=RuntimeDirectoryMode=0750 \
    --property=StateDirectory=ocservia-privd \
    --property=StateDirectoryMode=0700 \
    --property=NoNewPrivileges=yes \
    --property=PrivateTmp=yes \
    --property=PrivateDevices=yes \
    --property=ProtectSystem=strict \
    --property=ProtectHome=yes \
    --property=ProtectKernelTunables=yes \
    --property=ProtectKernelModules=yes \
    --property=ProtectControlGroups=yes \
    --property=RestrictAddressFamilies='AF_UNIX AF_NETLINK' \
    --property=IPAddressDeny=any \
    --property=CapabilityBoundingSet=CAP_DAC_OVERRIDE \
    --property=ReadWritePaths='-/etc/ocserv /var/lib/ocservia-privd /var/lib/ocservia-upgrade' \
    --property=UMask=0007 \
    --property=TasksMax=32 \
    --property=LimitNOFILE=128 \
    "${binary}" \
      --socket "${socket}" \
      --agent-uid 65534 \
      --node-id "${node_id}" \
      --controller-command-key-file "${config_dir}/controller-command.pub.pem" \
      --attestation-key-file "${attestation_key}" \
      --user-password-seal-key-file "${config_dir}/user-seal.pem" \
      --user-password-seal-key-id user-f3-v1 \
      --user-password-seal-public-key-sha256 "${user_hash}" \
      --p12-password-seal-key-file "${config_dir}/p12-seal.pem" \
      --p12-password-seal-key-id p12-f3-v1 \
      --p12-password-seal-public-key-sha256 "${p12_hash}" >/dev/null
}

wait_active() {
  for _ in $(seq 1 100); do
    if [[ "$(sudo systemctl is-active "${unit}" 2>/dev/null || true)" == active ]] && \
      sudo test -S "${socket}"; then
      return 0
    fi
    sleep 0.1
  done
  sudo journalctl -u "${unit}" --no-pager -n 100 >&2 || true
  return 1
}

start_privd
wait_active
first_pid="$(sudo systemctl show -p MainPID --value "${unit}")"
first_key_hash="$(sudo sha256sum "${attestation_key}" | awk '{print $1}')"
sudo systemctl show "${unit}" \
  -p ActiveState -p SubState -p MainPID -p User -p Group -p NoNewPrivileges \
  -p RestrictAddressFamilies -p IPAddressDeny -p CapabilityBoundingSet \
  | tee "${ARTIFACT_DIR}/systemd-properties-before.txt" >/dev/null
test "${first_pid}" -gt 1
test "$(sudo stat -c '%u:%g:%a' "${state_dir}")" = '0:65534:700'
test "$(sudo stat -c '%u:%g:%a:%h' "${attestation_key}")" = '0:65534:600:1'
if sudo -u nobody test -r "${attestation_key}"; then
  echo "unprivileged Agent identity can read the privd attestation key" >&2
  exit 1
fi
if sudo ss -ltnp | grep -Fq "pid=${first_pid},"; then
  echo "privd opened a TCP listener" >&2
  exit 1
fi

sudo systemctl restart "${unit}"
wait_active
second_pid="$(sudo systemctl show -p MainPID --value "${unit}")"
test "${second_pid}" -gt 1
test "${second_pid}" != "${first_pid}"
test "$(sudo sha256sum "${attestation_key}" | awk '{print $1}')" = "${first_key_hash}"

sudo chmod 0644 "${attestation_key}"
if sudo systemctl restart "${unit}"; then
  sleep 1
fi
test "$(sudo systemctl is-active "${unit}" 2>/dev/null || true)" != active
sudo journalctl -u "${unit}" --no-pager -n 100 \
  | tee "${ARTIFACT_DIR}/fail-closed-journal.log" >/dev/null
grep -Fq 'attestation key metadata invalid' "${ARTIFACT_DIR}/fail-closed-journal.log"
sudo systemctl stop "${unit}" >/dev/null 2>&1 || true
sudo chmod 0600 "${attestation_key}"
sudo systemctl reset-failed "${unit}" >/dev/null 2>&1 || true
start_privd
wait_active

sudo "${binary}" attestation-registration "${attestation_key}" "${node_id}" \
  "$(printf '11%.0s' {1..32})" "$(printf '22%.0s' {1..32})" \
  | tee "${ARTIFACT_DIR}/attestation-registration.json" >/dev/null
jq -e '.version == 1 and (.key_id | length > 0) and (.public_key | length > 0) and (.signature | length > 0)' \
  "${ARTIFACT_DIR}/attestation-registration.json" >/dev/null

(cd "${ROOT}/rust" && cargo test --locked -p ocservia-privd -p ocservia-privd-attestation -p ocservia-ocserv-adapter) \
  2>&1 | tee "${ARTIFACT_DIR}/privd-pki-tests.log"
# The package smoke asserts a pristine host before its rejection tests, and
# the running unit no longer needs the directory after its final start.
if [[ "${upgrade_state_dir_created}" == yes ]]; then
  sudo rm -rf -- "${upgrade_state_dir}"
  upgrade_state_dir_created=no
fi
RUN_ID="${RUN_ID}-package" ARTIFACT_DIR="${ARTIFACT_DIR}/package" \
  "${ROOT}/scripts/i18-agent-package-smoke.sh"

sudo systemctl show "${unit}" -p ActiveState -p SubState -p MainPID \
  | tee "${ARTIFACT_DIR}/systemd-properties-after.txt" >/dev/null
printf '%s\n' \
  'F3=PASS' \
  'privd_root_boundary=PASS' \
  'agent_key_access=DENIED' \
  'network_listener=ABSENT' \
  'systemd_restart=PASS' \
  'attestation_key_restart_stability=PASS' \
  'unsafe_key_fail_closed=PASS' \
  'certificate_lifecycle=PASS' \
  'package_upgrade_rollback=PASS' >"${ARTIFACT_DIR}/f3-summary.txt"
echo "F3 systemd/root/privd/PKI acceptance passed"
