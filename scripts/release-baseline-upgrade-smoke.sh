#!/usr/bin/env bash
# Published-baseline upgrade smoke: install the real published baseline
# release (default v0.1.1) from GitHub Release assets, then upgrade it to the
# locally built candidate package with the native package manager. This is
# the deterministic v0.1.1 -> candidate hop a pre-v2 node uses to reach the
# first Controller-upgrade-capable baseline; it proves the cross-version
# package lifecycle (state preservation, rollback snapshot, no automatic
# enable) rather than a fabricated old version. Baselines recorded with
# production-relay capability additionally prove that a production managed
# node established the way that baseline's own payload documents (its
# verified install-agent.sh with INSTALL_PRODUCTION_RELAYS=true, then
# operator relays.env, relay token, and identity state) upgrades into the
# candidate with the relay drop-in, operator relay configuration, and
# identity intact; baselines recorded with published RPMs repeat the same
# upgrade through the real RPM lifecycle inside a systemd Rocky Linux
# container. Run after scripts/release-native-package-smoke.sh has left the
# host clean.
set -Eeuo pipefail

report_error_line() {
  local status=$?
  printf 'baseline upgrade smoke failed at line %s\n' "${BASH_LINENO[0]:-unknown}" >&2
  return "${status}"
}
trap report_error_line ERR

RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
VERSION="${VERSION:?candidate VERSION is required (plain SemVer)}"
CANDIDATE_DEB="${CANDIDATE_DEB:?CANDIDATE_DEB path is required}"
BASELINE_RELEASE="${BASELINE_RELEASE:-v0.1.1}"
if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || [[ "${BASELINE_RELEASE}" != v[0-9]* ]]; then
  echo "VERSION must be plain SemVer and BASELINE_RELEASE a v-prefixed tag" >&2
  exit 2
fi
BASELINE_VERSION="${BASELINE_RELEASE#v}"
# The baseline GitHub release is not immutable, so the tag alone does not pin
# the historical artifacts. This table fixes the exact SHA256SUMS bytes each
# smoked baseline release must match, and a release without a pin here
# refuses to run; the signature and package-digest checks below extend the
# pinned identity to the installed package. Each pinned baseline also records
# whether its package layout predates the upgrader binary/unit and the
# read-only --version query, whether its payload could establish production
# relay state, and whether it published RPM assets.
case "${BASELINE_RELEASE}" in
  v0.1.1)
    baseline_sums_sha256=518a4e6e0393dfc5378d117069c7affdeeb26d7dea84521e128c40256d11a1d9
    baseline_has_upgrader=no
    baseline_has_version_query=no
    baseline_has_production_relays=no
    baseline_has_rpm=no
    ;;
  v0.3.0)
    baseline_sums_sha256=018c7d7f1c4f6b5f5745c7d6fa076a6f51a53b1c1050eb26729dce3606394ed0
    baseline_has_upgrader=yes
    baseline_has_version_query=yes
    baseline_has_production_relays=yes
    baseline_has_rpm=yes
    ;;
  v0.4.0)
    baseline_sums_sha256=52c6294d4e999864f087e00cd17d21df04542749f461a5ff0bce401a0d9b73cc
    baseline_has_upgrader=yes
    baseline_has_version_query=yes
    baseline_has_production_relays=yes
    baseline_has_rpm=yes
    ;;
  *) echo "no pinned SHA256SUMS identity for baseline release ${BASELINE_RELEASE}" >&2; exit 2 ;;
esac
case "${BASELINE_RELEASE#v}" in
  *.*.*) ;;
  *) echo "baseline release must carry a plain SemVer version" >&2; exit 2 ;;
esac
if [[ ! -f "${CANDIDATE_DEB}" ]]; then
  echo "candidate deb not found: ${CANDIDATE_DEB}" >&2
  exit 2
fi
if [[ "${RUN_ID}" == *[^a-zA-Z0-9._-]* ]]; then
  echo "RUN_ID contains unsafe characters" >&2
  exit 2
