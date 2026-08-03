#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr}"
SYSCONFDIR="${SYSCONFDIR:-/etc}"
STATE_DIR="${STATE_DIR:-/var/lib/ocservia-agent}"

if [[ ${EUID} -ne 0 ]]; then
  echo "install-agent.sh must run as root" >&2
  exit 1
fi

if ! getent group ocserv-agent >/dev/null; then
  groupadd --system ocserv-agent
fi
if ! getent passwd ocserv-agent >/dev/null; then
  useradd --system --gid ocserv-agent --home-dir "${STATE_DIR}" --shell /usr/sbin/nologin ocserv-agent
fi

install -d -m 0755 "${DESTDIR}${PREFIX}/libexec/ocservia" "${DESTDIR}${PREFIX}/lib/systemd/system"
install -d -o ocserv-agent -g ocserv-agent -m 0700 "${DESTDIR}${STATE_DIR}" "${DESTDIR}${STATE_DIR}/identity"
install -d -o root -g ocserv-agent -m 0750 "${DESTDIR}${SYSCONFDIR}/ocservia-agent"
install -m 0755 "${ROOT}/rust/target/release/ocservia-agent" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-agent"
install -m 0755 "${ROOT}/rust/target/release/ocservia-privd" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-privd"
install -m 0644 "${ROOT}/deploy/systemd/ocservia-agent.service" "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service"
install -m 0644 "${ROOT}/deploy/systemd/ocservia-privd.service" "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-privd.service"

agent_uid="$(id -u ocserv-agent)"
printf 'AGENT_UID=%s\n' "${agent_uid}" >"${DESTDIR}${SYSCONFDIR}/ocservia-agent/privd.env"
chmod 0640 "${DESTDIR}${SYSCONFDIR}/ocservia-agent/privd.env"
chown root:ocserv-agent "${DESTDIR}${SYSCONFDIR}/ocservia-agent/privd.env"
if [[ ! -e "${DESTDIR}${SYSCONFDIR}/ocservia-agent/agent.env" ]]; then
  install -o root -g ocserv-agent -m 0640 "${ROOT}/deploy/systemd/agent.env.example" "${DESTDIR}${SYSCONFDIR}/ocservia-agent/agent.env"
fi

if [[ -z "${DESTDIR}" ]]; then
  systemctl daemon-reload
fi
