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
"${new}/scripts/rollback-agent.sh"
cmp "${old}/rust/target/release/ocservia-agent" "${DESTDIR}/usr/libexec/ocservia/ocservia-agent"
"${new}/scripts/upgrade-agent.sh"
cmp "${new}/rust/target/release/ocservia-agent" "${DESTDIR}/usr/libexec/ocservia/ocservia-agent"
sha256sum -c "${work}/snapshot"
echo 'Identical-package retry preserves rollback; rollback/re-upgrade still works (installer unit test only)'
