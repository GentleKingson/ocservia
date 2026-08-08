#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr}"
SYSCONFDIR="${SYSCONFDIR:-/etc}"
STATE_DIR="${STATE_DIR:-/var/lib/ocservia-agent}"
AGENT_UID="${AGENT_UID:-997}"
AGENT_GID="${AGENT_GID:-997}"
INSTALL_PRODUCTION_RELAYS="${INSTALL_PRODUCTION_RELAYS:-false}"

if [[ ${EUID} -ne 0 ]]; then
  echo "install-agent.sh must run as root" >&2
  exit 1
fi

if [[ -z "${DESTDIR}" ]]; then
  if ! getent group ocserv-agent >/dev/null; then
    groupadd --system ocserv-agent
  fi
  if ! getent passwd ocserv-agent >/dev/null; then
    useradd --system --gid ocserv-agent --home-dir "${STATE_DIR}" --shell /usr/sbin/nologin ocserv-agent
  fi
  agent_owner=ocserv-agent
  agent_group=ocserv-agent
  AGENT_UID="$(id -u ocserv-agent)"
else
  if [[ "${DESTDIR}" != /* || "${DESTDIR}" == "/" ]] || ! [[ "${AGENT_UID}" =~ ^[0-9]+$ && "${AGENT_GID}" =~ ^[0-9]+$ ]]; then
    echo "DESTDIR must be an absolute staging root and numeric agent IDs are required" >&2
    exit 2
  fi
  agent_owner="${AGENT_UID}"
  agent_group="${AGENT_GID}"
fi

install -d -m 0755 "${DESTDIR}${PREFIX}/libexec/ocservia" "${DESTDIR}${PREFIX}/lib/systemd/system"
install -d -o "${agent_owner}" -g "${agent_group}" -m 0700 "${DESTDIR}${STATE_DIR}" "${DESTDIR}${STATE_DIR}/identity"
install -d -o root -g "${agent_group}" -m 0750 "${DESTDIR}${SYSCONFDIR}/ocservia-agent"
install -m 0755 "${ROOT}/rust/target/release/ocservia-agent" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-agent"
install -m 0755 "${ROOT}/rust/target/release/ocservia-privd" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-privd"
install -m 0644 "${ROOT}/deploy/systemd/ocservia-agent.service" "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service"
install -m 0644 "${ROOT}/deploy/systemd/ocservia-privd.service" "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-privd.service"

printf 'AGENT_UID=%s\n' "${AGENT_UID}" >"${DESTDIR}${SYSCONFDIR}/ocservia-agent/privd.env"
chmod 0640 "${DESTDIR}${SYSCONFDIR}/ocservia-agent/privd.env"
chown root:"${agent_group}" "${DESTDIR}${SYSCONFDIR}/ocservia-agent/privd.env"
if [[ ! -e "${DESTDIR}${SYSCONFDIR}/ocservia-agent/agent.env" ]]; then
  install -o root -g "${agent_group}" -m 0640 "${ROOT}/deploy/systemd/agent.env.example" "${DESTDIR}${SYSCONFDIR}/ocservia-agent/agent.env"
fi

if [[ "${INSTALL_PRODUCTION_RELAYS}" == true ]]; then
  install -d -m 0755 "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service.d"
  install -m 0644 "${ROOT}/deploy/production/systemd/ocservia-agent-relays.conf" \
    "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
  if [[ ! -e "${DESTDIR}${SYSCONFDIR}/ocservia-agent/relays.env" ]]; then
    install -o root -g "${agent_group}" -m 0640 "${ROOT}/deploy/production/systemd/relays.env.example" \
      "${DESTDIR}${SYSCONFDIR}/ocservia-agent/relays.env"
  fi
fi

if [[ -z "${DESTDIR}" ]]; then
  systemctl daemon-reload
fi
