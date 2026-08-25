#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
# shellcheck disable=SC1091
source "${ROOT}/scripts/env.sh"

OUTPUT_DIR="${OUTPUT_DIR:?OUTPUT_DIR is required}"
VERSION="${VERSION:?VERSION is required}"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:?SOURCE_DATE_EPOCH is required}"
PACKAGE_ARCH="${PACKAGE_ARCH:?PACKAGE_ARCH is required}"
AGENT_TRUSTED_KEY_SHA256="${AGENT_TRUSTED_KEY_SHA256:?AGENT_TRUSTED_KEY_SHA256 is required}"

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]] || ! [[ "${SOURCE_DATE_EPOCH}" =~ ^[0-9]+$ ]]; then
  echo "VERSION must be SemVer and SOURCE_DATE_EPOCH must be numeric" >&2
  exit 2
fi
case "${PACKAGE_ARCH}" in
  amd64 | arm64) ;;
  *)
    echo "PACKAGE_ARCH must be amd64 or arm64" >&2
    exit 2
    ;;
esac
if [[ ! "${AGENT_TRUSTED_KEY_SHA256}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "AGENT_TRUSTED_KEY_SHA256 must be 64 lowercase hexadecimal characters" >&2
  exit 2
fi
case "${PACKAGE_ARCH}" in
  amd64) rpm_arch=x86_64 ;;
  arm64) rpm_arch=aarch64 ;;
esac
if ! command -v nfpm >/dev/null 2>&1; then
  echo "nfpm is required on PATH (scripts/bootstrap.sh native-packages)" >&2
  exit 1
fi

archive="${OUTPUT_DIR}/ocservia-agent-${VERSION}-linux-${PACKAGE_ARCH}.tar.gz"
for suffix in "" ".sha256" ".sha256.sig" ".sha256.pub.pem"; do
  if [[ ! -f "${archive}${suffix}" ]]; then
    echo "missing signed archive input ${archive}${suffix}; run package-agent.sh first" >&2
    exit 1
  fi
done

staging="$(mktemp -d "${TMPDIR:-/tmp}/ocservia-native-package.XXXXXX")"
cleanup() { rm -rf -- "${staging}"; }
trap cleanup EXIT INT TERM
payload="${staging}/payload/usr/share/ocservia-agent"
mkdir -p -- "${payload}"
install -m 0644 -- "${archive}" "${archive}.sha256" "${archive}.sha256.sig" "${payload}/"
install -m 0644 -- "${archive}.sha256.pub.pem" "${payload}/release-signing.pub.pem"
printf '%s\n' "${AGENT_TRUSTED_KEY_SHA256}" >"${payload}/trusted-release-key.sha256"
install -m 0755 -- "${ROOT}/scripts/verify-agent-package.sh" "${payload}/verify-agent-package.sh"

render() {
  sed -e "s/@VERSION@/${VERSION}/g" -e "s/@ARCH@/${PACKAGE_ARCH}/g" "$1" >"$2"
  chmod 0755 -- "$2"
}
render "${ROOT}/deploy/package/nfpm.yaml" "${staging}/nfpm.yaml"
render "${ROOT}/deploy/package/postinstall.sh" "${staging}/postinstall.sh"
render "${ROOT}/deploy/package/preremove.sh" "${staging}/preremove.sh"

deb="${OUTPUT_DIR}/ocservia-agent_${VERSION}_${PACKAGE_ARCH}.deb"
rpm="${OUTPUT_DIR}/ocservia-agent-${VERSION}-1.${rpm_arch}.rpm"
(cd "${staging}" && SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH}" \
  nfpm package --config nfpm.yaml --packager deb --target "${deb}")
(cd "${staging}" && SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH}" \
  nfpm package --config nfpm.yaml --packager rpm --target "${rpm}")
test -s "${deb}" && test -s "${rpm}"
printf '%s\n%s\n' "${deb}" "${rpm}"
