#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
POSTGRES_IMAGE='postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f'
[[ "${RUN_ID}" != *[^a-zA-Z0-9._-]* ]] || { echo "RUN_ID contains unsafe characters" >&2; exit 2; }

work="${RUNNER_TEMP:-/tmp}/ocservia-external-postgres-${RUN_ID}"
source_network="ocservia-external-postgres-source-${RUN_ID}"
egress_network="ocservia-external-postgres-egress-${RUN_ID}"
source_container="${source_network}-database"
restore_container="${source_network}-restore"
backup_container="${egress_network}-backup"
control_image="${egress_network}-control-image"
backup_image="${egress_network}-backup-image"
password="$(openssl rand -hex 24)"
backup_password="$(openssl rand -hex 24)"
owner=(); ((EUID == 0)) || owner=(sudo)

mkdir -p "${work}/tls" "${work}/secrets" "${work}/backup" "${work}/restore" "${ARTIFACT_DIR}"
chmod 0700 "${work}" "${work}/tls" "${work}/secrets" "${work}/backup" "${work}/restore"

cleanup() {
  local status=$?
  docker logs "${source_container}" >"${ARTIFACT_DIR}/${source_container}.log" 2>&1 || true
  docker logs "${restore_container}" >"${ARTIFACT_DIR}/${restore_container}.log" 2>&1 || true
  docker rm -f "${source_container}" "${restore_container}" "${backup_container}" >/dev/null 2>&1 || true
  docker network rm "${source_network}" "${egress_network}" >/dev/null 2>&1 || status=1
  docker image rm -f "${control_image}" "${backup_image}" >/dev/null 2>&1 || status=1
  "${owner[@]}" rm -rf -- "${work}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

docker network create "${source_network}" >/dev/null
docker network create "${egress_network}" >/dev/null
gateway="$(docker network inspect -f '{{(index .IPAM.Config 0).Gateway}}' "${egress_network}")"
[[ "${gateway}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "cannot determine egress gateway" >&2; exit 1; }

openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=ocservia test CA' \
  -keyout "${work}/tls/ca.key" -out "${work}/tls/ca.crt" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -subj "/CN=${gateway}" \
  -keyout "${work}/tls/server.key" -out "${work}/tls/server.csr" >/dev/null 2>&1
printf 'subjectAltName=IP:%s\nextendedKeyUsage=serverAuth\n' "${gateway}" >"${work}/tls/server.ext"
openssl x509 -req -days 1 -in "${work}/tls/server.csr" -CA "${work}/tls/ca.crt" \
  -CAkey "${work}/tls/ca.key" -CAcreateserial -extfile "${work}/tls/server.ext" \
  -out "${work}/tls/server.crt" >/dev/null 2>&1
chmod 0444 "${work}/tls/ca.crt" "${work}/tls/server.crt"
chmod 0400 "${work}/tls/server.key"
"${owner[@]}" chown 999:999 "${work}/tls" "${work}/tls/server.key" "${work}/tls/server.crt"

docker build -f "${ROOT}/control-plane/Dockerfile" -t "${control_image}" "${ROOT}" >"${ARTIFACT_DIR}/control-image-build.log"
docker build -f "${ROOT}/deploy/production/backup.Dockerfile" -t "${backup_image}" "${ROOT}" >"${ARTIFACT_DIR}/backup-image-build.log"
docker run --rm -v "${work}/backup:/backup" --entrypoint chown "${POSTGRES_IMAGE}" -R 999:999 /backup

docker run -d --name "${source_container}" --network "${source_network}" -p 0:5432 \
  -e POSTGRES_PASSWORD="${password}" -e POSTGRES_DB=ocservia \
  -v "${work}/tls:/tls:ro" "${POSTGRES_IMAGE}" \
  -c ssl=on -c ssl_cert_file=/tls/server.crt -c ssl_key_file=/tls/server.key >/dev/null
source_ready=false
for _ in $(seq 1 90); do
  if docker logs "${source_container}" 2>&1 | grep -Fq 'PostgreSQL init process complete; ready for start up.' \
    && docker exec -e PGPASSWORD="${password}" "${source_container}" pg_isready -U postgres -d ocservia >/dev/null 2>&1; then
    source_ready=true
    break
  fi
  sleep 1
done
[[ "${source_ready}" == true ]] || { echo "external PostgreSQL source did not become ready" >&2; exit 1; }
host_port="$(docker port "${source_container}" 5432/tcp | sed -n 's/.*://p' | head -1)"
[[ "${host_port}" =~ ^[0-9]+$ ]] || { echo "cannot determine published PostgreSQL port" >&2; exit 1; }

docker exec -u postgres "${source_container}" sh -c \
  "printf '%s\n' 'hostssl replication ocservia_backup all scram-sha-256' >>\"\${PGDATA}/pg_hba.conf\" && pg_ctl reload" >/dev/null
docker exec -e PGPASSWORD="${password}" "${source_container}" psql -v ON_ERROR_STOP=1 -U postgres -d ocservia \
  -c "CREATE ROLE ocservia_app LOGIN PASSWORD '$(openssl rand -hex 24)'; CREATE ROLE ocservia_backup LOGIN REPLICATION PASSWORD '${backup_password}';" \
  >"${ARTIFACT_DIR}/roles.log"

printf '%064d\n' 1 >"${work}/secrets/session-key"
printf '%064d\n' 2 >"${work}/secrets/audit-checkpoint-key"
printf '%064d\n' 3 >"${work}/secrets/audit-event-key"
chmod 0444 "${work}/secrets/session-key" "${work}/secrets/audit-checkpoint-key"
chmod 0400 "${work}/secrets/audit-event-key"
"${owner[@]}" chown 65534:65532 "${work}/secrets/audit-event-key"

control_mounts=(
  -v "${work}/tls/ca.crt:/run/secrets/database_ca:ro"
  -v "${work}/secrets/session-key:/run/secrets/session_key:ro"
  -v "${work}/secrets/audit-checkpoint-key:/run/secrets/audit_checkpoint_key:ro"
  -v "${work}/secrets/audit-event-key:/run/secrets/audit_event_key:ro"
)
run_migrate() {
  local database_url="$1"; shift
  docker run --rm --network "${egress_network}" "$@" \
    -e OCSERV_ENVIRONMENT=production -e OCSERV_DATABASE_BACKEND=postgres \
    -e OCSERV_DATABASE_URL="${database_url}" -e OCSERV_DATABASE_TLS_CA_FILE=/run/secrets/database_ca \
    -e OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app -e OCSERV_LOCAL_AUTH_ENABLED=true \
    -e OCSERV_PUBLIC_ORIGIN=https://controller.example.test \
    -e OCSERV_SESSION_KEY_FILE=/run/secrets/session_key \
    -e OCSERV_AUDIT_CHECKPOINT_KEY_FILE=/run/secrets/audit_checkpoint_key \
    -e OCSERV_AUDIT_EVENT_KEY_ID=external-postgres-smoke \
    -e OCSERV_AUDIT_EVENT_KEY_FILE=/run/secrets/audit_event_key \
    "${control_mounts[@]}" "${control_image}" --migrate-only
}

base_url="postgres://postgres:${password}@${gateway}:${host_port}/ocservia"
docker run --rm --network "${egress_network}" \
  -v "${work}/tls/ca.crt:/run/secrets/database_ca:ro" --entrypoint psql "${backup_image}" \
  "${base_url}?sslmode=verify-full&sslrootcert=/run/secrets/database_ca" -At -c 'SELECT 1' \
  >"${ARTIFACT_DIR}/verified-tls-connectivity.log"
if run_migrate "${base_url}?sslmode=require" >"${ARTIFACT_DIR}/plaintext-rejection.log" 2>&1; then
  echo "external PostgreSQL without verified TLS was accepted" >&2
  exit 1
fi
if run_migrate "postgres://postgres:${password}@wrong.external.test:${host_port}/ocservia?sslmode=verify-full" \
  --add-host "wrong.external.test:${gateway}" >"${ARTIFACT_DIR}/hostname-mismatch.log" 2>&1; then
  echo "external PostgreSQL hostname mismatch was accepted" >&2
  exit 1
fi
run_migrate "${base_url}?sslmode=verify-full" >"${ARTIFACT_DIR}/migration.log"

docker exec -e PGPASSWORD="${password}" "${source_container}" psql -At -U postgres -d ocservia \
  -c "INSERT INTO identities (id, issuer, subject, email, display_name, created_at, updated_at) VALUES ('00000000-0000-7000-8000-000000000001','external-smoke','restore-probe','restore@example.test','Restore Probe',now(),now());" \
  >"${ARTIFACT_DIR}/source-seed.log"
schema_version="$(docker exec -e PGPASSWORD="${password}" "${source_container}" psql -At -U postgres -d ocservia \
  -c 'SELECT "current_schema" FROM controller_schema_compatibility WHERE singleton')"
[[ "${schema_version}" =~ ^[0-9]+$ ]] || { echo "external PostgreSQL migration did not produce schema metadata" >&2; exit 1; }
printf 'schema_version=%s\n' "${schema_version}" >"${ARTIFACT_DIR}/source-schema.log"

printf '%s:%s:ocservia:ocservia_backup:%s\n%s:%s:replication:ocservia_backup:%s\n' \
  "${gateway}" "${host_port}" "${backup_password}" "${gateway}" "${host_port}" "${backup_password}" \
  >"${work}/postgres.pgpass"
chmod 0444 "${work}/postgres.pgpass"
docker run --name "${backup_container}" --network "${egress_network}" \
  -e PGHOST="${gateway}" -e PGPORT="${host_port}" -e PGDATABASE=ocservia -e PGUSER=ocservia_backup \
  -e PGSSLMODE=verify-full -e PGSSLROOTCERT=/run/secrets/database_ca -e POSTGRES_SERVER_MAJOR=17 \
  -e PGPASS_SOURCE=/run/secrets/postgres_pgpass -e BACKUP_ROOT=/backup -e RUN_ID="${RUN_ID}" \
  -v "${work}/postgres.pgpass:/run/secrets/postgres_pgpass:ro" \
  -v "${work}/tls/ca.crt:/run/secrets/database_ca:ro" -v "${work}/backup:/backup" \
  "${backup_image}" --once >"${ARTIFACT_DIR}/backup.log"

source_networks="$(docker inspect -f '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}} {{end}}' "${source_container}")"
backup_networks="$(docker inspect -f '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}} {{end}}' "${backup_container}")"
[[ "${source_networks}" == "${source_network} " && "${backup_networks}" == "${egress_network} " ]] || {
  echo "external database and backup unexpectedly share a network" >&2; exit 1;
}
docker rm "${backup_container}" >/dev/null

backup_id="$(cat "${work}/backup/LATEST")"
backup_dir="${work}/backup/base/${backup_id}"
cp -a "${backup_dir}/." "${work}/restore/"
printf 'corrupt\n' >>"${work}/restore/PG_VERSION"
if docker run --rm -v "${work}/restore:/restore:ro" --entrypoint pg_verifybackup \
  "${POSTGRES_IMAGE}" /restore >"${ARTIFACT_DIR}/corrupt-rejection.log" 2>&1; then
  echo "corrupt external PostgreSQL backup unexpectedly verified" >&2
  exit 1
fi
"${owner[@]}" rm -rf -- "${work}/restore"
mkdir -m 0700 "${work}/restore"
cp -a "${backup_dir}/." "${work}/restore/"
rm -f "${work}/restore/standby.signal"
docker run --rm -v "${work}/restore:/restore" "${POSTGRES_IMAGE}" \
  bash -ceu 'chown -R postgres:postgres /restore && chmod 0700 /restore'
docker run -d --name "${restore_container}" --network "${source_network}" \
  -v "${work}/restore:/var/lib/postgresql/data" "${POSTGRES_IMAGE}" >/dev/null
for _ in $(seq 1 90); do
  if docker exec "${restore_container}" pg_isready -U postgres -d ocservia >/dev/null 2>&1; then break; fi
  sleep 1
done
restored="$(docker exec "${restore_container}" psql -At -U postgres -d ocservia -c "SELECT subject FROM identities WHERE subject='restore-probe'")"
[[ "${restored}" == restore-probe ]] || { echo "isolated external PostgreSQL restore failed" >&2; exit 1; }
printf 'backup_id=%s\nrestore_marker=%s\nsource_network=%s\nbackup_network=%s\n' \
  "${backup_id}" "${restored}" "${source_network}" "${egress_network}" >"${ARTIFACT_DIR}/restore-summary.txt"
