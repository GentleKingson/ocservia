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
openssl genpkey -algorithm ED25519 -out "${work}/controller-command.key" >/dev/null 2>&1
controller_endpoint="$(openssl pkey -in "${work}/signing.key" -pubout -outform DER \
  | tail -c 32 | od -An -tx1 | tr -d ' \n')"
substitute_controller_endpoint="$(openssl pkey -in "${work}/controller-command.key" -pubout -outform DER \
  | tail -c 32 | od -An -tx1 | tr -d ' \n')"
fresh_identity="${work}/fresh-identity"
mkdir -m 0700 "${fresh_identity}"
prepared_endpoint="$("${ROOT}/rust/target/release/ocservia-agent" \
  --identity-dir "${fresh_identity}" --controller "${controller_endpoint}" \
  --prepare-enrollment)"
[[ "${prepared_endpoint}" =~ ^[0-9a-f]{64}$ ]] \
  || { echo "fresh enrollment did not print a lowercase hexadecimal EndpointID" >&2; exit 1; }
test "${prepared_endpoint}" = "$("${ROOT}/rust/target/release/ocservia-agent" \
  --identity-dir "${fresh_identity}" --controller "${controller_endpoint}" \
  --prepare-enrollment)" \
  || { echo "fresh enrollment preparation rotated the endpoint identity" >&2; exit 1; }
test "$(stat -c '%a' "${fresh_identity}/endpoint.key")" = 600
test "$(stat -c '%a' "${fresh_identity}/controller.endpoint")" = 600
if "${ROOT}/rust/target/release/ocservia-agent" \
  --identity-dir "${fresh_identity}" \
  --controller "${substitute_controller_endpoint}" \
  --prepare-enrollment >/dev/null 2>&1; then
  echo "fresh enrollment preparation accepted controller pin substitution" >&2
  exit 1
fi
echo "fresh enrollment identity preparation passed"
chmod 0600 "${work}/signing.key"
openssl pkey -in "${work}/signing.key" -pubout -out "${work}/trusted.pub.pem" >/dev/null 2>&1
openssl pkey -pubin -in "${work}/trusted.pub.pem" -outform DER -out "${work}/trusted.der"
trusted_fingerprint="$(sha256sum "${work}/trusted.der" | awk '{print $1}')"
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

expected_production_agent_exec_start="/usr/libexec/ocservia/ocservia-agent --controller \$CONTROLLER_ENDPOINT_ID --node-id \$NODE_ID --controller-command-key-file \$CONTROLLER_COMMAND_VERIFICATION_KEY_FILE --user-password-seal-key-id \$USER_PASSWORD_SEAL_KEY_ID --user-password-seal-public-key-sha256 \$USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256 --p12-password-seal-key-id \$P12_PASSWORD_SEAL_KEY_ID --p12-password-seal-public-key-sha256 \$P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256 --relay-mode custom --relay-url \$RELAY_URL_A --relay-url \$RELAY_URL_B --relay-token-file /etc/ocservia-agent/relay-access-token"
assert_production_agent_exec_start
assert_privd_command_authority() {
  local unit="${rootfs}/usr/lib/systemd/system/ocservia-privd.service"
  grep -Fxq 'EnvironmentFile=/etc/ocservia-agent/agent.env' "${unit}" \
    || { echo "privd does not load the pinned node/key environment" >&2; exit 1; }
  grep -Fxq "ExecStart=/usr/libexec/ocservia/ocservia-privd --agent-uid \$AGENT_UID --node-id \$NODE_ID --controller-command-key-file \$CONTROLLER_COMMAND_VERIFICATION_KEY_FILE --user-password-seal-key-file \$USER_PASSWORD_SEAL_PRIVATE_KEY_FILE --user-password-seal-key-id \$USER_PASSWORD_SEAL_KEY_ID --user-password-seal-public-key-sha256 \$USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256 --p12-password-seal-key-file \$P12_PASSWORD_SEAL_PRIVATE_KEY_FILE --p12-password-seal-key-id \$P12_PASSWORD_SEAL_KEY_ID --p12-password-seal-public-key-sha256 \$P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256" "${unit}" \
    || { echo "privd does not independently pin command authority" >&2; exit 1; }
}
assert_privd_command_authority
echo "agent package install passed"
sudo install -o 61000 -g 61000 -m 0600 /dev/null "${rootfs}/var/lib/ocservia-agent/identity/controller.key"
sudo install -o 61000 -g 61000 -m 0600 /dev/null "${rootfs}/var/lib/ocservia-agent/agent.db"

