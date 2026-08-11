#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/env.sh
source "${ROOT}/scripts/env.sh"

RUN_ID="${RUN_ID:-local-slice-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}-${GITHUB_JOB:-job}-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
PREFIX="$(printf '%s' "${RUN_ID}" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
TMP_BASE="${RUNNER_TEMP:-${ROOT}/tmp}"
mkdir -p "${TMP_BASE}"
chmod 0700 "${TMP_BASE}"
# Keep Unix socket paths below Linux sockaddr_un.sun_path while mktemp supplies
# per-run uniqueness inside the runner-owned temporary directory.
TMP_ROOT="$(mktemp -d "${TMP_BASE%/}/ocsv-ls-XXXXXX")"
SOCKET="${TMP_ROOT}/run/transportd.sock"
TRUST_SOCKET="${TMP_ROOT}/trust/control-plane.sock"
POSTGRES="${PREFIX}-postgres"
API_PORT=$((20000 + $(printf '%s' "${RUN_ID}" | cksum | awk '{print $1}') % 20000))
AUTH_TOKEN="local-slice-integration-token-32-characters"
ARTIFACT_DIR="${ARTIFACT_DIR:-}"
PIDS=()

cleanup() {
  local exit_code=$? cleanup_exit=0
  trap - EXIT INT TERM
  set +e
  if [[ ${exit_code} -ne 0 ]]; then
    for log in transportd.log control.log; do
      if [[ -f "${TMP_ROOT}/${log}" ]]; then
        echo "--- ${log} ---" >&2
        tail -n 200 "${TMP_ROOT}/${log}" >&2
      fi
    done
    docker logs "${POSTGRES}" >&2 || true
  fi
  if [[ -n "${ARTIFACT_DIR}" ]]; then
    mkdir -p "${ARTIFACT_DIR}" || cleanup_exit=1
    docker ps -a --filter "name=^/${POSTGRES}$" >"${ARTIFACT_DIR}/docker-ps.txt" 2>&1 || true
    docker logs "${POSTGRES}" >"${ARTIFACT_DIR}/postgres.log" 2>&1 || true
    [[ -f "${TMP_ROOT}/control.log" ]] && cp -f "${TMP_ROOT}/control.log" "${ARTIFACT_DIR}/control-plane.log"
    [[ -f "${TMP_ROOT}/transportd.log" ]] && cp -f "${TMP_ROOT}/transportd.log" "${ARTIFACT_DIR}/transportd.log"
    cp -f "${TMP_ROOT}"/*.sse "${TMP_ROOT}"/*.headers "${ARTIFACT_DIR}/" 2>/dev/null || true
  fi
  for pid in "${PIDS[@]:-}"; do
    [[ -n "${pid}" ]] || continue
    kill -TERM "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  done
  docker rm -f "${POSTGRES}" >/dev/null 2>&1 || cleanup_exit=1
  if docker inspect "${POSTGRES}" >/dev/null 2>&1; then
    echo "local-slice integration left container ${POSTGRES}" >&2
    cleanup_exit=1
  fi
  rm -rf "${TMP_ROOT}"
  if ((exit_code != 0)); then
    exit "${exit_code}"
  fi
  exit "${cleanup_exit}"
}
trap cleanup EXIT INT TERM

mkdir -p "${TMP_ROOT}/run" "${TMP_ROOT}/trust"
chmod 0700 "${TMP_ROOT}/trust"
(cd "${ROOT}/control-plane" && go build -trimpath -o "${TMP_ROOT}/ocserv-control" ./cmd/ocserv-control)
(cd "${ROOT}/rust" && cargo build --locked --package ocservia-transportd-stub)

docker run -d --name "${POSTGRES}" \
  -e POSTGRES_DB=ocservia -e POSTGRES_USER=ocservia_owner -e POSTGRES_PASSWORD=test-owner-only \
  -p "127.0.0.1::5432" postgres:17-bookworm >/dev/null
for _ in $(seq 1 60); do
  if docker exec "${POSTGRES}" psql -U ocservia_owner -d ocservia -Atc "SELECT 1" >/dev/null 2>&1; then break; fi
  sleep 1
done
port="$(docker port "${POSTGRES}" 5432/tcp | sed -n 's/.*://p')"
owner_url="postgres://ocservia_owner:test-owner-only@127.0.0.1:${port}/ocservia?sslmode=disable"
runtime_url="postgres://ocservia_app:test-runtime-only@127.0.0.1:${port}/ocservia?sslmode=disable"
docker exec "${POSTGRES}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "CREATE ROLE ocservia_app LOGIN PASSWORD 'test-runtime-only'" >/dev/null
OCSERV_ENVIRONMENT=test OCSERV_DATABASE_URL="${owner_url}" OCSERV_RUNTIME_DATABASE_ROLE=ocservia_app \
  "${TMP_ROOT}/ocserv-control" --migrate-only

start_stub() {
  "${ROOT}/rust/target/debug/ocservia-transportd-stub" --socket "${SOCKET}" --queue-capacity 8 \
    --control-plane-uid "$(id -u)" --control-plane-gid "$(id -g)" \
    >"${TMP_ROOT}/transportd.log" 2>&1 &
  STUB_PID=$!
  PIDS+=("${STUB_PID}")
  for _ in $(seq 1 50); do
    [[ -S "${SOCKET}" ]] && break
    sleep 0.1
  done
  [[ -S "${SOCKET}" ]]
  [[ "$(stat -c '%a' "${SOCKET}" 2>/dev/null || stat -f '%Lp' "${SOCKET}")" == "660" ]]
}

start_control() {
  OCSERV_ENVIRONMENT=development OCSERV_HTTP_ADDRESS="127.0.0.1:${API_PORT}" \
    OCSERV_DATABASE_URL="${runtime_url}" OCSERV_LOCAL_SIMULATOR=true \
    OCSERV_DEV_AUTH_TOKEN="${AUTH_TOKEN}" \
    OCSERV_TRANSPORT_SOCKET="${SOCKET}" OCSERV_TRANSPORT_QUEUE_CAPACITY=8 \
    OCSERV_TRANSPORT_UID="$(id -u)" OCSERV_TRANSPORT_GID="$(id -g)" \
    OCSERV_TRUST_SOCKET="${TRUST_SOCKET}" \
    "${TMP_ROOT}/ocserv-control" --role=all >"${TMP_ROOT}/control.log" 2>&1 &
  CONTROL_PID=$!
  PIDS+=("${CONTROL_PID}")
  for _ in $(seq 1 60); do
    if curl --fail --silent "http://127.0.0.1:${API_PORT}/readyz" >/dev/null; then break; fi
    sleep 0.2
  done
  curl --fail --silent "http://127.0.0.1:${API_PORT}/readyz" >/dev/null
}

stop_pid() {
  local pid=$1
  kill -TERM "${pid}" 2>/dev/null || true
  wait "${pid}" 2>/dev/null || true
  local index
  for index in "${!PIDS[@]}"; do
    if [[ "${PIDS[index]}" == "${pid}" ]]; then
      PIDS[index]=""
    fi
  done
  return 0
}

create_probe() {
  local body=$1
  curl --fail --silent -H 'Content-Type: application/json' \
    -d "${body}" "http://127.0.0.1:${API_PORT}/api/v1/development/simulations"
}

wait_state() {
  local operation_id=$1 expected=$2 response
  for _ in $(seq 1 100); do
    response="$(curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/operations/${operation_id}")"
    if [[ "$(jq -r .state <<<"${response}")" == "${expected}" ]]; then
      printf '%s\n' "${response}"
      return 0
    fi
    sleep 0.2
  done
  return 1
}

start_stub
start_control
fd_before="$(find "/proc/${CONTROL_PID}/fd" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l || printf '0')"

normal="$(create_probe '{"heartbeat_count":3,"delay_millis":25}')"
normal_id="$(jq -r .id <<<"${normal}")"
normal_node="$(jq -r .node_id <<<"${normal}")"
wait_state "${normal_id}" succeeded >/dev/null
events="$(curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/events?page_size=200")"
jq -e --arg node "${normal_node}" '[.items[] | select(.node_id == $node)] | length == 5' <<<"${events}" >/dev/null
jq -e --arg node "${normal_node}" 'all(.items[] | select(.node_id == $node); .traceparent | test("^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$"))' <<<"${events}" >/dev/null

node_version="$(docker exec "${POSTGRES}" psql -U ocservia_owner -d ocservia -Atc "SELECT version FROM nodes WHERE id='${normal_node}'")"
synthetic="$(curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: synthetic-echo-1' \
  -H "If-Match: \"revision-${node_version}\"" \
  -d '{"kind":"echo","message":"i09"}' \
  "http://127.0.0.1:${API_PORT}/api/v1/nodes/${normal_node}/synthetic-commands")"
synthetic_id="$(jq -r .id <<<"${synthetic}")"
wait_state "${synthetic_id}" dispatched >/dev/null
replayed="$(curl --fail --silent -D "${TMP_ROOT}/replay.headers" -H "Authorization: Bearer ${AUTH_TOKEN}" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: synthetic-echo-1' \
  -d "{\"kind\":\"echo\",\"message\":\"i09\",\"expected_version\":${node_version}}" \
  "http://127.0.0.1:${API_PORT}/api/v1/nodes/${normal_node}/synthetic-commands")"
test "$(jq -r .id <<<"${replayed}")" = "${synthetic_id}"
grep -qi '^Idempotency-Replayed: true' "${TMP_ROOT}/replay.headers"
test "$(curl --silent --output /dev/null --write-out '%{http_code}' -H "Authorization: Bearer ${AUTH_TOKEN}" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: synthetic-echo-1' \
  -d "{\"kind\":\"echo\",\"message\":\"different\",\"expected_version\":${node_version}}" \
  "http://127.0.0.1:${API_PORT}/api/v1/nodes/${normal_node}/synthetic-commands")" = "409"
curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" \
  "http://127.0.0.1:${API_PORT}/api/v1/operations/queue-metrics" |
  jq -e '.outbox_unpublished_total == 0 and .command_queue_depth == 0 and .command_unknown_total == 0' >/dev/null
timeout 2s curl --no-buffer --silent -H "Authorization: Bearer ${AUTH_TOKEN}" \
  "http://127.0.0.1:${API_PORT}/api/v1/operations/${synthetic_id}/events" >"${TMP_ROOT}/operation.sse" || true
grep -q '^event: operation' "${TMP_ROOT}/operation.sse"
grep -q '"state":"dispatched"' "${TMP_ROOT}/operation.sse"

page="$(curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/events?page_size=2")"
jq -e '.page.has_more == true and (.page.next_cursor | type == "string") and (.items | length == 2)' <<<"${page}" >/dev/null

high_event_id="019cf100-0000-7000-8000-ffffffffffff"
low_event_id="019cf100-0000-7000-8000-000000000001"
docker exec "${POSTGRES}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "INSERT INTO transport_events (event_id, node_id, event_type, occurred_at, traceparent, payload, transport_cursor_valid) VALUES ('${high_event_id}', '${normal_node}', 'heartbeat', now(), '00-0123456789abcdef0123456789abcdef-0123456789abcdef-01', '', false), ('${low_event_id}', '${normal_node}', 'heartbeat', now(), '00-0123456789abcdef0123456789abcdef-0123456789abcdef-01', '', false)" >/dev/null
curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/events?page_size=200&after=${high_event_id}" |
  jq -e --arg id "${low_event_id}" 'any(.items[]; .id == $id)' >/dev/null

duplicate="$(create_probe '{"heartbeat_count":3,"delay_millis":10,"duplicate_event":true}')"
duplicate_id="$(jq -r .id <<<"${duplicate}")"
duplicate_node="$(jq -r .node_id <<<"${duplicate}")"
wait_state "${duplicate_id}" succeeded >/dev/null
test "$(docker exec "${POSTGRES}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM transport_events WHERE node_id = '${duplicate_node}'")" = "5"

failed="$(create_probe '{"heartbeat_count":1,"return_error":true}')"
wait_state "$(jq -r .id <<<"${failed}")" failed >/dev/null

disconnected="$(create_probe '{"heartbeat_count":1,"disconnect_after":true}')"
disconnected_id="$(jq -r .id <<<"${disconnected}")"
disconnected_node="$(jq -r .node_id <<<"${disconnected}")"
wait_state "${disconnected_id}" succeeded >/dev/null
for _ in $(seq 1 50); do
  status="$(docker exec "${POSTGRES}" psql -U ocservia_owner -d ocservia -Atc "SELECT status FROM nodes WHERE id = '${disconnected_node}'")"
  [[ "${status}" == "offline" ]] && break
  sleep 0.1
done
[[ "${status}" == "offline" ]]

nullable_operation_id="019cf000-0000-7000-8000-000000000001"
docker exec "${POSTGRES}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "INSERT INTO operations (id, workspace_id, state, request_id, created_at, updated_at) SELECT '${nullable_operation_id}', id, 'draft', 'nullable-identifiers', now(), now() FROM workspaces WHERE slug = 'local-simulator'" >/dev/null
curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/operations/${nullable_operation_id}" |
  jq -e '.id == "019cf000-0000-7000-8000-000000000001" and (has("node_id") | not) and (has("command_id") | not)' >/dev/null
curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/operations?page_size=200" |
  jq -e --arg id "${nullable_operation_id}" 'any(.items[]; .id == $id and (has("node_id") | not) and (has("command_id") | not))' >/dev/null

operations_page="$(curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/operations?page_size=2")"
jq -e '.page.has_more == true and (.page.next_cursor | type == "string") and (.items | length == 2)' <<<"${operations_page}" >/dev/null
operations_cursor="$(jq -r .page.next_cursor <<<"${operations_page}")"
curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/operations?page_size=2&cursor=${operations_cursor}" |
  jq -e --arg cursor "${operations_cursor}" '(.items | length) >= 1 and all(.items[]; .id != $cursor)' >/dev/null

first_event="$(jq -r '.items[0].id' <<<"${events}")"
timeout 4s curl --no-buffer --silent --limit-rate 32 -H "Authorization: Bearer ${AUTH_TOKEN}" -H "Last-Event-ID: ${first_event}" \
  "http://127.0.0.1:${API_PORT}/api/v1/events/stream" >"${TMP_ROOT}/resumed.sse" &
sse_pid=$!
PIDS+=("${sse_pid}")
sleep 0.3
resume="$(create_probe '{"heartbeat_count":2,"delay_millis":10}')"
wait_state "$(jq -r .id <<<"${resume}")" succeeded >/dev/null
wait "${sse_pid}" 2>/dev/null || true
grep -q '^id: ' "${TMP_ROOT}/resumed.sse"
if grep -q "^id: ${first_event}$" "${TMP_ROOT}/resumed.sse"; then
  echo "SSE replay repeated Last-Event-ID" >&2
  exit 1
fi

latest_event="$(curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/events?page_size=200" | jq -r '.items[-1].id')"
for subscriber in one two; do
  timeout 4s curl --no-buffer --silent -H "Authorization: Bearer ${AUTH_TOKEN}" -H "Last-Event-ID: ${latest_event}" \
    "http://127.0.0.1:${API_PORT}/api/v1/events/stream" >"${TMP_ROOT}/subscriber-${subscriber}.sse" &
  PIDS+=("$!")
done
sleep 0.3
fanout="$(create_probe '{"heartbeat_count":1,"delay_millis":10}')"
wait_state "$(jq -r .id <<<"${fanout}")" succeeded >/dev/null
for subscriber in one two; do
  for _ in $(seq 1 40); do
    grep -q '^id: ' "${TMP_ROOT}/subscriber-${subscriber}.sse" && break
    sleep 0.1
  done
  grep -q '^id: ' "${TMP_ROOT}/subscriber-${subscriber}.sse"
done

# Model an in-flight operation whose original command trace differs from the
# connection-level disconnect event emitted when transportd disappears.
docker exec "${POSTGRES}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "UPDATE operations SET state = 'running' WHERE id = '${normal_id}'" >/dev/null
queued_operation_id="019cf000-0000-7000-8000-000000000002"
docker exec "${POSTGRES}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "INSERT INTO operations (id, workspace_id, node_id, command_id, state, request_id, created_at, updated_at) SELECT '${queued_operation_id}', workspace_id, node_id, '019cf000-0000-7000-8000-000000000003', 'queued', 'queued-during-disconnect', now(), now() FROM operations WHERE id = '${normal_id}'; INSERT INTO local_slice_jobs (operation_id, command_envelope, traceparent, available_at, expires_at, created_at) SELECT '${queued_operation_id}', command_envelope, traceparent, now() + interval '1 hour', now() + interval '2 hours', now() FROM local_slice_jobs WHERE operation_id = '${normal_id}'" >/dev/null
stop_pid "${STUB_PID}"
rm -f "${SOCKET}"
expired="$(create_probe '{"heartbeat_count":1}')"
expired_id="$(jq -r .id <<<"${expired}")"
docker exec "${POSTGRES}" psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia -c \
  "UPDATE local_slice_jobs SET expires_at = now() - interval '1 second' WHERE operation_id = '${expired_id}'" >/dev/null
wait_state "${expired_id}" expired >/dev/null
recover="$(create_probe '{"heartbeat_count":2,"delay_millis":10}')"
recover_id="$(jq -r .id <<<"${recover}")"
sleep 1
stop_pid "${CONTROL_PID}"
start_stub
start_control
wait_state "${recover_id}" succeeded >/dev/null
wait_state "${normal_id}" unknown >/dev/null
wait_state "${queued_operation_id}" queued >/dev/null
test "$(docker exec "${POSTGRES}" psql -U ocservia_owner -d ocservia -Atc "SELECT status FROM nodes WHERE id = '${normal_node}'")" = "offline"
test "$(docker exec "${POSTGRES}" psql -U ocservia_owner -d ocservia -Atc "SELECT transport_cursor_valid FROM transport_events WHERE node_id = '${normal_node}' ORDER BY ingest_sequence DESC LIMIT 1")" = "f"
test "$(docker exec "${POSTGRES}" psql -U ocservia_owner -d ocservia -Atc "SELECT count(*) FROM transport_events WHERE transport_cursor_valid")" -gt 0
curl --fail --silent -H "Authorization: Bearer ${AUTH_TOKEN}" "http://127.0.0.1:${API_PORT}/api/v1/events?page_size=200" |
  jq -e --arg node "${normal_node}" '[.items[] | select(.node_id == $node)] | last | .type == "disconnected"' >/dev/null

for _ in $(seq 1 20); do
  curl --fail --silent "http://127.0.0.1:${API_PORT}/readyz" >/dev/null
done
fd_after="$(find "/proc/${CONTROL_PID}/fd" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l || printf '0')"
if ((fd_before > 0 && fd_after > fd_before + 20)); then
  echo "control-plane file descriptors grew without bound: ${fd_before} -> ${fd_after}" >&2
  exit 1
fi

echo "local slice integration passed"
