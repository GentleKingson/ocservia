#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/p1-resilience-capacity-lib.sh
source "${ROOT}/scripts/p1-resilience-capacity-lib.sh"
RUN_ID="${RUN_ID:-p1-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-${GITHUB_JOB:-job}-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
P1_PROFILE="${P1_PROFILE:-full}"
AGENT_COUNT="${AGENT_COUNT:-500}"
HEARTBEAT_COUNT="${HEARTBEAT_COUNT:-2}"
HEARTBEAT_INTERVAL_MS="${HEARTBEAT_INTERVAL_MS:-30000}"
REQUEST_CONCURRENCY="${REQUEST_CONCURRENCY:-32}"
QUEUE_CAPACITY="${QUEUE_CAPACITY:-2048}"
PREFIX="$(printf '%s' "${RUN_ID}" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-ocservia-i08-${PREFIX}}"
TMP_BASE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
TMP_ROOT="${TMP_BASE%/}/ocservia-${PREFIX}"
OVERRIDE="${TMP_ROOT}/compose.override.yaml"
RESULTS="${TMP_ROOT}/operations.tsv"
RESOURCE_SAMPLES="${TMP_ROOT}/resource-samples.jsonl"
SAMPLE_PHASE_FILE="${TMP_ROOT}/sample-phase"
SAMPLE_PAUSE_FILE="${TMP_ROOT}/sample-paused"
SAMPLE_ACTIVE_FILE="${TMP_ROOT}/sample-active"
ARTIFACT_DIR="${ARTIFACT_DIR:-}"
AUTH_TOKEN="${OCSERV_DEV_AUTH_TOKEN:-local-development-token-32-characters}"
API_PORT=$((22000 + $(printf '%s' "${RUN_ID}" | cksum | awk '{print $1}') % 20000))
SAMPLE_PID=""
SAMPLER_EXIT=0
# These state variables are read and updated by the sourced sampler helpers.
# shellcheck disable=SC2034
SAMPLER_REAPED=0
# shellcheck disable=SC2034
SAMPLER_STOP_REQUESTED=0
TEST_EXIT=1
TRAP_EXIT=0
CLEANUP_EXIT=0
MINIMUM_RESOURCE_SAMPLES="${MINIMUM_RESOURCE_SAMPLES:-10}"
MINIMUM_SAMPLE_SPAN_SECONDS=0

if [[ ! "${P1_PROFILE}" =~ ^(smoke|full)$ ]]; then
  echo "P1_PROFILE must be smoke or full" >&2
  exit 2
fi
if [[ -z "${PREFIX}" ]]; then
  echo "RUN_ID must contain at least one ASCII letter or digit" >&2
  exit 2
fi
if [[ "$(cd "$(dirname "${TMP_ROOT}")" && pwd)/$(basename "${TMP_ROOT}")" == "${ROOT}" ]]; then
  echo "temporary run directory must not equal the source checkout" >&2
  exit 2
fi

validate_capacity_settings "${AGENT_COUNT}" "${HEARTBEAT_COUNT}" "${HEARTBEAT_INTERVAL_MS}" "${REQUEST_CONCURRENCY}" "${QUEUE_CAPACITY}"
if [[ ! "${MINIMUM_RESOURCE_SAMPLES}" =~ ^[1-9][0-9]*$ ]]; then
  echo "MINIMUM_RESOURCE_SAMPLES must be a positive integer" >&2
  exit 2
fi
MINIMUM_SAMPLE_SPAN_SECONDS=$((HEARTBEAT_COUNT * HEARTBEAT_INTERVAL_MS / 1000))

export OCSERV_HTTP_PORT="${API_PORT}"
export OCSERV_VERSION="i08-test"
OCSERV_COMMIT="$(git -C "${ROOT}" rev-parse --short HEAD 2>/dev/null || printf worktree)"
export OCSERV_COMMIT
COMPOSE=(docker compose -p "${COMPOSE_PROJECT}" -f "${ROOT}/deploy/compose/compose.yaml" -f "${OVERRIDE}")

