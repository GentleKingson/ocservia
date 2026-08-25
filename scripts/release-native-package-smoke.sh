#!/usr/bin/env bash
set -Eeuo pipefail

report_error_line() {
  local status=$?
  printf 'native package lifecycle failed at line %s\n' "${BASH_LINENO[0]:-unknown}" >&2
  return "${status}"
}
trap report_error_line ERR

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
if [[ "${RUN_ID}" == *[^a-zA-Z0-9._-]* ]]; then
  echo "RUN_ID contains unsafe characters" >&2
  exit 2
fi
case "$(uname -m)" in
  x86_64) PACKAGE_ARCH=amd64 ;;
  aarch64) PACKAGE_ARCH=arm64 ;;
  *)
    echo "native package smoke requires a supported native host architecture, got $(uname -m)" >&2
    exit 2
    ;;
esac
case "${PACKAGE_ARCH}" in
  amd64) rpm_arch=x86_64 ;;
  arm64) rpm_arch=aarch64 ;;
esac
if ! command -v dpkg-deb >/dev/null 2>&1 || ! command -v docker >/dev/null 2>&1; then
  echo "native package smoke requires dpkg-deb and docker on the host" >&2
  exit 2
fi
# This smoke drives real host installation state; refuse to run anywhere that
# already carries an Agent installation so cleanup can stay scoped.
if sudo test -e /usr/libexec/ocservia || sudo test -e /etc/ocservia-agent || \
  getent passwd ocserv-agent >/dev/null 2>&1 || \
  sudo dpkg-query -W -f='${Status}' ocservia-agent 2>/dev/null | grep -q "install ok installed"; then
  echo "host already carries an ocservia Agent installation; refusing to run" >&2
  exit 2
fi

work="${RUNNER_TEMP:-/tmp}/ocservia-native-package-${RUN_ID}"
pkg_dir="${work}/packages"
container="ocservia-rpm-${RUN_ID:0:60}"
container_image="ocservia-rpm-smoke-${RUN_ID:0:50}"
mkdir -p "${pkg_dir}" "${ARTIFACT_DIR}"
chmod 0700 "${work}"

cleanup() {
  local status=$?
  {
    sudo apt-get remove -y ocservia-agent ||
      sudo dpkg --remove --force-remove ocservia-agent || true
  } >"${ARTIFACT_DIR}/cleanup-apt-remove.log" 2>&1
  sudo rm -rf -- /etc/ocservia-agent /var/lib/ocservia-agent /var/lib/ocservia-upgrade \
    /var/lib/ocservia-privd /usr/share/ocservia-agent /usr/libexec/ocservia \
    /usr/lib/systemd/system/ocservia-agent.service /usr/lib/systemd/system/ocservia-privd.service \
    /usr/lib/systemd/system/ocservia-agent.service.d || status=1
  sudo userdel ocserv-agent >/dev/null 2>&1 || true
  sudo groupdel ocserv-agent >/dev/null 2>&1 || true
  sudo systemctl daemon-reload >/dev/null 2>&1 || true
  docker rm -f -- "${container}" >/dev/null 2>&1 || true
  docker rmi -f -- "${container_image}" >/dev/null 2>&1 || true
  rm -rf -- "${work}" || status=1
  exit "${status}"
}
trap cleanup EXIT INT TERM

(cd "${ROOT}/rust" && cargo build --locked --release --package ocservia-agent --package ocservia-privd)
for binary in ocservia-agent ocservia-privd; do
  file_output="$(file -b "${ROOT}/rust/target/release/${binary}")"
  if [[ "${PACKAGE_ARCH}" == amd64 && "${file_output}" != *"x86-64"* ]] ||
    [[ "${PACKAGE_ARCH}" == arm64 && "${file_output}" != *"aarch64"* ]]; then
    echo "built ${binary} is not a native ${PACKAGE_ARCH} ELF binary: ${file_output}" >&2
    exit 1
  fi
done
echo "native ${PACKAGE_ARCH} release build passed"

