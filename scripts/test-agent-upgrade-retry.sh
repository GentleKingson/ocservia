#!/usr/bin/env bash
# Focused installer regression with stub binaries and DESTDIR. Never upgrade evidence.
set -euo pipefail
[[ "${EUID}" == 0 ]] || { echo 'run this test in an isolated root container' >&2; exit 2; }
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT
umask 077
mkdir -p "${work}/source/rust/target/release" "${work}/source/scripts" "${work}/products" "${work}/rootfs"
cp -a "${ROOT}/deploy" "${work}/source/"
cp "${ROOT}/scripts/"{package-agent,install-agent,upgrade-agent,rollback-agent,uninstall-agent,verify-agent-package}.sh "${work}/source/scripts/"
export DESTDIR="${work}/rootfs" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true
export OUTPUT_DIR="${work}/products" AGENT_SIGNING_KEY="${work}/signing.key" SOURCE_DATE_EPOCH=1786147200
case "$(uname -m)" in aarch64) export PACKAGE_ARCH=arm64 ;; x86_64) export PACKAGE_ARCH=amd64 ;; *) exit 2 ;; esac
openssl genpkey -algorithm ED25519 -out "${AGENT_SIGNING_KEY}" >/dev/null 2>&1
openssl pkey -in "${AGENT_SIGNING_KEY}" -pubout -out "${work}/public.pem" >/dev/null 2>&1
AGENT_TRUSTED_KEY_SHA256="$(openssl pkey -pubin -in "${work}/public.pem" -outform DER | sha256sum | awk '{print $1}')"
export AGENT_TRUSTED_KEY_SHA256
package() {
  local version="$1" binary archive
  for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
    printf '#!/bin/sh\necho "%s %s"\n' "${binary}" "${version}" >"${work}/source/rust/target/release/${binary}"
    chmod 755 "${work}/source/rust/target/release/${binary}"
  done
  VERSION="${version}" bash "${work}/source/scripts/package-agent.sh" >/dev/null
  archive="${OUTPUT_DIR}/ocservia-agent-${version}-linux-${PACKAGE_ARCH}.tar.gz"
  bash "${ROOT}/scripts/verify-agent-package.sh" "${archive}" "${archive}.sha256" "${archive}.sha256.sig" "${work}/public.pem"
}
old="$(package 1.0.0)"
new="$(package 1.0.1)"
"${old}/scripts/install-agent.sh"
config="${DESTDIR}/etc/ocservia-agent"
install -o root -g 61000 -m 640 "${work}/public.pem" "${config}/command.pem"
for name in user p12; do
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "${config}/${name}-password-seal-private.pem" >/dev/null 2>&1
done
user_hash="$(openssl pkey -in "${config}/user-password-seal-private.pem" -pubout -outform DER | sha256sum | awk '{print $1}')"
p12_hash="$(openssl pkey -in "${config}/p12-password-seal-private.pem" -pubout -outform DER | sha256sum | awk '{print $1}')"
cat >"${config}/agent.env" <<EOF
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/command.pem
USER_PASSWORD_SEAL_KEY_ID=user
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${user_hash}
P12_PASSWORD_SEAL_KEY_ID=p12
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=${p12_hash}
EOF
chown root:61000 "${config}/agent.env"
chmod 640 "${config}/agent.env"
"${new}/scripts/upgrade-agent.sh"
snapshot="${DESTDIR}/var/lib/ocservia-upgrade/upgrade-backup"
sha256sum "${snapshot}/"* >"${work}/snapshot"
"${new}/scripts/upgrade-agent.sh"
sha256sum -c "${work}/snapshot"
assert_retry_rejected() {
  local label="$1" expected="$2"
  find "${DESTDIR}" -type f -exec sha256sum {} + | sort >"${work}/before-rejection"
  if "${new}/scripts/upgrade-agent.sh" >"${work}/rejection.log" 2>&1; then
    echo "unsafe identical retry accepted: ${label}" >&2; exit 1
  fi
  grep -F "${expected}" "${work}/rejection.log"
  find "${DESTDIR}" -type f -exec sha256sum {} + | sort >"${work}/after-rejection"
  cmp "${work}/before-rejection" "${work}/after-rejection"
}
mv "${snapshot}/MANIFEST.sha256" "${work}/manifest"
assert_retry_rejected missing-manifest 'missing, non-regular, or symlinked file'
mv "${work}/manifest" "${snapshot}/MANIFEST.sha256"
cp -p "${snapshot}/ocservia-agent.previous" "${work}/agent.previous"
printf 'damage\n' >>"${snapshot}/ocservia-agent.previous"
assert_retry_rejected corrupt-member 'digest does not match its trusted manifest'
cp -p "${work}/agent.previous" "${snapshot}/ocservia-agent.previous"
rollback="${DESTDIR}/usr/libexec/ocservia/ocservia-agent-rollback"
mv "${rollback}" "${work}/rollback"
ln -s "${work}/rollback" "${rollback}"
assert_retry_rejected rollback-symlink 'must be regular files'
rm "${rollback}"
mv "${work}/rollback" "${rollback}"
chmod 777 "${rollback}"
assert_retry_rejected rollback-mode 'unsafe owner, group, mode, or link count'
chmod 755 "${rollback}"
chown 61000:61000 "${rollback}"
assert_retry_rejected rollback-owner 'unsafe owner, group, mode, or link count'
chown root:root "${rollback}"
"${new}/scripts/upgrade-agent.sh"
sha256sum -c "${work}/snapshot"
"${new}/scripts/rollback-agent.sh"
cmp "${old}/rust/target/release/ocservia-agent" "${DESTDIR}/usr/libexec/ocservia/ocservia-agent"
"${new}/scripts/upgrade-agent.sh"
cmp "${new}/rust/target/release/ocservia-agent" "${DESTDIR}/usr/libexec/ocservia/ocservia-agent"
sha256sum -c "${work}/snapshot"
echo 'Identical-package retry preserves rollback; rollback/re-upgrade still works (installer unit test only)'

