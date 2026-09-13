#!/usr/bin/env bash
set -euo pipefail

BACKUP_ROOT="${BACKUP_ROOT:-/var/lib/ocservia-backup}"
BACKUP_INTERVAL_SECONDS="${BACKUP_INTERVAL_SECONDS:-900}"
BACKUP_RETENTION_COUNT="${BACKUP_RETENTION_COUNT:-8}"
DATABASE_BACKEND="${DATABASE_BACKEND:?DATABASE_BACKEND is required}"
MYSQL_DATABASE="${MYSQL_DATABASE:?MYSQL_DATABASE is required}"
MYSQL_CONFIG_FILE="${MYSQL_CONFIG_FILE:?MYSQL_CONFIG_FILE is required}"
RUN_ID="${RUN_ID:-backup-$$}"

case "${DATABASE_BACKEND}" in mysql|mariadb) ;; *) echo "DATABASE_BACKEND must be mysql or mariadb" >&2; exit 2 ;; esac
if [[ "${BACKUP_ROOT}" != /* || "${RUN_ID}" == *[^a-zA-Z0-9._-]* || ! "${MYSQL_DATABASE}" =~ ^[A-Za-z0-9_]{1,64}$ ]]; then
  echo "backup root, database name or RUN_ID is invalid" >&2
  exit 2
fi
if [[ ! -f "${MYSQL_CONFIG_FILE}" || -L "${MYSQL_CONFIG_FILE}" || "$(stat -c %a "${MYSQL_CONFIG_FILE}")" != 600 ]]; then
  echo "MYSQL_CONFIG_FILE must be a mode-0600 regular file" >&2
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
mkdir -p "${BACKUP_ROOT}/logical"
for path in "${BACKUP_ROOT}" "${BACKUP_ROOT}/logical"; do
  [[ -d "${path}" && ! -L "${path}" ]] || { echo "backup path must be a real directory: ${path}" >&2; exit 1; }
done

run_backup() {
  local lock="${BACKUP_ROOT}/.backup.lock" timestamp staging final latest_tmp version client dump
  local -a dump_options=()
  mkdir "${lock}" 2>/dev/null || { echo "another backup is active or a stale lock needs operator review" >&2; return 1; }
  timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
  staging="${BACKUP_ROOT}/logical/.${timestamp}-${RUN_ID}.tmp"
  final="${BACKUP_ROOT}/logical/${timestamp}"
  latest_tmp="${BACKUP_ROOT}/.LATEST-${RUN_ID}.tmp"
  cleanup_backup() {
    rm -rf -- "${staging}"
    rm -f -- "${latest_tmp}"
    [[ ! -d "${lock}" ]] || rmdir "${lock}" || true
  }
  trap cleanup_backup RETURN
  [[ ! -e "${final}" ]] || { echo "backup destination already exists: ${final}" >&2; return 1; }
  mkdir "${staging}"

  if [[ "${DATABASE_BACKEND}" == mysql ]]; then
    client=mysql
    dump=mysqldump
    dump_options+=(--set-gtid-purged=OFF)
  else
    client=mariadb
    dump=mariadb-dump
  fi
  version="$("${client}" --defaults-extra-file="${MYSQL_CONFIG_FILE}" --batch --skip-column-names -e 'SELECT VERSION()')"
  case "${DATABASE_BACKEND}:${version}" in
    mysql:8.4.10*) ;;
    mariadb:12.3.2-MariaDB*) ;;
    *) echo "database server does not match the pinned ${DATABASE_BACKEND} backup contract" >&2; return 1 ;;
  esac

  "${dump}" --defaults-extra-file="${MYSQL_CONFIG_FILE}" \
    --single-transaction --quick --hex-blob --routines --events --triggers \
    --default-character-set=utf8mb4 --skip-lock-tables --no-tablespaces "${dump_options[@]}" \
    --databases "${MYSQL_DATABASE}" \
    >"${staging}/database.sql"
  printf 'format=ocservia-logical-v1\nbackend=%s\nserver_version=%s\ndatabase=%s\ncreated_at=%s\n' \
    "${DATABASE_BACKEND}" "${version}" "${MYSQL_DATABASE}" "${timestamp}" >"${staging}/metadata"
  (cd "${staging}" && sha256sum database.sql metadata >SHA256SUMS && sha256sum -c SHA256SUMS)
  mv -- "${staging}" "${final}"
  printf '%s\n' "${timestamp}" >"${latest_tmp}"
  mv -- "${latest_tmp}" "${BACKUP_ROOT}/LATEST"

  mapfile -t backups < <(find "${BACKUP_ROOT}/logical" -mindepth 1 -maxdepth 1 -type d -name '20??????T??????Z' -print | sort -r)
  if (( ${#backups[@]} > BACKUP_RETENTION_COUNT )); then
    printf '%s\0' "${backups[@]:BACKUP_RETENTION_COUNT}" | xargs -0r rm -rf --
  fi
  rmdir "${lock}"
  echo "backup completed id=${timestamp} backend=${DATABASE_BACKEND}"
}

if [[ "${1:-}" == --once ]]; then run_backup; exit; fi
[[ $# -eq 0 ]] || { echo "usage: mysql-backup.sh [--once]" >&2; exit 2; }
while true; do run_backup; sleep "${BACKUP_INTERVAL_SECONDS}"; done
