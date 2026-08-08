#!/usr/bin/env bash
set -euo pipefail

DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr}"
SYSCONFDIR="${SYSCONFDIR:-/etc}"
STATE_DIR="${STATE_DIR:-/var/lib/ocservia-agent}"
PURGE_STATE=false
if [[ ${1:-} == "--purge-state" ]]; then
  PURGE_STATE=true
elif [[ $# -ne 0 ]]; then
  echo "usage: uninstall-agent.sh [--purge-state]" >&2
  exit 2
fi

if [[ ${EUID} -ne 0 ]]; then
  echo "uninstall-agent.sh must run as root" >&2
  exit 1
fi
if [[ -n "${DESTDIR}" && ( "${DESTDIR}" != /* || "${DESTDIR}" == "/" ) ]]; then
  echo "DESTDIR must be an absolute staging root" >&2
  exit 2
fi

if [[ -z "${DESTDIR}" ]]; then
  systemctl disable --now ocservia-agent.service ocservia-privd.service 2>/dev/null || true
fi
rm -f "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service" \
  "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-privd.service" \
  "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf"
rmdir "${DESTDIR}${PREFIX}/lib/systemd/system/ocservia-agent.service.d" 2>/dev/null || true
rm -f "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-agent" "${DESTDIR}${PREFIX}/libexec/ocservia/ocservia-privd"
rmdir "${DESTDIR}${PREFIX}/libexec/ocservia" 2>/dev/null || true
if [[ -z "${DESTDIR}" ]]; then
  systemctl daemon-reload
fi

if [[ ${PURGE_STATE} == true ]]; then
  rm -rf "${DESTDIR}${STATE_DIR}" "${DESTDIR}/var/lib/ocservia-privd" "${DESTDIR}${SYSCONFDIR}/ocservia-agent"
  if [[ -z "${DESTDIR}" ]]; then
    userdel ocserv-agent 2>/dev/null || true
    groupdel ocserv-agent 2>/dev/null || true
  fi
fi
