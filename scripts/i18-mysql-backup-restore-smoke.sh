#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
ENGINE="${ENGINE:?ENGINE must be mysql}"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
case "${ENGINE}" in
  mysql)
    SERVER_IMAGE='mysql:8.4.10@sha256:8dbcf531a03aade657e181b9cf2f1d1803ce621a1d55610cb44cb531ab7d7db6'
    DOCKERFILE=deploy/production/backup.mysql.Dockerfile
    CLIENT=mysql
    ;;
  *) echo "ENGINE must be mysql" >&2; exit 2 ;;
esac
[[ "${RUN_ID}" != *[^a-zA-Z0-9._-]* ]] || { echo "RUN_ID contains unsafe characters" >&2; exit 2; }

work="${RUNNER_TEMP:-/tmp}/ocservia-${ENGINE}-backup-${RUN_ID}"
network="ocservia-${ENGINE}-backup-${RUN_ID}"
source_container="${network}-source"
target_container="${network}-target"
backup_container="${network}-backup"
backup_image="${network}-image"
password="$(openssl rand -hex 24)"
backup_password="$(openssl rand -hex 24)"
mkdir -p "${work}/backup" "${work}/corrupt" "${ARTIFACT_DIR}"
chmod 0700 "${work}" "${work}/backup" "${work}/corrupt"

cleanup() {
  local status=$?
  docker logs "${source_container}" >"${ARTIFACT_DIR}/${source_container}.log" 2>&1 || true
  docker logs "${target_container}" >"${ARTIFACT_DIR}/${target_container}.log" 2>&1 || true
  docker rm -fv "${source_container}" "${target_container}" "${backup_container}" >/dev/null 2>&1 || true
  docker network rm "${network}" >/dev/null 2>&1 || status=1
  docker image rm -f "${backup_image}" >/dev/null 2>&1 || status=1
  if declare -F set_backup_owner >/dev/null; then
    set_backup_owner "$(id -u):$(id -g)" >/dev/null 2>&1 || true
  fi
  rm -rf -- "${work}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

docker network create "${network}" >/dev/null
docker build -f "${ROOT}/${DOCKERFILE}" -t "${backup_image}" "${ROOT}" >"${ARTIFACT_DIR}/backup-image-build.log"
set_backup_owner() {
  docker run --rm -v "${work}/backup:/backup" --entrypoint chown "${SERVER_IMAGE}" -R "$1" /backup
}
set_backup_owner 999:999
docker run -d --name "${source_container}" --network "${network}" --network-alias source \
  -e MYSQL_ROOT_PASSWORD="${password}" "${SERVER_IMAGE}" >/dev/null

for _ in $(seq 1 90); do
  if docker exec "${source_container}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot -p"${password}" -e 'SELECT 1' >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "${source_container}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot -p"${password}" -e 'SELECT 1' >/dev/null
# Use the current SQL journal definition for this transport-only fixture.
# Its deliberately synthetic checksum is not evidence of a valid migration chain;
# the second database below exercises genuine origin/checksum validation.
docker exec "${source_container}" "${CLIENT}" -uroot -p"${password}" -e 'CREATE DATABASE ocservia'
python3 - "${ROOT}" <<'PYSQL' | docker exec -i "${source_container}" "${CLIENT}" -uroot -p"${password}" ocservia
import pathlib, re, sys
sql = (pathlib.Path(sys.argv[1]) / 'control-plane/internal/database/mysql/mysql/schema.sql').read_text()
for block in re.findall(r'-- ocservia:step=.*?-- ocservia:end-step\n', sql, re.S):
    if '"object":"schema_revisions"' in block:
        print(block[block.index('CREATE TABLE'):block.index('-- ocservia:end-step')])

PYSQL
docker exec -i "${source_container}" "${CLIENT}" -uroot -p"${password}" <<'SQL'
USE ocservia;
INSERT INTO schema_revisions(epoch,revision,checksum,state,step,verified_at) VALUES(2,1,REPEAT('0',64),'verified',1,CURRENT_TIMESTAMP(6));
CREATE TABLE audit_events(id INT PRIMARY KEY,event_hash BINARY(32) NOT NULL,event_mac BINARY(32) NOT NULL);
INSERT INTO audit_events VALUES(1,REPEAT(0x11,32),REPEAT(0x22,32));
CREATE TABLE identities(id INT PRIMARY KEY,marker VARCHAR(32));
INSERT INTO identities VALUES(1,'source');
CREATE TABLE scheduler_leadership(id INT PRIMARY KEY,epoch BIGINT NOT NULL,lease_until BIGINT);
INSERT INTO scheduler_leadership VALUES(1,7,TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))-1000000);
CREATE TABLE operations(id INT PRIMARY KEY,state VARCHAR(32),idempotency_key VARCHAR(64));
INSERT INTO operations VALUES(1,'succeeded','restore-idempotency');
CREATE TABLE commands(id INT PRIMARY KEY,state VARCHAR(32));
INSERT INTO commands VALUES(1,'succeeded');
CREATE TRIGGER identities_marker BEFORE INSERT ON identities FOR EACH ROW SET NEW.marker=LOWER(NEW.marker);
CREATE PROCEDURE restore_probe() SELECT COUNT(*) FROM identities;
CREATE EVENT restore_event ON SCHEDULE EVERY 1 DAY DO INSERT INTO identities VALUES(99,'EVENT');
SQL
docker exec "${source_container}" "${CLIENT}" -uroot -p"${password}" -e \
  "CREATE USER 'backup'@'%' IDENTIFIED BY '${backup_password}'; GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON ocservia.* TO 'backup'@'%'; GRANT SHOW_ROUTINE ON *.* TO 'backup'@'%';"

