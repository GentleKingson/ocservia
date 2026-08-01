#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

RUN_ID="${RUN_ID:-I02-local-$(date -u +%Y%m%dT%H%M%SZ)}"
PREFIX="$(printf '%s' "${RUN_ID}" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
TMP_ROOT="${TMPDIR:-/tmp}/ocservia-${RUN_ID}"
BIN="${TMP_ROOT}/ocserv-control"
PIDS=()
CONTAINERS=()

cleanup() {
  local exit_code=$?
  for pid in "${PIDS[@]:-}"; do
    kill -TERM "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  done
  for container in "${CONTAINERS[@]:-}"; do
    docker rm -f "${container}" >/dev/null 2>&1 || true
  done
  rm -rf "${TMP_ROOT}"
  exit "${exit_code}"
}
trap cleanup EXIT INT TERM

mkdir -p "${TMP_ROOT}"
(cd "${ROOT}/control-plane" && go build -trimpath -o "${BIN}" ./cmd/ocserv-control)

wait_for_postgres() {
  local container=$1
  for _ in $(seq 1 60); do
    if docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT 1" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

wait_for_http() {
  local url=$1
  for _ in $(seq 1 60); do
    if curl --fail --silent "${url}" >/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}

wait_for_tcp() {
  local host=$1
  local port=$2
  for _ in $(seq 1 60); do
    if (exec 3<>"/dev/tcp/${host}/${port}") 2>/dev/null; then
      exec 3>&- 3<&-
      return 0
    fi
    sleep 1
  done
  return 1
}

stop_process() {
  local pid=$1
  kill -TERM "${pid}"
  wait "${pid}"
}

for major in 17 18; do
  container="${PREFIX}-pg${major}"
  CONTAINERS+=("${container}")
  docker run -d --name "${container}" \
    -e POSTGRES_DB=ocservia -e POSTGRES_USER=ocservia_owner -e POSTGRES_PASSWORD=test-owner-only \
    -p "127.0.0.1::5432" "postgres:${major}-bookworm" >/dev/null
  wait_for_postgres "${container}"
  port="$(docker port "${container}" 5432/tcp | sed -n 's/.*://p')"
  owner_url="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/ocservia?sslmode=disable"
  runtime_url="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/ocservia?sslmode=disable"
  api_port=$((18100 + major))

  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "CREATE ROLE ocservia_app LOGIN PASSWORD 'test-runtime-only'" >/dev/null
  OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${owner_url}" \
    OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app "${BIN}" --migrate-only \
    >"${TMP_ROOT}/pg${major}-migrate.log" 2>&1

  OCSERV_ENVIRONMENT=test OCSERV_HTTP_ADDRESS="127.0.0.1:${api_port}" \
    OCSERV_DATABASE_URL="${runtime_url}" "${BIN}" --role=all \
    >"${TMP_ROOT}/pg${major}-fresh.log" 2>&1 &
  pid=$!
  PIDS+=("${pid}")
  wait_for_http "http://127.0.0.1:${api_port}/readyz"
  curl --fail --silent "http://127.0.0.1:${api_port}/livez" >/dev/null
  curl --fail --silent "http://127.0.0.1:${api_port}/version" | grep -q '"role":"all"'
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 1")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('workspaces','nodes','operations','audit_events')")" = "4"
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_app -d ocservia -c "
    INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ('00000000-0000-7000-8000-000000000001', 'One', 'one', now(), now()), ('00000000-0000-7000-8000-000000000002', 'Two', 'two', now(), now());
    INSERT INTO nodes (id, workspace_id, name, status, created_at, updated_at) VALUES ('00000000-0000-7000-8000-000000000003', '00000000-0000-7000-8000-000000000001', 'node', 'approved', now(), now());
    INSERT INTO audit_events (id, workspace_id, occurred_at, actor_type, actor_id, action, resource_type, request_id, result, event_hash) VALUES ('00000000-0000-7000-8000-000000000004', '00000000-0000-7000-8000-000000000001', now(), 'system', 'test', 'test', 'workspace', 'request', 'intent', decode('00', 'hex'));
  " >/dev/null
  if docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_app -d ocservia -c "INSERT INTO operations (id, workspace_id, node_id, state, request_id, created_at, updated_at) VALUES ('00000000-0000-7000-8000-000000000005', '00000000-0000-7000-8000-000000000002', '00000000-0000-7000-8000-000000000003', 'draft', 'request', now(), now())" >/dev/null 2>&1; then
    echo "cross-workspace operation was accepted" >&2
    exit 1
  fi
  if docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_app -d ocservia -c "DELETE FROM audit_events" >/dev/null 2>&1; then
    echo "audit event deletion was accepted" >&2
    exit 1
  fi
  if docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_app -d ocservia -c "ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only" >/dev/null 2>&1; then
    echo "runtime role disabled the audit append-only trigger" >&2
    exit 1
  fi
  stop_process "${pid}"

  OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${owner_url}" \
    OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app "${BIN}" --migrate-only \
    >"${TMP_ROOT}/pg${major}-repeat-migrate.log" 2>&1

  OCSERV_ENVIRONMENT=test OCSERV_HTTP_ADDRESS="127.0.0.1:${api_port}" \
    OCSERV_DATABASE_URL="${runtime_url}" "${BIN}" --role=all \
    >"${TMP_ROOT}/pg${major}-repeat.log" 2>&1 &
  pid=$!
  PIDS+=("${pid}")
  wait_for_http "http://127.0.0.1:${api_port}/readyz"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations")" = "1"
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "INSERT INTO schema_migrations (version, name, checksum) VALUES (2, '000002_future.up.sql', decode(repeat('00', 32), 'hex'))" >/dev/null
  test "$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:${api_port}/readyz")" = "503"
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 2" >/dev/null
  wait_for_http "http://127.0.0.1:${api_port}/readyz"

  docker stop "${container}" >/dev/null
  test "$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:${api_port}/readyz")" = "503"
  stop_process "${pid}"

  docker start "${container}" >/dev/null
  wait_for_postgres "${container}"
  port="$(docker port "${container}" 5432/tcp | sed -n 's/.*://p')"
  owner_url="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/ocservia?sslmode=disable"
  runtime_url="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/ocservia?sslmode=disable"
  wait_for_tcp 127.0.0.1 "${port}"
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "INSERT INTO schema_migrations (version, name, checksum) VALUES (2, '000002_future.up.sql', decode(repeat('00', 32), 'hex'))" >/dev/null
  if OCSERV_ENVIRONMENT=test OCSERV_HTTP_ADDRESS="127.0.0.1:${api_port}" \
    OCSERV_DATABASE_URL="${runtime_url}" "${BIN}" --role=all \
    >"${TMP_ROOT}/pg${major}-unknown-version.log" 2>&1; then
    echo "binary accepted an unknown schema version" >&2
    exit 1
  fi
  if ! grep -Fq 'database schema version 2 is unknown to this binary' "${TMP_ROOT}/pg${major}-unknown-version.log"; then
    cat "${TMP_ROOT}/pg${major}-unknown-version.log" >&2
    echo "binary failed for an unexpected reason with an unknown schema version" >&2
    exit 1
  fi
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 2" >/dev/null
done

