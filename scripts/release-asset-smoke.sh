#!/usr/bin/env bash
# Published bytes only; run on a disposable native systemd runner.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if (($# != 1)) || [[ ! "$1" =~ ^v[0-9]+[.][0-9]+[.][0-9]+(-rc[.][1-9][0-9]*)?$ || "$1" == v0.0.0 ]]; then
  echo 'usage: release-asset-smoke.sh <published vX.Y.Z or vX.Y.Z-rc.N>' >&2
  exit 2
fi
release_tag="$1"
version="${release_tag#v}"
: "${RUN_ID:?RUN_ID is required}" "${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
[[ "${RUN_ID}" != *[^a-zA-Z0-9._-]* ]] || { echo 'unsafe RUN_ID' >&2; exit 2; }
case "$(uname -m)" in
  x86_64) arch=amd64; rpm_arch=x86_64 ;;
  aarch64) arch=arm64; rpm_arch=aarch64 ;;
  *) echo 'a native amd64 or arm64 runner is required' >&2; exit 2 ;;
esac
mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
work="$(mktemp -d)"
result=FAIL
cleanup() {
  local status=$?
  trap - EXIT
  printf 'RELEASE_ASSET_DOWNLOAD=%s\n' "${result}" >>"${ARTIFACT_DIR}/release-assets-summary.txt"
  rm -rf -- "${work}"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
printf 'release_tag=%s\narch=%s\nharness_commit=%s\n' \
  "${release_tag}" "${arch}" "$(git -C "${ROOT}" rev-parse HEAD)" >"${ARTIFACT_DIR}/release-assets-summary.txt"
: >"${ARTIFACT_DIR}/release-assets-downloads.txt"
: >"${ARTIFACT_DIR}/release-assets-sha256.txt"

download() {
  local url="$1" name="$2"
  printf '%s\n' "${url}" >>"${ARTIFACT_DIR}/release-assets-downloads.txt"
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --tlsv1.2 --connect-timeout 15 --max-time 300 --retry 2 \
    --output "${work}/${name}" "${url}"
  test -s "${work}/${name}"
  (cd "${work}" && sha256sum -- "${name}") >>"${ARTIFACT_DIR}/release-assets-sha256.txt"
}

base="https://github.com/GentleKingson/ocservia/releases/download/${release_tag}"
archive="ocservia-agent-${version}-linux-${arch}.tar.gz"
for name in "${archive}" "${archive}.sha256" \
  "ocservia-agent_${version}-1_${arch}.deb" "ocservia-agent-${version}-1.${rpm_arch}.rpm" \
  managed-node-bootstrap.sh; do
  download "${base}/${name}" "${name}"
done
# Check exactly the requested archive, never arbitrary paths from a checksum file.
checksum="$(cat "${work}/${archive}.sha256")"
digest="${checksum%% *}"
[[ "${digest}" =~ ^[0-9a-f]{64}$ && "${checksum}" == "${digest}  ${archive}" ]]
[[ "$(sha256sum "${work}/${archive}" | awk '{print $1}')" == "${digest}" ]]
printf 'published_archive_checksum=PASS\n' >>"${ARTIFACT_DIR}/release-assets-summary.txt"

# The Nodes wizard and Stage-0 now use the same fixed Release asset.
grep -qxF "readonly DEFAULT_VERSION=\"${release_tag}\"" "${work}/managed-node-bootstrap.sh"
chmod 0700 "${work}/managed-node-bootstrap.sh"
printf 'installer_embedded_version=PASS\n' >>"${ARTIFACT_DIR}/release-assets-summary.txt"

# The fixture suite also manages the ocserv-agent account. Guard before it,
# not just later inside the native smoke's own pristine-host check.
sudo -n true
for path in /usr/libexec/ocservia /etc/ocservia-agent /etc/ocservia \
  /var/lib/ocservia-agent /var/lib/ocservia-privd /var/lib/ocservia-upgrade /var/lib/ocservia-rebind; do
  if sudo test -e "${path}"; then
    echo "host already carries Agent state: ${path}" >&2
    exit 1
  fi
done
if getent passwd ocserv-agent >/dev/null || getent group ocserv-agent >/dev/null || \
  dpkg-query -W -f='${Status}' ocservia-agent 2>/dev/null | grep -q 'install ok installed'; then
  echo 'host already carries an Agent account or package' >&2
  exit 1
fi

# These are explicitly fixture checks: exercise the published installer's
# fixed-version URL, download failure and frozen-staging digest rejection.
INSTALLER_UNDER_TEST="${work}/managed-node-bootstrap.sh" TEST_RELEASE_VERSION="${version}" \
  bash "${ROOT}/scripts/test-managed-node-install.sh" release-contracts \
  >"${ARTIFACT_DIR}/published-installer-contracts.log" 2>&1
grep -Fxq "Managed node release contracts passed (${version})" "${ARTIFACT_DIR}/published-installer-contracts.log"
printf 'published_installer_contracts=PASS (fault injection)\n' >>"${ARTIFACT_DIR}/release-assets-summary.txt"

# The existing smoke executes the actual downloaded native binaries/packages,
# including installation and corrupt embedded-payload rejection. No rebuilds.
PRODUCTS_DIR="${work}" VERSION="${version}" STUB_BINARIES=false RC_LIFECYCLE=false \
  ARTIFACT_DIR="${ARTIFACT_DIR}/native-smoke" \
  bash "${ROOT}/scripts/release-native-package-smoke.sh" \
  >"${ARTIFACT_DIR}/native-package-smoke.log" 2>&1
printf 'published_native_install_and_rejection=PASS\n' >>"${ARTIFACT_DIR}/release-assets-summary.txt"
result=PASS
