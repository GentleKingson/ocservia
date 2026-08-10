#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
if [[ "${RUN_ID}" == *[^a-zA-Z0-9._-]* ]]; then
  echo "RUN_ID contains unsafe characters" >&2
  exit 2
fi
work="${RUNNER_TEMP:-/tmp}/ocservia-i18-package-${RUN_ID}"
mkdir -p "${work}" "${ARTIFACT_DIR}"
chmod 0700 "${work}"
cleanup() {
  local status=$?
  sudo rm -rf -- "${work}" || status=1
  exit "${status}"
}
trap cleanup EXIT INT TERM

(cd "${ROOT}/rust" && cargo build --locked --release --package ocservia-agent --package ocservia-privd)
echo "agent package release build passed"
openssl genpkey -algorithm ED25519 -out "${work}/signing.key" >/dev/null 2>&1
chmod 0600 "${work}/signing.key"
openssl pkey -in "${work}/signing.key" -pubout -out "${work}/trusted.pub.pem" >/dev/null 2>&1
openssl pkey -pubin -in "${work}/trusted.pub.pem" -outform DER -out "${work}/trusted.der"
trusted_fingerprint="$(sha256sum "${work}/trusted.der" | awk '{print $1}')"
openssl genpkey -algorithm ED25519 -out "${work}/controller-command.key" >/dev/null 2>&1
openssl pkey -in "${work}/controller-command.key" -pubout \
  -out "${work}/controller-command.pub.pem" >/dev/null 2>&1
archive="$(OUTPUT_DIR="${ARTIFACT_DIR}" AGENT_SIGNING_KEY="${work}/signing.key" VERSION=1.0.0 \
  SOURCE_DATE_EPOCH=1786147200 "${ROOT}/scripts/package-agent.sh")"
AGENT_TRUSTED_KEY_SHA256="${trusted_fingerprint}" "${ROOT}/scripts/verify-agent-package.sh" "${archive}" "${archive}.sha256" \
  "${archive}.sha256.sig" "${work}/trusted.pub.pem" >"${ARTIFACT_DIR}/verification.log"
echo "agent package signature verification passed"

openssl genpkey -algorithm ED25519 -out "${work}/substitute.key" >/dev/null 2>&1
openssl pkey -in "${work}/substitute.key" -pubout -out "${work}/substitute.pub.pem" >/dev/null 2>&1
openssl pkeyutl -sign -rawin -inkey "${work}/substitute.key" -in "${archive}.sha256" -out "${work}/substitute.sig"
if AGENT_TRUSTED_KEY_SHA256="${trusted_fingerprint}" "${ROOT}/scripts/verify-agent-package.sh" \
  "${archive}" "${archive}.sha256" "${work}/substitute.sig" "${work}/substitute.pub.pem" >/dev/null 2>&1; then
  echo "substituted package signing key was trusted" >&2
  exit 1
fi
echo "agent package signer substitution rejection passed"

tar -xzf "${archive}" -C "${work}"
package_root="${work}/ocservia-agent-1.0.0"
rootfs="${work}/rootfs"
grep -Fxq 'agent_protocol=1.1' "${package_root}/MANIFEST"
mkdir -p "${rootfs}"
sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
  "${package_root}/scripts/install-agent.sh"
test -x "${rootfs}/usr/libexec/ocservia/ocservia-agent" || { echo "installed Agent binary is missing" >&2; exit 1; }
test -f "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf" \
  || { echo "production relay drop-in is missing" >&2; exit 1; }

production_agent_exec_start() {
  awk '
    /^ExecStart=/ {
      value = substr($0, length("ExecStart=") + 1)
      if (value == "") {
        count = 0
        delete commands
      } else {
        commands[++count] = value
      }
    }
    END {
      if (count != 1) {
        exit 1
      }
      print commands[1]
    }
  ' \
    "${rootfs}/usr/lib/systemd/system/ocservia-agent.service" \
    "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
}

assert_production_agent_exec_start() {
  local effective_exec_start

  effective_exec_start="$(production_agent_exec_start)" \
    || { echo "production Agent must have exactly one effective ExecStart" >&2; exit 1; }
  if [[ "${effective_exec_start}" != "${expected_production_agent_exec_start}" ]]; then
    echo "production Agent effective ExecStart does not match the authenticated relay command" >&2
    exit 1
  fi
}