openssl genpkey -algorithm ED25519 -out "${work}/signing.key" >/dev/null 2>&1
chmod 0600 "${work}/signing.key"
openssl pkey -in "${work}/signing.key" -pubout -out "${work}/trusted.pub.pem" >/dev/null 2>&1
openssl pkey -pubin -in "${work}/trusted.pub.pem" -outform DER -out "${work}/trusted.der"
trusted_fingerprint="$(sha256sum "${work}/trusted.der" | awk '{print $1}')"
controller_endpoint="$(openssl pkey -in "${work}/signing.key" -pubout -outform DER \
  | tail -c 32 | od -An -tx1 | tr -d ' \n')"
openssl genpkey -algorithm ED25519 -out "${work}/controller-command.key" >/dev/null 2>&1
openssl pkey -in "${work}/controller-command.key" -pubout \
  -out "${work}/controller-command.pub.pem" >/dev/null 2>&1
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${work}/user-password-seal-private.pem" >/dev/null 2>&1
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${work}/p12-password-seal-private.pem" >/dev/null 2>&1
user_seal_hash="$(openssl rsa -in "${work}/user-password-seal-private.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
p12_seal_hash="$(openssl rsa -in "${work}/p12-password-seal-private.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"

build_packages() {
  local version="$1"
  OUTPUT_DIR="${pkg_dir}" AGENT_SIGNING_KEY="${work}/signing.key" VERSION="${version}" \
    PACKAGE_ARCH="${PACKAGE_ARCH}" SOURCE_DATE_EPOCH=1786147200 \
    "${ROOT}/scripts/package-agent.sh" >/dev/null
  OUTPUT_DIR="${pkg_dir}" VERSION="${version}" PACKAGE_ARCH="${PACKAGE_ARCH}" \
    SOURCE_DATE_EPOCH=1786147200 AGENT_TRUSTED_KEY_SHA256="${trusted_fingerprint}" \
    "${ROOT}/scripts/package-native-agent.sh" >/dev/null
}
build_packages 1.0.0
build_packages 1.0.1
deb_old="${pkg_dir}/ocservia-agent_1.0.0_${PACKAGE_ARCH}.deb"
deb_new="${pkg_dir}/ocservia-agent_1.0.1_${PACKAGE_ARCH}.deb"
rpm_old="${pkg_dir}/ocservia-agent-1.0.0-1.${rpm_arch}.rpm"
rpm_new="${pkg_dir}/ocservia-agent-1.0.1-1.${rpm_arch}.rpm"

for field in Package Version Architecture; do
  value="$(dpkg-deb -f "${deb_old}" "${field}")"
  # nfpm appends the release component to the deb version (1.0.0-1).
  case "${field}:${value}" in
    Package:ocservia-agent | Version:1.0.0-1 | Architecture:"${PACKAGE_ARCH}") ;;
    *)
      echo "deb metadata field ${field} has unexpected value ${value}" >&2
      exit 1
      ;;
  esac
done
echo "deb metadata validation passed"

agent_binary_sha="$(sha256sum "${ROOT}/rust/target/release/ocservia-agent" | awk '{print $1}')"

assert_installed_state() {
  local context="$1" expected_version="$2" binary_sha
  sudo test -x /usr/libexec/ocservia/ocservia-agent \
    || { echo "${context}: Agent binary missing" >&2; exit 1; }
  sudo test -x /usr/libexec/ocservia/ocservia-privd \
    || { echo "${context}: privd binary missing" >&2; exit 1; }
  sudo test -x /usr/libexec/ocservia/ocservia-agent-rollback \
    || { echo "${context}: rollback command missing" >&2; exit 1; }
  sudo test -f /usr/lib/systemd/system/ocservia-agent.service \
    || { echo "${context}: agent unit missing" >&2; exit 1; }
  sudo test -f /usr/lib/systemd/system/ocservia-privd.service \
    || { echo "${context}: privd unit missing" >&2; exit 1; }
  sudo test -f "/usr/share/ocservia-agent/ocservia-agent-${expected_version}-linux-${PACKAGE_ARCH}.tar.gz" \
    || { echo "${context}: embedded archive missing" >&2; exit 1; }
  getent passwd ocserv-agent >/dev/null \
    || { echo "${context}: ocserv-agent user missing" >&2; exit 1; }
  for unit in ocservia-agent.service ocservia-privd.service; do
    enabled="$(sudo systemctl is-enabled "${unit}" 2>/dev/null || true)"
    [[ "${enabled}" != "enabled" ]] \
      || { echo "${context}: ${unit} was enabled automatically" >&2; exit 1; }
    active="$(sudo systemctl is-active "${unit}" 2>/dev/null || true)"
    [[ "${active}" == "inactive" ]] \
      || { echo "${context}: ${unit} is ${active} without configuration" >&2; exit 1; }
  done
  binary_sha="$(sudo sha256sum /usr/libexec/ocservia/ocservia-agent | awk '{print $1}')"
  [[ "${binary_sha}" == "${agent_binary_sha}" ]] \
    || { echo "${context}: installed Agent binary does not match the built binary" >&2; exit 1; }
}

