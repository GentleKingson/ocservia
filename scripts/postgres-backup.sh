#!/usr/bin/env bash
set -euo pipefail

BACKUP_ROOT="${BACKUP_ROOT:-/var/lib/ocservia-backup}"
BACKUP_INTERVAL_SECONDS="${BACKUP_INTERVAL_SECONDS:-900}"
BACKUP_RETENTION_COUNT="${BACKUP_RETENTION_COUNT:-8}"
POSTGRES_SERVER_MAJOR="${POSTGRES_SERVER_MAJOR:-18}"
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
if [[ "${POSTGRES_SERVER_MAJOR}" != 18 ]]; then
  echo "POSTGRES_SERVER_MAJOR must be 18" >&2
  exit 2
fi

umask 077
validate_backup_dir() {
  local path="$1" dir_uid dir_mode
  if [[ -L "${path}" || ! -d "${path}" ]]; then
    echo "backup path must be a real directory: ${path}" >&2
    exit 1
  fi
  IFS=: read -r dir_uid dir_mode < <(stat -c '%u:%a' "${path}")
  if [[ "${dir_uid}" != "$(id -u)" || "${dir_mode}" != 700 ]]; then
    echo "backup path ${path} must be owned by the backup user with mode 0700" >&2
    exit 1
  fi
}
validate_backup_dir "${BACKUP_ROOT}"
if [[ ! -e "${BACKUP_ROOT}/base" && ! -L "${BACKUP_ROOT}/base" ]]; then
  mkdir -m 0700 -- "${BACKUP_ROOT}/base"
fi
validate_backup_dir "${BACKUP_ROOT}/base"

run_backup() (
  local lock="${BACKUP_ROOT}/.backup.lock"
  local timestamp staging final latest_tmp server_version retained
  local staging_created=false latest_created=false backups
  if [[ -L "${lock}" ]]; then
    echo "backup lock must not be a symlink" >&2
    return 1
  fi
  if [[ -d "${lock}" ]] && ! rmdir "${lock}" 2>/dev/null; then
    echo "legacy backup lock directory is not empty" >&2
    return 1
  fi
  exec 9>"${lock}" || return 1
  if ! flock -n 9; then
    echo "another backup is active" >&2
    return 1
  fi
  timestamp="$(date -u +%Y%m%dT%H%M%SZ)" || return 1
  staging="${BACKUP_ROOT}/base/.${timestamp}-${RUN_ID}.tmp"
  final="${BACKUP_ROOT}/base/${timestamp}"
  latest_tmp="${BACKUP_ROOT}/.LATEST-${RUN_ID}.tmp"
  cleanup_backup() {
    if [[ "${staging_created}" == true ]]; then
      rm -rf -- "${staging}"
    fi
    if [[ "${latest_created}" == true ]]; then
      rm -f -- "${latest_tmp}"
    fi
  }
  trap cleanup_backup EXIT

  if [[ -e "${staging}" || -L "${staging}" ]]; then
    echo "backup staging path already exists: ${staging}" >&2
    return 1
  fi
  if [[ -e "${final}" || -L "${final}" ]]; then
    echo "backup destination already exists: ${final}" >&2
    return 1
  fi
  if [[ -L "${BACKUP_ROOT}/LATEST" || ( -e "${BACKUP_ROOT}/LATEST" && ! -f "${BACKUP_ROOT}/LATEST" ) ]]; then
    echo "LATEST marker must be a regular file when present" >&2
    return 1
  fi
  if [[ -e "${latest_tmp}" || -L "${latest_tmp}" ]]; then
    echo "temporary LATEST path already exists: ${latest_tmp}" >&2
    return 1
  fi

  server_version="$(psql --no-password --tuples-only --no-align --command 'SHOW server_version_num')" || return 1
  if [[ ! "${server_version}" =~ ^[0-9]+$ ]] || (( server_version / 10000 != POSTGRES_SERVER_MAJOR )); then
    echo "PostgreSQL server must be major version ${POSTGRES_SERVER_MAJOR}" >&2
    return 1
  fi

  mkdir -m 0700 -- "${staging}" || return 1
  staging_created=true
  # Plain staging lets pg_verifybackup check both data and streamed WAL.
  pg_basebackup \
    --pgdata="${staging}/data" \
    --format=plain \
    --wal-method=stream \
    --checkpoint=spread \
    --manifest-checksums=SHA256 \
    --no-password || return 1
  pg_verifybackup "${staging}/data" || return 1
  tar -czf "${staging}/base.tar.gz" -C "${staging}/data" . || return 1
  # Keep the WAL start readable by the previous backup worker after rollback.
  cp -- "${staging}/data/backup_label" "${staging}/backup_label" || return 1
  rm -rf -- "${staging}/data" || return 1
  printf '%s\n' "${timestamp}" >"${staging}/OCSERVIA_BACKUP_ID" || return 1
  sync "${staging}/base.tar.gz" "${staging}/backup_label" "${staging}/OCSERVIA_BACKUP_ID" "${staging}" || return 1
  mv -T -- "${staging}" "${final}" || return 1
  staging_created=false
  sync "${BACKUP_ROOT}/base" || return 1
  latest_created=true
  printf '%s\n' "${timestamp}" >"${latest_tmp}" || return 1
  sync "${latest_tmp}" || return 1
  mv -T -- "${latest_tmp}" "${BACKUP_ROOT}/LATEST" || return 1
  latest_created=false
  sync "${BACKUP_ROOT}" || return 1

  retained="$(find "${BACKUP_ROOT}/base" -regextype posix-extended -mindepth 1 -maxdepth 1 -type d \
    -regex '.*/20[0-9]{6}T[0-9]{6}Z' ! -name "${timestamp}" -printf '%f\n' | sort -r)" || return 1
  if [[ -n "${retained}" ]]; then
    mapfile -t backups <<<"${retained}"
    if (( ${#backups[@]} >= BACKUP_RETENTION_COUNT )); then
      printf '%s\0' "${backups[@]:BACKUP_RETENTION_COUNT-1}" |
        xargs -0r -I '{}' rm -rf -- "${BACKUP_ROOT}/base/{}" || return 1
    fi
  fi
  echo "backup completed id=${timestamp}"
)

if [[ "${1:-}" == "--once" ]]; then
  run_backup
  exit
fi
if [[ $# -ne 0 ]]; then
  echo "usage: postgres-backup.sh [--once]" >&2
  exit 2
fi
while true; do
  if ! run_backup; then
    echo "backup attempt failed; retrying in ${BACKUP_INTERVAL_SECONDS} seconds" >&2
  fi
  sleep "${BACKUP_INTERVAL_SECONDS}"
done