cat >"${work}/legacy-agent.env" <<EOF
CONTROLLER_ENDPOINT_ID=${controller_endpoint}
NODE_ID=00000000-0000-7000-8000-000000000000
EOF
sudo install -o root -g 61000 -m 0640 "${work}/legacy-agent.env" \
  "${rootfs}/etc/ocservia-agent/agent.env"
sed -e 's/ --controller-command-key-file [^ ]*//' \
  -e 's/ --user-password-seal-key-id [^ ]*//' \
  -e 's/ --user-password-seal-public-key-sha256 [^ ]*//' \
  -e 's/ --p12-password-seal-key-id [^ ]*//' \
  -e 's/ --p12-password-seal-public-key-sha256 [^ ]*//' \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service" >"${work}/legacy-agent.service"
sudo install -o root -g root -m 0644 "${work}/legacy-agent.service" \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service"
sed -e 's/ --controller-command-key-file [^ ]*//' \
  -e 's/ --user-password-seal-key-id [^ ]*//' \
  -e 's/ --user-password-seal-public-key-sha256 [^ ]*//' \
  -e 's/ --p12-password-seal-key-id [^ ]*//' \
  -e 's/ --p12-password-seal-public-key-sha256 [^ ]*//' \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf" \
  >"${work}/legacy-agent-relays.conf"
sudo install -o root -g root -m 0644 "${work}/legacy-agent-relays.conf" \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
sed -e '/^EnvironmentFile=\/etc\/ocservia-agent\/agent.env$/d' \
  -e 's/ --node-id .*//' \
  "${rootfs}/usr/lib/systemd/system/ocservia-privd.service" >"${work}/legacy-privd.service"
sudo install -o root -g root -m 0644 "${work}/legacy-privd.service" \
  "${rootfs}/usr/lib/systemd/system/ocservia-privd.service"
if grep -Fq -- '--controller-command-key-file' \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service" \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf" \
  "${rootfs}/usr/lib/systemd/system/ocservia-privd.service"; then
  echo "legacy systemd fixture still requires the new key argument" >&2
  exit 1
fi

