#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUTPUT_DIR="${OUTPUT_DIR:?OUTPUT_DIR is required}"
AGENT_SIGNING_KEY="${AGENT_SIGNING_KEY:?AGENT_SIGNING_KEY is required}"
VERSION="${VERSION:?VERSION is required}"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:?SOURCE_DATE_EPOCH is required}"
PACKAGE_ARCH="${PACKAGE_ARCH:?PACKAGE_ARCH is required}"

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
if [[ ! -f "${AGENT_SIGNING_KEY}" || -L "${AGENT_SIGNING_KEY}" ]]; then
  echo "signing key must be a regular file" >&2
  exit 1
fi
for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
  test -x "${ROOT}/rust/target/release/${binary}"
done
# Every packaged binary must carry exactly this release identity: the Agent
# heartbeat, the upgrade downgrade fence, and the MANIFEST must all report
# the same version, otherwise reconciliation can never observe success.
for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
  reported="$("${ROOT}/rust/target/release/${binary}" --version | awk '{print $NF}')"
  if [[ "${reported}" != "${VERSION}" ]]; then
    echo "${binary} reports release ${reported}, expected ${VERSION}" >&2
    exit 1
  fi
done

umask 077
mkdir -p -- "${OUTPUT_DIR}"
staging="$(mktemp -d "${TMPDIR:-/tmp}/ocservia-agent-package.XXXXXX")"
cleanup() { rm -rf -- "${staging}"; }
trap cleanup EXIT INT TERM
package_root="${staging}/ocservia-agent-${VERSION}"
mkdir -p -- "${package_root}/rust/target/release" "${package_root}/deploy/systemd" \
  "${package_root}/deploy/production/systemd" "${package_root}/scripts"
install -m 0755 -- "${ROOT}/rust/target/release/ocservia-agent" "${ROOT}/rust/target/release/ocservia-privd" \
  "${ROOT}/rust/target/release/ocservia-upgrader" "${package_root}/rust/target/release/"
install -m 0644 -- "${ROOT}/deploy/systemd/agent.env.example" "${ROOT}/deploy/systemd/ocservia-agent.service" \
  "${ROOT}/deploy/systemd/ocservia-privd.service" "${ROOT}/deploy/systemd/ocservia-upgrader@.service" \
  "${package_root}/deploy/systemd/"
install -m 0644 -- "${ROOT}/deploy/production/systemd/ocservia-agent-relays.conf" \
  "${ROOT}/deploy/production/systemd/relays.env.example" "${package_root}/deploy/production/systemd/"
install -m 0755 -- "${ROOT}/scripts/install-agent.sh" "${ROOT}/scripts/upgrade-agent.sh" \
  "${ROOT}/scripts/rollback-agent.sh" "${ROOT}/scripts/uninstall-agent.sh" \
  "${ROOT}/scripts/verify-agent-package.sh" "${package_root}/scripts/"
printf 'version=%s\narch=%s\nagent_protocol=1.1\nplatform_compatibility=N,N-1 minor\n' \
  "${VERSION}" "${PACKAGE_ARCH}" >"${package_root}/MANIFEST"

archive="${OUTPUT_DIR}/ocservia-agent-${VERSION}-linux-${PACKAGE_ARCH}.tar.gz"
tar --sort=name --mtime="@${SOURCE_DATE_EPOCH}" --owner=0 --group=0 --numeric-owner \
  -C "${staging}" -czf "${archive}" "ocservia-agent-${VERSION}"
checksum="${archive}.sha256"
(printf '%s  %s\n' "$(sha256sum -- "${archive}" | awk '{print $1}')" "$(basename -- "${archive}")" >"${checksum}")
openssl pkeyutl -sign -rawin -inkey "${AGENT_SIGNING_KEY}" -in "${checksum}" -out "${checksum}.sig"
openssl pkey -in "${AGENT_SIGNING_KEY}" -pubout -out "${checksum}.pub.pem" >/dev/null 2>&1
printf '%s\n' "${archive}"
