#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f}"
[[ "${RUN_ID}" != *[^a-zA-Z0-9._-]* ]] || { echo "RUN_ID contains unsafe characters" >&2; exit 2; }

tmp_base="$(realpath -e "${RUNNER_TEMP:-${TMPDIR:-/tmp}}")"
work="${tmp_base}/ocservia-postgres-rotation-${RUN_ID}"
project="ocservia-pg-rotation-${RUN_ID}"
compose_file="${work}/compose.yaml"
state_dir="${work}/state"
secret_dir="${work}/secrets"
new_dir="${work}/new"
compose=(docker compose -p "${project}" -f "${compose_file}")

cleanup() {
  local status=$?
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf -- "${work}"
  if docker ps -a --format '{{.Names}}' | grep -Fq "${project}"; then
    echo "credential rotation fixture cleanup failed" >&2
    status=1
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM

mkdir -p "${secret_dir}" "${new_dir}" "${state_dir}" "${ARTIFACT_DIR}"
chmod 0700 "${work}" "${secret_dir}" "${new_dir}" "${state_dir}"
old_app='old-app-9!z:credential'
old_backup='old-backup-7@q:credential'
new_app='new-app-4#x:credential'
new_backup="new-backup-2\$v:credential"
printf '%s\n' 'owner-fixture-password' >"${secret_dir}/postgres-owner-password"
printf '%s\n' "${old_app}" >"${secret_dir}/postgres-app-password"
printf '%s\n' "${old_backup}" >"${secret_dir}/postgres-backup-password"
printf '%s\n' 'postgres://ocservia_app:old-app-9%21z%3Acredential@postgres:5432/ocservia?sslmode=disable' >"${secret_dir}/database-app-url"
printf 'postgres:5432:replication:ocservia_backup:old-backup-7@q\\:credential\n' >"${secret_dir}/postgres.pgpass"
printf '%s\n' "${new_app}" >"${new_dir}/app"
printf '%s\n' "${new_backup}" >"${new_dir}/backup"
chmod 0444 "${secret_dir}"/*
chmod 0600 "${new_dir}/app" "${new_dir}/backup"

cat >"${compose_file}" <<YAML
name: ${project}
services:
  postgres:
    image: ${POSTGRES_IMAGE}
    environment:
      POSTGRES_DB: ocservia
      POSTGRES_USER: ocservia_owner
      POSTGRES_PASSWORD_FILE: /run/secrets/postgres_owner_password
      POSTGRES_INITDB_ARGS: --auth-host=scram-sha-256
    secrets:
      - postgres_owner_password
      - postgres_app_password
      - postgres_backup_password
    volumes:
      - ${ROOT}/deploy/production/postgres-init/001-runtime-role.sh:/docker-entrypoint-initdb.d/001-runtime-role.sh:ro
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ocservia_owner -d ocservia"]
      interval: 1s
      timeout: 2s
      retries: 30
  control-plane:
    image: ${POSTGRES_IMAGE}
    entrypoint: ["/bin/sh", "-ceu"]
    command:
      - |
        if [ -f /state/fail-once ]; then rm -f /state/fail-once; exit 42; fi
        url="\$(cat /run/secrets/database_app_url)"
        psql "\$\${url}" --no-password --command 'SELECT 1' >/dev/null
        touch /state/control-connected
        exec sleep infinity
    secrets: [database_app_url]
    volumes: ["${state_dir}:/state"]
    depends_on:
      postgres: {condition: service_healthy}
    healthcheck:
      test: ["CMD-SHELL", "test -f /state/control-connected"]
      interval: 1s
      timeout: 2s
      retries: 10
  backup:
    image: ${POSTGRES_IMAGE}
    entrypoint: ["/bin/sh", "-ceu"]
    command:
      - |
        install -m 0600 /run/secrets/postgres_pgpass /tmp/pgpass
        rm -rf /tmp/base
        PGPASSFILE=/tmp/pgpass pg_basebackup --no-password --host postgres --username ocservia_backup --pgdata /tmp/base --wal-method=none --checkpoint=fast >/dev/null
        rm -rf /tmp/base
        touch /state/backup-connected
        exec sleep infinity
    secrets: [postgres_pgpass]
    volumes: ["${state_dir}:/state"]
    depends_on:
      postgres: {condition: service_healthy}
    healthcheck:
      test: ["CMD-SHELL", "test -f /state/backup-connected"]
      interval: 1s
      timeout: 2s
      retries: 30
volumes:
  pgdata: {}
secrets:
  postgres_owner_password: {file: ${secret_dir}/postgres-owner-password}
  postgres_app_password: {file: ${secret_dir}/postgres-app-password}
  postgres_backup_password: {file: ${secret_dir}/postgres-backup-password}
  database_app_url: {file: ${secret_dir}/database-app-url}
  postgres_pgpass: {file: ${secret_dir}/postgres.pgpass}
YAML

"${compose[@]}" up -d --wait --wait-timeout 90
control_before="$("${compose[@]}" ps -q control-plane)"
backup_before="$("${compose[@]}" ps -q backup)"

touch "${state_dir}/fail-once"
rm -f "${state_dir}/control-connected" "${state_dir}/backup-connected"
if OCSERV_SECRET_DIR="${secret_dir}" \
  OCSERV_NEW_POSTGRES_APP_PASSWORD_FILE="${new_dir}/app" \
  OCSERV_NEW_POSTGRES_BACKUP_PASSWORD_FILE="${new_dir}/backup" \
  OCSERV_ROTATION_COMPOSE_FILE="${compose_file}" \
  OCSERV_ROTATION_WAIT_TIMEOUT_SECONDS=10 \
  "${ROOT}/deploy/production/rotate-postgres-credentials.sh" >"${ARTIFACT_DIR}/recovery.log" 2>&1; then
  echo "credential rotation unexpectedly succeeded through a forced client restart failure" >&2
  exit 1
fi
grep -Fq 'previous verifier, secrets, and clients were restored' "${ARTIFACT_DIR}/recovery.log"
test "$(cat "${secret_dir}/postgres-app-password")" = "${old_app}"
test "$(cat "${secret_dir}/postgres-backup-password")" = "${old_backup}"
"${compose[@]}" ps --status running --services | grep -Fxq control-plane
"${compose[@]}" ps --status running --services | grep -Fxq backup

rm -f "${state_dir}/control-connected" "${state_dir}/backup-connected"
OCSERV_SECRET_DIR="${secret_dir}" \
  OCSERV_NEW_POSTGRES_APP_PASSWORD_FILE="${new_dir}/app" \
  OCSERV_NEW_POSTGRES_BACKUP_PASSWORD_FILE="${new_dir}/backup" \
  OCSERV_ROTATION_COMPOSE_FILE="${compose_file}" \
  OCSERV_TERMINATE_OLD_POSTGRES_SESSIONS=true \
  OCSERV_ROTATION_WAIT_TIMEOUT_SECONDS=90 \
  "${ROOT}/deploy/production/rotate-postgres-credentials.sh" >"${ARTIFACT_DIR}/success.log" 2>&1

test "$(cat "${secret_dir}/postgres-app-password")" = "${new_app}"
test "$(cat "${secret_dir}/postgres-backup-password")" = "${new_backup}"
grep -Fq 'new-app-4%23x%3Acredential' "${secret_dir}/database-app-url"
grep -Fq "new-backup-2\$v\\\\:credential" "${secret_dir}/postgres.pgpass"
test -f "${state_dir}/control-connected"
test -f "${state_dir}/backup-connected"
control_after="$("${compose[@]}" ps -q control-plane)"
backup_after="$("${compose[@]}" ps -q backup)"
[[ "${control_after}" != "${control_before}" && "${backup_after}" != "${backup_before}" ]]

for password in "${old_app}" "${old_backup}" "${new_app}" "${new_backup}"; do
  if grep -Fq "${password}" "${ARTIFACT_DIR}/recovery.log" "${ARTIFACT_DIR}/success.log"; then
    echo "credential rotation log exposed a password" >&2
    exit 1
  fi
done

echo "PostgreSQL application and backup credential rotation validation passed"
