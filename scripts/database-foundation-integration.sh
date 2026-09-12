#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
scope="${DATABASE_TEST_SCOPE:-full}"
case "${scope}" in
  full|regression) ;;
  *) echo 'DATABASE_TEST_SCOPE must be full or regression' >&2; exit 2 ;;
esac
if [[ "${scope}" != full && -n "${DATABASE_FULL_PART+x}" ]]; then
  echo 'DATABASE_FULL_PART is only valid with DATABASE_TEST_SCOPE=full' >&2; exit 2
fi
part="${DATABASE_FULL_PART-all}"
case "${part}" in
  all|current|history) ;;
  *) echo 'DATABASE_FULL_PART must be all, current or history' >&2; exit 2 ;;
esac
ENGINE="${ENGINE:?ENGINE must be mysql or mariadb}"
case "${ENGINE}" in
  mysql) IMAGE='mysql:8.4.10@sha256:8dbcf531a03aade657e181b9cf2f1d1803ce621a1d55610cb44cb531ab7d7db6'; CLIENT=mysql ;;
  mariadb) IMAGE='mariadb:12.3.2@sha256:a02fe89cb597d4375812b2eac90cf9d0775d4686daa7f7cc750ebbcad7525bbc'; CLIENT=mariadb ;;
  *) echo 'ENGINE must be mysql or mariadb' >&2; exit 2 ;;
esac
NAME="ocservia-pr02-${ENGINE}-$$"
TLS_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/ocservia-pr02-tls-XXXXXX")"
TLS_DIR="${TLS_ROOT}/certs"
mkdir "${TLS_DIR}"
diagnostics() {
  local query
  # Avoid query text (which may contain credentials); retain bounded wait/I/O evidence.
  for query in \
    'SELECT ID,USER,HOST,DB,COMMAND,TIME,STATE FROM information_schema.PROCESSLIST ORDER BY TIME DESC LIMIT 20' \
    'SHOW ENGINE INNODB STATUS' \
    'SELECT DIGEST,COUNT_STAR,SUM_TIMER_WAIT,SUM_LOCK_TIME FROM performance_schema.events_statements_summary_by_digest ORDER BY SUM_TIMER_WAIT DESC LIMIT 20' \
    "SELECT OBJECT_TYPE,OBJECT_SCHEMA,OBJECT_NAME,LOCK_TYPE,LOCK_STATUS FROM performance_schema.metadata_locks WHERE LOCK_STATUS='PENDING' LIMIT 20" \
    'SELECT EVENT_NAME,COUNT_STAR,SUM_TIMER_WAIT FROM performance_schema.file_summary_by_event_name ORDER BY SUM_TIMER_WAIT DESC LIMIT 20'; do
    timeout 10s docker exec -e MYSQL_PWD=pr02-isolated-test-root "${NAME}" "${CLIENT}" \
      --protocol=TCP -h127.0.0.1 -uroot -e "${query}" 2>&1 \
      | sed -E '/password|IDENTIFIED|@tcp\(/Id; s/pr02-[a-z-]+/(redacted)/g' \
      | head -c 16384 || true
  done
}
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ "${scope}" == full && ${status} -ne 0 ]]; then
    echo "Failed full shard: ${ENGINE}/${part} (exit ${status})" >&2
    diagnostics || true
  fi
  docker rm -fv "${NAME}" >/dev/null 2>&1 || true
  rm -rf "${TLS_ROOT}"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=ocservia-pr02-test' \
  -addext 'subjectAltName=IP:127.0.0.1' -keyout "${TLS_DIR}/server-key.pem" \
  -out "${TLS_DIR}/server-cert.pem" >/dev/null 2>&1
