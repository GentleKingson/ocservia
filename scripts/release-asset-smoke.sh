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

base="https://github.com/GentleKingson/ocservia/releases/download/${release_tag}"
for name in "ocservia-agent_${version}-1_${arch}.deb" "ocservia-agent-${version}-1.${rpm_arch}.rpm" \
  managed-node-bootstrap.sh; do
  url="${base}/${name}"
  printf '%s\n' "${url}" >>"${ARTIFACT_DIR}/release-assets-downloads.txt"
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --tlsv1.2 --connect-timeout 15 --max-time 300 --retry 2 \
    --output "${work}/${name}" "${url}"
  test -s "${work}/${name}"
done
# The Nodes wizard and Stage-0 now use the same fixed Release asset.
grep -qxF "readonly DEFAULT_VERSION=\"${release_tag}\"" "${work}/managed-node-bootstrap.sh"
chmod 0700 "${work}/managed-node-bootstrap.sh"
printf 'installer_embedded_version=PASS\n' >>"${ARTIFACT_DIR}/release-assets-summary.txt"

# Dedicated clean ARM64 enrollment host with ocserv prerequisites and an
# operator-issued one-time token. The installer downloads the DEB itself.
if [[ -n "${RELEASE_JOIN_TOKEN_FILE:-}" ]]; then
  [[ "${arch}" == arm64 && "$( . /etc/os-release; echo "${ID}:${VERSION_ID}" )" == ubuntu:26.04 ]]
  if dpkg-query -W -f='${db:Status-Status}' ocservia-agent 2>/dev/null | grep -qx installed; then
    echo 'published enrollment requires a clean Agent host' >&2
    exit 1
  fi
  : "${RELEASE_DEB_SHA256:?supply the independently verified release DEB SHA-256}"
  sudo bash "${work}/managed-node-bootstrap.sh" --non-interactive --join-token-file "${RELEASE_JOIN_TOKEN_FILE}" \
    --package-sha256 "${RELEASE_DEB_SHA256}" >"${work}/enrollment.log"
  grep -Fxq ONLINE "${work}/enrollment.log"
  printf 'published_arm64_enrollment=PASS\n' >>"${ARTIFACT_DIR}/release-assets-summary.txt"
  result=PASS
  exit 0
fi

# The existing smoke executes the actual downloaded native binaries/packages,
# including package ownership, installation and removal.
PRODUCTS_DIR="${work}" VERSION="${version}" RC_LIFECYCLE=false \
  ARTIFACT_DIR="${ARTIFACT_DIR}/native-smoke" \
  bash "${ROOT}/scripts/release-native-package-smoke.sh" \
  >"${ARTIFACT_DIR}/native-package-smoke.log" 2>&1
printf 'published_native_lifecycle=PASS\n' >>"${ARTIFACT_DIR}/release-assets-summary.txt"
result=PASS
