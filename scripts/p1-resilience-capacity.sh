#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:-I08-$(date -u +%Y%m%dT%H%M%SZ)-worktree}"
AGENT_COUNT="${AGENT_COUNT:-500}"
HEARTBEAT_COUNT="${HEARTBEAT_COUNT:-2}"
HEARTBEAT_INTERVAL_MS="${HEARTBEAT_INTERVAL_MS:-30000}"
REQUEST_CONCURRENCY="${REQUEST_CONCURRENCY:-32}"
QUEUE_CAPACITY="${QUEUE_CAPACITY:-2048}"
PREFIX="$(printf '%s' "${RUN_ID}" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
COMPOSE_PROJECT="${COMPOSE_PROJECT:-ocservia-i08-${PREFIX}}"
TMP_ROOT="${TMPDIR:-/tmp}/ocservia-${RUN_ID}"
OVERRIDE="${TMP_ROOT}/compose.override.yaml"
RESULTS="${TMP_ROOT}/operations.tsv"
RESOURCE_SAMPLES="${TMP_ROOT}/resource-samples.jsonl"
ARTIFACT_DIR="${ARTIFACT_DIR:-}"
API_PORT=$((22000 + $(printf '%s' "${RUN_ID}" | cksum | awk '{print $1}') % 20000))
SAMPLE_PID=""

if [[ "$(cd "$(dirname "${TMP_ROOT}")" && pwd)/$(basename "${TMP_ROOT}")" == "${ROOT}" ]]; then
  echo "temporary run directory must not equal the source checkout" >&2
  exit 2
fi

for setting in "${AGENT_COUNT}" "${HEARTBEAT_COUNT}" "${HEARTBEAT_INTERVAL_MS}" "${REQUEST_CONCURRENCY}" "${QUEUE_CAPACITY}"; do
  if [[ ! "${setting}" =~ ^[1-9][0-9]*$ ]]; then
    echo "capacity settings must be positive integers" >&2
    exit 2
  fi
done
if ((AGENT_COUNT > 500 || HEARTBEAT_COUNT > 32 || HEARTBEAT_INTERVAL_MS > 30000 || QUEUE_CAPACITY > 4096)); then
  echo "capacity settings exceed the repeatable I08 envelope" >&2
  exit 2
fi

export OCSERV_HTTP_PORT="${API_PORT}"
export OCSERV_VERSION="i08-test"
OCSERV_COMMIT="$(git -C "${ROOT}" rev-parse --short HEAD 2>/dev/null || printf worktree)"
export OCSERV_COMMIT
COMPOSE=(docker compose -p "${COMPOSE_PROJECT}" -f "${ROOT}/deploy/compose/compose.yaml" -f "${OVERRIDE}")

cleanup() {
  local exit_code=$?
  trap - EXIT INT TERM
  if [[ -n "${SAMPLE_PID}" ]]; then
    kill -TERM "${SAMPLE_PID}" 2>/dev/null || true
    wait "${SAMPLE_PID}" 2>/dev/null || true
  fi
  if [[ -n "${ARTIFACT_DIR}" && "${ARTIFACT_DIR}" != "${TMP_ROOT}"* ]]; then
    mkdir -p "${ARTIFACT_DIR}"
    cp -f "${RESULTS}" "${RESOURCE_SAMPLES}" "${TMP_ROOT}/metrics.txt" "${TMP_ROOT}/summary.json" "${TMP_ROOT}/slow.sse" "${TMP_ROOT}/interrupted-operation.json" "${ARTIFACT_DIR}/" 2>/dev/null || true
    "${COMPOSE[@]}" logs --no-color >"${ARTIFACT_DIR}/compose.log" 2>&1 || true
  fi
  "${COMPOSE[@]}" down --volumes --remove-orphans --rmi local >/dev/null 2>&1 || true
  local leftovers
  leftovers="$(docker ps -a --filter "label=com.docker.compose.project=${COMPOSE_PROJECT}" -q)"
  leftovers+="$(docker volume ls --filter "label=com.docker.compose.project=${COMPOSE_PROJECT}" -q)"
  leftovers+="$(docker network ls --filter "label=com.docker.compose.project=${COMPOSE_PROJECT}" -q)"
  if [[ -n "${leftovers}" ]]; then
    echo "scoped cleanup left Compose resources for ${COMPOSE_PROJECT}" >&2
    exit_code=1
  else
    echo "scoped cleanup verified for ${COMPOSE_PROJECT}"
  fi
  rm -rf "${TMP_ROOT}"
  exit "${exit_code}"
}
trap cleanup EXIT INT TERM