{ sudo dpkg -i "${deb_old}"; } >"${ARTIFACT_DIR}/deb-install.log" 2>&1
assert_installed_state "deb install" 1.0.0
echo "deb install lifecycle passed"

provision_upgrade_fixtures() {
  cat >"${work}/upgrade-agent.env" <<EOF
CONTROLLER_ENDPOINT_ID=${controller_endpoint}
NODE_ID=00000000-0000-7000-8000-000000000001
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem
USER_PASSWORD_SEAL_KEY_ID=user-password-v1
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}
P12_PASSWORD_SEAL_KEY_ID=p12-password-v1
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${p12_seal_hash}
EOF
  sudo install -o root -g ocserv-agent -m 0640 "${work}/upgrade-agent.env" \
    /etc/ocservia-agent/agent.env
  sudo install -o root -g ocserv-agent -m 0640 "${work}/controller-command.pub.pem" \
    /etc/ocservia-agent/controller-command-verification-key.pem
  sudo install -o root -g root -m 0600 "${work}/user-password-seal-private.pem" \
    /etc/ocservia-agent/user-password-seal-private.pem
  sudo install -o root -g root -m 0600 "${work}/p12-password-seal-private.pem" \
    /etc/ocservia-agent/p12-password-seal-private.pem
}

assert_upgraded_state() {
  local context="$1" expected_version="$2"
  assert_installed_state "${context}" "${expected_version}"
  sudo test -f /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256 \
    || { echo "${context}: upgrade rollback snapshot manifest missing" >&2; exit 1; }
  sudo test "$(sudo awk 'END { print NR }' /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256)" -eq 5 \
    || { echo "${context}: upgrade rollback snapshot is incomplete" >&2; exit 1; }
  sudo grep -Fxq "USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}" /etc/ocservia-agent/agent.env \
    || { echo "${context}: upgrade lost the configured agent environment" >&2; exit 1; }
}

provision_upgrade_fixtures
{ sudo dpkg -i "${deb_new}"; } >"${ARTIFACT_DIR}/deb-upgrade.log" 2>&1
assert_upgraded_state "deb upgrade" 1.0.1
echo "deb upgrade lifecycle passed"

{ sudo apt-get remove -y ocservia-agent; } >"${ARTIFACT_DIR}/deb-remove.log" 2>&1
sudo test ! -e /usr/libexec/ocservia/ocservia-agent \
  || { echo "deb remove retained the Agent binary" >&2; exit 1; }
sudo test ! -e /usr/share/ocservia-agent \
  || { echo "deb remove retained the package payload" >&2; exit 1; }
sudo test -f /etc/ocservia-agent/agent.env \
  || { echo "deb remove discarded the preserved Agent configuration" >&2; exit 1; }
sudo test -d /var/lib/ocservia-agent \
  || { echo "deb remove discarded the preserved Agent state" >&2; exit 1; }
getent passwd ocserv-agent >/dev/null \
  || { echo "deb remove discarded the ocserv-agent user" >&2; exit 1; }
echo "deb removal state preservation passed"

