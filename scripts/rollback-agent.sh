#!/usr/bin/env bash
set -euo pipefail

DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr}"
STATE_DIR="${STATE_DIR:-/var/lib/ocservia-agent}"
BACKUP_DIR="${BACKUP_DIR:-${DESTDIR}${STATE_DIR}/upgrade-backup}"

if [[ ${EUID} -ne 0 ]]; then
  echo "rollback-agent.sh must run as root" >&2
  exit 1
fi
if [[ -n "${DESTDIR}" && ( "${DESTDIR}" != /* || "${DESTDIR}" == "/" ) ]] || \
  [[ "${PREFIX}" != /* || "${STATE_DIR}" != /* || "${BACKUP_DIR}" != /* ]]; then
  echo "DESTDIR, PREFIX, STATE_DIR, and BACKUP_DIR must identify absolute paths" >&2
  exit 2
fi

required_backups=(
  ocservia-agent.previous
  ocservia-privd.previous
  ocservia-agent.service.previous
  ocservia-privd.service.previous
)
for backup in "${required_backups[@]}"; do
  path="${BACKUP_DIR}/${backup}"
  if [[ ! -f "${path}" || -L "${path}" ]]; then
    echo "matched rollback snapshot is incomplete: ${backup}" >&2
    exit 1
  fi
done

relay_backup="${BACKUP_DIR}/ocservia-agent-relays.conf.previous"
relay_absent="${BACKUP_DIR}/ocservia-agent-relays.conf.absent"
for relay_state in "${relay_backup}" "${relay_absent}"; do
  if [[ -e "${relay_state}" || -L "${relay_state}" ]] && \
    [[ ! -f "${relay_state}" || -L "${relay_state}" ]]; then
    echo "matched rollback snapshot contains an unsafe relay state file" >&2
    exit 1
  fi
done
if [[ -f "${relay_backup}" && ! -L "${relay_backup}" ]]; then
  restore_relay=true
else
  restore_relay=false
fi
if [[ -f "${relay_absent}" && ! -L "${relay_absent}" ]]; then
  remove_relay=true
else
  remove_relay=false
fi
if [[ "${restore_relay}" == "${remove_relay}" ]]; then
  echo "matched rollback snapshot has ambiguous relay drop-in state" >&2
  exit 1
fi

if [[ -z "${DESTDIR}" ]]; then
  systemctl stop ocservia-agent.service ocservia-privd.service
fi

libexec="${DESTDIR}${PREFIX}/libexec/ocservia"
systemd="${DESTDIR}${PREFIX}/lib/systemd/system"
relay_directory="${systemd}/ocservia-agent.service.d"
install -d -m 0755 "${libexec}" "${systemd}"
install -m 0755 "${BACKUP_DIR}/ocservia-agent.previous" "${libexec}/ocservia-agent"
install -m 0755 "${BACKUP_DIR}/ocservia-privd.previous" "${libexec}/ocservia-privd"
install -m 0644 "${BACKUP_DIR}/ocservia-agent.service.previous" \
  "${systemd}/ocservia-agent.service"
install -m 0644 "${BACKUP_DIR}/ocservia-privd.service.previous" \
  "${systemd}/ocservia-privd.service"
if [[ "${restore_relay}" == true ]]; then
  install -d -m 0755 "${relay_directory}"
  install -m 0644 "${relay_backup}" "${relay_directory}/10-production-relays.conf"
else
  rm -f -- "${relay_directory}/10-production-relays.conf"
  rmdir "${relay_directory}" 2>/dev/null || true
fi

if [[ -z "${DESTDIR}" ]]; then
  systemctl daemon-reload
  systemctl start ocservia-privd.service
  systemctl start ocservia-agent.service
fi
echo "Agent, privd, and systemd units restored from one matched rollback snapshot"