fi
case "$(uname -m)" in
  x86_64) PACKAGE_ARCH=amd64 ;;
  aarch64) PACKAGE_ARCH=arm64 ;;
  *)
    echo "baseline upgrade smoke requires a supported native host architecture, got $(uname -m)" >&2
    exit 2
    ;;
esac
case "${PACKAGE_ARCH}" in
  amd64) rpm_arch=x86_64 ;;
  arm64) rpm_arch=aarch64 ;;
esac
if ! command -v dpkg-deb >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
  echo "baseline upgrade smoke requires dpkg-deb and curl on the host" >&2
  exit 2
fi
if [[ "${baseline_has_rpm}" == yes ]]; then
  CANDIDATE_RPM="${CANDIDATE_RPM:?CANDIDATE_RPM path is required for baselines with published RPMs}"
  if [[ ! -f "${CANDIDATE_RPM}" ]]; then
    echo "candidate rpm not found: ${CANDIDATE_RPM}" >&2
    exit 2
  fi
  if ! command -v docker >/dev/null 2>&1; then
    echo "baseline upgrade smoke requires docker on the host for the RPM lifecycle" >&2
    exit 2
  fi
fi
# This smoke drives real host installation state; refuse to run anywhere that
# already carries an Agent installation so cleanup can stay scoped.
if sudo test -e /usr/libexec/ocservia || sudo test -e /etc/ocservia-agent || \
  getent passwd ocserv-agent >/dev/null 2>&1 || \
  sudo dpkg-query -W -f='${Status}' ocservia-agent 2>/dev/null | grep -q "install ok installed"; then
  echo "host already carries an ocservia Agent installation; refusing to run" >&2
  exit 2
fi

work="${RUNNER_TEMP:-/tmp}/ocservia-baseline-upgrade-${RUN_ID}"
download_dir="${work}/baseline"
pkg_dir="${work}/packages"
container="ocservia-baseline-rpm-${RUN_ID:0:52}"
container_image="ocservia-baseline-rpm-img-${RUN_ID:0:47}"
mkdir -p "${download_dir}" "${pkg_dir}" "${ARTIFACT_DIR}"
chmod 0700 "${work}"

cleanup() {
  local status=$?
  {
    sudo apt-get remove -y ocservia-agent ||
      sudo dpkg --remove --force-remove ocservia-agent || true
  } >"${ARTIFACT_DIR}/baseline-cleanup-apt-remove.log" 2>&1
  sudo rm -rf -- /etc/ocservia-agent /var/lib/ocservia-agent /var/lib/ocservia-upgrade \
    /var/lib/ocservia-privd /usr/share/ocservia-agent /usr/libexec/ocservia \
    /usr/lib/systemd/system/ocservia-agent.service /usr/lib/systemd/system/ocservia-privd.service \
    /usr/lib/systemd/system/ocservia-upgrader@.service \
    /usr/lib/systemd/system/ocservia-agent.service.d || status=1
  sudo rm -f -- /etc/ocservia/agent-install-production-relays || status=1
  sudo userdel ocserv-agent >/dev/null 2>&1 || true
  sudo groupdel ocserv-agent >/dev/null 2>&1 || true
  sudo systemctl daemon-reload >/dev/null 2>&1 || true
  if [[ "${baseline_has_rpm}" == yes ]]; then
    docker rm -f -- "${container}" >/dev/null 2>&1 || true
    docker rmi -f -- "${container_image}" >/dev/null 2>&1 || true
  fi
  rm -rf -- "${work}" || status=1
  exit "${status}"
}
trap cleanup EXIT INT TERM

# The pinned SHA256SUMS digest carries the historical identity: the tag
# locates the assets, the pin fixes their bytes. Verify it before trusting
# any other downloaded asset, then verify the manifest signature and the
# baseline package digests; this validates release-set integrity, not the
# production signing key (that is provisioned out of band on real hosts).
base_url="https://github.com/GentleKingson/ocservia/releases/download/${BASELINE_RELEASE}"
baseline_deb="ocservia-agent_${BASELINE_VERSION}_${PACKAGE_ARCH}.deb"
baseline_rpm="ocservia-agent-${BASELINE_VERSION}-1.${rpm_arch}.rpm"
baseline_assets=("${baseline_deb}" SHA256SUMS SHA256SUMS.sig release-signing.pub.pem)
if [[ "${baseline_has_rpm}" == yes ]]; then
  baseline_assets=("${baseline_rpm}" "${baseline_assets[@]}")
