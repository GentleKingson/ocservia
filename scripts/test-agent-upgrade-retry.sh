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
cp "${ROOT}/scripts/"{rebind-agent,retain-agent}.py "${work}/source/scripts/"
export DESTDIR="${work}/rootfs" AGENT_UID=61000 AGENT_GID=61000 INSTALL_PRODUCTION_RELAYS=true
export OUTPUT_DIR="${work}/products" SOURCE_DATE_EPOCH=1786147200
case "$(uname -m)" in aarch64) export PACKAGE_ARCH=arm64 ;; x86_64) export PACKAGE_ARCH=amd64 ;; *) exit 2 ;; esac
openssl genpkey -algorithm ED25519 -out "${work}/controller-command.key" >/dev/null 2>&1
openssl pkey -in "${work}/controller-command.key" -pubout -out "${work}/public.pem" >/dev/null 2>&1
lifecycle_files=(
  'scripts/rebind-agent.py:usr/libexec/ocservia/ocservia-agent-rebind:755'
  'scripts/retain-agent.py:usr/libexec/ocservia/ocservia-agent-retention:755'
  'deploy/systemd/ocservia-agent-retention.service:usr/lib/systemd/system/ocservia-agent-retention.service:644'
  'deploy/systemd/ocservia-agent-retention.timer:usr/lib/systemd/system/ocservia-agent-retention.timer:644'
)
package() {
  local version="$1" binary archive entry source destination mode
  for entry in "${lifecycle_files[@]}"; do
    IFS=: read -r source destination mode <<<"${entry}"
    printf '\n# lifecycle fixture %s\n' "${version}" >>"${work}/source/${source}"
  done
  for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
    # shellcheck disable=SC2016 # Expanded by the installed fixture executable.
    printf '#!/bin/sh\nif [ "${1:-}" = --binding-version ]; then echo 1; else echo "%s %s"; fi\n' \
      "${binary}" "${version}" >"${work}/source/rust/target/release/${binary}"
    chmod 755 "${work}/source/rust/target/release/${binary}"
  done
  VERSION="${version}" bash "${work}/source/scripts/package-agent.sh" >/dev/null
  archive="${OUTPUT_DIR}/ocservia-agent-${version}-linux-${PACKAGE_ARCH}.tar.gz"
  bash "${ROOT}/scripts/verify-agent-package.sh" "${archive}" "$(awk '{print $1}' "${archive}.sha256")"
}
assert_lifecycle_matches() {
  local package_root="$1" entry source destination mode
  for entry in "${lifecycle_files[@]}"; do
    IFS=: read -r source destination mode <<<"${entry}"
    cmp "${package_root}/${source}" "${DESTDIR}/${destination}"
    test "$(stat -c '%u:%g:%a:%h' "${DESTDIR}/${destination}")" = "0:0:${mode}:1"
  done
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
printf 'ocservia-binding-v1\n018f0c2e-7b1a-7c3d-8e9f-0123456789ab\n%s\n%s\n%s\nclear\n' \
  "$(printf '11%.0s' {1..32})" "$(printf '22%.0s' {1..32})" "$(printf '33%.0s' {1..32})" >"${config}/active-binding"
chmod 640 "${config}/active-binding"
printf 'identity and command evidence must survive\n' >"${DESTDIR}/var/lib/ocservia-agent/identity/evidence"
sha256sum "${config}/active-binding" "${DESTDIR}/var/lib/ocservia-agent/identity/evidence" >"${work}/binding-evidence"
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
assert_lifecycle_matches "${old}"
"${new}/scripts/upgrade-agent.sh"
cmp "${new}/rust/target/release/ocservia-agent" "${DESTDIR}/usr/libexec/ocservia/ocservia-agent"
sha256sum -c "${work}/snapshot"
echo 'Identical-package retry preserves rollback; rollback/re-upgrade still works (installer unit test only)'

# The expected quartet is independent of the snapshot's manifest. Different
# package comments above make an omitted restore observable even with stubs.
assert_lifecycle_matches "${new}"
test "$(wc -l <"${snapshot}/MANIFEST.sha256")" -eq 13
cp -a "${snapshot}" "${work}/valid-snapshot"
restore_snapshot() {
  rm -rf -- "${snapshot}"
  cp -a "${work}/valid-snapshot" "${snapshot}"
}
fixture_manifest() {
  (cd "${snapshot}" && find . -maxdepth 1 -type f \( -name '*.previous' -o -name '*.absent' \) \
    -printf '%f\n' | sort | xargs sha256sum) >"${snapshot}/MANIFEST.sha256"
}
record_files() {
  find "${DESTDIR}" -printf '%y %p %u:%g:%m:%n %l\n' | sort
  find "${DESTDIR}" -type f -exec sha256sum {} + | sort
}
assert_rollback_rejected() {
  local label="$1" expected="$2"
  record_files >"${work}/before-rollback-rejection"
  if "${new}/scripts/rollback-agent.sh" >"${work}/rollback-rejection.log" 2>&1; then
    echo "unsafe rollback accepted: ${label}" >&2; exit 1
  fi
  grep -F "${expected}" "${work}/rollback-rejection.log"
  record_files >"${work}/after-rollback-rejection"
  cmp "${work}/before-rollback-rejection" "${work}/after-rollback-rejection"
}
for entry in "${lifecycle_files[@]}"; do
  IFS=: read -r source destination mode <<<"${entry}"
  name="$(basename "${destination}")"
  for damage in digest missing missing-record symlink hardlink mode duplicate absent; do
    restore_snapshot
    member="${snapshot}/${name}.previous"
    case "${damage}" in
      digest) printf 'tampered\n' >>"${member}" ;;
      missing) rm "${member}" ;;
      missing-record) sed -i "/  ${name}\.previous\$/d" "${snapshot}/MANIFEST.sha256" ;;
      symlink) rm "${member}"; ln -s "${work}/valid-snapshot/${name}.previous" "${member}" ;;
      hardlink) ln "${member}" "${snapshot}/alias" ;;
      mode) chmod 777 "${member}" ;;
      duplicate)
        awk 'NR == 1 { first=$0 } NR == 2 { $0=first } { print }' "${snapshot}/MANIFEST.sha256" >"${work}/duplicate-manifest"
        install -m 600 "${work}/duplicate-manifest" "${snapshot}/MANIFEST.sha256" ;;
      absent) install -m 600 /dev/null "${snapshot}/${name}.absent" ;;
    esac
    assert_rollback_rejected "${name}-${damage}" 'Agent rollback blocked before modification:'
  done
  restore_snapshot
  # Destination checks must also protect files about to be removed.
  mv "${DESTDIR}/${destination}" "${work}/destination"
  ln -s "${work}/destination" "${DESTDIR}/${destination}"
  assert_rollback_rejected "${name}-destination-symlink" 'not a regular file'
  assert_retry_rejected "${name}-source-symlink" 'regular file'
  rm "${DESTDIR}/${destination}"
  mv "${work}/destination" "${DESTDIR}/${destination}"
  for damage in hardlink mode owner; do
    case "${damage}" in
      hardlink) ln "${DESTDIR}/${destination}" "${work}/destination-alias" ;;
      mode) chmod 777 "${DESTDIR}/${destination}" ;;
      owner) chown 61000:61000 "${DESTDIR}/${destination}" ;;
    esac
    assert_rollback_rejected "${name}-destination-${damage}" 'unsafe owner, group, mode, or link count'
    assert_retry_rejected "${name}-source-${damage}" 'unsafe owner, group, mode, or link count'
    rm -f "${work}/destination-alias"
    chmod "${mode}" "${DESTDIR}/${destination}"
    chown root:root "${DESTDIR}/${destination}"
  done
