#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKUP_DIR="${BACKUP_DIR:-/var/lib/ocservia-agent/upgrade-backup}"

if [[ ${EUID} -ne 0 ]]; then
  echo "upgrade-agent.sh must run as root" >&2
  exit 1
fi

install -d -o root -g root -m 0700 "${BACKUP_DIR}"
for binary in ocservia-agent ocservia-privd; do
  if [[ -f "/usr/libexec/ocservia/${binary}" ]]; then
    install -m 0755 "/usr/libexec/ocservia/${binary}" "${BACKUP_DIR}/${binary}.previous"
  fi
done

"${ROOT}/scripts/install-agent.sh"
systemctl try-restart ocservia-privd.service ocservia-agent.service