cleanup() {
  TRAP_EXIT=$?
  TEST_EXIT=${TRAP_EXIT}
  trap - EXIT INT TERM
  set +e
  if [[ -n "${SAMPLE_PID}" ]] && ! stop_sampler; then
    TEST_EXIT=1
  fi
  if [[ -n "${ARTIFACT_DIR}" && "${ARTIFACT_DIR}" != "${TMP_ROOT}"* ]] && ! mkdir -p "${ARTIFACT_DIR}"; then
    CLEANUP_EXIT=1
  fi
  if [[ -n "${ARTIFACT_DIR}" && "${ARTIFACT_DIR}" != "${TMP_ROOT}"* ]]; then
    df -h >"${ARTIFACT_DIR}/disk-after.txt" 2>&1 || true
    "${COMPOSE[@]}" ps --all >"${ARTIFACT_DIR}/docker-ps.txt" 2>&1 || true
    "${COMPOSE[@]}" logs --no-color >"${ARTIFACT_DIR}/docker-compose.log" 2>&1 || true
  fi
  "${COMPOSE[@]}" down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || CLEANUP_EXIT=1
  local leftovers
  leftovers="$(docker ps -a --filter "label=com.docker.compose.project=${COMPOSE_PROJECT}" -q)"
  leftovers+="$(docker volume ls --filter "label=com.docker.compose.project=${COMPOSE_PROJECT}" -q)"
  leftovers+="$(docker network ls --filter "label=com.docker.compose.project=${COMPOSE_PROJECT}" -q)"
  if [[ -n "${leftovers}" ]]; then
    echo "scoped cleanup left Compose resources for ${COMPOSE_PROJECT}" >&2
    CLEANUP_EXIT=1
  else
    echo "scoped cleanup verified for ${COMPOSE_PROJECT}"
  fi
  printf 'test_exit=%s sampler_exit=%s trap_exit=%s cleanup_exit=%s\n' \
    "${TEST_EXIT}" "${SAMPLER_EXIT}" "${TRAP_EXIT}" "${CLEANUP_EXIT}" >"${TMP_ROOT}/exit-status.log"
  if [[ -n "${ARTIFACT_DIR}" && "${ARTIFACT_DIR}" != "${TMP_ROOT}"* ]]; then
    [[ -f "${RESULTS}" ]] && cp -f "${RESULTS}" "${ARTIFACT_DIR}/operations.tsv"
    [[ -f "${RESOURCE_SAMPLES}" ]] && cp -f "${RESOURCE_SAMPLES}" "${ARTIFACT_DIR}/p1-resource-samples.jsonl"
    [[ -f "${TMP_ROOT}/metrics.txt" ]] && cp -f "${TMP_ROOT}/metrics.txt" "${ARTIFACT_DIR}/p1-metrics.txt"
    [[ -f "${TMP_ROOT}/summary.json" ]] && cp -f "${TMP_ROOT}/summary.json" "${ARTIFACT_DIR}/p1-summary.json"
    [[ -f "${TMP_ROOT}/slow.sse" ]] && cp -f "${TMP_ROOT}/slow.sse" "${ARTIFACT_DIR}/slow.sse"
    [[ -f "${TMP_ROOT}/interrupted-operation-initial.json" ]] && cp -f "${TMP_ROOT}/interrupted-operation-initial.json" "${ARTIFACT_DIR}/interrupted-operation-initial.json"
    [[ -f "${TMP_ROOT}/interrupted-operation-final.json" ]] && cp -f "${TMP_ROOT}/interrupted-operation-final.json" "${ARTIFACT_DIR}/interrupted-operation-final.json"
    [[ -f "${TMP_ROOT}/interrupted-operation-summary.json" ]] && cp -f "${TMP_ROOT}/interrupted-operation-summary.json" "${ARTIFACT_DIR}/interrupted-operation-summary.json"
    [[ -f "${TMP_ROOT}/run-parameters.txt" ]] && cp -f "${TMP_ROOT}/run-parameters.txt" "${ARTIFACT_DIR}/run-parameters.txt"
    cp -f "${TMP_ROOT}/exit-status.log" "${ARTIFACT_DIR}/p1-exit-status.log" 2>/dev/null || CLEANUP_EXIT=1
    printf 'test_exit=%s sampler_exit=%s trap_exit=%s cleanup_exit=%s\n' \
      "${TEST_EXIT}" "${SAMPLER_EXIT}" "${TRAP_EXIT}" "${CLEANUP_EXIT}" >"${ARTIFACT_DIR}/p1-exit-status.log" 2>/dev/null || CLEANUP_EXIT=1
  fi
  rm -rf "${TMP_ROOT}"
  exit "$((TEST_EXIT != 0 || SAMPLER_EXIT != 0 || CLEANUP_EXIT != 0))"
}
trap cleanup EXIT INT TERM