sudo rm -rf -- /etc/ocservia-agent /var/lib/ocservia-agent /var/lib/ocservia-upgrade \
  /var/lib/ocservia-privd
sudo userdel ocserv-agent
# userdel removes the primary group with the user when login.defs enables
# USERGROUPS_ENAB, so the explicit group cleanup is best effort here.
sudo groupdel ocserv-agent 2>/dev/null || true
sudo systemctl daemon-reload

# The stock rockylinux:9 image ships without systemd; build a one-off image
# that can run scriptlets exactly as a real systemd host would.
docker build --tag "${container_image}" - >"${ARTIFACT_DIR}/rpm-image-build.log" 2>&1 <<'DOCKERFILE'
FROM rockylinux:9
RUN dnf install -y systemd openssl && dnf clean all
DOCKERFILE
docker run --privileged --detach --name "${container}" \
  --volume "${pkg_dir}":/packages:ro "${container_image}" /sbin/init \
  >"${ARTIFACT_DIR}/rpm-container-start.log" 2>&1
for _ in $(seq 1 30); do
  state="$(docker exec "${container}" systemctl is-system-running 2>/dev/null || true)"
  [[ "${state}" == "running" || "${state}" == "degraded" ]] && break
  sleep 2
done
[[ "${state}" == "running" || "${state}" == "degraded" ]] \
  || { echo "rpm smoke container systemd never became ready (state: ${state})" >&2; exit 1; }

rpm_arch_actual="$(docker exec "${container}" rpm -qp --qf '%{ARCH}' "/packages/$(basename "${rpm_old}")")"
[[ "${rpm_arch_actual}" == "${rpm_arch}" ]] \
  || { echo "rpm package architecture is ${rpm_arch_actual}, expected ${rpm_arch}" >&2; exit 1; }
docker exec "${container}" rpm -ivh "/packages/$(basename "${rpm_old}")" \
  >"${ARTIFACT_DIR}/rpm-install.log" 2>&1

container_assert_installed() {
  local context="$1" expected_version="$2" binary_sha
  docker exec "${container}" test -x /usr/libexec/ocservia/ocservia-agent \
    || { echo "${context}: Agent binary missing" >&2; exit 1; }
  docker exec "${container}" test -x /usr/libexec/ocservia/ocservia-privd \
    || { echo "${context}: privd binary missing" >&2; exit 1; }
  docker exec "${container}" test -x /usr/libexec/ocservia/ocservia-agent-rollback \
    || { echo "${context}: rollback command missing" >&2; exit 1; }
  docker exec "${container}" test -f /usr/lib/systemd/system/ocservia-agent.service \
    || { echo "${context}: agent unit missing" >&2; exit 1; }
  docker exec "${container}" test -f /usr/lib/systemd/system/ocservia-privd.service \
    || { echo "${context}: privd unit missing" >&2; exit 1; }
  docker exec "${container}" test -f \
    "/usr/share/ocservia-agent/ocservia-agent-${expected_version}-linux-${PACKAGE_ARCH}.tar.gz" \
    || { echo "${context}: embedded archive missing" >&2; exit 1; }
  docker exec "${container}" getent passwd ocserv-agent >/dev/null \
    || { echo "${context}: ocserv-agent user missing" >&2; exit 1; }
  for unit in ocservia-agent.service ocservia-privd.service; do
    enabled="$(docker exec "${container}" systemctl is-enabled "${unit}" 2>/dev/null || true)"
    [[ "${enabled}" != "enabled" ]] \
      || { echo "${context}: ${unit} was enabled automatically" >&2; exit 1; }
    active="$(docker exec "${container}" systemctl is-active "${unit}" 2>/dev/null || true)"
    [[ "${active}" == "inactive" ]] \
      || { echo "${context}: ${unit} is ${active} without configuration" >&2; exit 1; }
  done
  binary_sha="$(docker exec "${container}" sha256sum /usr/libexec/ocservia/ocservia-agent | awk '{print $1}')"
  [[ "${binary_sha}" == "${agent_binary_sha}" ]] \
    || { echo "${context}: installed Agent binary does not match the built binary" >&2; exit 1; }
}
container_assert_installed "rpm install" 1.0.0
echo "rpm install lifecycle passed"