# The enclosing temporary directory is private on the host. The container's
# unprivileged database user must be able to read its isolated test key.
chmod 755 "${TLS_DIR}"
chmod 644 "${TLS_DIR}"/*.pem
# Ephemeral test credentials only, never deployment defaults. Loopback binding
# is required even in CI; the port is allocated by Docker to avoid collisions.
docker run -d --name "${NAME}" -p 127.0.0.1::3306 \
  -v "${TLS_DIR}:/tls:ro" \
  -e MYSQL_ROOT_PASSWORD=pr02-isolated-test-root -e MYSQL_DATABASE=ocservia \
  -e MARIADB_ROOT_PASSWORD=pr02-isolated-test-root -e MARIADB_DATABASE=ocservia \
  "${IMAGE}" --log-bin-trust-function-creators=1 \
  --ssl-ca=/tls/server-cert.pem --ssl-cert=/tls/server-cert.pem --ssl-key=/tls/server-key.pem >/dev/null
ready=false
for ((i=0; i<90; i++)); do
  # The image initializes through a temporary socket-only server. Wait for
  # the final TCP listener, not that server which is about to shut down.
  if docker exec -e MYSQL_PWD=pr02-isolated-test-root "${NAME}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot -Nse 'SELECT 1' >/dev/null 2>&1; then ready=true; break; fi
  sleep 1
done
[[ "${ready}" == true ]] || { echo 'database did not become ready' >&2; exit 1; }
docker exec -i -e MYSQL_PWD=pr02-isolated-test-root "${NAME}" "${CLIENT}" --protocol=TCP -h127.0.0.1 -uroot <<'SQL'
CREATE USER 'ocservia_owner'@'%' IDENTIFIED BY 'pr02-owner-test-only';
CREATE USER 'ocservia_app'@'%' IDENTIFIED BY 'pr02-runtime-test-only';
CREATE USER 'ocservia_maintenance'@'%' IDENTIFIED BY 'pr02-maintenance-test-only';
SQL
PORT="$(docker port "${NAME}" 3306/tcp | sed 's/127.0.0.1://')"
export PR02_ENGINE="${ENGINE}"
export PR02_TLS_CA_FILE="${TLS_DIR}/server-cert.pem"
export PR02_DSN="root:pr02-isolated-test-root@tcp(127.0.0.1:${PORT})/ocservia?tls=false"
if [[ "${scope}" == regression ]]; then
  (cd "${ROOT}/control-plane" && bash "${ROOT}/scripts/required-go-tests.sh" regression-mysql --select -race -timeout=60m)
  for group in regression-disconnect regression-outbox regression-fencing regression-auth regression-telemetry; do
    (cd "${ROOT}/control-plane" && bash "${ROOT}/scripts/required-go-tests.sh" "${group}" --select -race -timeout=10m)
  done
else
echo "Full database acceptance: ${ENGINE}/${part}"
selection="$(jq -nr --arg group backend-mysql-history --arg mode select \
  --rawfile manifest "${ROOT}/scripts/required-go-tests.txt" \
  -f "${ROOT}/scripts/check-required-go-tests.jq")"
IFS=$'\t' read -r package history_pattern <<<"${selection}"
[[ -n "${package}" && -n "${history_pattern}" && "${history_pattern}" != */* ]] || {
  echo 'history selection must contain only top-level tests' >&2; exit 2
}
if [[ "${part}" != history ]]; then
  (cd "${ROOT}/control-plane" && bash "${ROOT}/scripts/required-go-tests.sh" backend-mysql-current-full -race -timeout=60m "${package}" -skip "${history_pattern}")
fi
if [[ "${part}" != current ]]; then
  (cd "${ROOT}/control-plane" && bash "${ROOT}/scripts/required-go-tests.sh" backend-mysql-history --select -race -timeout=60m)
fi
if [[ "${part}" == history ]]; then exit 0; fi
(cd "${ROOT}/control-plane" && bash "${ROOT}/scripts/required-go-tests.sh" backend-coordination -race -timeout=10m ./internal/operations -run '^Test(OutboxBackend|FencingBackend|CoordinationDeadlockBackend)Integration$')
(cd "${ROOT}/control-plane" && bash "${ROOT}/scripts/required-go-tests.sh" backend-auth -race -timeout=10m ./internal/api -run '^TestAuthenticationBackend(HTTP|Safety|Legacy)Integration$')
(cd "${ROOT}/control-plane" && go test -count=1 -race -timeout=5m -v ./internal/telemetry -run '^TestTelemetryBackendWorkflowIntegration$')
fi
# Controller test/development selection must not unlock production startup.
(cd "${ROOT}/control-plane" && go test -count=1 ./internal/platform/config ./cmd/ocserv-db-foundation)
