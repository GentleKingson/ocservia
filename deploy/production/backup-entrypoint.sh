#!/usr/bin/env bash
set -euo pipefail

if [[ "${DATABASE_BACKEND:-postgres}" == mysql || "${DATABASE_BACKEND:-postgres}" == mariadb ]]; then
  source_file="${MYSQL_CONFIG_SOURCE:-/run/secrets/database_backup_config}"
  private_file="/tmp/ocservia-database.cnf"
  if [[ ! -f "${source_file}" || -L "${source_file}" ]]; then
    echo "database backup client configuration must be a regular file" >&2
    exit 1
  fi
  install -m 0600 "${source_file}" "${private_file}"
  export MYSQL_CONFIG_FILE="${private_file}"
  exec /usr/local/bin/ocservia-mysql-backup "$@"
fi

if [[ "${DATABASE_BACKEND:-postgres}" != postgres ]]; then
  echo "DATABASE_BACKEND must be postgres, mysql or mariadb" >&2
  exit 2
fi

source_file="${PGPASS_SOURCE:-/run/secrets/postgres_pgpass}"
private_file="/tmp/ocservia-postgres.pgpass"
if [[ ! -f "${source_file}" || -L "${source_file}" ]]; then
  echo "PostgreSQL passfile source must be a regular file" >&2
  exit 1
fi
install -m 0600 "${source_file}" "${private_file}"
export PGPASSFILE="${private_file}"
exec /usr/local/bin/ocservia-postgres-backup "$@"
