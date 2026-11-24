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

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc[.][1-9][0-9]*)?$ ]] || ! [[ "${SOURCE_DATE_EPOCH}" =~ ^[0-9]+$ ]]; then
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
case "${PACKAGE_ARCH}" in
  amd64) rpm_arch=x86_64 ;;
  arm64) rpm_arch=aarch64 ;;
esac
if ! command -v nfpm >/dev/null 2>&1; then
  echo "nfpm is required on PATH (scripts/bootstrap.sh native-packages)" >&2
  exit 1
fi

mkdir -p -- "${OUTPUT_DIR}"
OUTPUT_DIR="$(cd "${OUTPUT_DIR}" && pwd)"
config="$(mktemp)"
trap 'rm -f -- "${config}"' EXIT
sed -e "s/@VERSION@/${VERSION}/g" -e "s/@ARCH@/${PACKAGE_ARCH}/g" \
  "${ROOT}/deploy/package/nfpm.yaml" >"${config}"
cd "${ROOT}"
for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
  [[ "$(rust/target/release/"${binary}" --version)" == "${binary} ${VERSION}" ]]
done
deb="${OUTPUT_DIR}/ocservia-agent_${VERSION}-1_${PACKAGE_ARCH}.deb"
rpm="${OUTPUT_DIR}/ocservia-agent-${VERSION}-1.${rpm_arch}.rpm"
nfpm package --config "${config}" --packager deb --target "${deb}"
nfpm package --config "${config}" --packager rpm --target "${rpm}"
test -s "${deb}" && test -s "${rpm}"
printf '%s\n%s\n' "${deb}" "${rpm}"
