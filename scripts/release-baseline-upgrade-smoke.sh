#!/usr/bin/env bash
# Published-baseline upgrade smoke: install the real published baseline
# release (default v0.1.1) from GitHub Release assets, then upgrade it to the
# locally built candidate package with the native package manager. This is
# the deterministic v0.1.1 -> candidate hop a pre-v2 node uses to reach the
# first Controller-upgrade-capable baseline; it proves the cross-version
# package lifecycle (state preservation, rollback snapshot, no automatic
# enable) rather than a fabricated old version. Run after
# scripts/release-native-package-smoke.sh has left the host clean.
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
if ! command -v dpkg-deb >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
  echo "baseline upgrade smoke requires dpkg-deb and curl on the host" >&2
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

work="${RUNNER_TEMP:-/tmp}/ocservia-baseline-upgrade-${RUN_ID}"
download_dir="${work}/baseline"
mkdir -p "${download_dir}" "${ARTIFACT_DIR}"
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
  sudo userdel ocserv-agent >/dev/null 2>&1 || true
  sudo groupdel ocserv-agent >/dev/null 2>&1 || true
  sudo systemctl daemon-reload >/dev/null 2>&1 || true
  rm -rf -- "${work}" || status=1
  exit "${status}"
}
trap cleanup EXIT INT TERM

# The baseline identity is pinned by tag. Verify the manifest signature and
# the baseline deb digest against the published release assets before
# installing anything; this validates release-set integrity, not the
# production signing key (that is provisioned out of band on real hosts).
base_url="https://github.com/GentleKingson/ocservia/releases/download/${BASELINE_RELEASE}"
for asset in \
  "ocservia-agent_${BASELINE_VERSION}_${PACKAGE_ARCH}.deb" \
  SHA256SUMS SHA256SUMS.sig release-signing.pub.pem; do
  curl --fail --silent --show-error --location \
    -o "${download_dir}/${asset}" "${base_url}/${asset}"
done
openssl pkeyutl -verify -rawin -pubin -inkey "${download_dir}/release-signing.pub.pem" \
  -in "${download_dir}/SHA256SUMS" -sigfile "${download_dir}/SHA256SUMS.sig" \
  >"${ARTIFACT_DIR}/baseline-manifest-signature.log" 2>&1 \
  || { echo "published SHA256SUMS signature does not verify against the published key" >&2; exit 1; }
baseline_deb="ocservia-agent_${BASELINE_VERSION}_${PACKAGE_ARCH}.deb"
(cd "${download_dir}" && grep -F " ${baseline_deb}" SHA256SUMS | sha256sum -c --strict -)

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
  # user. The baseline release predates the --version argument entirely, so
  # its version identity stays pinned by the embedded archive and dpkg check
  # above.
  if [[ "${want_version_query}" == "yes" ]]; then
    version_output="$(sudo -u ocserv-agent /usr/libexec/ocservia/ocservia-agent --version)"
    [[ "${version_output}" == "ocservia-agent ${expected_version}" ]] \
      || { echo "${context}: observed version is '${version_output}', expected ${expected_version}" >&2; exit 1; }
  fi
}

{ sudo dpkg -i "${download_dir}/${baseline_deb}"; } >"${ARTIFACT_DIR}/baseline-install.log" 2>&1
assert_state "baseline install" "${BASELINE_VERSION}" no no
echo "published baseline ${BASELINE_RELEASE} install passed"

# The candidate upgrade preflight requires the shared command verification
# key plus two distinct root-owned sealing keys; provision the same fixtures
# the current-release smoke uses.
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
sudo install -o root -g ocserv-agent -m 0640 "${work}/upgrade-agent.env" \
  /etc/ocservia-agent/agent.env
sudo install -o root -g ocserv-agent -m 0640 "${work}/controller-command.pub.pem" \
  /etc/ocservia-agent/controller-command-verification-key.pem
sudo install -o root -g root -m 0600 "${work}/user-password-seal-private.pem" \
  /etc/ocservia-agent/user-password-seal-private.pem
sudo install -o root -g root -m 0600 "${work}/p12-password-seal-private.pem" \
  /etc/ocservia-agent/p12-password-seal-private.pem

{ sudo dpkg -i "${CANDIDATE_DEB}"; } >"${ARTIFACT_DIR}/baseline-candidate-upgrade.log" 2>&1
assert_state "candidate upgrade" "${VERSION}" yes yes
sudo test -f /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256 \
  || { echo "candidate upgrade: rollback snapshot manifest missing" >&2; exit 1; }
sudo test -s /var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256 \
  || { echo "candidate upgrade: rollback snapshot manifest is empty" >&2; exit 1; }
sudo grep -Fxq "USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_seal_hash}" /etc/ocservia-agent/agent.env \
  || { echo "candidate upgrade lost the configured agent environment" >&2; exit 1; }
echo "published baseline ${BASELINE_RELEASE} -> candidate ${VERSION} upgrade passed"

printf 'baseline_release=%s\ncandidate_version=%s\narch=%s\nbaseline_install=pass\nbaseline_upgrade=pass\nstate_preserved=pass\nno_automatic_enable=pass\n' \
  "${BASELINE_RELEASE}" "${VERSION}" "${PACKAGE_ARCH}" \
  >"${ARTIFACT_DIR}/baseline-upgrade-summary.txt"