mkdir -p "${TMP_ROOT}"
printf '%s\n' \
  'services:' \
  '  transportd-stub:' \
  '    command:' \
  '      - --socket' \
  '      - /run/ocserv-platform/transportd.sock' \
  '      - --queue-capacity' \
  "      - \"${QUEUE_CAPACITY}\"" \
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

sample_resources() {
  while true; do
    local runtime stub control_rss control_fd stub_rss stub_fd postgres_io
    runtime="$(curl --fail --silent "http://127.0.0.1:${API_PORT}/api/v1/development/runtime" 2>/dev/null || printf '{}')"
    stub="$("${COMPOSE[@]}" exec -T transportd-stub sh -c 'cat /run/ocserv-platform/stats.json 2>/dev/null || printf "{}"' 2>/dev/null || printf '{}')"
    control_rss="$("${COMPOSE[@]}" exec -T control-plane sh -c "awk '/VmRSS/ {print \$2}' /proc/1/status" 2>/dev/null || printf '0')"
    control_fd="$("${COMPOSE[@]}" exec -T control-plane sh -c 'find /proc/1/fd -mindepth 1 -maxdepth 1 | wc -l' 2>/dev/null || printf '0')"
    stub_rss="$("${COMPOSE[@]}" exec -T transportd-stub sh -c "awk '/VmRSS/ {print \$2}' /proc/1/status" 2>/dev/null || printf '0')"
    stub_fd="$("${COMPOSE[@]}" exec -T transportd-stub sh -c 'find /proc/1/fd -mindepth 1 -maxdepth 1 | wc -l' 2>/dev/null || printf '0')"
    postgres_io="$(psql_value "SELECT json_build_object('active',count(*) FILTER (WHERE state='active'),'waiting',count(*) FILTER (WHERE wait_event IS NOT NULL)) FROM pg_stat_activity" 2>/dev/null || printf '{}')"
    jq -cn --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson runtime "${runtime}" \
      --argjson stub "${stub}" --argjson postgres "${postgres_io}" \
      --argjson control_rss_kib "${control_rss:-0}" --argjson control_fd "${control_fd:-0}" \
      --argjson stub_rss_kib "${stub_rss:-0}" --argjson stub_fd "${stub_fd:-0}" \
      '{at:$at,runtime:$runtime,stub:$stub,postgres:$postgres,control_rss_kib:$control_rss_kib,control_fd:$control_fd,stub_rss_kib:$stub_rss_kib,stub_fd:$stub_fd}' \
      >>"${RESOURCE_SAMPLES}"
    sleep 5
  done
}

echo "starting ${AGENT_COUNT}-agent I08 load with ${HEARTBEAT_INTERVAL_MS} ms heartbeat cadence"
"${COMPOSE[@]}" up --build -d postgres otel-collector transportd-stub migrate control-plane
wait_ready
sample_resources &
SAMPLE_PID=$!

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
  "path mix: ${path_mix//$'\n'/, }" >"${TMP_ROOT}/metrics.txt"

