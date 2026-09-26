#!/usr/bin/env bash
set -euo pipefail
trap 'echo "database integration failed at line ${LINENO}" >&2' ERR

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"
scope="${DATABASE_TEST_SCOPE-full}"
case "${scope}" in
  smoke|compatibility) exec bash "${ROOT}/scripts/database-postgres-smoke.sh" ;;
  full|regression) ;;
  *) echo 'DATABASE_TEST_SCOPE must be smoke, compatibility, full or regression' >&2; exit 2 ;;
esac
# shellcheck source=scripts/go-test-environment.sh
source "${ROOT}/scripts/go-test-environment.sh"
require_test_commands go jq setsid ruby python3 curl sha256sum
require_test_docker
require_go_race
(cd "${ROOT}" && sha256sum -c docs/database-migrations.sha256)

UPSTREAM_MANIFEST="${ROOT}/docs/upstream/v4.9-post1.manifest.json"
EXPECTED_UPSTREAM_RECORD="$(jq -r '[(.repository | sub("^https://github.com/"; "")), .old.ref, .old.commit, .new.ref, .new.commit, .imported_at] | join("|")' "${UPSTREAM_MANIFEST}")"
EXPECTED_UPSTREAM_ROLLBACK='publication: revert PR15 independently; implementation: stop I14 scheduler/API, reconcile commands, revert PR14, then apply migration 000013 down only when policy and batch data need not be retained'

RUN_ID="${RUN_ID:-database-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-${GITHUB_JOB:-job}-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
PREFIX="$(printf '%s' "${RUN_ID}" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
TMP_BASE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
TMP_ROOT="$(mktemp -d "${TMP_BASE%/}/ocservia-${PREFIX}-XXXXXX")"
BIN="${OCSERVIA_CONTROL_BIN:-${TMP_ROOT}/ocserv-control}"
ARTIFACT_DIR="${ARTIFACT_DIR:-}"
export OCSERV_ENVIRONMENT=test
export OCSERV_AUDIT_EVENT_KEY_ID=test-audit-event-v1
export OCSERV_TEST_AUDIT_EVENT_KEY_HEX=1111111111111111111111111111111111111111111111111111111111111111
export OCSERV_AUDIT_CHECKPOINT_KEY=2222222222222222222222222222222222222222222222222222222222222222
PG_MAJOR="${PG_MAJOR:-all}"
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
    docker rm -fv "${container}" >/dev/null 2>&1 || cleanup_exit=1
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
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "${TMP_ROOT}"
export DATABASE_CASE_RESULTS="${TMP_ROOT}/required-case-results.jsonl"
STARTUP_BIN=""
if [[ -n "${OCSERVIA_CONTROL_BIN:-}" ]]; then
  [[ -x "${BIN}" ]] || {
    echo "OCSERVIA_CONTROL_BIN must name an executable file" >&2
    exit 2
  }
else
  (cd "${ROOT}/control-plane" && go build -trimpath -buildvcs=false -o "${BIN}" ./cmd/ocserv-control)
  STARTUP_BIN="${BIN}"
fi

TEST_CONTROL_PLANE="${ROOT}/control-plane"

case "${PG_MAJOR}" in
  all) POSTGRES_MAJORS=(17 18) ;;
  17 | 18) POSTGRES_MAJORS=("${PG_MAJOR}") ;;
  *)
    echo "PG_MAJOR must be all, 17, or 18" >&2
    exit 2
    ;;
esac

