#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

UPSTREAM_MANIFEST="${ROOT}/docs/upstream/v4.9-post1.manifest.json"
EXPECTED_UPSTREAM_RECORD="$(jq -r '[(.repository | sub("^https://github.com/"; "")), .old.ref, .old.commit, .new.ref, .new.commit, .imported_at] | join("|")' "${UPSTREAM_MANIFEST}")"
EXPECTED_UPSTREAM_ROLLBACK='publication: revert PR15 independently; implementation: stop I14 scheduler/API, reconcile commands, revert PR14, then apply migration 000013 down only when policy and batch data need not be retained'

RUN_ID="${RUN_ID:-database-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-${GITHUB_JOB:-job}-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
PREFIX="$(printf '%s' "${RUN_ID}" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
TMP_BASE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
TMP_ROOT="$(mktemp -d "${TMP_BASE%/}/ocservia-${PREFIX}-XXXXXX")"
BIN="${TMP_ROOT}/ocserv-control"
ARTIFACT_DIR="${ARTIFACT_DIR:-}"
API_PORT_BASE=$((18000 + $(printf '%s' "${RUN_ID}" | cksum | awk '{print $1}') % 10000))
PIDS=()
CONTAINERS=()

cleanup() {
  local exit_code=$? cleanup_exit=0 container
  trap - EXIT INT TERM
  set +e
  if [[ -n "${ARTIFACT_DIR}" ]]; then
    mkdir -p "${ARTIFACT_DIR}" || cleanup_exit=1
    docker ps -a --filter "name=${PREFIX}" >"${ARTIFACT_DIR}/docker-ps.txt" 2>&1 || true
    for container in "${CONTAINERS[@]:-}"; do
      [[ -n "${container}" ]] || continue
      docker logs "${container}" >"${ARTIFACT_DIR}/postgres-${container}.log" 2>&1 || true
    done
    cp -f "${TMP_ROOT}"/*.log "${ARTIFACT_DIR}/" 2>/dev/null || true
  fi
  for pid in "${PIDS[@]:-}"; do
    [[ -n "${pid}" ]] || continue
    kill -TERM "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  done
  for container in "${CONTAINERS[@]:-}"; do
    [[ -n "${container}" ]] || continue
    docker rm -f "${container}" >/dev/null 2>&1 || cleanup_exit=1
    if docker inspect "${container}" >/dev/null 2>&1; then
      echo "database integration left container ${container}" >&2
      cleanup_exit=1
    fi
  done
  rm -rf "${TMP_ROOT}"
  if ((exit_code != 0)); then
    exit "${exit_code}"
  fi
  exit "${cleanup_exit}"
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
  local index
  for index in "${!PIDS[@]}"; do
    if [[ "${PIDS[index]}" == "${pid}" ]]; then
      PIDS[index]=""
    fi
  done
  return 0
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
  api_port=$((API_PORT_BASE + major - 17))

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
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('workspaces','nodes','operations','audit_events','local_slice_jobs','transport_events','enrollment_tokens','node_endpoint_keys','node_capabilities','telemetry_ingest_batches','node_observed_snapshots','node_sessions','telemetry_security_events','telemetry_samples','telemetry_rollups_5m','telemetry_rollups_1h')")" = "16"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('commands','command_attempts','outbox_events','node_command_leases','operation_events')")" = "5"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 6")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 7")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 8")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 9")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 10")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 11")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 12")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 13")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('desired_users','desired_groups','observed_users','observed_groups')")" = "4"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('desired_user_policies','user_policy_mutations','observed_user_usage','user_usage_cursors','scheduler_leases','user_policy_enforcements','batch_operations','batch_operation_items','upstream_sync_records')")" = "9"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT concat_ws('|',repository,old_ref,old_commit,new_ref,new_commit,to_char(synced_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')) FROM upstream_sync_records")" = "${EXPECTED_UPSTREAM_RECORD}"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT rollback_ref FROM upstream_sync_records")" = "${EXPECTED_UPSTREAM_ROLLBACK}"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'operations' AND column_name = 'command_id' AND data_type = 'uuid'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'agent_command_results' AND column_name = 'semantic_payload_hash_version' AND data_type = 'smallint'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pg_constraint WHERE conrelid = 'agent_command_results'::regclass AND conname = 'agent_command_results_semantic_payload_hash_version_supported'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_column_privilege('ocservia_app', 'transport_events', 'transport_cursor_valid', 'UPDATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_column_privilege('ocservia_app', 'transport_events', 'event_type', 'UPDATE')")" = "f"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_sequence_privilege('ocservia_app', 'transport_events_ingest_sequence_seq', 'USAGE')")" = "t"
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_app -d ocservia -c "
    INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ('00000000-0000-7000-8000-000000000001', 'One', 'one', now(), now()), ('00000000-0000-7000-8000-000000000002', 'Two', 'two', now(), now());
    INSERT INTO nodes (id, workspace_id, name, status, created_at, updated_at) VALUES ('00000000-0000-7000-8000-000000000003', '00000000-0000-7000-8000-000000000001', 'node', 'active', now(), now());
    INSERT INTO audit_events (id, workspace_id, occurred_at, actor_type, actor_id, action, resource_type, request_id, result, event_hash) VALUES ('00000000-0000-7000-8000-000000000004', '00000000-0000-7000-8000-000000000001', now(), 'system', 'test', 'test', 'workspace', 'request', 'intent', decode(repeat('00', 32), 'hex'));
  " >/dev/null
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c "
    DO \$\$
    DECLARE start_at timestamptz := date_trunc('month', now()) - interval '2 months';
    DECLARE partition_name text := 'telemetry_samples_' || to_char(start_at, 'YYYYMM');
    BEGIN
      EXECUTE format('CREATE TABLE public.%I PARTITION OF public.telemetry_samples FOR VALUES FROM (%L) TO (%L)', partition_name, start_at, start_at + interval '1 month');
    END \$\$;
  " >/dev/null
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_app -d ocservia -c \
    "SELECT telemetry_drop_expired_partitions(now() - interval '14 days')" >/dev/null
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pg_class WHERE relname = 'telemetry_samples_' || to_char(date_trunc('month', now()) - interval '2 months', 'YYYYMM')")" = "0"
  stop_process "${pid}"
  (cd "${ROOT}/control-plane" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    go test -p 1 ./internal/operations ./internal/enrollment ./internal/localslice ./internal/telemetry ./internal/userstate ./internal/useroperations ./internal/approvals ./internal/audit ./internal/rbac ./internal/auth -run Integration -count=1)
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
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND tablename='agent_command_results' AND indexname IN ('agent_command_results_pkey','agent_command_results_command_created_idx')")" = "2"
  docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    <"${ROOT}/control-plane/migrations/000013_quota_expiry_batch_backport.down.sql" >/dev/null
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 13" >/dev/null
  docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    <"${ROOT}/control-plane/migrations/000012_user_group_desired_observed.down.sql" >/dev/null
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 12" >/dev/null
  docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    <"${ROOT}/control-plane/migrations/000011_oidc_rbac_approvals_audit.down.sql" >/dev/null
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 11" >/dev/null
  docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    <"${ROOT}/control-plane/migrations/000010_controlled_session_operations.down.sql" >/dev/null
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 10" >/dev/null
  docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    <"${ROOT}/control-plane/migrations/000009_restrict_semantic_payload_hash_versions.down.sql" >/dev/null
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 9" >/dev/null
  docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    <"${ROOT}/control-plane/migrations/000008_semantic_payload_hash_version.down.sql" >/dev/null
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 8" >/dev/null
  docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    <"${ROOT}/control-plane/migrations/000007_agent_command_results.down.sql" >/dev/null
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 7" >/dev/null
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='agent_command_results'")" = "0"
  OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${owner_url}" \
    OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app "${BIN}" --migrate-only \
    >"${TMP_ROOT}/pg${major}-up-after-down.log" 2>&1
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 7")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 8")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 9")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 10")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 11")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 12")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 13")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND tablename='agent_command_results' AND indexname IN ('agent_command_results_pkey','agent_command_results_command_created_idx')")" = "2"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='agent_command_results' AND column_name='semantic_payload_hash_version'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pg_constraint WHERE conrelid = 'agent_command_results'::regclass AND conname = 'agent_command_results_semantic_payload_hash_version_supported'")" = "1"

  OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${owner_url}" \
    OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app "${BIN}" --migrate-only \
    >"${TMP_ROOT}/pg${major}-repeat-migrate.log" 2>&1

  OCSERV_ENVIRONMENT=test OCSERV_HTTP_ADDRESS="127.0.0.1:${api_port}" \
    OCSERV_DATABASE_URL="${runtime_url}" "${BIN}" --role=all \
    >"${TMP_ROOT}/pg${major}-repeat.log" 2>&1 &
  pid=$!
  PIDS+=("${pid}")
  wait_for_http "http://127.0.0.1:${api_port}/readyz"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations")" = "13"
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "INSERT INTO schema_migrations (version, name, checksum) VALUES (14, '000014_future.up.sql', decode(repeat('00', 32), 'hex'))" >/dev/null
  test "$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:${api_port}/readyz")" = "503"
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 14" >/dev/null
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
    "INSERT INTO schema_migrations (version, name, checksum) VALUES (14, '000014_future.up.sql', decode(repeat('00', 32), 'hex'))" >/dev/null
  if OCSERV_ENVIRONMENT=test OCSERV_HTTP_ADDRESS="127.0.0.1:${api_port}" \
    OCSERV_DATABASE_URL="${runtime_url}" "${BIN}" --role=all \
    >"${TMP_ROOT}/pg${major}-unknown-version.log" 2>&1; then
    echo "binary accepted an unknown schema version" >&2
    exit 1
  fi
  if ! grep -Fq 'database schema version 14 is unknown to this binary' "${TMP_ROOT}/pg${major}-unknown-version.log"; then
    cat "${TMP_ROOT}/pg${major}-unknown-version.log" >&2
    echo "binary failed for an unexpected reason with an unknown schema version" >&2
    exit 1
  fi
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 14" >/dev/null
done

container="${PREFIX}-upgrade"
CONTAINERS+=("${container}")
docker run -d --name "${container}" \
  -e POSTGRES_DB=ocservia -e POSTGRES_USER=ocservia_owner -e POSTGRES_PASSWORD=test-owner-only \
  -p "127.0.0.1::5432" postgres:18-bookworm >/dev/null
wait_for_postgres "${container}"
port="$(docker port "${container}" 5432/tcp | sed -n 's/.*://p')"
docker exec "${container}" psql -U ocservia_owner -d ocservia -c \
  "CREATE TABLE pre_i03_marker (id bigint PRIMARY KEY); INSERT INTO pre_i03_marker (id) VALUES (1)" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "CREATE TABLE schema_migrations (version bigint PRIMARY KEY, name text NOT NULL, checksum bytea NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000001_foundation.up.sql" >/dev/null
if command -v sha256sum >/dev/null 2>&1; then
  foundation_checksum="$(sha256sum "${ROOT}/control-plane/migrations/000001_foundation.up.sql" | awk '{print $1}')"
else
  foundation_checksum="$(shasum -a 256 "${ROOT}/control-plane/migrations/000001_foundation.up.sql" | awk '{print $1}')"
fi
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "INSERT INTO schema_migrations (version, name, checksum) VALUES (1, '000001_foundation.up.sql', decode('${foundation_checksum}', 'hex'))" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "CREATE ROLE ocservia_app LOGIN PASSWORD 'test-runtime-only'" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c "
  INSERT INTO workspaces (id,name,slug,created_at,updated_at) VALUES ('00000000-0000-7000-8000-000000000010','Legacy','legacy',now(),now());
  INSERT INTO nodes (id,workspace_id,name,status,created_at,updated_at) VALUES ('00000000-0000-7000-8000-000000000011','00000000-0000-7000-8000-000000000010','legacy-node','approved',now(),now());
" >/dev/null
owner_url="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/ocservia?sslmode=disable"
runtime_url="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/ocservia?sslmode=disable"
OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${owner_url}" \
  OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app "${BIN}" --migrate-only \
  >"${TMP_ROOT}/upgrade.log" 2>&1
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 1")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 2")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 3")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 4")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 5")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 6")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 7")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 8")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 9")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 10")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 11")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 12")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 13")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pg_constraint WHERE conrelid = 'agent_command_results'::regclass AND conname = 'agent_command_results_semantic_payload_hash_version_supported'")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pre_i03_marker WHERE id = 1")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT status FROM nodes WHERE id = '00000000-0000-7000-8000-000000000011'")" = "pending"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM node_endpoint_keys WHERE node_id = '00000000-0000-7000-8000-000000000011'")" = "0"
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

docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000013_quota_expiry_batch_backport.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 13" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000012_user_group_desired_observed.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 12" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000011_oidc_rbac_approvals_audit.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 11" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000010_controlled_session_operations.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 10" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000009_restrict_semantic_payload_hash_versions.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 9" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000008_semantic_payload_hash_version.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 8" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000007_agent_command_results.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 7" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000006_operations_outbox.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 6" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000005_telemetry_observed.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 5" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000004_enrollment_trust.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 4" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000003_transport_path_changed.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 3" >/dev/null
docker exec -i "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
  <"${ROOT}/control-plane/migrations/000002_local_slice.down.sql" >/dev/null
docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "DELETE FROM schema_migrations WHERE version = 2" >/dev/null
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('local_slice_jobs','transport_events','enrollment_tokens','node_endpoint_keys','node_capabilities','telemetry_ingest_batches','node_observed_snapshots','node_sessions','telemetry_security_events','telemetry_samples','telemetry_rollups_5m','telemetry_rollups_1h','commands','command_attempts','outbox_events','node_command_leases','operation_events','agent_command_results','desired_users','desired_groups','observed_users','observed_groups','desired_user_policies','user_policy_mutations','observed_user_usage','user_usage_cursors','scheduler_leases','user_policy_enforcements','batch_operations','batch_operation_items','upstream_sync_records')")" = "0"
OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${owner_url}" \
  OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app "${BIN}" --migrate-only \
  >"${TMP_ROOT}/rollback-forward.log" 2>&1
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 2")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 3")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 4")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 5")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 6")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 7")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 8")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 9")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 10")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 11")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 12")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 13")" = "1"
test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pg_constraint WHERE conrelid = 'agent_command_results'::regclass AND conname = 'agent_command_results_semantic_payload_hash_version_supported'")" = "1"
