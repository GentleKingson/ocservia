#!/usr/bin/env bash
# Compare the immutable published checkpoint upgrade with current fresh SQL.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
mode="${1:-check}"
case "$mode" in check) ;; *) echo 'usage: database-mysql-snapshot.sh [check]' >&2; exit 2 ;; esac
name="ocservia-mysql-snapshot-$$"
cleanup() { docker rm -fv "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT
image='mysql:8.4.10@sha256:8dbcf531a03aade657e181b9cf2f1d1803ce621a1d55610cb44cb531ab7d7db6'
docker run -d --name "$name" -p 127.0.0.1::3306 -e MYSQL_ROOT_PASSWORD=pr02-isolated-test-root -e MYSQL_DATABASE=ocservia "$image" --log-bin-trust-function-creators=1 --innodb-flush-log-at-trx-commit=2 --sync-binlog=0 >/dev/null
ready=false
for ((i=0;i<90;i++)); do
 if docker exec -e MYSQL_PWD=pr02-isolated-test-root "$name" mysql --protocol=TCP -h127.0.0.1 -uroot -Nse 'SELECT 1' >/dev/null 2>&1; then ready=true; break; fi
 sleep 1
done
[[ "$ready" == true ]] || { echo 'snapshot MySQL not ready' >&2; exit 1; }
docker exec -i -e MYSQL_PWD=pr02-isolated-test-root "$name" mysql --protocol=TCP -h127.0.0.1 -uroot <<'SQL'
CREATE USER 'ocservia_owner'@'%' IDENTIFIED BY 'pr02-owner-test-only';
CREATE USER 'ocservia_app'@'%' IDENTIFIED BY 'pr02-runtime-test-only';
CREATE USER 'ocservia_maintenance'@'%' IDENTIFIED BY 'pr02-maintenance-test-only';
SELECT VERSION();
SQL
port="$(docker port "$name" 3306/tcp | sed 's/127.0.0.1://')"
export PR02_ENGINE=mysql PR02_DSN="root:pr02-isolated-test-root@tcp(127.0.0.1:${port})/ocservia?tls=false"
unset PR02_SNAPSHOT_DIRECTORY PR02_SNAPSHOT_CHECK

cd "${ROOT}/control-plane"
bash "${ROOT}/scripts/required-go-tests.sh" --smoke ./internal/database/mysql TestMySQLCutoverCheckpointEquivalence
