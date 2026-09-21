#!/usr/bin/env bash
# State-checker regression with stub lifecycle code, not native upgrade evidence.
set -euo pipefail
[[ "${EUID}" == 0 && -f /.dockerenv ]] || {
  echo 'run this test only in a fresh disposable root container' >&2; exit 2;
}
for path in /etc/ocservia /etc/ocservia-agent /usr/libexec/ocservia \
  /usr/share/ocservia-agent /var/lib/ocservia-agent /var/lib/ocservia-upgrade \
  /usr/lib/systemd/system/ocservia-agent.service.d \
  /usr/lib/systemd/system/ocservia-agent.service /usr/lib/systemd/system/ocservia-privd.service; do
  [[ ! -e "${path}" && ! -L "${path}" ]] || { echo "fixture path already exists: ${path}" >&2; exit 2; }
done
if getent passwd ocserv-agent >/dev/null; then echo 'fixture user already exists' >&2; exit 2; fi
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT
checker="${ROOT}/scripts/release-agent-state-check.sh"
case "$(uname -m)" in aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; *) exit 2 ;; esac
useradd --system ocserv-agent
package=/usr/share/ocservia-agent
libexec=/usr/libexec/ocservia
dropin=/usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf
launcher="${libexec}/ocservia-agent-relays"
mkdir -p "${work}/source/scripts" "${work}/source/rust/target/release" "${work}/old" \
  "${work}/bin" "${package}" "${libexec}" "$(dirname "${dropin}")" \
  /etc/ocservia-agent /var/lib/ocservia-agent/identity /var/lib/ocservia-upgrade/upgrade-backup
cp -a "${ROOT}/deploy" "${work}/source/"
cp "${ROOT}/scripts/"{package-agent,install-agent,upgrade-agent,rollback-agent,uninstall-agent,verify-agent-package}.sh "${work}/source/scripts/"
cat >"${work}/binary.c" <<'C'
#include <stdio.h>
#include <string.h>
int main(int argc, char **argv) {
  const char *name = strrchr(argv[0], '/');
  if (argc == 2 && strcmp(argv[1], "--version") == 0)
    printf("%s %s\n", name ? name + 1 : argv[0], VERSION);
  else
    puts("fixture-endpoint");
  return 0;
}
C
for version in 0.6.0 0.6.1; do
  cc -DVERSION="\"${version}\"" "${work}/binary.c" -o "${work}/binary"
  for name in ocservia-agent ocservia-privd ocservia-upgrader; do
    if [[ "${version}" == 0.6.0 ]]; then
      install -m 755 "${work}/binary" "${work}/old/${name}"
      install -m 755 "${work}/binary" "${libexec}/${name}"
    else
      install -m 755 "${work}/binary" "${work}/source/rust/target/release/${name}"
    fi
  done
done
printf '[Service]\nExecStart=/usr/libexec/ocservia/ocservia-agent --relay-url $RELAY_URL_A --relay-url $RELAY_URL_B\n' >"${work}/old/dropin"
install -m 644 "${work}/old/dropin" "${dropin}"
for unit in ocservia-agent.service ocservia-privd.service; do
  printf '[Service]\nExecStart=/usr/libexec/ocservia/%s\n' "${unit%.service}" >"${work}/old/${unit}"
  install -m 644 "${work}/old/${unit}" "/usr/lib/systemd/system/${unit}"
done
for name in agent.env controller-command-verification-key.pem user-password-seal-private.pem \
  p12-password-seal-private.pem relays.env relay-access-token; do
  printf 'fixture-%s\n' "${name}" >"/etc/ocservia-agent/${name}"
done
printf 'CONTROLLER_ENDPOINT_ID=fixture-controller\n' >/etc/ocservia-agent/agent.env
for name in identity-sentinel endpoint.key controller.endpoint; do
  printf 'fixture-%s\n' "${name}" >"/var/lib/ocservia-agent/identity/${name}"
done
openssl genpkey -algorithm ED25519 -out "${work}/old.key" >/dev/null 2>&1
openssl pkey -in "${work}/old.key" -pubout -out "${package}/release-signing.pub.pem" >/dev/null 2>&1
openssl pkey -pubin -in "${package}/release-signing.pub.pem" -outform DER | sha256sum | awk '{print $1}' >"${package}/trusted-release-key.sha256"
bash "${checker}" before "${work}/evidence" 0.6.0 "${arch}"

# Real signing/verification, but the negative preflight and rollback are stubs.
cat >"${work}/source/scripts/upgrade-agent.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "$(stat -c %a /etc/ocservia-agent/controller-command-verification-key.pem)" == 666 ]]
echo 'blocked before modification' >&2
exit 1
SH
openssl genpkey -algorithm ED25519 -out "${work}/candidate.key" >/dev/null 2>&1
OUTPUT_DIR="${package}" AGENT_SIGNING_KEY="${work}/candidate.key" VERSION=0.6.1 \
  SOURCE_DATE_EPOCH=1786147200 PACKAGE_ARCH="${arch}" bash "${work}/source/scripts/package-agent.sh" >/dev/null
