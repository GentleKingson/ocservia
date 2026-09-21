#!/usr/bin/env bash
# Assertions shared by the real DEB host and RPM container, not an installer.
set -euo pipefail
mode="${1:?}"; evidence="${2:?}"; version="${3:?}"; arch="${4:?}"
mkdir -p "${evidence}"
state() {
  local path
  for path in /etc/ocservia-agent/agent.env /etc/ocservia-agent/controller-command-verification-key.pem \
    /etc/ocservia/release-signing.pub.pem /etc/ocservia/trusted-release-key.sha256 \
    /etc/ocservia-agent/user-password-seal-private.pem /etc/ocservia-agent/p12-password-seal-private.pem \
    /etc/ocservia-agent/relays.env /etc/ocservia-agent/relay-access-token /etc/ocservia-agent/relay-ca.pem \
    /var/lib/ocservia-agent/identity/identity-sentinel \
    /var/lib/ocservia-agent/identity/endpoint.key /var/lib/ocservia-agent/identity/controller.endpoint; do
    test -f "${path}"
    sha256sum "${path}"
    stat -c '%n %U:%G %a' "${path}"
  done
}
# Package-owned Relay files may change across releases; operator state may not.
relay_runtime() {
  local path mode
  for path in /usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf \
    /usr/libexec/ocservia/ocservia-agent-relays; do
    mode=644
    if [[ "${path}" == /usr/libexec/ocservia/ocservia-agent-relays ]]; then
      mode=755
      if [[ ! -e "${path}" && ! -L "${path}" ]]; then
        printf 'absent %s\n' "${path}"
        continue
      fi
    fi
    [[ -f "${path}" && ! -L "${path}" ]]
    [[ "$(stat -c '%u:%g:%a' "${path}")" == "0:0:${mode}" ]]
    sha256sum "${path}"
    stat -c '%n %U:%G %a' "${path}"
  done
}
package_units() {
  local unit path
  for unit in ocservia-agent.service ocservia-privd.service; do
    path="/usr/lib/systemd/system/${unit}"
    [[ -f "${path}" && ! -L "${path}" ]]
    [[ "$(stat -c '%u:%g:%a' "${path}")" == 0:0:644 ]]
    sha256sum "${path}"
  done
}
verify_candidate_relays() (
  package=/usr/share/ocservia-agent
  archive="${package}/ocservia-agent-${version}-linux-${arch}.tar.gz"
  fingerprint="$(cat "${package}/trusted-release-key.sha256")"
  verified="$(AGENT_TRUSTED_KEY_SHA256="${fingerprint}" "${package}/verify-agent-package.sh" \
    "${archive}" "${archive}.sha256" "${archive}.sha256.sig" "${package}/release-signing.pub.pem")"
  trap 'rm -rf -- "${verified%%/extracted/*}"' EXIT
  cmp "${verified}/deploy/production/systemd/ocservia-agent-relays.conf" \
    /usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf
  cmp "${verified}/deploy/production/systemd/agent-relays.sh" \
    /usr/libexec/ocservia/ocservia-agent-relays
  for unit in ocservia-agent.service ocservia-privd.service; do
    cmp "${verified}/deploy/systemd/${unit}" "/usr/lib/systemd/system/${unit}"
  done
)
binaries() {
  local name machine
  for name in ocservia-agent ocservia-privd ocservia-upgrader; do
    case "${arch}" in amd64) machine='x86-64' ;; arm64) machine='ARM aarch64' ;; *) exit 2 ;; esac
    file "/usr/libexec/ocservia/${name}" | grep -F "${machine}"
    [[ "$("/usr/libexec/ocservia/${name}" --version)" == "${name} ${version}" ]]
  done
}
case "${mode}" in
  before)
    test ! -e /etc/ocservia/release-signing.pub.pem
    install -d -o root -g root -m 755 /etc/ocservia
    install -o root -g root -m 644 /usr/share/ocservia-agent/release-signing.pub.pem /etc/ocservia/release-signing.pub.pem
    install -o root -g root -m 600 /usr/share/ocservia-agent/trusted-release-key.sha256 /etc/ocservia/trusted-release-key.sha256
    test ! -e /etc/ocservia-agent/relay-ca.pem
    test ! -L /etc/ocservia-agent/relay-ca.pem
    openssl req -new -x509 -newkey ed25519 -nodes -days 1 -subj /CN=upgrade-relay-ca \
      -addext basicConstraints=critical,CA:TRUE -keyout /dev/null -out /etc/ocservia-agent/relay-ca.pem
    chmod 444 /etc/ocservia-agent/relay-ca.pem
    controller="$(sed -n 's/^CONTROLLER_ENDPOINT_ID=//p' /etc/ocservia-agent/agent.env)"
    runuser -u ocserv-agent -- /usr/libexec/ocservia/ocservia-agent \
      --identity-dir /var/lib/ocservia-agent/identity --controller "${controller}" --prepare-enrollment >"${evidence}/endpoint-id"
    state >"${evidence}/state.before"
    relay_runtime >"${evidence}/relay-runtime.before"
    package_units >"${evidence}/units.before"
    cp /usr/lib/systemd/system/ocservia-privd.service "${evidence}/privd.before"
    sha256sum /usr/libexec/ocservia/ocservia-{agent,privd,upgrader} >"${evidence}/binaries.before"
    binaries
    ;;
  after)
    state >"${evidence}/state.after"
    cmp "${evidence}/state.before" "${evidence}/state.after"
    verify_candidate_relays
    relay_runtime >"${evidence}/relay-runtime.after"
    package_units >"${evidence}/units.after"
    cp /usr/lib/systemd/system/ocservia-privd.service "${evidence}/privd.after"
    cmp "${evidence}/privd.before" /var/lib/ocservia-upgrade/upgrade-backup/ocservia-privd.service.previous
    binaries
    sha256sum /var/lib/ocservia-upgrade/upgrade-backup/* >"${evidence}/snapshot.before-retry"
    ;;
  retry)
    state >"${evidence}/state.retry"
    cmp "${evidence}/state.before" "${evidence}/state.retry"
    relay_runtime >"${evidence}/relay-runtime.retry"
    cmp "${evidence}/relay-runtime.after" "${evidence}/relay-runtime.retry"
    package_units >"${evidence}/units.retry"
    cmp "${evidence}/units.after" "${evidence}/units.retry"
    sha256sum -c "${evidence}/snapshot.before-retry"
    binaries
    ;;
  reject)
    package=/usr/share/ocservia-agent
    archive="${package}/ocservia-agent-${version}-linux-${arch}.tar.gz"
    fingerprint="$(cat "${package}/trusted-release-key.sha256")"
    mkdir "${evidence}/corrupt"
    corrupted="${evidence}/corrupt/$(basename "${archive}")"
    cp "${archive}" "${corrupted}"
    printf damaged >>"${corrupted}"
    if AGENT_TRUSTED_KEY_SHA256="${fingerprint}" "${package}/verify-agent-package.sh" \
      "${corrupted}" "${archive}.sha256" "${archive}.sha256.sig" "${package}/release-signing.pub.pem" \
      >"${evidence}/corrupt.log" 2>&1; then echo 'corrupted package accepted' >&2; exit 1; fi
    grep -Ei 'checksum|digest|sha256' "${evidence}/corrupt.log"
    rm "${corrupted}"
    rmdir "${evidence}/corrupt"
    verified="$(AGENT_TRUSTED_KEY_SHA256="${fingerprint}" "${package}/verify-agent-package.sh" \
      "${archive}" "${archive}.sha256" "${archive}.sha256.sig" "${package}/release-signing.pub.pem")"
    command_key=/etc/ocservia-agent/controller-command-verification-key.pem
    saved_mode="$(stat -c %a "${command_key}")"
    trap 'chmod "${saved_mode}" "${command_key}"; rm -rf -- "${verified%%/extracted/*}"' EXIT
    sha256sum /usr/libexec/ocservia/ocservia-{agent,privd,upgrader} >"${evidence}/precondition.before"
    chmod 0666 "${command_key}"
    if INSTALL_PRODUCTION_RELAYS=true "${verified}/scripts/upgrade-agent.sh" >"${evidence}/precondition.log" 2>&1; then
      echo 'unsafe trust accepted' >&2; exit 1
    fi
    grep -F 'blocked before modification' "${evidence}/precondition.log"
    chmod "${saved_mode}" "${command_key}"
    sha256sum -c "${evidence}/precondition.before"
    sha256sum -c "${evidence}/snapshot.before-retry"
    state >"${evidence}/state.rejected"
    cmp "${evidence}/state.before" "${evidence}/state.rejected"
    relay_runtime >"${evidence}/relay-runtime.rejected"
    cmp "${evidence}/relay-runtime.after" "${evidence}/relay-runtime.rejected"
    ;;
  rollback)
    /usr/libexec/ocservia/ocservia-agent-rollback
    sha256sum -c "${evidence}/binaries.before"
    binaries
    state >"${evidence}/state.rollback"
    cmp "${evidence}/state.before" "${evidence}/state.rollback"
    relay_runtime >"${evidence}/relay-runtime.rollback"
    cmp "${evidence}/relay-runtime.before" "${evidence}/relay-runtime.rollback"
    package_units >"${evidence}/units.rollback"
    cmp "${evidence}/units.before" "${evidence}/units.rollback"
    # Rollback requests restarts; this fixture does not prove online reporting.
    systemctl stop ocservia-agent.service ocservia-privd.service
    ;;
  *) exit 2 ;;
esac
