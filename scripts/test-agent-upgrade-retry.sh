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
# A different installed unit layout must not trigger re-enrollment or purge
# pre-existing artifacts. This fixture still supplies the current trust keys.
for service in agent privd; do
  printf '[Service]\nExecStart=/usr/libexec/ocservia/ocservia-%s\n' "${service}" \
    >"${DESTDIR}/usr/lib/systemd/system/ocservia-${service}.service"
done
artifact_dir="${DESTDIR}/var/lib/ocservia-privd/certificates/artifacts"
mkdir -p "${artifact_dir}"
artifact="${artifact_dir}/018f0c2e-7b1a-7c3d-8e9f-0123456789ab.p12"
printf 'existing artifact\n' >"${artifact}"
chmod 710 "${artifact_dir}"
sha256sum "${artifact}" >"${work}/artifact.sha256"
"${new}/scripts/upgrade-agent.sh"
sha256sum -c "${work}/artifact.sha256"
test "$(stat -c '%a' "${artifact_dir}")" = 710
test ! -e "${config}/sealing-keys-bound"
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

# A direct-executable service needs no launcher. Do not infer old software
# compatibility from that layout or rewrite the operator's Relay settings.
"${new}/scripts/rollback-agent.sh"
rm "${DESTDIR}/usr/libexec/ocservia/ocservia-agent-relays"
printf '[Service]\nExecStart=\nExecStart=/usr/libexec/ocservia/ocservia-agent\n' \
  >"${DESTDIR}/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
printf 'RELAY_URL_A=https://relay-a.example.test\nRELAY_URL_B=\n' >"${config}/relays.env"
cp "${config}/relays.env" "${work}/single-relays.env"
"${new}/scripts/upgrade-agent.sh"
cmp "${config}/relays.env" "${work}/single-relays.env"
test -f "${snapshot}/ocservia-agent-relays.absent"
"${new}/scripts/upgrade-agent.sh"
# A service actually referring to a missing launcher is still invalid,
# including in the read-only snapshot check used by identical retries.
cp -p "${snapshot}/ocservia-agent-relays.conf.previous" "${work}/direct-relays.conf"
cp -p "${snapshot}/MANIFEST.sha256" "${work}/direct-manifest"
cp "${new}/deploy/production/systemd/ocservia-agent-relays.conf" "${snapshot}/ocservia-agent-relays.conf.previous"
(cd "${snapshot}" && sha256sum -- *.previous *.absent) >"${snapshot}/MANIFEST.sha256"
if "${new}/scripts/rollback-agent.sh" --verify-only >"${work}/missing-launcher.log" 2>&1; then
  echo 'snapshot accepted a service with its required launcher missing' >&2; exit 1
fi
grep -F 'missing the production Relay launcher required by its service' "${work}/missing-launcher.log"
cp -p "${work}/direct-relays.conf" "${snapshot}/ocservia-agent-relays.conf.previous"
cp -p "${work}/direct-manifest" "${snapshot}/MANIFEST.sha256"
"${new}/scripts/rollback-agent.sh"
cmp "${config}/relays.env" "${work}/single-relays.env"
test ! -e "${DESTDIR}/usr/libexec/ocservia/ocservia-agent-relays"
# Verify signed target contents, not capabilities inferred from live settings.
fixture="${work}/package-fixture"
mkdir -p "${fixture}"
cp -a "${new}" "${fixture}/ocservia-agent-1.0.1"
target="${fixture}/ocservia-agent-1.0.1"
rm "${target}/.ocservia-package-verified" "${target}/deploy/production/systemd/agent-relays.sh"
archive="${work}/ocservia-agent-1.0.1-linux-${PACKAGE_ARCH}.tar.gz"
verify_fixture() {
  tar -czf "${archive}" -C "${fixture}" ocservia-agent-1.0.1
  (cd "${work}" && sha256sum "$(basename "${archive}")") >"${archive}.sha256"
  openssl pkeyutl -sign -rawin -inkey "${AGENT_SIGNING_KEY}" \
    -in "${archive}.sha256" -out "${archive}.sha256.sig"
  bash "${ROOT}/scripts/verify-agent-package.sh" "${archive}" "${archive}.sha256" \
    "${archive}.sha256.sig" "${work}/public.pem"
}
if verify_fixture >"${work}/invalid-package.log" 2>&1; then
  echo 'package accepted a service with its required launcher missing' >&2; exit 1
fi
grep -F 'package is missing the production Relay launcher' "${work}/invalid-package.log"
cp "${work}/direct-relays.conf" "${target}/deploy/production/systemd/ocservia-agent-relays.conf"
verify_fixture >"${work}/verified-package"
test -d "$(cat "${work}/verified-package")"
"${new}/scripts/upgrade-agent.sh"
"${new}/scripts/uninstall-agent.sh"
test ! -e "${DESTDIR}/usr/libexec/ocservia/ocservia-agent-relays"
test -f "${config}/relays.env"
echo 'Explicit target files, Relay configuration preservation and launcher uninstall passed'
