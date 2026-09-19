#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${ROOT}/scripts/env.sh"
source "${ROOT}/scripts/go-test-environment.sh"
require_test_commands go jq setsid
require_test_docker
case "${DATABASE_TEST_SCOPE:-smoke}" in
  smoke|compatibility) ;;
  *) echo 'PostgreSQL smoke requires smoke or compatibility scope' >&2; exit 2 ;;
esac
case "${PG_MAJOR:-17}" in
  17) image='postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f' ;;
  18) image='postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af' ;;
  *) echo 'PG_MAJOR must be 17 or 18' >&2; exit 2 ;;
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
docker run -d --name "${name}" -p '127.0.0.1::5432' \
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
if [[ "${DATABASE_TEST_SCOPE:-smoke}" == compatibility ]]; then
  docker exec "${name}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d postgres -c 'CREATE DATABASE upgrade_smoke' >/dev/null
  export OCSERV_TEST_UPGRADE_DATABASE_URL="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/upgrade_smoke?sslmode=disable"
  bash "${ROOT}/scripts/required-go-tests.sh" --smoke ./migrations TestDatabaseUpgradeSmoke
fi