cat >"${work}/backup.cnf" <<EOF
[client]
host=source
user=backup
password=${backup_password}
EOF
chmod 0444 "${work}/backup.cnf"
docker run --name "${backup_container}" --network "${network}" \
  -e DATABASE_BACKEND="${ENGINE}" -e MYSQL_DATABASE=ocservia \
  -e MYSQL_CONFIG_SOURCE=/run/secrets/database_backup_config -e BACKUP_ROOT=/backup \
  -e RUN_ID="${RUN_ID}" -v "${work}/backup.cnf:/run/secrets/database_backup_config:ro" \
  -v "${work}/backup:/backup" "${backup_image}" --once >"${ARTIFACT_DIR}/backup.log"
docker rm "${backup_container}" >/dev/null
set_backup_owner "$(id -u):$(id -g)"
first_backup_id="$(cat "${work}/backup/LATEST")"
sleep 1
set_backup_owner 999:999
docker run --name "${backup_container}" --network "${network}" \
  -e DATABASE_BACKEND="${ENGINE}" -e MYSQL_DATABASE=ocservia \
  -e MYSQL_CONFIG_SOURCE=/run/secrets/database_backup_config -e BACKUP_ROOT=/backup \
  -e BACKUP_RETENTION_COUNT=1 -e RUN_ID="${RUN_ID}-repeat" \
  -v "${work}/backup.cnf:/run/secrets/database_backup_config:ro" \
  -v "${work}/backup:/backup" "${backup_image}" --once >"${ARTIFACT_DIR}/backup-repeat.log"
docker rm "${backup_container}" >/dev/null
set_backup_owner "$(id -u):$(id -g)"
[[ ! -e "${work}/backup/logical/${first_backup_id}" ]] || { echo "backup retention did not remove the oldest dump" >&2; exit 1; }

backup_id="$(cat "${work}/backup/LATEST")"
backup_dir="${work}/backup/logical/${backup_id}"
docker run -d --name "${target_container}" --network "${network}" --network-alias target \
  -e MYSQL_ROOT_PASSWORD="${password}" "${SERVER_IMAGE}" >/dev/null
for _ in $(seq 1 90); do
  if docker exec "${target_container}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot -p"${password}" -e 'SELECT 1' >/dev/null 2>&1; then break; fi
  sleep 1
done
cat >"${work}/target.cnf" <<EOF
[client]
host=target
user=root
password=${password}
EOF
chmod 0600 "${work}/target.cnf"