mkdir -p "${TMP_ROOT}"
df -h >"${TMP_ROOT}/disk-before.txt"
printf 'profile=%s\nagent_count=%s\nheartbeat_count=%s\nheartbeat_interval_ms=%s\nrequest_concurrency=%s\nqueue_capacity=%s\nminimum_resource_samples=%s\n' \
  "${P1_PROFILE}" "${AGENT_COUNT}" "${HEARTBEAT_COUNT}" "${HEARTBEAT_INTERVAL_MS}" \
  "${REQUEST_CONCURRENCY}" "${QUEUE_CAPACITY}" "${MINIMUM_RESOURCE_SAMPLES}" \
  >"${TMP_ROOT}/run-parameters.txt"
if [[ -n "${ARTIFACT_DIR}" && "${ARTIFACT_DIR}" != "${TMP_ROOT}"* ]]; then
  mkdir -p "${ARTIFACT_DIR}"
  cp -f "${TMP_ROOT}/disk-before.txt" "${ARTIFACT_DIR}/disk-before.txt"
fi
printf '%s\n' \
  'services:' \
  '  transportd-stub:' \
  '    command:' \
  '      - --socket' \
  '      - /run/ocserv-platform/transportd.sock' \
  '      - --queue-capacity' \
  "      - \"${QUEUE_CAPACITY}\"" \
  '      - --control-plane-uid' \
  '      - "65534"' \
  '      - --control-plane-gid' \
  '      - "65532"' \
  '      - --capacity-telemetry' \
  '      - --stats-file' \
  '      - /run/ocserv-platform/stats.json' \
  '  control-plane:' \
  '    environment:' \
  "      OCSERV_TRANSPORT_QUEUE_CAPACITY: \"${QUEUE_CAPACITY}\"" >"${OVERRIDE}"

