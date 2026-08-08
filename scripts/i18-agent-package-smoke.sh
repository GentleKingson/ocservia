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
cleanup() { rm -rf -- "${work}"; }
trap cleanup EXIT INT TERM

(cd "${ROOT}/rust" && cargo build --locked --release --package ocservia-agent --package ocservia-privd)
openssl genpkey -algorithm ED25519 -out "${work}/signing.key" >/dev/null 2>&1
chmod 0600 "${work}/signing.key"
openssl pkey -in "${work}/signing.key" -pubout -out "${work}/trusted.pub.pem" >/dev/null 2>&1
openssl pkey -pubin -in "${work}/trusted.pub.pem" -outform DER -out "${work}/trusted.der"
trusted_fingerprint="$(sha256sum "${work}/trusted.der" | awk '{print $1}')"
archive="$(OUTPUT_DIR="${ARTIFACT_DIR}" AGENT_SIGNING_KEY="${work}/signing.key" VERSION=1.0.0 \
  SOURCE_DATE_EPOCH=1786147200 "${ROOT}/scripts/package-agent.sh")"
AGENT_TRUSTED_KEY_SHA256="${trusted_fingerprint}" "${ROOT}/scripts/verify-agent-package.sh" "${archive}" "${archive}.sha256" \
  "${archive}.sha256.sig" "${work}/trusted.pub.pem" >"${ARTIFACT_DIR}/verification.log"

openssl genpkey -algorithm ED25519 -out "${work}/substitute.key" >/dev/null 2>&1
openssl pkey -in "${work}/substitute.key" -pubout -out "${work}/substitute.pub.pem" >/dev/null 2>&1
openssl pkeyutl -sign -rawin -inkey "${work}/substitute.key" -in "${archive}.sha256" -out "${work}/substitute.sig"
if AGENT_TRUSTED_KEY_SHA256="${trusted_fingerprint}" "${ROOT}/scripts/verify-agent-package.sh" \
  "${archive}" "${archive}.sha256" "${work}/substitute.sig" "${work}/substitute.pub.pem" >/dev/null 2>&1; then
  echo "substituted package signing key was trusted" >&2
  exit 1
fi

tar -xzf "${archive}" -C "${work}"
package_root="${work}/ocservia-agent-1.0.0"
rootfs="${work}/rootfs"
mkdir -p "${rootfs}"
sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
  "${package_root}/scripts/install-agent.sh"
test -x "${rootfs}/usr/libexec/ocservia/ocservia-agent"
test -f "${rootfs}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
sudo install -o 61000 -g 61000 -m 0600 /dev/null "${rootfs}/var/lib/ocservia-agent/identity/controller.key"
sudo install -o 61000 -g 61000 -m 0600 /dev/null "${rootfs}/var/lib/ocservia-agent/agent.db"

sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
  "${package_root}/scripts/upgrade-agent.sh"
test -x "${rootfs}/var/lib/ocservia-agent/upgrade-backup/ocservia-agent.previous"
sudo env DESTDIR="${rootfs}" "${package_root}/scripts/uninstall-agent.sh"
test -f "${rootfs}/var/lib/ocservia-agent/identity/controller.key"
test -f "${rootfs}/var/lib/ocservia-agent/agent.db"
test ! -e "${rootfs}/usr/libexec/ocservia/ocservia-agent"

sudo env DESTDIR="${rootfs}" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true \
  "${package_root}/scripts/install-agent.sh"
sudo env DESTDIR="${rootfs}" "${package_root}/scripts/uninstall-agent.sh" --purge-state
test ! -e "${rootfs}/var/lib/ocservia-agent"
test ! -e "${rootfs}/etc/ocservia-agent"
printf 'install=pass\nupgrade=pass\nuninstall_preserves_state=pass\npurge=pass\n' \
  >"${ARTIFACT_DIR}/lifecycle-summary.txt"