fi
for asset in "${baseline_assets[@]}"; do
  curl --fail --silent --show-error --location \
    -o "${download_dir}/${asset}" "${base_url}/${asset}"
done
printf '%s  %s\n' "${baseline_sums_sha256}" "${download_dir}/SHA256SUMS" \
  | sha256sum -c --strict -
openssl pkeyutl -verify -rawin -pubin -inkey "${download_dir}/release-signing.pub.pem" \
  -in "${download_dir}/SHA256SUMS" -sigfile "${download_dir}/SHA256SUMS.sig" \
  >"${ARTIFACT_DIR}/baseline-manifest-signature.log" 2>&1 \
  || { echo "published SHA256SUMS signature does not verify against the published key" >&2; exit 1; }
(cd "${download_dir}" && grep -F " ${baseline_deb}" SHA256SUMS | sha256sum -c --strict -)
if [[ "${baseline_has_rpm}" == yes ]]; then
  (cd "${download_dir}" && grep -F " ${baseline_rpm}" SHA256SUMS | sha256sum -c --strict -)
fi

# The candidate upgrade preflight requires the shared command verification
# key plus two distinct root-owned sealing keys; provision the same fixtures
# the current-release smoke uses. The material is generated once and written
# per cycle so both DEB cycles and the RPM container share one identity.
openssl genpkey -algorithm ED25519 -out "${work}/controller-command.key" >/dev/null 2>&1
openssl pkey -in "${work}/controller-command.key" -pubout \
  -out "${work}/controller-command.pub.pem" >/dev/null 2>&1
controller_endpoint="$(openssl pkey -in "${work}/controller-command.key" -pubout -outform DER \
  | tail -c 32 | od -An -tx1 | tr -d ' \n')"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${work}/user-password-seal-private.pem" >/dev/null 2>&1
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
  -out "${work}/p12-password-seal-private.pem" >/dev/null 2>&1
user_seal_hash="$(openssl rsa -in "${work}/user-password-seal-private.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
p12_seal_hash="$(openssl rsa -in "${work}/p12-password-seal-private.pem" -pubout -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
cat >"${work}/upgrade-agent.env" <<EOF
CONTROLLER_ENDPOINT_ID=${controller_endpoint}
NODE_ID=00000000-0000-7000-8000-000000000001
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem
USER_PASSWORD_SEAL_KEY_ID=user-password-v1
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}
P12_PASSWORD_SEAL_KEY_ID=p12-password-v1
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${p12_seal_hash}
EOF
# Operator-owned production relay configuration and the relay access token
# the baseline documentation provisions out of band.
cat >"${work}/operator-relays.env" <<'EOF'
RELAY_URL_A=https://relay-one.example.net
RELAY_URL_B=https://relay-two.example.net
EOF
printf 'baseline-smoke-relay-access-token' >"${work}/relay-access-token"

provision_upgrade_fixtures() {
  sudo install -o root -g ocserv-agent -m 0640 "${work}/upgrade-agent.env" \
    /etc/ocservia-agent/agent.env
  sudo install -o root -g ocserv-agent -m 0640 "${work}/controller-command.pub.pem" \
    /etc/ocservia-agent/controller-command-verification-key.pem
  sudo install -o root -g root -m 0600 "${work}/user-password-seal-private.pem" \
    /etc/ocservia-agent/user-password-seal-private.pem
  sudo install -o root -g root -m 0600 "${work}/p12-password-seal-private.pem" \
    /etc/ocservia-agent/p12-password-seal-private.pem
}

