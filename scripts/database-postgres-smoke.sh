#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
source "${ROOT}/scripts/go-test-environment.sh"
# shellcheck source=scripts/postgres-test-common.sh
source "${ROOT}/scripts/postgres-test-common.sh"
require_test_commands go jq setsid python3
require_test_docker
require_go_race
case "${DATABASE_TEST_SCOPE:-smoke}" in
  smoke) ;;
  *) echo 'PostgreSQL smoke requires smoke scope' >&2; exit 2 ;;
esac
case "${PG_MAJOR:-18}" in
  18) image="${POSTGRES_18_IMAGE}" ;;
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
port="$(postgres_allocate_loopback_port)"
docker run -d --name "${name}" -p "127.0.0.1:${port}:5432" \
  -e POSTGRES_DB=ocservia -e POSTGRES_USER=ocservia_owner \
  -e POSTGRES_PASSWORD=test-owner-only "${image}" >/dev/null
bash "${ROOT}/scripts/database-artifact-policy.sh" postgres
postgres_wait_tcp "${name}" || { echo 'PostgreSQL did not become ready' >&2; exit 1; }
docker exec "${name}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  -c "CREATE ROLE ocservia_app LOGIN PASSWORD 'test-runtime-only'" >/dev/null
port="$(docker port "${name}" 5432/tcp | sed 's/127.0.0.1://')"
export OCSERV_TEST_OWNER_DATABASE_URL="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/ocservia?sslmode=disable"
export OCSERV_TEST_DATABASE_URL="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/ocservia?sslmode=disable"
cd "${ROOT}/control-plane"
bash "${ROOT}/scripts/required-go-tests.sh" --smoke ./internal/database/connection TestControllerLockSessionLoss
bash "${ROOT}/scripts/required-go-tests.sh" --smoke ./internal/platform/app TestDatabaseCoreSmoke
docker exec "${name}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d postgres -c 'CREATE DATABASE initialization_smoke' >/dev/null
export OCSERV_TEST_INITIALIZATION_DATABASE_URL="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/initialization_smoke?sslmode=disable&pool_max_conns=1"
bash "${ROOT}/scripts/required-go-tests.sh" --smoke ./migrations TestDatabaseInitializationSmoke
bash "${ROOT}/scripts/test-enrollment-restart.sh" "${name}"