container="${PREFIX}-upgrade"
CONTAINERS+=("${container}")
docker run -d --name "${container}" \
  -e POSTGRES_DB=ocservia -e POSTGRES_USER=ocservia_owner -e POSTGRES_PASSWORD=test-owner-only \
  -p "127.0.0.1::5432" postgres:18-bookworm >/dev/null
wait_for_postgres "${container}"
port="$(docker port "${container}" 5432/tcp | sed -n 's/.*://p')"
docker exec "${container}" psql -U ocservia_owner -d ocservia -c \
  "CREATE TABLE pre_i02_marker (id bigint PRIMARY KEY); INSERT INTO pre_i02_marker (id) VALUES (1)" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "CREATE ROLE ocservia_app LOGIN PASSWORD 'test-runtime-only'" >/dev/null
owner_url="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/ocservia?sslmode=disable"
runtime_url="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/ocservia?sslmode=disable"
OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${owner_url}" \
  OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app "${BIN}" --migrate-only \
  >"${TMP_ROOT}/upgrade.log" 2>&1
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 1")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pre_i02_marker WHERE id = 1")" = "1"
OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${runtime_url}" "${BIN}" --role=worker \
  >"${TMP_ROOT}/worker.log" 2>&1 &
pid=$!
PIDS+=("${pid}")
sleep 1
kill -0 "${pid}"
stop_process "${pid}"

for role in scheduler api; do
  OCSERV_ENVIRONMENT=test OCSERV_HTTP_ADDRESS="127.0.0.1:18199" \
    OCSERV_DATABASE_URL="${runtime_url}" "${BIN}" "--role=${role}" \
    >"${TMP_ROOT}/${role}.log" 2>&1 &
  pid=$!
  PIDS+=("${pid}")
  sleep 1
  kill -0 "${pid}"
  stop_process "${pid}"
done