done
restore_snapshot
sed -i '1s|  .*|  ../../outside|' "${snapshot}/MANIFEST.sha256"
assert_rollback_rejected manifest-path 'manifest is malformed'
for dependency in ocservia-agent-rebind ocservia-agent-retention ocservia-agent-retention.service; do
  restore_snapshot
  rm "${snapshot}/${dependency}.previous"
  install -m 600 /dev/null "${snapshot}/${dependency}.absent"
  fixture_manifest
  assert_rollback_rejected "missing-${dependency}-dependency" 'requires'
done
for legacy_count in 9 8; do
  restore_snapshot
  for entry in "${lifecycle_files[@]}"; do
    IFS=: read -r source destination mode <<<"${entry}"
    rm "${snapshot}/$(basename "${destination}").previous"
  done
  if [[ "${legacy_count}" == 8 ]]; then
    rm "${snapshot}/ocservia-agent-relays.previous"
    printf '[Service]\nExecStart=\nExecStart=/usr/libexec/ocservia/ocservia-agent\n' \
      >"${snapshot}/ocservia-agent-relays.conf.previous"
  fi
  fixture_manifest
  test "$(wc -l <"${snapshot}/MANIFEST.sha256")" -eq "${legacy_count}"
  "${new}/scripts/rollback-agent.sh" --verify-only
  assert_rollback_rejected "legacy-${legacy_count}-unknown" 'does not record prior'