container_provision_upgrade_fixtures() {
  docker cp -- "${work}/controller-command.pub.pem" "${container}:/tmp/controller-command.pub.pem"
  docker cp -- "${work}/user-password-seal-private.pem" "${container}:/tmp/user-password-seal-private.pem"
  docker cp -- "${work}/p12-password-seal-private.pem" "${container}:/tmp/p12-password-seal-private.pem"
  docker exec -i "${container}" bash -s <<EOF
set -euo pipefail
install -o root -g ocserv-agent -m 0640 /tmp/controller-command.pub.pem \
  /etc/ocservia-agent/controller-command-verification-key.pem
install -o root -g root -m 0600 /tmp/user-password-seal-private.pem \
  /etc/ocservia-agent/user-password-seal-private.pem
install -o root -g root -m 0600 /tmp/p12-password-seal-private.pem \
  /etc/ocservia-agent/p12-password-seal-private.pem
cat >/tmp/upgrade-agent.env <<'ENVEOF'
CONTROLLER_ENDPOINT_ID=${controller_endpoint}
NODE_ID=00000000-0000-7000-8000-000000000001
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem
USER_PASSWORD_SEAL_KEY_ID=user-password-v1
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}
P12_PASSWORD_SEAL_KEY_ID=p12-password-v1
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${p12_seal_hash}
ENVEOF
install -o root -g ocserv-agent -m 0640 /tmp/upgrade-agent.env /etc/ocservia-agent/agent.env
rm -f /tmp/controller-command.pub.pem /tmp/user-password-seal-private.pem \
  /tmp/p12-password-seal-private.pem /tmp/upgrade-agent.env
EOF
}

container_provision_upgrade_fixtures
docker exec "${container}" rpm -Uvh "/packages/$(basename "${rpm_new}")" \
  >"${ARTIFACT_DIR}/rpm-upgrade.log" 2>&1
container_assert_installed "rpm upgrade" 1.0.1
docker exec "${container}" test -f /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256 \
  || { echo "rpm upgrade rollback snapshot manifest missing" >&2; exit 1; }
docker exec "${container}" bash -c \
  'test "$(awk '\''END { print NR }'\'' /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256)" -eq 5' \
  || { echo "rpm upgrade rollback snapshot is incomplete" >&2; exit 1; }
docker exec "${container}" grep -Fxq "USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}" \
  /etc/ocservia-agent/agent.env \
  || { echo "rpm upgrade lost the configured agent environment" >&2; exit 1; }
echo "rpm upgrade lifecycle passed"

docker exec "${container}" rpm -e ocservia-agent >"${ARTIFACT_DIR}/rpm-remove.log" 2>&1
docker exec "${container}" test ! -e /usr/libexec/ocservia/ocservia-agent \
  || { echo "rpm erase retained the Agent binary" >&2; exit 1; }
docker exec "${container}" test ! -e /usr/share/ocservia-agent \
  || { echo "rpm erase retained the package payload" >&2; exit 1; }
docker exec "${container}" test -f /etc/ocservia-agent/agent.env \
  || { echo "rpm erase discarded the preserved Agent configuration" >&2; exit 1; }
docker exec "${container}" test -d /var/lib/ocservia-agent \
  || { echo "rpm erase discarded the preserved Agent state" >&2; exit 1; }
docker exec "${container}" getent passwd ocserv-agent >/dev/null \
  || { echo "rpm erase discarded the ocserv-agent user" >&2; exit 1; }
echo "rpm removal state preservation passed"

docker rm -f -- "${container}" >/dev/null
printf 'arch=%s\nelf_check=pass\ndeb_metadata=pass\ndeb_install=pass\ndeb_upgrade=pass\ndeb_remove_preserves_state=pass\nrpm_metadata=pass\nrpm_install=pass\nrpm_upgrade=pass\nrpm_erase_preserves_state=pass\n' \
  "${PACKAGE_ARCH}" >"${ARTIFACT_DIR}/native-package-summary.txt"