assert_local_bootstrap_schema() {
  local container=$1 database=$2
  test "$(docker exec "${container}" psql -U ocservia_owner -d "${database}" -Atc "SELECT count(*) FROM schema_migrations WHERE version = 32")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d "${database}" -Atc "SELECT has_table_privilege('ocservia_app','local_auth_attempts','SELECT,INSERT,UPDATE,DELETE') AND NOT has_table_privilege('ocservia_app','local_auth_attempts','TRUNCATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d "${database}" -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='local_auth_bootstrap'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d "${database}" -Atc "SELECT has_table_privilege('ocservia_app','local_auth_bootstrap','SELECT') AND has_table_privilege('ocservia_app','local_auth_bootstrap','INSERT') AND NOT has_table_privilege('ocservia_app','local_auth_bootstrap','UPDATE,DELETE,TRUNCATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d "${database}" -Atc "SELECT count(*) FROM schema_migrations WHERE version=34")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d "${database}" -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='local_auth_bootstrap' AND ((column_name='completion_pending' AND data_type='boolean' AND is_nullable='NO' AND column_default='false') OR (column_name='completed_at' AND data_type='timestamp with time zone' AND is_nullable='YES') OR (column_name='approver_identity_id' AND data_type='uuid' AND is_nullable='YES'))")" = "3"
  test "$(docker exec "${container}" psql -U ocservia_owner -d "${database}" -Atc "SELECT count(*) FROM pg_constraint WHERE conrelid='local_auth_bootstrap'::regclass AND convalidated AND ((conname='local_initialization_state' AND contype='c') OR (conname='local_auth_bootstrap_approver_identity_id_fkey' AND contype='f' AND confrelid='identities'::regclass AND confdeltype='r'))")" = "2"
  test "$(docker exec "${container}" psql -U ocservia_owner -d "${database}" -Atc "SELECT bool_and(has_column_privilege('ocservia_app','local_auth_bootstrap',column_name,'UPDATE') = (column_name IN ('completion_pending','completed_at','approver_identity_id'))) FROM information_schema.columns WHERE table_schema='public' AND table_name='local_auth_bootstrap'")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d "${database}" -Atc "SELECT NOT rolsuper AND NOT rolcreaterole AND NOT rolcreatedb AND NOT rolbypassrls AND NOT pg_has_role('ocservia_app','ocservia_owner','MEMBER') FROM pg_roles WHERE rolname='ocservia_app'")" = "t"
}

