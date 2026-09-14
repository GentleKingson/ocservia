#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENGINE="${ENGINE:?ENGINE must be mysql or mariadb}"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:?ARTIFACT_DIR is required}"
case "${ENGINE}" in
  mysql)
    SERVER_IMAGE='mysql:8.4.10@sha256:8dbcf531a03aade657e181b9cf2f1d1803ce621a1d55610cb44cb531ab7d7db6'
    DOCKERFILE=deploy/production/backup.mysql.Dockerfile
    CLIENT=mysql
    ;;
  mariadb)
    SERVER_IMAGE='mariadb:12.3.2@sha256:a02fe89cb597d4375812b2eac90cf9d0775d4686daa7f7cc750ebbcad7525bbc'
    DOCKERFILE=deploy/production/backup.mariadb.Dockerfile
    CLIENT=mariadb
    ;;
  *) echo "ENGINE must be mysql or mariadb" >&2; exit 2 ;;
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
  docker rm -f "${source_container}" "${target_container}" "${backup_container}" >/dev/null 2>&1 || true
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
  if docker exec "${source_container}" "${CLIENT}" -uroot -p"${password}" -e 'SELECT 1' >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "${source_container}" "${CLIENT}" -uroot -p"${password}" -e 'SELECT 1' >/dev/null
docker exec -i "${source_container}" "${CLIENT}" -uroot -p"${password}" <<'SQL'
CREATE DATABASE ocservia;
USE ocservia;
CREATE TABLE controller_schema_compatibility(singleton INT PRIMARY KEY,current_schema INT,minimum_compatible_controller_schema INT);
INSERT INTO controller_schema_compatibility VALUES(1,36,36);
CREATE TABLE audit_events(id INT PRIMARY KEY,event_hash BINARY(32) NOT NULL,event_mac BINARY(32) NOT NULL);
INSERT INTO audit_events VALUES(1,REPEAT(0x11,32),REPEAT(0x22,32));
CREATE TABLE identities(id INT PRIMARY KEY,marker VARCHAR(32));
INSERT INTO identities VALUES(1,'source');
CREATE TABLE scheduler_leadership(id INT PRIMARY KEY,epoch BIGINT NOT NULL,lease_until DATETIME(6));
INSERT INTO scheduler_leadership VALUES(1,7,UTC_TIMESTAMP(6)-INTERVAL 1 SECOND);
CREATE TABLE operations(id INT PRIMARY KEY,state VARCHAR(32),idempotency_key VARCHAR(64));
INSERT INTO operations VALUES(1,'succeeded','restore-idempotency');
CREATE TABLE commands(id INT PRIMARY KEY,state VARCHAR(32));
INSERT INTO commands VALUES(1,'succeeded');
CREATE TRIGGER identities_marker BEFORE INSERT ON identities FOR EACH ROW SET NEW.marker=LOWER(NEW.marker);
CREATE PROCEDURE restore_probe() SELECT COUNT(*) FROM identities;
CREATE EVENT restore_event ON SCHEDULE EVERY 1 DAY DO INSERT INTO identities VALUES(99,'EVENT');
SQL
if [[ "${ENGINE}" == mysql ]]; then
  docker exec "${source_container}" "${CLIENT}" -uroot -p"${password}" -e \
    "CREATE USER 'backup'@'%' IDENTIFIED BY '${backup_password}'; GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON ocservia.* TO 'backup'@'%'; GRANT SHOW_ROUTINE ON *.* TO 'backup'@'%';"
else
  docker exec "${source_container}" "${CLIENT}" -uroot -p"${password}" -e \
    "CREATE USER 'backup'@'%' IDENTIFIED BY '${backup_password}'; GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON ocservia.* TO 'backup'@'%'; GRANT SELECT ON mysql.proc TO 'backup'@'%';"
fi

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
  if docker exec "${target_container}" "${CLIENT}" -uroot -p"${password}" -e 'SELECT 1' >/dev/null 2>&1; then break; fi
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
printf 'backend=%s\nbackup_id=%s\nobjects=%s\n' "${ENGINE}" "${backup_id}" "${objects//$'\n'/,}" \
  >"${ARTIFACT_DIR}/restore-summary.txt"