set +e
docker run --rm --user 0:0 --network "${network}" --entrypoint /usr/local/bin/ocservia-mysql-restore-verify \
  -v "${backup_dir}:/backup:ro" -v "${work}/target.cnf:/target.cnf:ro" "${backup_image}" \
  --backend "${ENGINE}" --backup-dir /backup --target-config /target.cnf \
  >"${ARTIFACT_DIR}/restore-verify.log" 2>&1
restore_status=$?
set -e
[[ "${restore_status}" == 3 ]] || { echo "restore authority gate returned ${restore_status}, expected 3" >&2; exit 1; }

objects="$(docker exec "${target_container}" "${CLIENT}" -uroot -p"${password}" --batch --skip-column-names -e \
  "SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA='ocservia'; SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA='ocservia'; SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA='ocservia';")"
[[ "${objects}" == $'1\n1\n1' ]] || { echo "trigger/routine/event restore mismatch: ${objects}" >&2; exit 1; }

cp -a "${backup_dir}/." "${work}/corrupt/"
printf 'corrupt\n' >>"${work}/corrupt/database.sql"
if docker run --rm --user 0:0 --network "${network}" --entrypoint /usr/local/bin/ocservia-mysql-restore-verify \
  -v "${work}/corrupt:/backup:ro" -v "${work}/target.cnf:/target.cnf:ro" "${backup_image}" \
  --backend "${ENGINE}" --backup-dir /backup --target-config /target.cnf \
  >"${ARTIFACT_DIR}/corrupt-rejection.log" 2>&1; then
  echo "corrupt backup unexpectedly accepted" >&2
  exit 1
fi
# A checksum-valid dump with dirty provenance must fail the state summary.
mkdir "${work}/schema-invalid"
cp -a "${backup_dir}/." "${work}/schema-invalid/"
printf "\nUSE ocservia; UPDATE schema_revisions SET state='running',verified_at=NULL WHERE epoch=2 AND revision=1;\n" >>"${work}/schema-invalid/database.sql"
(cd "${work}/schema-invalid" && sha256sum database.sql metadata >SHA256SUMS)
docker exec "${target_container}" "${CLIENT}" -uroot -p"${password}" -e 'DROP DATABASE ocservia'
set +e
docker run --rm --user 0:0 --network "${network}" --entrypoint /usr/local/bin/ocservia-mysql-restore-verify \
  -v "${work}/schema-invalid:/backup:ro" -v "${work}/target.cnf:/target.cnf:ro" "${backup_image}" \
  --backend mysql --backup-dir /backup --target-config /target.cnf \
  >"${ARTIFACT_DIR}/dirty-schema-rejection.log" 2>&1
restore_status=$?
set -e
[[ "${restore_status}" == 1 ]]
grep -Fxq 'schema:failed' "${ARTIFACT_DIR}/dirty-schema-rejection.log"
grep -Fq 'restored data checks failed' "${ARTIFACT_DIR}/dirty-schema-rejection.log"

# Exercise the actual initialization artifact and canonical backend validator,
# independently of the synthetic routine/event transport fixture above.
(cd "${ROOT}/control-plane" && CGO_ENABLED=0 go build -trimpath -o "${work}/foundation" ./cmd/ocserv-db-foundation
  CGO_ENABLED=0 go build -trimpath -o "${work}/controller" ./cmd/ocserv-control)