wait_ready() {
  for _ in $(seq 1 120); do
    if curl --fail --silent "http://127.0.0.1:${API_PORT}/readyz" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

psql_value() {
  "${COMPOSE[@]}" exec -T postgres psql -U ocservia_owner -d ocservia -Atc "$1"
}

wait_succeeded() {
  local expected=$1
  for _ in $(seq 1 240); do
    if [[ "$(psql_value "SELECT count(*) FROM operations WHERE state = 'succeeded'")" -ge "${expected}" ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

create_probe() {
  local heartbeat_count=$1 delay_millis=$2
  curl --fail --silent -H 'Content-Type: application/json' \
    -d "{\"heartbeat_count\":${heartbeat_count},\"delay_millis\":${delay_millis}}" \
    "http://127.0.0.1:${API_PORT}/api/v1/development/simulations"
}

read_stub_stats() {
  local value
  for _ in $(seq 1 20); do
    if value="$("${COMPOSE[@]}" exec -T transportd-stub cat /run/ocserv-platform/stats.json 2>/dev/null)" && \
      jq -e 'type == "object"' <<<"${value}" >/dev/null; then
      printf '%s\n' "${value}"
      return 0
    fi
    sleep 0.1
  done
  echo "transport stats snapshot is unavailable or invalid" >&2
  return 1
}

record_sample() {
  local phase=$1 database_available=${2:-true}
  local runtime stub control_rss control_fd stub_rss stub_fd postgres_io
  runtime="$(curl --fail --silent --max-time 5 "http://127.0.0.1:${API_PORT}/api/v1/development/runtime")"
  stub="$(read_stub_stats)"
  control_rss="$("${COMPOSE[@]}" exec -T control-plane sh -c "awk '/VmRSS/ {print \$2}' /proc/1/status")"
  control_fd="$("${COMPOSE[@]}" exec -T control-plane sh -c 'find /proc/1/fd -mindepth 1 -maxdepth 1 | wc -l')"
  stub_rss="$("${COMPOSE[@]}" exec -T transportd-stub sh -c "awk '/VmRSS/ {print \$2}' /proc/1/status")"
  stub_fd="$("${COMPOSE[@]}" exec -T transportd-stub sh -c 'find /proc/1/fd -mindepth 1 -maxdepth 1 | wc -l')"
  if [[ "${database_available}" == true ]]; then
    postgres_io="$(psql_value "SELECT json_build_object('active',count(*) FILTER (WHERE state='active'),'waiting',count(*) FILTER (WHERE wait_event IS NOT NULL),'available',true) FROM pg_stat_activity")"
  else
    postgres_io='{"active":-1,"waiting":-1,"available":false}'
  fi
  for value in "${control_rss}" "${control_fd}" "${stub_rss}" "${stub_fd}"; do
    [[ "${value}" =~ ^[0-9]+$ ]] || { echo "resource sample contains a non-numeric process value" >&2; return 1; }
  done
  jq -e '(.goroutines|type)=="number" and (.db_acquired|type)=="number" and (.db_idle|type)=="number" and (.db_total|type)=="number"' <<<"${runtime}" >/dev/null
  jq -e '(.active_tasks|type)=="number" and (.task_capacity|type)=="number"' <<<"${stub}" >/dev/null
  jq -e '(.active|type)=="number" and (.waiting|type)=="number" and (.available|type)=="boolean"' <<<"${postgres_io}" >/dev/null
  jq -cn --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg phase "${phase}" \
    --argjson epoch_seconds "$(date -u +%s)" --argjson runtime "${runtime}" \
    --argjson stub "${stub}" --argjson postgres "${postgres_io}" \
    --argjson control_rss_kib "${control_rss}" --argjson control_fd "${control_fd}" \
    --argjson stub_rss_kib "${stub_rss}" --argjson stub_fd "${stub_fd}" \
    '{at:$at,epoch_seconds:$epoch_seconds,phase:$phase,runtime:$runtime,stub:$stub,postgres:$postgres,control_rss_kib:$control_rss_kib,control_fd:$control_fd,stub_rss_kib:$stub_rss_kib,stub_fd:$stub_fd}' \
    >>"${RESOURCE_SAMPLES}"
}

set_sample_phase() {
  printf '%s\n' "$1" >"${SAMPLE_PHASE_FILE}.next"
  mv "${SAMPLE_PHASE_FILE}.next" "${SAMPLE_PHASE_FILE}"
}

sample_resources() {
  trap 'rm -f "${SAMPLE_ACTIVE_FILE}"; exit 0' TERM
  while true; do
    if [[ -e "${SAMPLE_PAUSE_FILE}" ]]; then
      sleep 0.1
      continue
    fi
    : >"${SAMPLE_ACTIVE_FILE}"
    if [[ -e "${SAMPLE_PAUSE_FILE}" ]]; then
      rm -f "${SAMPLE_ACTIVE_FILE}"
      sleep 0.1
      continue
    fi
    if ! record_sample "$(<"${SAMPLE_PHASE_FILE}")"; then
      rm -f "${SAMPLE_ACTIVE_FILE}"
      return 1
    fi
    rm -f "${SAMPLE_ACTIVE_FILE}"
    sleep 5
  done
}

pause_periodic_sampler() {
  set_sample_phase "$1"
  : >"${SAMPLE_PAUSE_FILE}"
  for _ in $(seq 1 100); do
    [[ ! -e "${SAMPLE_ACTIVE_FILE}" ]] && break
    check_sampler
    sleep 0.1
  done
  if [[ -e "${SAMPLE_ACTIVE_FILE}" ]]; then
    echo "resource sampler did not pause within 10 seconds" >&2
    return 1
  fi
  check_sampler
}

resume_periodic_sampler() {
  rm -f "${SAMPLE_PAUSE_FILE}"
  check_sampler
}

sample_phase_now() {
  local phase=$1 database_available=${2:-true}
  set_sample_phase "${phase}"
  check_sampler
  record_sample "${phase}" "${database_available}"
  check_sampler
}

echo "starting ${P1_PROFILE} profile: ${AGENT_COUNT} agents, ${HEARTBEAT_COUNT} heartbeats, ${HEARTBEAT_INTERVAL_MS} ms cadence, concurrency ${REQUEST_CONCURRENCY}, queue ${QUEUE_CAPACITY}"
RUN_STARTED_EPOCH="$(date -u +%s)"
"${COMPOSE[@]}" up --build -d postgres otel-collector transportd-stub migrate control-plane
wait_ready
set_sample_phase capacity-load
sample_resources &
SAMPLE_PID=$!
sample_phase_now capacity-load

export API_PORT HEARTBEAT_COUNT HEARTBEAT_INTERVAL_MS TMP_ROOT
# The child Bash expands the exported load settings.
# shellcheck disable=SC2016
seq 1 "${AGENT_COUNT}" | xargs -P "${REQUEST_CONCURRENCY}" -I '{}' bash -c '
  index=$1
  body="${TMP_ROOT}/operation-${index}.json"
  latency=$(curl --fail --silent --output "${body}" --write-out "%{time_total}" \
    -H "Content-Type: application/json" \
    -d "{\"heartbeat_count\":${HEARTBEAT_COUNT},\"delay_millis\":${HEARTBEAT_INTERVAL_MS}}" \
    "http://127.0.0.1:${API_PORT}/api/v1/development/simulations")
  jq -r --arg index "${index}" --arg latency "${latency}" "[\$index,.id,.node_id,\$latency]|@tsv" "${body}"
' _ '{}' >"${RESULTS}"
test "$(wc -l <"${RESULTS}" | tr -d ' ')" = "${AGENT_COUNT}"
wait_succeeded "${AGENT_COUNT}"
check_sampler

latency_percentiles="$(cut -f4 "${RESULTS}" | sort -n | awk '
  { values[NR]=$1 }
  END {
    p50=int((NR*50+99)/100); p95=int((NR*95+99)/100); p99=int((NR*99+99)/100);
    printf "p50=%s p95=%s p99=%s", values[p50], values[p95], values[p99]
  }')"
completion_percentiles="$(psql_value "SELECT format('p50=%s p95=%s p99=%s', round((percentile_cont(0.50) WITHIN GROUP (ORDER BY extract(epoch FROM updated_at-created_at)*1000))::numeric,2), round((percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM updated_at-created_at)*1000))::numeric,2), round((percentile_cont(0.99) WITHIN GROUP (ORDER BY extract(epoch FROM updated_at-created_at)*1000))::numeric,2)) FROM operations")"
telemetry_batches="$(psql_value 'SELECT count(*) FROM telemetry_ingest_batches')"
path_mix="$(psql_value "SELECT coalesce(path->>'mode','unknown'),count(*) FROM node_observed_snapshots GROUP BY 1 ORDER BY 1")"
test "${telemetry_batches}" -ge "$((AGENT_COUNT * HEARTBEAT_COUNT))"
grep -q direct <<<"${path_mix}"
grep -q relay <<<"${path_mix}"
echo "request latency seconds: ${latency_percentiles}"
echo "operation completion milliseconds: ${completion_percentiles}"
echo "telemetry batches: ${telemetry_batches}"
echo "path mix: ${path_mix//$'\n'/, }"
printf '%s\n' "request latency seconds: ${latency_percentiles}" \
  "operation completion milliseconds: ${completion_percentiles}" \
  "telemetry batches: ${telemetry_batches}" \
  "path mix: ${path_mix//$'\n'/, }" \
  "request drops: 0" \
  "request retries: 0" >"${TMP_ROOT}/metrics.txt"

# A throttled SSE reader must not prevent fresh probes from completing.
sample_phase_now slow-sse
last_event="$(curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" \
  "http://127.0.0.1:${API_PORT}/api/v1/events?page_size=200" | jq -r '.items[-1].id')"
timeout 40s curl --silent --no-buffer --limit-rate 16 \
  -H "Authorization: Bearer ${AUTH_TOKEN}" -H "Last-Event-ID: ${last_event}" \
  "http://127.0.0.1:${API_PORT}/api/v1/events/stream" >"${TMP_ROOT}/slow.sse" &
slow_pid=$!
before="$(psql_value "SELECT count(*) FROM operations WHERE state = 'succeeded'")"
for _ in $(seq 1 8); do create_probe 1 25 >/dev/null; done
wait_succeeded "$((before + 8))"
kill -TERM "${slow_pid}" 2>/dev/null || true
wait "${slow_pid}" 2>/dev/null || true
check_sampler
echo "slow SSE consumer recovery passed"

# Controller restart must reconnect to the retained stream and continue ingest.
pause_periodic_sampler controller-restart
"${COMPOSE[@]}" restart control-plane >/dev/null
wait_ready
resume_periodic_sampler
sample_phase_now controller-restart
before="$(psql_value "SELECT count(*) FROM operations WHERE state = 'succeeded'")"
create_probe 2 100 >/dev/null
wait_succeeded "$((before + 1))"
echo "controller restart recovery passed"

# A transport restart invalidates the old cursor, then new work must converge.
create_probe 2 5000 >"${TMP_ROOT}/interrupted-operation-initial.json"
interrupted_id="$(jq -r .id "${TMP_ROOT}/interrupted-operation-initial.json")"
for _ in $(seq 1 40); do
  interrupted_state="$(psql_value "SELECT state FROM operations WHERE id='${interrupted_id}'")"
  [[ "${interrupted_state}" == "running" ]] && break
  sleep 0.25
done
test "${interrupted_state}" = "running"
sample_phase_now transport-interrupt
pause_periodic_sampler transport-recovery
"${COMPOSE[@]}" kill -s SIGKILL transportd-stub >/dev/null
"${COMPOSE[@]}" up -d --no-deps transportd-stub >/dev/null
for _ in $(seq 1 60); do
  if "${COMPOSE[@]}" exec -T transportd-stub test -S /run/ocserv-platform/transportd.sock; then break; fi
  sleep 1
done
resume_periodic_sampler
sample_phase_now transport-recovery
before="$(psql_value "SELECT count(*) FROM operations WHERE state = 'succeeded'")"
create_probe 2 100 >/dev/null
wait_succeeded "$((before + 1))"
read_interrupted_state() {
  psql_value "SELECT state FROM operations WHERE id='$1'"
}
if ! wait_for_interrupted_operation "${interrupted_id}" 60 0.5 read_interrupted_state; then
  psql_value "SELECT json_build_object('id',operation.id,'state',operation.state,'updated_at',operation.updated_at,'dispatched_at',job.dispatched_at,'attempts',job.attempts,'last_error',job.last_error) FROM operations AS operation JOIN local_slice_jobs AS job ON job.operation_id=operation.id WHERE operation.id='${interrupted_id}'" >&2 || true
  "${COMPOSE[@]}" logs --no-color --tail 80 control-plane transportd-stub >&2 || true
  exit 1
fi
interrupted_state="$(read_interrupted_state "${interrupted_id}")"
interrupted_operation_is_final "${interrupted_state}"
psql_value "SELECT json_build_object('id',operation.id,'state',operation.state,'updated_at',operation.updated_at,'dispatched_at',job.dispatched_at,'attempts',job.attempts,'last_error',job.last_error) FROM operations AS operation JOIN local_slice_jobs AS job ON job.operation_id=operation.id WHERE operation.id='${interrupted_id}'" \
  >"${TMP_ROOT}/interrupted-operation-final.json"
write_interrupted_operation_evidence \
  "${TMP_ROOT}/interrupted-operation-initial.json" \
  "${TMP_ROOT}/interrupted-operation-final.json" \
  "${TMP_ROOT}/interrupted-operation-summary.json"
check_sampler
echo "transport restart recovery passed; interrupted outcome=${interrupted_state}"

# Database loss must make readiness fail closed and recover after the same DB returns.
pause_periodic_sampler postgres-paused
"${COMPOSE[@]}" pause postgres >/dev/null
for _ in $(seq 1 20); do
  status="$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:${API_PORT}/readyz" || true)"
  [[ "${status}" == "503" ]] && break
  sleep 1