host_reset_cycle() {
  sudo apt-get remove -y ocservia-agent >/dev/null 2>&1 \
    || sudo dpkg --remove --force-remove ocservia-agent >/dev/null 2>&1 || true
  sudo rm -rf -- /etc/ocservia-agent /etc/ocservia /var/lib/ocservia-agent \
    /var/lib/ocservia-upgrade /var/lib/ocservia-privd /usr/share/ocservia-agent \
    /usr/libexec/ocservia /usr/lib/systemd/system/ocservia-agent.service \
    /usr/lib/systemd/system/ocservia-privd.service \
    /usr/lib/systemd/system/ocservia-upgrader@.service \
    /usr/lib/systemd/system/ocservia-agent.service.d
  sudo userdel ocserv-agent >/dev/null 2>&1 || true
  sudo groupdel ocserv-agent >/dev/null 2>&1 || true
  sudo systemctl daemon-reload >/dev/null 2>&1 || true
}

assert_state() {
  local context="$1" expected_version="$2" want_upgrader="$3" want_version_query="$4"
  local version_output
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
  if [[ "${want_upgrader}" == "yes" ]]; then
    sudo test -x /usr/libexec/ocservia/ocservia-upgrader \
      || { echo "${context}: upgrader binary missing" >&2; exit 1; }
    sudo test -f /usr/lib/systemd/system/ocservia-upgrader@.service \
      || { echo "${context}: upgrader unit missing" >&2; exit 1; }
  fi
  for unit in ocservia-agent.service ocservia-privd.service; do
    enabled="$(sudo systemctl is-enabled "${unit}" 2>/dev/null || true)"
    [[ "${enabled}" != "enabled" ]] \
      || { echo "${context}: ${unit} was enabled automatically" >&2; exit 1; }
    active="$(sudo systemctl is-active "${unit}" 2>/dev/null || true)"
    [[ "${active}" == "inactive" ]] \
      || { echo "${context}: ${unit} is ${active} without configuration" >&2; exit 1; }
  done
  # The candidate binary answers the read-only version query as the service
  # user. A baseline that predates the --version argument keeps its version
  # identity pinned by the embedded archive check above instead.
  if [[ "${want_version_query}" == "yes" ]]; then
    version_output="$(sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent --version)"
    [[ "${version_output}" == "ocservia-agent ${expected_version}" ]] \
      || { echo "${context}: observed version is '${version_output}', expected ${expected_version}" >&2; exit 1; }
  fi
}

assert_production_relays() {
  local context="$1"
  sudo test -f /usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf \
    || { echo "${context}: production relay drop-in missing" >&2; exit 1; }
  sudo test -f /etc/ocservia-agent/relays.env \
    || { echo "${context}: relays.env missing" >&2; exit 1; }
}

# Reproduce the baseline release's documented production setup on an already
# baseline-installed host: verify the baseline package's own embedded archive
# with its own embedded trust material, then run its install-agent.sh with
# INSTALL_PRODUCTION_RELAYS=true, exactly as that release's operator
# documentation prescribes for a production managed node.
establish_baseline_production_relays() {
  local log_file="$1"
  # shellcheck disable=SC2024  # the redirect deliberately stays unprivileged:
  # only the inner lifecycle runs as root, the log keeps runner ownership.
  sudo bash -c '
    set -euo pipefail
    package_root=/usr/share/ocservia-agent
    archive="${package_root}/ocservia-agent-'"${BASELINE_VERSION}"'-linux-'"${PACKAGE_ARCH}"'.tar.gz"
    fingerprint="$(cat -- "${package_root}/trusted-release-key.sha256")"
    verified_root="$(AGENT_TRUSTED_KEY_SHA256="${fingerprint}" \
      "${package_root}/verify-agent-package.sh" \
      "${archive}" "${archive}.sha256" "${archive}.sha256.sig" \
      "${package_root}/release-signing.pub.pem")"
    INSTALL_PRODUCTION_RELAYS=true "${verified_root}/scripts/install-agent.sh"
    rm -rf -- "${verified_root%%/extracted/*}"
  ' >"${log_file}" 2>&1
}

{ sudo dpkg -i "${download_dir}/${baseline_deb}"; } >"${ARTIFACT_DIR}/baseline-install.log" 2>&1
assert_state "baseline install" "${BASELINE_VERSION}" \
  "${baseline_has_upgrader}" "${baseline_has_version_query}"