foundation() {
  local server_container="${network}-$1" mode="$2"
  docker run --rm --user "$(id -u):$(id -g)" --network "container:${server_container}" \
    -e OCSERV_ENVIRONMENT=test -e OCSERV_DATABASE_BACKEND=mysql \
    -e "OCSERV_DATABASE_URL=root:${password}@tcp(127.0.0.1:3306)/snapshot_restore?tls=false" \
    -v "${work}/foundation:/foundation:ro" --entrypoint /foundation "${backup_image}" --mode="${mode}"
}
controller_migrate() {
  local server_container="${network}-$1"
  docker run --rm --user "$(id -u):$(id -g)" --network "container:${server_container}" \
    -e OCSERV_ENVIRONMENT=test -e OCSERV_DATABASE_BACKEND=mysql \
    -e "OCSERV_DATABASE_URL=root:${password}@tcp(127.0.0.1:3306)/snapshot_restore?tls=false" \
    -e OCSERV_RUNTIME_DATABASE_ROLE=snapshot_runtime@% -e OCSERV_LOCAL_AUTH_ENABLED=true \
    -e OCSERV_AUDIT_EVENT_KEY_ID=backup-test -e "OCSERV_TEST_AUDIT_EVENT_KEY_HEX=$(printf '%064d' 1)" \
    -e "OCSERV_AUDIT_CHECKPOINT_KEY=$(printf '%064d' 2)" -e "OCSERV_SESSION_KEY=$(printf '%064d' 3)" \
    -v "${work}/controller:/controller:ro" --entrypoint /controller "${backup_image}" --migrate-only
}
for container in "${source_container}" "${target_container}"; do
  docker exec "${container}" "${CLIENT}" -uroot -p"${password}" -e \
    "CREATE USER 'snapshot_runtime'@'%' IDENTIFIED BY 'snapshot-test-only';"
done
docker exec "${source_container}" "${CLIENT}" -uroot -p"${password}" -e \
  "CREATE DATABASE snapshot_restore; GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON snapshot_restore.* TO 'backup'@'%';"
controller_migrate source >"${ARTIFACT_DIR}/snapshot-initialize.log" 2>&1
foundation source check >"${ARTIFACT_DIR}/snapshot-source-check.log" 2>&1
snapshot_checksum="$(foundation source schema-artifact-checksum)"
source_checksum="$(docker exec "${source_container}" "${CLIENT}" -uroot -p"${password}" -Nse \
  "SELECT checksum FROM snapshot_restore.schema_revisions WHERE epoch=2 AND revision=1 AND state='verified'")"
[[ "${source_checksum}" == "${snapshot_checksum}" ]]
docker exec "${source_container}" "${CLIENT}" -uroot -p"${password}" -e \
  "INSERT INTO snapshot_restore.identities(id,issuer,subject,created_at,updated_at) VALUES(UNHEX(REPEAT('11',16)),'backup-test','snapshot-restore-marker',0,0); UPDATE snapshot_restore.scheduler_leadership SET lease_until=TIMESTAMPDIFF(MICROSECOND,'2000-01-01',UTC_TIMESTAMP(6))-1000000 WHERE id=1;"
receipt_query="SELECT CONCAT_WS('|',epoch,revision,checksum,state,step,started_at,verified_at) FROM schema_revisions ORDER BY epoch,revision;
SELECT TABLE_COMMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='schema_revisions';
SELECT CONCAT('legacy_tables:',COUNT(*)) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME IN ('backend_migrations','backend_migration_steps','backend_schema_revisions','backend_schema_revision_steps','backend_schema_snapshot','backend_schema_snapshot_steps','time_migration_decisions','controller_schema_compatibility');"
receipts() {
  docker exec "$1" "${CLIENT}" -uroot -p"${password}" --database=snapshot_restore -Nse "${receipt_query}"
}
receipts "${source_container}" >"${ARTIFACT_DIR}/snapshot-source-receipts.txt"
grep -Fxq 'legacy_tables:0' "${ARTIFACT_DIR}/snapshot-source-receipts.txt"
set_backup_owner 999:999
docker run --name "${backup_container}" --network "${network}" \
  -e DATABASE_BACKEND=mysql -e MYSQL_DATABASE=snapshot_restore \
  -e MYSQL_CONFIG_SOURCE=/run/secrets/database_backup_config -e BACKUP_ROOT=/backup \
  -e RUN_ID="${RUN_ID}-snapshot" -v "${work}/backup.cnf:/run/secrets/database_backup_config:ro" \
  -v "${work}/backup:/backup" "${backup_image}" --once >"${ARTIFACT_DIR}/snapshot-backup.log"
docker rm "${backup_container}" >/dev/null
set_backup_owner "$(id -u):$(id -g)"
snapshot_backup_id="$(cat "${work}/backup/LATEST")"
set +e
docker run --rm --user 0:0 --network "${network}" --entrypoint /usr/local/bin/ocservia-mysql-restore-verify \
  -v "${work}/backup/logical/${snapshot_backup_id}:/backup:ro" -v "${work}/target.cnf:/target.cnf:ro" "${backup_image}" \
  --backend mysql --backup-dir /backup --target-config /target.cnf --old-writers-fenced \
  >"${ARTIFACT_DIR}/snapshot-restore.log" 2>&1