expected_production_agent_exec_start="/usr/libexec/ocservia/ocservia-agent --controller \$CONTROLLER_ENDPOINT_ID --node-id \$NODE_ID --controller-command-key-file \$CONTROLLER_COMMAND_VERIFICATION_KEY_FILE --relay-mode custom --relay-url \$RELAY_URL_A --relay-url \$RELAY_URL_B --relay-token-file /etc/ocservia-agent/relay-access-token"
assert_production_agent_exec_start
echo "agent package install passed"
sudo install -o 61000 -g 61000 -m 0600 /dev/null "${rootfs}/var/lib/ocservia-agent/identity/controller.key"
sudo install -o 61000 -g 61000 -m 0600 /dev/null "${rootfs}/var/lib/ocservia-agent/agent.db"

cat >"${work}/legacy-agent.env" <<'EOF'
CONTROLLER_ENDPOINT_ID=replace-with-approved-controller-endpoint-id
NODE_ID=00000000-0000-7000-8000-000000000000
EOF
sudo install -o root -g 61000 -m 0640 "${work}/legacy-agent.env" \
  "${rootfs}/etc/ocservia-agent/agent.env"
sed 's/ --controller-command-key-file [^ ]*//' \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service" >"${work}/legacy-agent.service"
sudo install -o root -g root -m 0644 "${work}/legacy-agent.service" \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service"
sed 's/ --controller-command-key-file [^ ]*//' \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf" \
  >"${work}/legacy-agent-relays.conf"
sudo install -o root -g root -m 0644 "${work}/legacy-agent-relays.conf" \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
if grep -Fq -- '--controller-command-key-file' \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service" \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"; then
  echo "legacy systemd fixture still requires the new key argument" >&2
  exit 1
fi

installed_state() {
  sudo sha256sum \
    "${rootfs}/usr/libexec/ocservia/ocservia-agent" \
    "${rootfs}/usr/libexec/ocservia/ocservia-privd" \
    "${rootfs}/usr/lib/systemd/system/ocservia-agent.service" \
    "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
  sudo stat -c '%n:%i:%Y:%s' \
    "${rootfs}/usr/libexec/ocservia/ocservia-agent" \
    "${rootfs}/usr/libexec/ocservia/ocservia-privd" \
    "${rootfs}/usr/lib/systemd/system/ocservia-agent.service" \
    "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
}

capture_upgrade() {
  local log="$1"
  local output status
  set +e
  output="$(sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
    "${package_root}/scripts/upgrade-agent.sh" 2>&1)"
  status=$?
  set -e
  printf '%s\n' "${output}" >"${log}"
  return "${status}"
}

before_rejected_upgrade="$(installed_state)"
assert_rejected_upgrade_untouched() {
  local reason="$1"
  test ! -e "${rootfs}/var/lib/ocservia-agent/upgrade-backup" \
    || { echo "${reason} created a backup directory" >&2; exit 1; }
  test "${before_rejected_upgrade}" = "$(installed_state)" \
    || { echo "${reason} modified installed files" >&2; exit 1; }
}

if capture_upgrade "${ARTIFACT_DIR}/legacy-upgrade-missing-variable.log"; then
  echo "legacy Agent upgrade proceeded without a command verification key setting" >&2
  exit 1
fi
grep -Fq 'blocked before modification' "${ARTIFACT_DIR}/legacy-upgrade-missing-variable.log"
grep -Fq 'CONTROLLER_COMMAND_VERIFICATION_KEY_FILE' "${ARTIFACT_DIR}/legacy-upgrade-missing-variable.log"
sudo cmp -s "${work}/legacy-agent.env" "${rootfs}/etc/ocservia-agent/agent.env" \
  || { echo "rejected legacy upgrade changed agent.env" >&2; exit 1; }
assert_rejected_upgrade_untouched "rejected legacy upgrade"
echo 'CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem' \
  >>"${work}/legacy-agent.env"
