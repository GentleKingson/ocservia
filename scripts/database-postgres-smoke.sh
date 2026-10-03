#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
source "${ROOT}/scripts/go-test-environment.sh"
require_test_commands go jq setsid python3
require_test_docker
require_go_race
case "${DATABASE_TEST_SCOPE:-smoke}" in
  smoke) ;;
  *) echo 'PostgreSQL smoke requires smoke scope' >&2; exit 2 ;;
esac
case "${PG_MAJOR:-18}" in
  18) image='postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af' ;;
  *) echo 'PG_MAJOR must be 18' >&2; exit 2 ;;
esac
name="ocservia-pg-smoke-$$"
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  docker rm -fv "${name}" >/dev/null 2>&1 || true
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# A stable host port keeps the original pool/DSN valid across container restart.
port="$(python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    print(sock.getsockname()[1])
PY
)"
docker run -d --name "${name}" -p "127.0.0.1:${port}:5432" \
  -e POSTGRES_DB=ocservia -e POSTGRES_USER=ocservia_owner \
  -e POSTGRES_PASSWORD=test-owner-only "${image}" >/dev/null
ready=false
for ((i=0; i<60; i++)); do
  if docker exec -e PGPASSWORD=test-owner-only "${name}" psql -h127.0.0.1 -U ocservia_owner -d ocservia -Atc 'SELECT 1' >/dev/null 2>&1; then ready=true; break; fi
  sleep 1
done
[[ "${ready}" == true ]] || { echo 'PostgreSQL did not become ready' >&2; exit 1; }
docker exec "${name}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  -c "CREATE ROLE ocservia_app LOGIN PASSWORD 'test-runtime-only'" >/dev/null
port="$(docker port "${name}" 5432/tcp | sed 's/127.0.0.1://')"
export OCSERV_TEST_OWNER_DATABASE_URL="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/ocservia?sslmode=disable"
export OCSERV_TEST_DATABASE_URL="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/ocservia?sslmode=disable"
unset PR02_DSN PR02_ENGINE
cd "${ROOT}/control-plane"
bash "${ROOT}/scripts/required-go-tests.sh" --smoke ./internal/platform/app TestDatabaseCoreSmoke
docker exec "${name}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d postgres -c 'CREATE DATABASE initialization_smoke' >/dev/null
export OCSERV_TEST_INITIALIZATION_DATABASE_URL="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/initialization_smoke?sslmode=disable"
bash "${ROOT}/scripts/required-go-tests.sh" --smoke ./migrations TestDatabaseInitializationSmoke
bash "${ROOT}/scripts/test-enrollment-restart.sh" "${name}"
