#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f}"

if [[ "${RUN_ID}" == *[^a-zA-Z0-9._-]* ]]; then
  echo "RUN_ID contains unsafe characters" >&2
  exit 2
fi
work="${RUNNER_TEMP:-/tmp}/ocservia-i18-backup-${RUN_ID}"
network="ocservia-i18-backup-${RUN_ID}"
source_container="${network}-source"
restore_container="${network}-restore"
backup_container="${network}-backup"
backup_image="${network}-image"
password="$(openssl rand -hex 24)"
mkdir -p "${work}/backup" "${work}/restore" "${ARTIFACT_DIR}"
chmod 0700 "${work}" "${work}/backup" "${work}/restore"
printf '%s:5432:replication:postgres:%s\n' "${source_container}" "${password}" >"${work}/postgres.pgpass"
chmod 0644 "${work}/postgres.pgpass"

cleanup() {
  local status=$?
  for container in "${source_container}" "${restore_container}" "${backup_container}"; do
    docker logs "${container}" >"${ARTIFACT_DIR}/${container}.log" 2>&1 || true
  done
  docker rm -f "${source_container}" "${restore_container}" "${backup_container}" >/dev/null 2>&1 || true
  if ! docker network rm "${network}" >/dev/null 2>&1; then
    status=1
  fi
  if ! docker run --rm -v "${work}:/work" --entrypoint chown "${POSTGRES_IMAGE}" \
    -R "$(id -u):$(id -g)" /work >/dev/null 2>&1; then
    status=1
  fi
  rm -rf -- "${work}"
  if docker ps -a --format '{{.Names}}' | grep -Fq "${network}"; then
    echo "scoped container cleanup failed" >&2
    status=1
  fi
  if docker network inspect "${network}" >/dev/null 2>&1; then
    echo "scoped network cleanup failed" >&2
    status=1
  fi
  docker image rm -f "${backup_image}" >/dev/null 2>&1 || status=1
  if docker image inspect "${backup_image}" >/dev/null 2>&1; then
    echo "scoped backup image cleanup failed" >&2
    status=1
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM

docker network create "${network}" >/dev/null
docker build -f "${ROOT}/deploy/production/backup.Dockerfile" -t "${backup_image}" "${ROOT}" \
  >"${ARTIFACT_DIR}/backup-image-build.log"
docker run -d --name "${source_container}" --network "${network}" \
  -e POSTGRES_PASSWORD="${password}" -e POSTGRES_DB=ocservia \
  "${POSTGRES_IMAGE}" >/dev/null
source_ready=false
for _ in $(seq 1 60); do
  if docker logs "${source_container}" 2>&1 | grep -Fq 'PostgreSQL init process complete; ready for start up.' \
    && docker exec -e PGPASSWORD="${password}" "${source_container}" pg_isready -U postgres -d ocservia >/dev/null 2>&1; then
    source_ready=true
    break
  fi
  sleep 1
done
if [[ "${source_ready}" != true ]]; then
  echo "source PostgreSQL did not become ready" >&2
  exit 1
fi
docker exec -u postgres "${source_container}" sh -c \
  "printf '%s\\n' 'host replication postgres all scram-sha-256' >>\"\${PGDATA}/pg_hba.conf\" && pg_ctl reload" \
  >"${ARTIFACT_DIR}/replication-hba.log"
docker exec -e PGPASSWORD="${password}" "${source_container}" \
  psql -v ON_ERROR_STOP=1 -U postgres -d ocservia \
  -c "CREATE TABLE restore_marker(value text NOT NULL); INSERT INTO restore_marker VALUES ('i18-restored');" \
  >"${ARTIFACT_DIR}/source-seed.log"

docker run --name "${backup_container}" --network "${network}" \
  --user "$(id -u):$(id -g)" \
  -e PGHOST="${source_container}" -e PGDATABASE=ocservia -e PGUSER=postgres \
  -e PGPASS_SOURCE=/run/secrets/postgres_pgpass -e BACKUP_ROOT=/backup -e RUN_ID="${RUN_ID}" \
  -v "${work}/postgres.pgpass:/run/secrets/postgres_pgpass:ro" \
  -v "${work}/backup:/backup" \
  "${backup_image}" --once \
  >"${ARTIFACT_DIR}/backup.log"

docker rm "${backup_container}" >/dev/null
touch "${work}/backup/wal/000000010000000000000000"
rm "${work}/backup/.backup.lock"
mkdir "${work}/backup/.backup.lock"
sleep 1
docker run --name "${backup_container}" --network "${network}" \
  --user "$(id -u):$(id -g)" \
  -e PGHOST="${source_container}" -e PGDATABASE=ocservia -e PGUSER=postgres \
  -e PGPASS_SOURCE=/run/secrets/postgres_pgpass -e BACKUP_ROOT=/backup -e RUN_ID="${RUN_ID}-restart" \
  -v "${work}/postgres.pgpass:/run/secrets/postgres_pgpass:ro" \
  -v "${work}/backup:/backup" \
  "${backup_image}" --once \
  >"${ARTIFACT_DIR}/backup-after-stale-lock.log"
test ! -e "${work}/backup/wal/000000010000000000000000"

docker rm "${backup_container}" >/dev/null
docker run -d --name "${backup_container}" --user "$(id -u):$(id -g)" \
  -v "${work}/backup:/backup" --entrypoint flock "${backup_image}" \
  /backup/.backup.lock sleep 30 >/dev/null
sleep 1
docker kill "${backup_container}" >/dev/null
docker rm "${backup_container}" >/dev/null
sleep 1
docker run --name "${backup_container}" --network "${network}" \
  --user "$(id -u):$(id -g)" \
  -e PGHOST="${source_container}" -e PGDATABASE=ocservia -e PGUSER=postgres \
  -e PGPASS_SOURCE=/run/secrets/postgres_pgpass -e BACKUP_ROOT=/backup -e RUN_ID="${RUN_ID}-after-kill" \
  -v "${work}/postgres.pgpass:/run/secrets/postgres_pgpass:ro" \
  -v "${work}/backup:/backup" \
  "${backup_image}" --once \
  >"${ARTIFACT_DIR}/backup-after-killed-lock-holder.log"

backup_id="$(cat "${work}/backup/LATEST")"
test -f "${work}/backup/base/${backup_id}/backup_manifest"
docker stop "${source_container}" >/dev/null
cp -a "${work}/backup/base/${backup_id}/." "${work}/restore/"
rm -f "${work}/restore/standby.signal"
docker run --rm -v "${work}/restore:/restore" "${POSTGRES_IMAGE}" \
  bash -ceu 'chown -R postgres:postgres /restore && chmod 0700 /restore'
docker run -d --name "${restore_container}" --network "${network}" \
  -e POSTGRES_PASSWORD="${password}" -v "${work}/restore:/var/lib/postgresql/data" \
  "${POSTGRES_IMAGE}" >/dev/null
restore_ready=false
for _ in $(seq 1 60); do
  if docker exec -e PGPASSWORD="${password}" "${restore_container}" pg_isready -U postgres -d ocservia >/dev/null 2>&1; then
    restore_ready=true
    break
  fi
  sleep 1
done
if [[ "${restore_ready}" != true ]]; then
  echo "restored PostgreSQL did not become ready" >&2
  exit 1
fi
restored="$(docker exec -e PGPASSWORD="${password}" "${restore_container}" psql -At -U postgres -d ocservia -c 'SELECT value FROM restore_marker')"
if [[ "${restored}" != "i18-restored" ]]; then
  echo "restore marker mismatch" >&2
  exit 1
fi
printf 'backup_id=%s\nrestore_marker=%s\n' "${backup_id}" "${restored}" >"${ARTIFACT_DIR}/restore-summary.txt"