sudo install -o root -g 61000 -m 0640 "${work}/legacy-agent.env" \
  "${rootfs}/etc/ocservia-agent/agent.env"
if capture_upgrade "${ARTIFACT_DIR}/legacy-upgrade-missing-key.log"; then
  echo "legacy Agent upgrade proceeded without the configured command verification key" >&2
  exit 1
fi
grep -Fq 'blocked before modification' "${ARTIFACT_DIR}/legacy-upgrade-missing-key.log"
assert_rejected_upgrade_untouched "upgrade with a missing key"
sudo install -o root -g 61000 -m 0644 "${work}/controller-command.pub.pem" \
  "${rootfs}/etc/ocservia-agent/controller-command-verification-key.pem"
if capture_upgrade "${ARTIFACT_DIR}/legacy-upgrade-unsafe-key.log"; then
  echo "legacy Agent upgrade accepted an unsafe command verification key mode" >&2
  exit 1
fi
grep -Fq 'blocked before modification' "${ARTIFACT_DIR}/legacy-upgrade-unsafe-key.log"
assert_rejected_upgrade_untouched "upgrade with an unsafe key"
sudo chmod 0640 "${rootfs}/etc/ocservia-agent/controller-command-verification-key.pem"
sudo install -d -o root -g 61000 -m 0770 "${rootfs}/etc/ocservia-agent/unsafe"
sudo install -o root -g 61000 -m 0640 "${work}/controller-command.pub.pem" \
  "${rootfs}/etc/ocservia-agent/unsafe/controller-command-verification-key.pem"
sed 's|=/etc/ocservia-agent/controller-command|=/etc/ocservia-agent/unsafe/controller-command|' \
  "${work}/legacy-agent.env" >"${work}/unsafe-ancestry-agent.env"
sudo install -o root -g 61000 -m 0640 "${work}/unsafe-ancestry-agent.env" \
  "${rootfs}/etc/ocservia-agent/agent.env"
if capture_upgrade "${ARTIFACT_DIR}/legacy-upgrade-unsafe-ancestry.log"; then
  echo "legacy Agent upgrade accepted unsafe key ancestry" >&2
  exit 1
fi
grep -Fq 'blocked before modification' "${ARTIFACT_DIR}/legacy-upgrade-unsafe-ancestry.log"
assert_rejected_upgrade_untouched "upgrade with unsafe key ancestry"
sudo install -o root -g 61000 -m 0640 "${work}/legacy-agent.env" \
  "${rootfs}/etc/ocservia-agent/agent.env"
echo "legacy Agent upgrade fail-closed preflight passed"

sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
  "${package_root}/scripts/upgrade-agent.sh"
sudo test -x "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-agent.previous" \
  || { echo "Agent upgrade backup is missing" >&2; exit 1; }
assert_production_agent_exec_start
echo "agent package upgrade passed"
sudo env DESTDIR="${rootfs}" "${package_root}/scripts/uninstall-agent.sh"
sudo test -f "${rootfs}/var/lib/ocservia-agent/identity/controller.key" \
  || { echo "uninstall removed the controller key" >&2; exit 1; }
sudo test -f "${rootfs}/var/lib/ocservia-agent/agent.db" \
  || { echo "uninstall removed the command journal" >&2; exit 1; }
test ! -e "${rootfs}/usr/libexec/ocservia/ocservia-agent" \
  || { echo "uninstall retained the Agent binary" >&2; exit 1; }
echo "agent package uninstall preservation passed"

sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
  "${package_root}/scripts/install-agent.sh"
sudo env DESTDIR="${rootfs}" "${package_root}/scripts/uninstall-agent.sh" --purge-state
test ! -e "${rootfs}/var/lib/ocservia-agent" || { echo "purge retained Agent state" >&2; exit 1; }
test ! -e "${rootfs}/etc/ocservia-agent" || { echo "purge retained Agent configuration" >&2; exit 1; }
echo "agent package purge passed"
printf 'install=pass\nlegacy_upgrade_preflight=pass\nupgrade=pass\nuninstall_preserves_state=pass\npurge=pass\n' \
  >"${ARTIFACT_DIR}/lifecycle-summary.txt"