done
test "${status}" = "503"
sample_phase_now postgres-paused false
pause_periodic_sampler postgres-recovered
"${COMPOSE[@]}" unpause postgres >/dev/null
wait_ready
resume_periodic_sampler
sample_phase_now postgres-recovered
before="$(psql_value "SELECT count(*) FROM operations WHERE state = 'succeeded'")"
create_probe 1 25 >/dev/null
wait_succeeded "$((before + 1))"
check_sampler
echo "database outage recovery passed"

stop_sampler
TOTAL_RUN_SECONDS=$(($(date -u +%s) - RUN_STARTED_EPOCH))
validate_resource_samples "${RESOURCE_SAMPLES}" "${TMP_ROOT}/summary.json" "${MINIMUM_RESOURCE_SAMPLES}" \
  "${MINIMUM_SAMPLE_SPAN_SECONDS}" "${SAMPLER_EXIT}" "${I08_REQUIRED_SAMPLE_PHASES}" "${TOTAL_RUN_SECONDS}"
jq --arg profile "${P1_PROFILE}" '. + {profile:$profile}' "${TMP_ROOT}/summary.json" \
  >"${TMP_ROOT}/summary.json.next"
mv "${TMP_ROOT}/summary.json.next" "${TMP_ROOT}/summary.json"
printf 'total run seconds: %s\n' "${TOTAL_RUN_SECONDS}" >>"${TMP_ROOT}/metrics.txt"
cat "${TMP_ROOT}/summary.json"
TEST_EXIT=0
echo "P1 resilience and initial capacity validation passed"