echo "published baseline ${BASELINE_RELEASE} install passed"

provision_upgrade_fixtures
{ sudo dpkg -i "${CANDIDATE_DEB}"; } >"${ARTIFACT_DIR}/baseline-candidate-upgrade.log" 2>&1
assert_state "candidate upgrade" "${VERSION}" yes yes
sudo test -f /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256 \
  || { echo "candidate upgrade: rollback snapshot manifest missing" >&2; exit 1; }
sudo test -s /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256 \
  || { echo "candidate upgrade: rollback snapshot manifest is empty" >&2; exit 1; }
sudo grep -Fxq "USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}" /etc/ocservia-agent/agent.env \
  || { echo "candidate upgrade lost the configured agent environment" >&2; exit 1; }
echo "published baseline ${BASELINE_RELEASE} -> candidate ${VERSION} upgrade passed"
host_reset_cycle

if [[ "${baseline_has_production_relays}" == yes ]]; then
  { sudo dpkg -i "${download_dir}/${baseline_deb}"; } \
    >"${ARTIFACT_DIR}/baseline-production-install.log" 2>&1
  assert_state "baseline production install" "${BASELINE_VERSION}" \
    "${baseline_has_upgrader}" "${baseline_has_version_query}"
  establish_baseline_production_relays "${ARTIFACT_DIR}/baseline-production-relays-setup.log"
  assert_production_relays "baseline production setup"
  sudo grep -Fxq 'RELAY_URL_A=https://relay-a.example.com' /etc/ocservia-agent/relays.env \
    || { echo "baseline production setup did not install the relays.env example" >&2; exit 1; }
  # Operator-owned production state on top of the baseline-installed example.
  sudo touch /var/lib/ocservia-agent/identity/identity-sentinel
  sudo install -o root -g ocserv-agent -m 0640 "${work}/operator-relays.env" \
    /etc/ocservia-agent/relays.env
  sudo install -o root -g ocserv-agent -m 0640 "${work}/relay-access-token" \
    /etc/ocservia-agent/relay-access-token
  provision_upgrade_fixtures
  { sudo dpkg -i "${CANDIDATE_DEB}"; } \
    >"${ARTIFACT_DIR}/baseline-production-candidate-upgrade.log" 2>&1
  assert_state "candidate production upgrade" "${VERSION}" yes yes
  assert_production_relays "candidate production upgrade"
  sudo cmp -s "${work}/operator-relays.env" /etc/ocservia-agent/relays.env \
    || { echo "candidate production upgrade replaced the operator relays.env" >&2; exit 1; }
  sudo grep -Fxq 'baseline-smoke-relay-access-token' /etc/ocservia-agent/relay-access-token \
    || { echo "candidate production upgrade lost the operator relay access token" >&2; exit 1; }
  sudo test "$(sudo stat -c '%U:%G' /etc/ocservia-agent/relay-access-token)" == "root:ocserv-agent" \
    || { echo "candidate production upgrade changed the relay token ownership" >&2; exit 1; }
  sudo test -f /var/lib/ocservia-agent/identity/identity-sentinel \
    || { echo "candidate production upgrade discarded the preserved identity state" >&2; exit 1; }
  sudo test -f /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256 \
    || { echo "candidate production upgrade: rollback snapshot manifest missing" >&2; exit 1; }
  sudo test -f /var/lib/ocservia-upgrade/upgrade-backup/ocservia-agent-relays.conf.previous \
    || { echo "candidate production upgrade did not snapshot the relay drop-in" >&2; exit 1; }
  sudo grep -Fxq "USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}" /etc/ocservia-agent/agent.env \
    || { echo "candidate production upgrade lost the configured agent environment" >&2; exit 1; }
  # A baseline production node never carried the one-shot production request
  # marker; the upgrade must follow the drop-in presence path without
  # creating one.
  sudo test ! -e /etc/ocservia/agent-install-production-relays \
    || { echo "candidate production upgrade created a stale production request marker" >&2; exit 1; }
  echo "published baseline ${BASELINE_RELEASE} production node -> candidate ${VERSION} upgrade passed"
  host_reset_cycle
