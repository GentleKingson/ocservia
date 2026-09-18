#!/usr/bin/env bash
set -euo pipefail

install -d -o postgres -g postgres -m 0700 /var/lib/ocservia-backup/wal /var/lib/ocservia-backup/base
password="$(cat /run/secrets/postgres_app_password)"
backup_password="$(cat /run/secrets/postgres_backup_password)"
if [[ -z "${password}" || "${password}" == *$'\n'* || -z "${backup_password}" || "${backup_password}" == *$'\n'* ]]; then
  echo "postgres application or backup password is invalid" >&2
  exit 1
fi

runtime_encoded="$(printf '%s' "${password}" | base64 | tr -d '\n')"
backup_encoded="$(printf '%s' "${backup_password}" | base64 | tr -d '\n')"
if ! {
  printf "\\set runtime_encoded '%s'\n" "${runtime_encoded}"
  printf "\\set backup_encoded '%s'\n" "${backup_encoded}"
  cat <<'SQL'
-- Do not log secret-bearing statements, including failed role creation.
SET log_statement = 'none';
SET log_min_error_statement = 'panic';
SELECT format('CREATE ROLE ocservia_app LOGIN PASSWORD %L', convert_from(decode(:'runtime_encoded', 'base64'), current_setting('client_encoding')::name))
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ocservia_app') \gexec
SELECT format('CREATE ROLE ocservia_backup LOGIN REPLICATION PASSWORD %L', convert_from(decode(:'backup_encoded', 'base64'), current_setting('client_encoding')::name))
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ocservia_backup') \gexec
SQL
} | psql --no-psqlrc --set=ON_ERROR_STOP=1 --username "${POSTGRES_USER}" --dbname "${POSTGRES_DB}" >/dev/null 2>&1; then
  echo "PostgreSQL runtime role initialization failed" >&2
  exit 1
fi
printf '%s\n' 'host replication ocservia_backup all scram-sha-256' >>"${PGDATA}/pg_hba.conf"
unset password
unset backup_password
unset runtime_encoded backup_encoded