cat >"${work}/legacy-agent" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [[ $# -gt 0 ]]; do
  case "$1" in
    --controller|--node-id|--relay-url|--relay-token-file)
      [[ $# -ge 2 ]]
      shift 2
      ;;
    --relay-mode)
      [[ $# -ge 2 && "$2" == custom ]]
      shift 2
      ;;
    *)
      echo "legacy Agent unknown argument: $1" >&2
      exit 2
      ;;
  esac
done
echo "legacy Agent arguments accepted"
EOF
cat >"${work}/legacy-privd" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [[ $# -gt 0 ]]; do
  case "$1" in
    --socket|--agent-uid)
      [[ $# -ge 2 ]]
      shift 2
      ;;
    *)
      echo "legacy privd unknown argument: $1" >&2
      exit 2
      ;;
  esac
done
echo "legacy privd arguments accepted"
EOF
chmod 0755 "${work}/legacy-agent" "${work}/legacy-privd"
sudo install -o root -g root -m 0755 "${work}/legacy-agent" \
  "${rootfs}/usr/libexec/ocservia/ocservia-agent"
sudo install -o root -g root -m 0755 "${work}/legacy-privd" \
  "${rootfs}/usr/libexec/ocservia/ocservia-privd"

installed_state() {
  sudo sha256sum \
    "${rootfs}/usr/libexec/ocservia/ocservia-agent" \
    "${rootfs}/usr/libexec/ocservia/ocservia-privd" \
    "${rootfs}/usr/libexec/ocservia/ocservia-agent-rollback" \
    "${rootfs}/usr/lib/systemd/system/ocservia-agent.service" \
    "${rootfs}/usr/lib/systemd/system/ocservia-privd.service" \
    "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
  sudo stat -c '%n:%i:%Y:%s' \
    "${rootfs}/usr/libexec/ocservia/ocservia-agent" \
    "${rootfs}/usr/libexec/ocservia/ocservia-privd" \
    "${rootfs}/usr/libexec/ocservia/ocservia-agent-rollback" \
    "${rootfs}/usr/lib/systemd/system/ocservia-agent.service" \
    "${rootfs}/usr/lib/systemd/system/ocservia-privd.service" \
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
sudo install -o 61000 -g 61000 -m 0600 "${work}/controller-command.pub.pem" \
  "${rootfs}/etc/ocservia-agent/controller-command-verification-key.pem"
before_agent_owned_key_rejection="$({
  installed_state
  sudo sha256sum \
    "${rootfs}/etc/ocservia-agent/agent.env" \
    "${rootfs}/etc/ocservia-agent/controller-command-verification-key.pem"
  sudo stat -c '%n:%u:%g:%a:%h:%i:%Y:%s' \
    "${rootfs}/etc/ocservia-agent/agent.env" \
    "${rootfs}/etc/ocservia-agent/controller-command-verification-key.pem"
})"
if capture_upgrade "${ARTIFACT_DIR}/legacy-upgrade-agent-owned-key.log"; then
  echo "legacy Agent upgrade accepted an Agent-owned command verification key" >&2
  exit 1
fi
grep -Fq 'blocked before modification' "${ARTIFACT_DIR}/legacy-upgrade-agent-owned-key.log"
grep -Fq 'so Agent and privd can both load it' "${ARTIFACT_DIR}/legacy-upgrade-agent-owned-key.log"
assert_rejected_upgrade_untouched "upgrade with an Agent-owned key"
test "${before_agent_owned_key_rejection}" = "$({
  installed_state
  sudo sha256sum \
    "${rootfs}/etc/ocservia-agent/agent.env" \
    "${rootfs}/etc/ocservia-agent/controller-command-verification-key.pem"
  sudo stat -c '%n:%u:%g:%a:%h:%i:%Y:%s' \
    "${rootfs}/etc/ocservia-agent/agent.env" \
    "${rootfs}/etc/ocservia-agent/controller-command-verification-key.pem"
})" || { echo "rejected Agent-owned key upgrade modified installed state" >&2; exit 1; }
sudo install -o root -g 61000 -m 0640 "${work}/controller-command.pub.pem" \
  "${rootfs}/etc/ocservia-agent/controller-command-verification-key.pem"
if capture_upgrade "${ARTIFACT_DIR}/legacy-upgrade-missing-password-sealing-keys.log"; then
  echo "legacy Agent upgrade proceeded without purpose-separated password sealing keys" >&2
  exit 1
fi
grep -Fq 'blocked before modification' "${ARTIFACT_DIR}/legacy-upgrade-missing-password-sealing-keys.log"
grep -Fq 'password sealing key' "${ARTIFACT_DIR}/legacy-upgrade-missing-password-sealing-keys.log"
assert_rejected_upgrade_untouched "upgrade without password sealing keys"
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
sudo install -d -o 61000 -g 61000 -m 0700 "${rootfs}/etc/ocservia-agent/agent-owned"
sudo install -o root -g 61000 -m 0640 "${work}/controller-command.pub.pem" \
  "${rootfs}/etc/ocservia-agent/agent-owned/controller-command-verification-key.pem"
sed 's|=/etc/ocservia-agent/controller-command|=/etc/ocservia-agent/agent-owned/controller-command|' \
  "${work}/legacy-agent.env" >"${work}/agent-owned-ancestry-agent.env"
sudo install -o root -g 61000 -m 0640 "${work}/agent-owned-ancestry-agent.env" \
  "${rootfs}/etc/ocservia-agent/agent.env"
if capture_upgrade "${ARTIFACT_DIR}/legacy-upgrade-agent-owned-ancestry.log"; then
  echo "legacy Agent upgrade accepted Agent-owned command key ancestry" >&2
  exit 1
fi
grep -Fq 'blocked before modification' "${ARTIFACT_DIR}/legacy-upgrade-agent-owned-ancestry.log"
assert_rejected_upgrade_untouched "upgrade with Agent-owned key ancestry"
sudo install -o root -g 61000 -m 0640 "${work}/legacy-agent.env" \
  "${rootfs}/etc/ocservia-agent/agent.env"
echo "legacy Agent upgrade fail-closed preflight passed"

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${work}/user-password-seal-private.pem" >/dev/null 2>&1
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${work}/p12-password-seal-private.pem" >/dev/null 2>&1
user_seal_hash="$(openssl rsa -in "${work}/user-password-seal-private.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
p12_seal_hash="$(openssl rsa -in "${work}/p12-password-seal-private.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
cat >>"${work}/legacy-agent.env" <<EOF
USER_PASSWORD_SEAL_KEY_ID=user-password-v1
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}
P12_PASSWORD_SEAL_KEY_ID=p12-password-v1
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${p12_seal_hash}
EOF
sudo install -o root -g 61000 -m 0640 "${work}/legacy-agent.env" \
  "${rootfs}/etc/ocservia-agent/agent.env"
sudo install -o root -g root -m 0600 "${work}/user-password-seal-private.pem" \
  "${rootfs}/etc/ocservia-agent/user-password-seal-private.pem"
sudo install -o root -g root -m 0600 "${work}/p12-password-seal-private.pem" \
  "${rootfs}/etc/ocservia-agent/p12-password-seal-private.pem"

	test "$(sudo stat -c '%u:%g:%a' -- "${rootfs}/etc/ocservia-agent/controller-command-verification-key.pem")" = "0:61000:640" \
	  || { echo "trusted shared command key metadata changed before upgrade" >&2; exit 1; }
	if capture_upgrade "${ARTIFACT_DIR}/legacy-upgrade-missing-sealing-enrollment.log"; then
	  echo "legacy Agent upgrade proceeded without Controller-side sealing-key enrollment" >&2
	  exit 1
	fi
	grep -Fq 'blocked before modification' "${ARTIFACT_DIR}/legacy-upgrade-missing-sealing-enrollment.log"
	grep -Fq 'one-time sealing-key enrollment' "${ARTIFACT_DIR}/legacy-upgrade-missing-sealing-enrollment.log"
	assert_rejected_upgrade_untouched "upgrade without sealing-key enrollment"

	legacy_artifact_id="018f0c2e-7b1a-7c3d-8e9f-0123456789ab"
	legacy_artifact_dir="${rootfs}/var/lib/ocservia-privd/certificates/artifacts"
	sudo install -d -o root -g 61000 -m 0710 "${rootfs}/var/lib/ocservia-privd"
	sudo install -d -o root -g 61000 -m 0710 "${rootfs}/var/lib/ocservia-privd/certificates"
	sudo install -d -o root -g 61000 -m 0710 "${legacy_artifact_dir}"
	printf 'legacy-p12' >"${work}/legacy-artifact.p12"
	sudo install -o root -g 61000 -m 0640 "${work}/legacy-artifact.p12" \
	  "${legacy_artifact_dir}/${legacy_artifact_id}.p12"
	chmod 0711 "${work}"
	sudo setpriv --reuid=61000 --regid=61000 --clear-groups \
	  test -r "${legacy_artifact_dir}/${legacy_artifact_id}.p12" \
	  || { echo "legacy fixture was not readable by the Agent UID" >&2; exit 1; }

	sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
	  ENROLLMENT_MIGRATION_CONFIRMED=true \
	  "${package_root}/scripts/upgrade-agent.sh"
	test "$(sudo stat -c '%u:%a' -- "${legacy_artifact_dir}")" = "0:700"
	sudo test ! -e "${legacy_artifact_dir}/${legacy_artifact_id}.p12"
	if sudo setpriv --reuid=61000 --regid=61000 --clear-groups \
	  test -r "${legacy_artifact_dir}/${legacy_artifact_id}.p12"; then
	  echo "Agent UID retained access to a legacy P12 after upgrade" >&2
	  exit 1
	fi
	chmod 0700 "${work}"
	test "$(sudo stat -c '%u:%g:%a' -- "${rootfs}/etc/ocservia-agent/sealing-keys-bound")" = "0:0:600"
	sudo grep -Fxq "node_id=00000000-0000-7000-8000-000000000000" \
	  "${rootfs}/etc/ocservia-agent/sealing-keys-bound"
	sudo grep -Fxq "user_sha256=${user_seal_hash}" "${rootfs}/etc/ocservia-agent/sealing-keys-bound"
	sudo grep -Fxq "p12_sha256=${p12_seal_hash}" "${rootfs}/etc/ocservia-agent/sealing-keys-bound"
for backup in \
  ocservia-agent.previous \
  ocservia-privd.previous \
  ocservia-agent.service.previous \
  ocservia-privd.service.previous \
  ocservia-agent-relays.conf.previous; do
  sudo test -f "${rootfs}/var/lib/ocservia-agent/upgrade-backup/${backup}" \
    || { echo "matched rollback snapshot is missing ${backup}" >&2; exit 1; }
done
assert_production_agent_exec_start
assert_privd_command_authority
echo "agent package upgrade passed"

sudo test ! -e "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-agent-relays.conf.absent" \
  || { echo "rollback snapshot records conflicting relay states" >&2; exit 1; }
sudo cmp -s "${work}/legacy-agent" \
  "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-agent.previous"
sudo cmp -s "${work}/legacy-privd" \
  "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-privd.previous"
sudo cmp -s "${work}/legacy-agent.service" \
  "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-agent.service.previous"
sudo cmp -s "${work}/legacy-privd.service" \
  "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-privd.service.previous"
sudo cmp -s "${work}/legacy-agent-relays.conf" \
  "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-agent-relays.conf.previous"

sudo env DESTDIR="${rootfs}" \
  "${rootfs}/usr/libexec/ocservia/ocservia-agent-rollback"
sudo cmp -s "${work}/legacy-agent" "${rootfs}/usr/libexec/ocservia/ocservia-agent"
sudo cmp -s "${work}/legacy-privd" "${rootfs}/usr/libexec/ocservia/ocservia-privd"
sudo cmp -s "${work}/legacy-agent.service" \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service"
sudo cmp -s "${work}/legacy-privd.service" \
  "${rootfs}/usr/lib/systemd/system/ocservia-privd.service"
sudo cmp -s "${work}/legacy-agent-relays.conf" \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
grep -Fxq "ExecStart=/usr/libexec/ocservia/ocservia-privd --agent-uid \$AGENT_UID" \
  "${rootfs}/usr/lib/systemd/system/ocservia-privd.service"
sudo "${rootfs}/usr/libexec/ocservia/ocservia-privd" --agent-uid 61000 \
  | grep -Fxq 'legacy privd arguments accepted'
sudo "${rootfs}/usr/libexec/ocservia/ocservia-agent" \
  --controller legacy-controller --node-id 00000000-0000-7000-8000-000000000000 \
  --relay-mode custom --relay-url https://relay-a.invalid \
  --relay-url https://relay-b.invalid --relay-token-file /etc/ocservia-agent/relay-access-token \
  | grep -Fxq 'legacy Agent arguments accepted'
echo "matched Agent/privd binary and systemd rollback passed"

sudo rm -f -- \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
  "${package_root}/scripts/upgrade-agent.sh"
sudo test -f "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-agent-relays.conf.absent"
sudo test ! -e "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-agent-relays.conf.previous"
sudo test -f \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
sudo env DESTDIR="${rootfs}" \
  "${rootfs}/usr/libexec/ocservia/ocservia-agent-rollback"
sudo test ! -e \
  "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
echo "absent production relay drop-in rollback passed"

sudo env DESTDIR="${rootfs}" "${package_root}/scripts/uninstall-agent.sh"
sudo test -f "${rootfs}/var/lib/ocservia-agent/identity/controller.key" \
  || { echo "uninstall removed the controller key" >&2; exit 1; }
sudo test -f "${rootfs}/var/lib/ocservia-agent/agent.db" \
  || { echo "uninstall removed the command journal" >&2; exit 1; }
test ! -e "${rootfs}/usr/libexec/ocservia/ocservia-agent" \
  || { echo "uninstall retained the Agent binary" >&2; exit 1; }
test ! -e "${rootfs}/usr/libexec/ocservia/ocservia-agent-rollback" \
  || { echo "uninstall retained the rollback command" >&2; exit 1; }
echo "agent package uninstall preservation passed"

sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
  "${package_root}/scripts/install-agent.sh"
sudo env DESTDIR="${rootfs}" "${package_root}/scripts/uninstall-agent.sh" --purge-state
test ! -e "${rootfs}/var/lib/ocservia-agent" || { echo "purge retained Agent state" >&2; exit 1; }
test ! -e "${rootfs}/etc/ocservia-agent" || { echo "purge retained Agent configuration" >&2; exit 1; }
echo "agent package purge passed"
printf 'install=pass\nlegacy_upgrade_preflight=pass\nupgrade=pass\nrollback=pass\nuninstall_preserves_state=pass\npurge=pass\n' \
  >"${ARTIFACT_DIR}/lifecycle-summary.txt"