archive="${package}/ocservia-agent-0.6.1-linux-${arch}.tar.gz"
install -m 755 "${ROOT}/scripts/verify-agent-package.sh" "${package}/verify-agent-package.sh"
cp "${archive}.sha256.pub.pem" "${package}/release-signing.pub.pem"
openssl pkey -pubin -in "${package}/release-signing.pub.pem" -outform DER | sha256sum | awk '{print $1}' >"${package}/trusted-release-key.sha256"
install -m 755 "${work}/source/rust/target/release/"* "${libexec}/"
install -m 644 "${ROOT}/deploy/production/systemd/ocservia-agent-relays.conf" "${dropin}"
install -m 755 "${ROOT}/deploy/production/systemd/agent-relays.sh" "${launcher}"
for unit in ocservia-agent.service ocservia-privd.service; do
  install -m 644 "${work}/old/${unit}" "/var/lib/ocservia-upgrade/upgrade-backup/${unit}.previous"
  install -m 644 "${ROOT}/deploy/systemd/${unit}" "/usr/lib/systemd/system/${unit}"
done
printf 'fixture snapshot\n' >/var/lib/ocservia-upgrade/upgrade-backup/member
bash "${checker}" after "${work}/evidence" 0.6.1 "${arch}"
bash "${checker}" retry "${work}/evidence" 0.6.1 "${arch}"
bash "${checker}" reject "${work}/evidence" 0.6.1 "${arch}"
expect_failure() {
  local label="$1" mode="${2:-after}" version="${3:-0.6.1}"
  if bash "${checker}" "${mode}" "${work}/evidence" "${version}" "${arch}" >"${work}/failure.log" 2>&1; then
    echo "state checker accepted ${label}" >&2; exit 1
  fi
}
for target in "${dropin}" "${launcher}" /usr/lib/systemd/system/ocservia-agent.service \
  /usr/lib/systemd/system/ocservia-privd.service; do
  cp -p "${target}" "${work}/saved"
  printf 'tampered\n' >>"${target}"
  expect_failure "changed ${target}"
  expect_failure "retry changed ${target}" retry
  cp -p "${work}/saved" "${target}"
  chmod 777 "${target}"
  expect_failure "unsafe mode ${target}"
  cp -p "${work}/saved" "${target}"
  chown ocserv-agent "${target}"
  expect_failure "unsafe owner ${target}"
  cp -p "${work}/saved" "${target}"
  rm "${target}"
  expect_failure "missing ${target}"
  ln -s "${work}/saved" "${target}"
  expect_failure "symlinked ${target}"
  rm "${target}"
  cp -p "${work}/saved" "${target}"
  bash "${checker}" after "${work}/evidence" 0.6.1 "${arch}"
done
for target in /etc/ocservia-agent/relays.env /etc/ocservia/release-signing.pub.pem \
  /var/lib/ocservia-agent/identity/endpoint.key; do
  cp -p "${target}" "${work}/saved"
  printf 'tampered\n' >>"${target}"
  expect_failure "changed operator state ${target}"
  cp -p "${work}/saved" "${target}"
done
cp "${archive}" "${work}/archive"
printf 'damaged\n' >>"${archive}"
expect_failure 'corrupted signed candidate payload'
cp "${work}/archive" "${archive}"
bash "${checker}" after "${work}/evidence" 0.6.1 "${arch}"
cat >"${libexec}/ocservia-agent-rollback" <<SH
#!/usr/bin/env bash
set -euo pipefail
install -m 755 '${work}/old/ocservia-agent' '${work}/old/ocservia-privd' '${work}/old/ocservia-upgrader' '${libexec}/'
install -m 644 '${work}/old/dropin' '${dropin}'
install -m 644 '${work}/old/ocservia-agent.service' '${work}/old/ocservia-privd.service' /usr/lib/systemd/system/
if [[ "\${KEEP_LAUNCHER:-false}" != true ]]; then rm -f '${launcher}'; fi
SH
chmod 755 "${libexec}/ocservia-agent-rollback"
printf '#!/bin/sh\nexit 0\n' >"${work}/bin/systemctl"
chmod 755 "${work}/bin/systemctl"
export PATH="${work}/bin:${PATH}"
export KEEP_LAUNCHER=true
expect_failure 'rollback leaving the candidate launcher' rollback 0.6.0
unset KEEP_LAUNCHER
bash "${checker}" rollback "${work}/evidence" 0.6.0 "${arch}"
echo 'Relay runtime and unit authenticity, state preservation, retry/rejection and legacy rollback contracts passed'
