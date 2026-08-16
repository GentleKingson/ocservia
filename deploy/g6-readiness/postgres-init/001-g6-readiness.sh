#!/usr/bin/env bash
set -euo pipefail

# G6 HA/PITR harness initializer. Runs once inside the primary bootstrap via
# docker-entrypoint-initdb.d. All credentials are test-only values generated
# per workflow run and injected through environment variables.

replication_password="${G6_REPLICATION_PASSWORD:?G6_REPLICATION_PASSWORD is required}"
app_password="${G6_APP_PASSWORD:?G6_APP_PASSWORD is required}"
if [[ "${replication_password}" == *$'\n'* || "${app_password}" == *$'\n'* ]]; then
  echo "postgres harness passwords must not contain newlines" >&2
  exit 1
fi

psql --set=ON_ERROR_STOP=1 --username "${POSTGRES_USER}" --dbname "${POSTGRES_DB}" \
  --set=replication_password="${replication_password}" \
  --set=app_password="${app_password}" <<'SQL'
SELECT format('CREATE ROLE ocservia_replication LOGIN REPLICATION PASSWORD %L',
              :'replication_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ocservia_replication') \gexec
SELECT format('CREATE ROLE ocservia_app LOGIN PASSWORD %L', :'app_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ocservia_app') \gexec
SQL
unset replication_password app_password

cat >>"${PGDATA}/postgresql.conf" <<'CONF'

# ocservia G6 HA/PITR harness: streaming replication and WAL archiving.
wal_level = replica
archive_mode = on
archive_command = 'test ! -f /var/lib/postgresql/archive/%f && cp %p /var/lib/postgresql/archive/%f'
archive_timeout = '30s'
max_wal_senders = 5
max_replication_slots = 4
wal_keep_size = '256MB'
password_encryption = 'scram-sha-256'
CONF

printf '%s\n' 'host replication ocservia_replication all scram-sha-256' >>"${PGDATA}/pg_hba.conf"
printf '%s\n' 'host all ocservia_app all scram-sha-256' >>"${PGDATA}/pg_hba.conf"
