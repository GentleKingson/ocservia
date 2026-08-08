#!/usr/bin/env bash
set -euo pipefail

BACKUP_ROOT="${BACKUP_ROOT:-/var/lib/ocservia-backup}"
BACKUP_INTERVAL_SECONDS="${BACKUP_INTERVAL_SECONDS:-900}"
BACKUP_RETENTION_COUNT="${BACKUP_RETENTION_COUNT:-8}"
RUN_ID="${RUN_ID:-backup-$$}"

if [[ "${BACKUP_ROOT}" != /* || "${RUN_ID}" == *[^a-zA-Z0-9._-]* ]]; then
  echo "backup root must be absolute and RUN_ID must be safe" >&2
  exit 2
fi
if ! [[ "${BACKUP_INTERVAL_SECONDS}" =~ ^[0-9]+$ ]] || (( BACKUP_INTERVAL_SECONDS < 60 || BACKUP_INTERVAL_SECONDS > 86400 )); then
  echo "BACKUP_INTERVAL_SECONDS must be 60..86400" >&2
  exit 2
fi
if ! [[ "${BACKUP_RETENTION_COUNT}" =~ ^[0-9]+$ ]] || (( BACKUP_RETENTION_COUNT < 1 || BACKUP_RETENTION_COUNT > 128 )); then
  echo "BACKUP_RETENTION_COUNT must be 1..128" >&2
  exit 2
fi

umask 077
mkdir -p "${BACKUP_ROOT}/base" "${BACKUP_ROOT}/wal"
for path in "${BACKUP_ROOT}" "${BACKUP_ROOT}/base" "${BACKUP_ROOT}/wal"; do
  if [[ -L "${path}" || ! -d "${path}" ]]; then
    echo "backup path must be a real directory: ${path}" >&2
    exit 1
  fi
done

run_backup() {
  local lock="${BACKUP_ROOT}/.backup.lock"
  local timestamp staging final latest_tmp oldest_wal
  if [[ -d "${lock}" ]] && ! rmdir "${lock}" 2>/dev/null; then
    echo "legacy backup lock directory is not empty" >&2
    return 1
  fi
  exec 9>"${lock}"
  if ! flock -n 9; then
    echo "another backup is active" >&2
    return 1
  fi
  timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
  staging="${BACKUP_ROOT}/base/.${timestamp}-${RUN_ID}.tmp"
  final="${BACKUP_ROOT}/base/${timestamp}"
  latest_tmp="${BACKUP_ROOT}/.LATEST-${RUN_ID}.tmp"
  cleanup_backup() {
    rm -rf -- "${staging}"
    rm -f -- "${latest_tmp}"
    flock -u 9 || true
    exec 9>&-
  }
  trap cleanup_backup RETURN

  if [[ -e "${final}" ]]; then
    echo "backup destination already exists: ${final}" >&2
    return 1
  fi
  pg_basebackup \
    --pgdata="${staging}" \
    --format=plain \
    --wal-method=stream \
    --checkpoint=fast \
    --manifest-checksums=SHA256 \
    --no-password
  pg_verifybackup "${staging}"
  printf '%s\n' "${timestamp}" >"${staging}/OCSERVIA_BACKUP_ID"
  mv -- "${staging}" "${final}"
  printf '%s\n' "${timestamp}" >"${latest_tmp}"
  mv -- "${latest_tmp}" "${BACKUP_ROOT}/LATEST"

  mapfile -t backups < <(find "${BACKUP_ROOT}/base" -mindepth 1 -maxdepth 1 -type d -name '20??????T??????Z' -print | sort -r)
  if (( ${#backups[@]} > BACKUP_RETENTION_COUNT )); then
    printf '%s\0' "${backups[@]:BACKUP_RETENTION_COUNT}" | xargs -0r rm -rf --
  fi
  mapfile -t backups < <(find "${BACKUP_ROOT}/base" -mindepth 1 -maxdepth 1 -type d -name '20??????T??????Z' -print | sort)
  oldest_wal="$(sed -n 's/^START WAL LOCATION:.*(file \([0-9A-F]\{24\}\)).*/\1/p' "${backups[0]}/backup_label")"
  if [[ ! "${oldest_wal}" =~ ^[0-9A-F]{24}$ ]]; then
    echo "oldest retained backup has no valid WAL start" >&2
    return 1
  fi
  pg_archivecleanup "${BACKUP_ROOT}/wal" "${oldest_wal}"
  echo "backup completed id=${timestamp}"
}

if [[ "${1:-}" == "--once" ]]; then
  run_backup
  exit
fi
if [[ $# -ne 0 ]]; then
  echo "usage: postgres-backup.sh [--once]" >&2
  exit 2
fi
while true; do
  run_backup
  sleep "${BACKUP_INTERVAL_SECONDS}"
done