fi

if [[ "${baseline_has_rpm}" == yes ]]; then
  candidate_rpm_name="$(basename "${CANDIDATE_RPM}")"
  install -m 0644 -- "${CANDIDATE_RPM}" "${pkg_dir}/${candidate_rpm_name}"
  install -m 0644 -- "${download_dir}/${baseline_rpm}" "${pkg_dir}/${baseline_rpm}"
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
    || { echo "rpm baseline container systemd never became ready (state: ${state})" >&2; exit 1; }

  container_assert_installed() {
    local context="$1" expected_version="$2" want_upgrader="$3"
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
    if [[ "${want_upgrader}" == yes ]]; then
      docker exec "${container}" test -x /usr/libexec/ocservia/ocservia-upgrader \
        || { echo "${context}: upgrader binary missing" >&2; exit 1; }
      docker exec "${container}" test -f /usr/lib/systemd/system/ocservia-upgrader@.service \
        || { echo "${context}: upgrader unit missing" >&2; exit 1; }
    fi
    for unit in ocservia-agent.service ocservia-privd.service; do
      enabled="$(docker exec "${container}" systemctl is-enabled "${unit}" 2>/dev/null || true)"
      [[ "${enabled}" != "enabled" ]] \
        || { echo "${context}: ${unit} was enabled automatically" >&2; exit 1; }
      active="$(docker exec "${container}" systemctl is-active "${unit}" 2>/dev/null || true)"
      [[ "${active}" == "inactive" ]] \
        || { echo "${context}: ${unit} is ${active} without configuration" >&2; exit 1; }
    done
  }

  container_provision_upgrade_fixtures() {
    docker cp -- "${work}/controller-command.pub.pem" "${container}:/tmp/controller-command.pub.pem"
    docker cp -- "${work}/user-password-seal-private.pem" "${container}:/tmp/user-password-seal-private.pem"
    docker cp -- "${work}/p12-password-seal-private.pem" "${container}:/tmp/p12-password-seal-private.pem"
    docker cp -- "${work}/upgrade-agent.env" "${container}:/tmp/upgrade-agent.env"
    docker exec "${container}" bash -c '
      set -euo pipefail
      install -o root -g ocserv-agent -m 0640 /tmp/controller-command.pub.pem \
        /etc/ocservia-agent/controller-command-verification-key.pem
      install -o root -g root -m 0600 /tmp/user-password-seal-private.pem \
        /etc/ocservia-agent/user-password-seal-private.pem
      install -o root -g root -m 0600 /tmp/p12-password-seal-private.pem \
        /etc/ocservia-agent/p12-password-seal-private.pem
      install -o root -g ocserv-agent -m 0640 /tmp/upgrade-agent.env \
        /etc/ocservia-agent/agent.env
      rm -f /tmp/controller-command.pub.pem /tmp/user-password-seal-private.pem \
        /tmp/p12-password-seal-private.pem /tmp/upgrade-agent.env
    '
  }

  docker exec "${container}" rpm -ivh "/packages/${baseline_rpm}" \
    >"${ARTIFACT_DIR}/rpm-baseline-install.log" 2>&1
  container_assert_installed "rpm baseline install" "${BASELINE_VERSION}" "${baseline_has_upgrader}"
  # Same documented production setup as the DEB cycle, executed with the
  # baseline package's own embedded payload inside the container.
  docker exec "${container}" bash -c '
    set -euo pipefail
    package_root=/usr/share/ocservia-agent
    archive="${package_root}/ocservia-agent-'"${BASELINE_VERSION}"'-linux-'"${PACKAGE_ARCH}"'.tar.gz"
    fingerprint="$(cat -- "${package_root}/trusted-release-key.sha256")"
    verified_root="$(AGENT_TRUSTED_KEY_SHA256="${fingerprint}" \
      "${package_root}/verify-agent-package.sh" \
      "${archive}" "${archive}.sha256" "${archive}.sha256.sig" \
      "${package_root}/release-signing.pub.pem")"
    INSTALL_PRODUCTION_RELAYS=true "${verified_root}/scripts/install-agent.sh"
    rm -rf -- "${verified_root%%/extracted/*}"
  ' >"${ARTIFACT_DIR}/rpm-baseline-production-setup.log" 2>&1
  docker exec "${container}" test -f \
    /usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf \
    || { echo "rpm baseline production setup: relay drop-in missing" >&2; exit 1; }
  docker cp -- "${work}/operator-relays.env" "${container}:/tmp/operator-relays.env"
  docker cp -- "${work}/relay-access-token" "${container}:/tmp/relay-access-token"
  docker exec "${container}" bash -c '
    set -euo pipefail
    touch /var/lib/ocservia-agent/identity/identity-sentinel
    install -o root -g ocserv-agent -m 0640 /tmp/operator-relays.env \
      /etc/ocservia-agent/relays.env
    install -o root -g ocserv-agent -m 0640 /tmp/relay-access-token \
      /etc/ocservia-agent/relay-access-token
    rm -f /tmp/operator-relays.env /tmp/relay-access-token
  '
  container_provision_upgrade_fixtures

  docker exec "${container}" rpm -Uvh "/packages/${candidate_rpm_name}" \
    >"${ARTIFACT_DIR}/rpm-candidate-upgrade.log" 2>&1
  container_assert_installed "rpm candidate upgrade" "${VERSION}" yes
  docker exec "${container}" test -f \
    /usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf \
    || { echo "rpm candidate upgrade: production relay drop-in missing" >&2; exit 1; }
  docker exec "${container}" grep -Fxq 'RELAY_URL_A=https://relay-one.example.net' \
    /etc/ocservia-agent/relays.env \
    || { echo "rpm candidate upgrade replaced the operator relays.env" >&2; exit 1; }
  docker exec "${container}" grep -Fxq 'baseline-smoke-relay-access-token' \
    /etc/ocservia-agent/relay-access-token \
    || { echo "rpm candidate upgrade lost the operator relay access token" >&2; exit 1; }
  docker exec "${container}" test -f /var/lib/ocservia-agent/identity/identity-sentinel \
    || { echo "rpm candidate upgrade discarded the preserved identity state" >&2; exit 1; }
  docker exec "${container}" test -f /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256 \
    || { echo "rpm candidate upgrade: rollback snapshot manifest missing" >&2; exit 1; }
  docker exec "${container}" test -f \
    /var/lib/ocservia-upgrade/upgrade-backup/ocservia-agent-relays.conf.previous \
    || { echo "rpm candidate upgrade did not snapshot the relay drop-in" >&2; exit 1; }
  docker exec "${container}" grep -Fxq \
    "USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}" /etc/ocservia-agent/agent.env \
    || { echo "rpm candidate upgrade lost the configured agent environment" >&2; exit 1; }
  docker exec "${container}" test ! -e /etc/ocservia/agent-install-production-relays \
    || { echo "rpm candidate upgrade created a stale production request marker" >&2; exit 1; }
  echo "published baseline ${BASELINE_RELEASE} RPM production node -> candidate ${VERSION} upgrade passed"

  docker rm -f -- "${container}" >/dev/null
  docker rmi -f -- "${container_image}" >/dev/null
fi

{
  printf 'baseline_release=%s\ncandidate_version=%s\narch=%s\nbaseline_install=pass\nbaseline_upgrade=pass\nstate_preserved=pass\nno_automatic_enable=pass\n' \
    "${BASELINE_RELEASE}" "${VERSION}" "${PACKAGE_ARCH}"
  if [[ "${baseline_has_production_relays}" == yes ]]; then
    printf 'baseline_production_node_upgrade=pass\n'
  fi
  if [[ "${baseline_has_rpm}" == yes ]]; then
    printf 'rpm_baseline_install=pass\nrpm_production_node_upgrade=pass\n'
  fi
} >"${ARTIFACT_DIR}/baseline-upgrade-summary.txt"
