#!/usr/bin/env bash
set -euo pipefail

install -d -o postgres -g postgres -m 0700 /var/lib/ocservia-backup/wal /var/lib/ocservia-backup/base
password="$(cat /run/secrets/postgres_app_password)"
backup_password="$(cat /run/secrets/postgres_backup_password)"
if [[ -z "${password}" || "${password}" == *$'\n'* || -z "${backup_password}" || "${backup_password}" == *$'\n'* ]]; then
  echo "postgres application or backup password is invalid" >&2
  exit 1
fi

psql --set=ON_ERROR_STOP=1 --username "${POSTGRES_USER}" --dbname "${POSTGRES_DB}" \
  --set=runtime_password="${password}" --set=backup_password="${backup_password}" <<'SQL'
SELECT format('CREATE ROLE ocservia_app LOGIN PASSWORD %L', :'runtime_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ocservia_app') \gexec
SELECT format('CREATE ROLE ocservia_backup LOGIN REPLICATION PASSWORD %L', :'backup_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ocservia_backup') \gexec
SQL
unset password
unset backup_password