# A throttled SSE reader must not prevent fresh probes from completing.
last_event="$(curl --fail --silent "http://127.0.0.1:${API_PORT}/api/v1/events?page_size=200" | jq -r '.items[-1].id')"
timeout 40s curl --silent --no-buffer --limit-rate 16 -H "Last-Event-ID: ${last_event}" \
  "http://127.0.0.1:${API_PORT}/api/v1/events/stream" >"${TMP_ROOT}/slow.sse" &
slow_pid=$!
before="$(psql_value "SELECT count(*) FROM operations WHERE state = 'succeeded'")"
for _ in $(seq 1 8); do create_probe 1 25 >/dev/null; done
wait_succeeded "$((before + 8))"
kill -TERM "${slow_pid}" 2>/dev/null || true
wait "${slow_pid}" 2>/dev/null || true
echo "slow SSE consumer recovery passed"

# Controller restart must reconnect to the retained stream and continue ingest.
"${COMPOSE[@]}" restart control-plane >/dev/null
wait_ready
before="$(psql_value "SELECT count(*) FROM operations WHERE state = 'succeeded'")"
create_probe 2 100 >/dev/null
wait_succeeded "$((before + 1))"
echo "controller restart recovery passed"

# A transport restart invalidates the old cursor, then new work must converge.
create_probe 2 5000 >"${TMP_ROOT}/interrupted-operation.json"
interrupted_id="$(jq -r .id "${TMP_ROOT}/interrupted-operation.json")"
for _ in $(seq 1 40); do
  interrupted_state="$(psql_value "SELECT state FROM operations WHERE id='${interrupted_id}'")"
  [[ "${interrupted_state}" == "running" ]] && break
  sleep 0.25
done
test "${interrupted_state}" = "running"
"${COMPOSE[@]}" kill -s SIGKILL transportd-stub >/dev/null
"${COMPOSE[@]}" up -d transportd-stub >/dev/null
for _ in $(seq 1 60); do
  if "${COMPOSE[@]}" exec -T transportd-stub test -S /run/ocserv-platform/transportd.sock; then break; fi
  sleep 1
done
before="$(psql_value "SELECT count(*) FROM operations WHERE state = 'succeeded'")"
create_probe 2 100 >/dev/null
wait_succeeded "$((before + 1))"
interrupted_state="$(psql_value "SELECT state FROM operations WHERE id='${interrupted_id}'")"
case "${interrupted_state}" in unknown | queued | dispatched) ;; *) echo "unexpected interrupted state: ${interrupted_state}" >&2; exit 1 ;; esac
echo "transport restart recovery passed; interrupted outcome=${interrupted_state}"

# Database loss must make readiness fail closed and recover after the same DB returns.
"${COMPOSE[@]}" pause postgres >/dev/null
for _ in $(seq 1 20); do
  status="$(curl --silent --output /dev/null --write-out '%{http_code}' "http://127.0.0.1:${API_PORT}/readyz" || true)"
  [[ "${status}" == "503" ]] && break
  sleep 1
done
test "${status}" = "503"
"${COMPOSE[@]}" unpause postgres >/dev/null
wait_ready
before="$(psql_value "SELECT count(*) FROM operations WHERE state = 'succeeded'")"
create_probe 1 25 >/dev/null
wait_succeeded "$((before + 1))"
echo "database outage recovery passed"

kill -TERM "${SAMPLE_PID}" 2>/dev/null || true
wait "${SAMPLE_PID}" 2>/dev/null || true
SAMPLE_PID=""
jq -s '{samples:length,max_goroutines:(map(.runtime.goroutines // 0)|max),max_tokio_tasks:(map(.stub.active_tasks // 0)|max),max_control_rss_kib:(map(.control_rss_kib)|max),max_stub_rss_kib:(map(.stub_rss_kib)|max),max_control_fd:(map(.control_fd)|max),max_stub_fd:(map(.stub_fd)|max),max_db_acquired:(map(.runtime.db_acquired // 0)|max)}' "${RESOURCE_SAMPLES}" | tee "${TMP_ROOT}/summary.json"
echo "P1 resilience and initial capacity validation passed"
