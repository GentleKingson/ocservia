#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:17.10-bookworm}"

if [[ "${RUN_ID}" == *[^a-zA-Z0-9._-]* ]]; then
  echo "RUN_ID contains unsafe characters" >&2
  exit 2
fi
work="${RUNNER_TEMP:-/tmp}/ocservia-i18-backup-${RUN_ID}"
network="ocservia-i18-backup-${RUN_ID}"
source_container="${network}-source"
restore_container="${network}-restore"
backup_container="${network}-backup"
password="$(openssl rand -hex 24)"
mkdir -p "${work}/backup" "${work}/restore" "${ARTIFACT_DIR}"
chmod 0700 "${work}" "${work}/backup" "${work}/restore"

cleanup() {
  local status=$?
  for container in "${source_container}" "${restore_container}" "${backup_container}"; do
    docker logs "${container}" >"${ARTIFACT_DIR}/${container}.log" 2>&1 || true
  done
  docker rm -f "${source_container}" "${restore_container}" "${backup_container}" >/dev/null 2>&1 || true
  docker network rm "${network}" >/dev/null 2>&1 || true
  rm -rf -- "${work}"
  if docker ps -a --format '{{.Names}}' | grep -Fq "${network}"; then
    echo "scoped container cleanup failed" >&2
    status=1
  fi
  exit "${status}"
}
trap cleanup EXIT INT TERM

docker network create "${network}" >/dev/null
docker run -d --name "${source_container}" --network "${network}" \
  -e POSTGRES_PASSWORD="${password}" -e POSTGRES_DB=ocservia \
  "${POSTGRES_IMAGE}" >/dev/null
source_ready=false
for _ in $(seq 1 60); do
  if docker exec -e PGPASSWORD="${password}" "${source_container}" pg_isready -U postgres -d ocservia >/dev/null 2>&1; then
    source_ready=true
    break
  fi
  sleep 1
done
if [[ "${source_ready}" != true ]]; then
  echo "source PostgreSQL did not become ready" >&2
  exit 1
fi
docker exec -e PGPASSWORD="${password}" "${source_container}" \
  psql -v ON_ERROR_STOP=1 -U postgres -d ocservia \
  -c "CREATE TABLE restore_marker(value text NOT NULL); INSERT INTO restore_marker VALUES ('i18-restored');" \
  >"${ARTIFACT_DIR}/source-seed.log"

docker run --name "${backup_container}" --network "${network}" \
  --user "$(id -u):$(id -g)" \
  -e PGHOST="${source_container}" -e PGDATABASE=ocservia -e PGUSER=postgres \
  -e PGPASSWORD="${password}" -e BACKUP_ROOT=/backup -e RUN_ID="${RUN_ID}" \
  -v "${ROOT}/scripts/postgres-backup.sh:/usr/local/bin/ocservia-postgres-backup:ro" \
  -v "${work}/backup:/backup" \
  "${POSTGRES_IMAGE}" /usr/local/bin/ocservia-postgres-backup --once \
  >"${ARTIFACT_DIR}/backup.log"

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