done
restore_snapshot
"${new}/scripts/rollback-agent.sh"
# Rebind alone remains a valid old layout; only dependency edges are required.
for layout in absent rebind-only; do
  for entry in "${lifecycle_files[@]}"; do
    IFS=: read -r source destination mode <<<"${entry}"
    rm -f "${DESTDIR}/${destination}"
  done
  if [[ "${layout}" == rebind-only ]]; then
    install -m 755 "${old}/scripts/rebind-agent.py" "${DESTDIR}/usr/libexec/ocservia/ocservia-agent-rebind"
  fi
  "${new}/scripts/upgrade-agent.sh"
  sha256sum "${snapshot}/"* >"${work}/optional-snapshot"
  "${new}/scripts/upgrade-agent.sh"
  sha256sum -c "${work}/optional-snapshot"
  if [[ "${layout}" == absent ]]; then
    destination="${DESTDIR}/usr/libexec/ocservia/ocservia-agent-retention"
    mv "${destination}" "${work}/removal-target"
    ln -s "${work}/removal-target" "${destination}"
    assert_rollback_rejected absent-destination-symlink 'not a regular file'
    rm "${destination}"
    mv "${work}/removal-target" "${destination}"
  fi
  driver_hash="$(sha256sum "${rollback}")"
  "${rollback}"
  test "$(sha256sum "${rollback}")" = "${driver_hash}"
  for entry in "${lifecycle_files[@]}"; do
    IFS=: read -r source destination mode <<<"${entry}"
    if [[ "${layout}" == rebind-only && "${destination}" == */ocservia-agent-rebind ]]; then
      cmp "${old}/${source}" "${DESTDIR}/${destination}"
    else
      test ! -e "${DESTDIR}/${destination}"
    fi
  done
done

# Fail inside real install-agent.sh after the new binaries were installed.
# Native/direct retries must preserve the first snapshot until verified recovery.
mkdir "${work}/fail-install"
cat >"${work}/fail-install/install" <<'EOF'
#!/bin/bash
[[ "${!#}" != "${DESTDIR}/usr/libexec/ocservia/${OCSERV_TEST_FAIL_TARGET}" ]] || exit 73
exec /usr/bin/install "$@"
EOF
chmod 755 "${work}/fail-install/install"
for fail_target in ocservia-agent ocservia-agent-retention; do
  "${old}/scripts/install-agent.sh"
  # Model an installed reader that cannot recover the new snapshot format.
  # The real candidate driver must be published before even the first binary.
  cat >"${rollback}" <<'EOF'
#!/bin/bash
case "$(wc -l <"${DESTDIR}/var/lib/ocservia-upgrade/upgrade-backup/MANIFEST.sha256")" in
  8|9) exit 0 ;;
  *) echo 'old snapshot reader cannot recover this format' >&2; exit 92 ;;
esac
EOF
  if (umask 0777; OCSERV_TEST_FAIL_TARGET="${fail_target}" PATH="${work}/fail-install:${PATH}" "${new}/scripts/upgrade-agent.sh"); then
    echo 'interrupted installation unexpectedly succeeded' >&2; exit 1
  fi
  test -f "${DESTDIR}/var/lib/ocservia-upgrade/installing-package"
  test "$(stat -c '%u:%g:%a:%h' "${DESTDIR}/var/lib/ocservia-upgrade/installing-package")" = 0:0:600:1
  cmp "${rollback}" "${new}/scripts/rollback-agent.sh"
  sha256sum "${snapshot}/"* >"${work}/interrupted-snapshot"
  assert_retry_rejected interrupted-install 'unfinished package installation requires verified rollback before retry'
  sha256sum -c "${work}/interrupted-snapshot"
  "${rollback}"
  test ! -e "${DESTDIR}/var/lib/ocservia-upgrade/installing-package"
  assert_lifecycle_matches "${old}"
done
"${new}/scripts/upgrade-agent.sh"
assert_lifecycle_matches "${new}"
pending="${DESTDIR}/var/lib/ocservia-upgrade/installing-package"
commit_record="${DESTDIR}/var/lib/ocservia-upgrade/installed-commit"
sha256sum "${snapshot}/"* >"${work}/completed-snapshot"
cp -p "${commit_record}" "${pending}"
chmod 640 "${pending}"
assert_retry_rejected pending-mode 'pending installation record is malformed or unsafe'
assert_rollback_rejected pending-mode 'pending installation record is malformed or unsafe'
chmod 600 "${pending}"
printf 'archive_sha256=%064d\n' 0 >"${pending}"
assert_retry_rejected pending-other-package 'unfinished package installation requires verified rollback before retry'
cp -p "${commit_record}" "${pending}"
"${new}/scripts/upgrade-agent.sh"
test ! -e "${pending}"
sha256sum -c "${work}/completed-snapshot"
sha256sum -c "${work}/binding-evidence"
echo 'Lifecycle quartet restore/remove, legacy unknown, unsafe snapshots and interrupted retry passed'

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
# Verify target contents, not capabilities inferred from live settings.
fixture="${work}/package-fixture"
mkdir -p "${fixture}"
cp -a "${new}" "${fixture}/ocservia-agent-1.0.1"
target="${fixture}/ocservia-agent-1.0.1"
rm "${target}/.ocservia-package-verified" "${target}/deploy/production/systemd/agent-relays.sh"
archive="${work}/ocservia-agent-1.0.1-linux-${PACKAGE_ARCH}.tar.gz"
verify_fixture() {
  tar -czf "${archive}" -C "${fixture}" ocservia-agent-1.0.1
  (cd "${work}" && sha256sum "$(basename "${archive}")") >"${archive}.sha256"
  bash "${ROOT}/scripts/verify-agent-package.sh" "${archive}" "$(awk '{print $1}' "${archive}.sha256")"
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