restore_status=$?
set -e
[[ "${restore_status}" == 0 ]] || { echo "snapshot restore data checks returned ${restore_status}, expected 0" >&2; exit 1; }
# Keep exact pre/post-restore definitions as evidence if canonical validation fails.
for endpoint in source target; do
  docker exec "${network}-${endpoint}" "${CLIENT}" -uroot -p"${password}" \
    --database=snapshot_restore --batch --raw -e 'SHOW CREATE TABLE identities' \
    >"${ARTIFACT_DIR}/snapshot-${endpoint}-identities.sql"
done
foundation target check >"${ARTIFACT_DIR}/snapshot-restored-check.log" 2>&1
controller_migrate target >"${ARTIFACT_DIR}/snapshot-restored-migrate.log" 2>&1
foundation target check >>"${ARTIFACT_DIR}/snapshot-restored-check.log" 2>&1
receipts "${target_container}" >"${ARTIFACT_DIR}/snapshot-restored-receipts.txt"
cmp "${ARTIFACT_DIR}/snapshot-source-receipts.txt" "${ARTIFACT_DIR}/snapshot-restored-receipts.txt"
[[ "$(docker exec "${target_container}" "${CLIENT}" -uroot -p"${password}" -Nse \
  "SELECT subject FROM snapshot_restore.identities WHERE id=UNHEX(REPEAT('11',16))")" == snapshot-restore-marker ]]
runtime_read="$(docker exec -e MYSQL_PWD=snapshot-test-only "${target_container}" "${CLIENT}" \
  --protocol=TCP -h127.0.0.1 -usnapshot_runtime --database=snapshot_restore -Nse \
  "SELECT subject FROM identities WHERE id=UNHEX(REPEAT('11',16)); SELECT COUNT(*) FROM schema_revisions WHERE epoch=2 AND revision=1 AND state='verified';")"
[[ "${runtime_read}" == $'snapshot-restore-marker\n1' ]]
printf '%s\n' "${runtime_read}" >"${ARTIFACT_DIR}/snapshot-runtime-read.log"
if docker exec -e MYSQL_PWD=snapshot-test-only "${target_container}" "${CLIENT}" \
  --protocol=TCP -h127.0.0.1 -usnapshot_runtime --database=snapshot_restore \
  -e 'CREATE TABLE runtime_ddl_must_fail(id INT)' >"${ARTIFACT_DIR}/snapshot-runtime-ddl.log" 2>&1; then
  echo 'restored runtime account unexpectedly has owner DDL privileges' >&2
  exit 1
fi
grep -Fq 'CREATE command denied' "${ARTIFACT_DIR}/snapshot-runtime-ddl.log"
if docker exec -e MYSQL_PWD=snapshot-test-only "${target_container}" "${CLIENT}" \
  --protocol=TCP -h127.0.0.1 -usnapshot_runtime --database=snapshot_restore \
  -e 'UPDATE schema_revisions SET checksum=checksum' >"${ARTIFACT_DIR}/snapshot-runtime-journal-write.log" 2>&1; then
  echo 'restored runtime can write the migration journal' >&2
  exit 1
fi
grep -Fq 'UPDATE command denied' "${ARTIFACT_DIR}/snapshot-runtime-journal-write.log"
printf 'snapshot_checksum=%s\nsnapshot_backup_id=%s\nvalidation=passed\nreceipts=unchanged\nlegacy_tables=absent\nruntime_read=passed\nruntime_ddl=denied\n' \
  "${snapshot_checksum}" "${snapshot_backup_id}" >"${ARTIFACT_DIR}/snapshot-restore-summary.txt"

printf 'backend=%s\nbackup_id=%s\nobjects=%s\n' "${ENGINE}" "${backup_id}" "${objects//$'\n'/,}" \
  >"${ARTIFACT_DIR}/restore-summary.txt"