# A single-Relay operator configuration survives upgrade and same-package
# retries, but cannot be handed to a snapshot without the new launcher.
"${new}/scripts/rollback-agent.sh"
rm "${DESTDIR}/usr/libexec/ocservia/ocservia-agent-relays"
printf 'RELAY_URL_A=https://relay-a.example.test\nRELAY_URL_B=\n' >"${config}/relays.env"
cp "${config}/relays.env" "${work}/single-relays.env"
"${new}/scripts/upgrade-agent.sh"
cmp "${config}/relays.env" "${work}/single-relays.env"
test -f "${snapshot}/ocservia-agent-relays.absent"
"${new}/scripts/upgrade-agent.sh"
find "${DESTDIR}" -type f -exec sha256sum {} + | sort >"${work}/before-rollback"
if "${new}/scripts/rollback-agent.sh" >"${work}/blocked-rollback.log" 2>&1; then
  echo 'old snapshot accepted single Relay configuration' >&2; exit 1
fi
grep -F 'target predates single Relay support' "${work}/blocked-rollback.log"
find "${DESTDIR}" -type f -exec sha256sum {} + | sort >"${work}/after-rollback"
cmp "${work}/before-rollback" "${work}/after-rollback"
printf 'RELAY_URL_A=https://relay-a.example.test\nRELAY_URL_B=https://relay-b.example.test\n' >"${config}/relays.env"
"${new}/scripts/rollback-agent.sh"
test ! -e "${DESTDIR}/usr/libexec/ocservia/ocservia-agent-relays"
"${new}/scripts/upgrade-agent.sh"
"${new}/scripts/uninstall-agent.sh"
test ! -e "${DESTDIR}/usr/libexec/ocservia/ocservia-agent-relays"
test -f "${config}/relays.env"
echo 'Single Relay preservation, legacy rollback preflight and launcher uninstall passed'