wait_for_postgres() {
  local container=$1
  # TCP probe: the entrypoint's initdb temporary server listens on the Unix
  # socket only, so a socket probe can report ready before the final server.
  for _ in $(seq 1 60); do
    if docker exec -e PGPASSWORD=test-owner-only "${container}" psql -h 127.0.0.1 -U ocservia_owner -d ocservia -Atc "SELECT 1" >/dev/null 2>&1; then return 0; fi
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

clone_database() {
  local container=$1 source=$2 destination=$3
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d postgres -c \
    "CREATE DATABASE ${destination} TEMPLATE ${source}" >/dev/null
}

checked_go_tests() {
  local group=$1
  shift
  (
    if [[ "${group}" == backend-controller-startup ]]; then
      # Ignore inherited test overrides; only reuse the current binary built above.
      export OCSERVIA_TEST_STARTUP_BIN="${STARTUP_BIN}"
    fi
    cd "${ROOT}/control-plane"
    exec bash "${ROOT}/scripts/required-go-tests.sh" "${group}" "$@" -timeout=3m
  ) &
  local test_pid=$! index=${#PIDS[@]}
  PIDS+=("${test_pid}")
  wait "${test_pid}"
  PIDS[index]=""
}

assert_auth_fixture_cleanup() {
  local container=$1 database=$2
  test "$(docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d "${database}" -Atc "
    SELECT (SELECT count(*) FROM local_auth_bootstrap)
      + (SELECT count(*) FROM pg_constraint WHERE conname IN ('r4_fail_approver','r4_fail_password','p4_reject_bootstrap','p4_reject_create','p4_reject_reset','pr03_audit_failure') OR conname LIKE 'r6_%')
      + (SELECT count(*) FROM role_bindings b JOIN workspaces w ON w.id=b.workspace_id WHERE w.slug LIKE 'r4-%' OR w.slug LIKE 'bootstrap-%' OR w.slug LIKE 'pr03-%')
  ")" = 0
}

for major in "${POSTGRES_MAJORS[@]}"; do
  : >"${DATABASE_CASE_RESULTS}"
  container="${PREFIX}-pg${major}"
  CONTAINERS+=("${container}")
  # Keep the selected loopback port stable across the lifecycle restart test.
  port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  case "${major}" in
    17) postgres_image='postgres:17.10-bookworm@sha256:9b18b78397054fce88a9552e9d5a3ad5bb7fd258c5b3cc1c5028e46373d6ea8f' ;;
    18) postgres_image='postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af' ;;
  esac
  docker run -d --name "${container}" \
    -e POSTGRES_DB=ocservia -e POSTGRES_USER=ocservia_owner -e POSTGRES_PASSWORD=test-owner-only \
    -p "127.0.0.1:${port}:5432" "${postgres_image}" >/dev/null
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
  assert_local_bootstrap_schema "${container}" ocservia
  latest_owner_url="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/ocservia_latest?sslmode=disable"
  latest_runtime_url="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/ocservia_latest?sslmode=disable"
  # CLI bootstrap and role lifecycle fixtures must not alter the shared
  # database used by the authentication and migration acceptance below.
  clone_database "${container}" ocservia ocservia_startup
  OCSERV_TEST_DATABASE_URL="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/ocservia_startup?sslmode=disable" \
    OCSERV_TEST_OWNER_DATABASE_URL="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/ocservia_startup?sslmode=disable" \
    checked_go_tests backend-controller-startup --select -race -p 1 -parallel 1
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d postgres -c 'DROP DATABASE ocservia_startup' >/dev/null

  if [[ "${scope}" == regression ]]; then
    OCSERV_DATABASE_URL="${owner_url}" OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app \
      "${BIN}" --migrate-only >"${TMP_ROOT}/pg${major}-migrate-repeat.log" 2>&1
    assert_local_bootstrap_schema "${container}" ocservia
    for group in regression-postgres regression-outbox regression-fencing regression-auth regression-auth-postgres regression-oidc regression-telemetry backend-policy-config backend-policy-certificates backend-policy-userstate backend-policy-useroperations; do
      OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
        checked_go_tests "${group}" --select -race -p 1 -parallel 1
    done
    # Deliberate corruption is last, after all current-schema business checks.
    docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
      "UPDATE schema_migrations SET checksum=decode(repeat('00',32),'hex') WHERE version=1" >/dev/null
    if OCSERV_DATABASE_URL="${owner_url}" OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app \
      "${BIN}" --migrate-only >"${TMP_ROOT}/pg${major}-checksum-rejected.log" 2>&1; then
      echo 'migration accepted a checksum mismatch' >&2; exit 1
    fi
    grep -Fq 'migration 1 checksum does not match the applied schema' "${TMP_ROOT}/pg${major}-checksum-rejected.log"
    required_cases="$(jq -s 'map(.required) | add // 0' "${DATABASE_CASE_RESULTS}")"
    echo "PostgreSQL ${major} regression: current migration, repeat migration, permissions and checksum guards passed"
    echo "Database acceptance required cases: backend=postgres${major} shard=regression passed=${required_cases} skipped=0"
  else
  clone_database "${container}" ocservia ocservia_latest
  (cd "${ROOT}/control-plane" && OCSERV_TEST_DATABASE_URL="${latest_runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-audit-postgres -p 1 ./internal/database/...)
  (cd "${ROOT}/control-plane" && OCSERV_TEST_DATABASE_URL="${latest_runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-coordination -race -p 1 ./internal/operations -run '^Test(OutboxBackend|FencingBackend|CoordinationDeadlockBackend)Integration$')

  # Scheduler leadership tests need an idle lease, so they run before any
  # long-lived control-plane process acquires leadership on this database.
  # -race is required here: these are the only tests that exercise the
  # coordination package against a database, so this is where the race
  # detector actually observes renewal, loss, and session snapshotting.
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" \
    go test -p 1 -race ./internal/coordination ./internal/connectionowner ./internal/ownersession -run Integration -count=1)
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${owner_url}" \
    go test -p 1 ./migrations -run '^TestMigrationFailureIsAtomicIntegration$' -count=1)
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    go test -p 1 ./internal/api -run '^TestReadinessRequiresDatabaseConnectivityIntegration$' -count=1)

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
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 14")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 15")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 16")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 17")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 18")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 19")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 20")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 21")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 22")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 23")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 24")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 25")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 26")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 29")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 30")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations WHERE version = 31")" = "1"
  assert_local_bootstrap_schema "${container}" ocservia
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='node_bootstrap_tokens'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','node_bootstrap_tokens','SELECT,INSERT,UPDATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='scheduler_leadership'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='connection_owner_fencing'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('privd_attestation_enrollment_credentials','node_privd_attestation_keys')")" = "2"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND ((table_name='agent_command_results' AND column_name IN ('receipt_verification_status','receipt_failure_reason','privd_attestation_key_id','effect_record_id','effect_sequence','receipt_sha256','privileged_result_proof')) OR (table_name='certificates' AND column_name IN ('csr_receipt_verified_at','csr_receipt_sha256','csr_privd_attestation_key_id','csr_effect_record_id','csr_der_sha256','csr_requested_subject_sha256','issue_certificate_version')))")" = "14"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('transport_event_cursor','transport_event_quarantine')")" = "2"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='transport_event_quarantine'")" = "7"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='transport_event_quarantine' AND column_name='payload'")" = "0"
	test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='audit_events' AND column_name IN ('auth_version','event_key_id','event_mac')")" = "3"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='node_sealing_keys'")" = "1"
	test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='artifact_operations' AND column_name IN ('certificate_version','active_grant_id','active_grant_subject','active_grant_expires_at','consume_grant','consume_sha256','consume_size','consume_actor_id','consume_session_id','consume_request_id')")" = "10"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('approval_authority_resources','approval_batch_items')")" = "2"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND ((table_name='approval_requests' AND column_name='authority_snapshot_at') OR (table_name='role_bindings' AND column_name='approval_id') OR (table_name='artifact_operations' AND column_name='approval_id') OR (table_name='certificates' AND column_name='version'))")" = "4"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='node_trust_convergence'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','node_trust_convergence','SELECT,INSERT,UPDATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='node_config_state' AND column_name='desired_revision'")" = "1"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('node_config_state','config_plans','config_apply_operations')")" = "3"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('certificates','artifact_operations','secret_provider_refs')")" = "3"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='security_alerts' AND column_name IN ('node_id','resource_type','resource_id')")" = "3"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT pg_get_constraintdef(oid) LIKE '%config_plan%' FROM pg_constraint WHERE conrelid='commands'::regclass AND conname='commands_payload_type_check'")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT pg_get_constraintdef(oid) LIKE '%certificate_csr%' AND pg_get_constraintdef(oid) LIKE '%certificate_p12%' AND pg_get_constraintdef(oid) LIKE '%certificate_revoke%' FROM pg_constraint WHERE conrelid='commands'::regclass AND conname='commands_payload_type_check'")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT pg_get_constraintdef(oid) LIKE '%object%' FROM pg_constraint WHERE conrelid='approval_requests'::regclass AND conname='approval_requests_request_summary_check'")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','node_config_state','SELECT,INSERT,UPDATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','config_plans','SELECT,INSERT')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','config_apply_operations','SELECT,INSERT,UPDATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','certificates','SELECT,INSERT,UPDATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','artifact_operations','SELECT,INSERT,UPDATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','secret_provider_refs','SELECT,INSERT,UPDATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND tablename='config_apply_operations' AND indexname='config_apply_operations_one_active_node_idx' AND indexdef LIKE '%UNIQUE%unknown%'")" = "1"
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
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','transport_event_cursor','SELECT,INSERT,UPDATE')")" = "t"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT has_table_privilege('ocservia_app','transport_event_quarantine','SELECT,INSERT') AND NOT has_table_privilege('ocservia_app','transport_event_quarantine','UPDATE') AND NOT has_table_privilege('ocservia_app','transport_event_quarantine','DELETE')")" = "t"
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_app -d ocservia -c "
    INSERT INTO workspaces (id, name, slug, created_at, updated_at) VALUES ('00000000-0000-7000-8000-000000000001', 'One', 'one', now(), now()), ('00000000-0000-7000-8000-000000000002', 'Two', 'two', now(), now());
    INSERT INTO nodes (id, workspace_id, name, status, created_at, updated_at) VALUES ('00000000-0000-7000-8000-000000000003', '00000000-0000-7000-8000-000000000001', 'node', 'active', now(), now());
    INSERT INTO node_bootstrap_tokens (id, workspace_id, token_hash, expected_environment, expires_at, created_by, created_at)
    VALUES ('00000000-0000-7000-8000-000000000030', '00000000-0000-7000-8000-000000000001', decode(repeat('30', 32), 'hex'), 'production', now() + interval '1 hour', 'runtime-privilege-test', now());
    UPDATE node_bootstrap_tokens
    SET bound_endpoint_id = decode(repeat('31', 32), 'hex'), consumed_node_id = '00000000-0000-7000-8000-000000000003', consumed_at = now()
    WHERE id = '00000000-0000-7000-8000-000000000030';
    SELECT id FROM node_bootstrap_tokens WHERE id = '00000000-0000-7000-8000-000000000030';
  " >/dev/null
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT authorization_revision > 0 FROM nodes WHERE id='00000000-0000-7000-8000-000000000003'")" = "t"
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
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-enrollment -p 1 ./internal/operations ./internal/enrollment ./internal/localslice -run Integration)
  (cd "${ROOT}/control-plane" && OCSERV_TEST_DATABASE_URL="${latest_runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url}" \
    go test -p 1 ./internal/telemetry -run Integration -count=1)
  OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/test-enrollment-restart.sh" "${container}"
  # User workflows retain revision-zero commands and singleton lease state.
  # Keep their fixtures out of other packages.
  for package in userstate useroperations; do
    fixture_database="ocservia_${package}_${major}"
    clone_database "${container}" ocservia "${fixture_database}"
    group="backend-policy-${package}"
    (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/${fixture_database}?sslmode=disable" \
      OCSERV_TEST_OWNER_DATABASE_URL="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/${fixture_database}?sslmode=disable" \
      bash "${ROOT}/scripts/required-go-tests.sh" "${group}" -p 1 "./internal/${package}" -run Integration)
    docker exec "${container}" dropdb -U ocservia_owner "${fixture_database}"
  done
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-policy-config -p 1 ./internal/configplan -run Integration)
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-policy-certificates -p 1 ./internal/certificates -run Integration)
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    go test -p 1 ./internal/approvals ./internal/audit ./internal/privdattestation -run Integration -count=1)
  # Local/RBAC fixtures retain the current production runtime privileges.
  # Clone before any auth fixture writes. Lifecycle bootstrap requires no prior
  # singleton/admin grants; its audit constraints must never affect other suites.
  clone_database "${container}" ocservia_latest ocservia_lifecycle
  clone_database "${container}" ocservia_latest ocservia_auth_backend
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url/ocservia_latest/ocservia_auth_backend}" \
    OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url/ocservia_latest/ocservia_auth_backend}" \
    checked_go_tests backend-auth -p 1 -parallel 1 ./internal/api -run '^TestAuthenticationBackend(HTTP|Safety|Legacy)Integration$'
  assert_auth_fixture_cleanup "${container}" ocservia_auth_backend
  docker exec "${container}" dropdb -U ocservia_owner ocservia_auth_backend
  clone_database "${container}" ocservia_latest ocservia_policy_api
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url/ocservia_latest/ocservia_policy_api}" \
    OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url/ocservia_latest/ocservia_policy_api}" \
    checked_go_tests backend-policy-api --select -race -p 1 -parallel 1
  docker exec "${container}" dropdb -U ocservia_owner ocservia_policy_api
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url}" \
    checked_go_tests database-auth -p 1 -parallel 1 ./internal/rbac ./internal/auth -run Integration
  assert_auth_fixture_cleanup "${container}" ocservia_latest
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url}" \
    checked_go_tests database-api -p 1 -parallel 1 ./internal/api \
      -run '^(TestLocalAccountHTTPSharedLimitsIntegration|TestAuthHTTPLoginLogoutIntegration|TestAuthLogSessionFailureIntegration|TestAuthenticationBackendHTTPIntegration)$'
  assert_auth_fixture_cleanup "${container}" ocservia_latest
  OCSERV_TEST_DATABASE_URL="${latest_runtime_url/ocservia_latest/ocservia_lifecycle}" \
    OCSERV_TEST_OWNER_DATABASE_URL="${latest_owner_url/ocservia_latest/ocservia_lifecycle}" \
    checked_go_tests database-lifecycle -p 1 -parallel 1 ./internal/api -run '^TestLocalUserLifecycleIntegration$'
  assert_auth_fixture_cleanup "${container}" ocservia_lifecycle
  docker exec "${container}" dropdb -U ocservia_owner ocservia_lifecycle
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" OCSERV_TEST_OWNER_DATABASE_URL="${owner_url}" \
    bash "${ROOT}/scripts/required-go-tests.sh" backend-policy-upgrades --select -p 1)
  (cd "${TEST_CONTROL_PLANE}" && OCSERV_TEST_DATABASE_URL="${runtime_url}" \
    go test -p 1 ./internal/api -run '^TestApprovalDetailRequiresEveryAuthorityScopeIntegration$|^TestBrowserTrustBoundaryBlocksCrossSiteCookieMutations$' -count=1)
  OCSERV_DATABASE_URL="${runtime_url}" "${BIN}" --role=scheduler \
    >"${TMP_ROOT}/pg${major}-audit-checkpoint.log" 2>&1 &
  checkpoint_pid=$!
  PIDS+=("${checkpoint_pid}")
  checkpoint_ready=false
  for _ in $(seq 1 60); do
    if ! kill -0 "${checkpoint_pid}" 2>/dev/null; then
      cat "${TMP_ROOT}/pg${major}-audit-checkpoint.log" >&2
      echo "audit checkpoint scheduler exited before anchoring the audit tail" >&2
      exit 1
    fi
    missing_checkpoints="$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "
      WITH tails AS (
        SELECT DISTINCT ON (workspace_id) workspace_id,id,event_hash
        FROM audit_events ORDER BY workspace_id,occurred_at DESC,id DESC
      )
      SELECT count(*) FROM tails t
      WHERE NOT EXISTS (
        SELECT 1 FROM audit_checkpoints c
        WHERE c.workspace_id=t.workspace_id AND c.through_event_id=t.id AND c.through_event_hash=t.event_hash
      )")"
    if [[ "${missing_checkpoints}" == "0" ]]; then
      checkpoint_ready=true
      break
    fi
    sleep 1
  done
  [[ "${checkpoint_ready}" == true ]] || { echo "audit tail was not checkpointed" >&2; exit 1; }
  stop_process "${checkpoint_pid}"
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
  OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${owner_url}" \
    OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app "${BIN}" --migrate-only \
    >"${TMP_ROOT}/pg${major}-repeat-migrate.log" 2>&1

  # Isolate HTTP readiness from the scheduler's intentional fatal-error exit.
  # The all-role process and scheduler are exercised separately above.
  OCSERV_ENVIRONMENT=test OCSERV_HTTP_ADDRESS="127.0.0.1:${api_port}" \
    OCSERV_DATABASE_URL="${runtime_url}" "${BIN}" --role=api \
    >"${TMP_ROOT}/pg${major}-repeat.log" 2>&1 &
  pid=$!
  PIDS+=("${pid}")
  wait_for_http "http://127.0.0.1:${api_port}/readyz"
  test "$(docker exec "${container}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM schema_migrations")" = "36"
  assert_local_bootstrap_schema "${container}" ocservia
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "INSERT INTO schema_migrations (version, name, checksum) VALUES (37, '000037_future.up.sql', decode(repeat('00', 32), 'hex'))" >/dev/null
  test "$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:${api_port}/readyz")" = "200"
  docker exec "${container}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
    "DELETE FROM schema_migrations WHERE version = 37" >/dev/null
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
  required_cases="$(jq -s 'map(.required) | add // 0' "${DATABASE_CASE_RESULTS}")"
  echo "PostgreSQL ${major} database integration complete"
  echo "Database acceptance required cases: backend=postgres${major} shard=full passed=${required_cases} skipped=0"
  fi
done
