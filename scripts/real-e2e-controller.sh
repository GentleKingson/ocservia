#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
ARTIFACT_DIR="${ARTIFACT_DIR:-${RUNNER_TEMP:?RUNNER_TEMP is required}/artifacts/real-e2e-controller}"
COMPOSE_PROJECT="${COMPOSE_PROJECT:?COMPOSE_PROJECT is required}"
COMPOSE_FILE="${ROOT}/deploy/real-e2e/controller.compose.yaml"
WORK="${RUNNER_TEMP:-/tmp}/ocservia-real-e2e-controller-${RUN_ID}"
STATE="${WORK}/state"
SECRETS="${WORK}/secrets"
OUTBOX="${WORK}/outbox"
WORKSPACE_ID="019d0000-0000-7000-8000-000000000001"

case "${RUN_ID}${COMPOSE_PROJECT}" in
  *[!a-zA-Z0-9._-]*) echo "run or Compose project ID contains unsafe characters" >&2; exit 2 ;;
esac
umask 077

OCSERV_CONTROLLER_ENDPOINT_ID="$(printf '0%.0s' {1..64})"
[[ ! -s "${STATE}/controller-endpoint-id" ]] || OCSERV_CONTROLLER_ENDPOINT_ID="$(<"${STATE}/controller-endpoint-id")"
export OCSERV_CONTROLLER_ENDPOINT_ID

compose() {
  docker compose --project-name "${COMPOSE_PROJECT}" --file "${COMPOSE_FILE}" "$@"
}

require_endpoint() {
  local value="${1:-}"
  [[ "${value}" =~ ^[0-9a-f]{64}$ ]] || {
    echo "invalid EndpointID" >&2
    return 1
  }
}

start_controller() {
  local endpoint
  mkdir -p "${STATE}" "${SECRETS}" "${OUTBOX}/controller-ready" "${ARTIFACT_DIR}"
  chmod 0700 "${WORK}" "${SECRETS}"
  OCSERV_CONTROLLER_ENDPOINT_ID="$(printf '0%.0s' {1..64})"
  export OCSERV_CONTROLLER_ENDPOINT_ID

  compose build controller-key-init migrate
  compose up --detach controller-key-init transport-runtime-init postgres
  compose run --rm migrate
  compose --profile bootstrap up --detach transport-endpoint-bootstrap
  for _ in {1..60}; do
    endpoint="$(compose logs --no-color transport-endpoint-bootstrap 2>/dev/null \
      | sed -n 's/.*"endpoint_id":"\([0-9a-f]\{64\}\)".*/\1/p' | tail -1)"
    [[ -n "${endpoint}" ]] && break
    sleep 1
  done
  require_endpoint "${endpoint:-}"
  compose --profile bootstrap stop transport-endpoint-bootstrap
  compose --profile bootstrap rm --force transport-endpoint-bootstrap

  export OCSERV_CONTROLLER_ENDPOINT_ID="${endpoint}"
  printf '%s\n' "${endpoint}" >"${STATE}/controller-endpoint-id"
  compose up --detach control-plane
  for _ in {1..60}; do
    if compose exec -T control-plane test -S /run/ocserv-trust/control-plane.sock; then
      break
    fi
    sleep 1
  done
  compose exec -T control-plane test -S /run/ocserv-trust/control-plane.sock
  compose up --detach transportd
  for _ in {1..60}; do
    if compose exec -T transportd test -S /run/ocserv-platform/transportd.sock; then
      break
    fi
    sleep 1
  done
  compose exec -T transportd test -S /run/ocserv-platform/transportd.sock
  compose exec -T control-plane curl --fail --silent --show-error \
    http://127.0.0.1:8080/readyz >/dev/null
  compose exec -T postgres psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    -c "INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES('${WORKSPACE_ID}','Cross-VM Real E2E','cross-vm-real-e2e',now(),now()) ON CONFLICT (id) DO NOTHING" >/dev/null

  printf '%s\n' "${endpoint}" >"${OUTBOX}/controller-ready/controller-endpoint-id"
  hostname >"${OUTBOX}/controller-ready/runner-instance"
  printf '%s\n' "${WORKSPACE_ID}" >"${OUTBOX}/controller-ready/workspace-id"
}

issue_enrollment_token() {
  local peer_dir="${1:?agent endpoint directory is required}"
  local endpoint peer_runner token_response
  endpoint="$(<"${peer_dir}/agent-endpoint-id")"
  peer_runner="$(<"${peer_dir}/runner-instance")"
  require_endpoint "${endpoint}"
  [[ -n "${peer_runner}" && "${peer_runner}" != "$(hostname)" ]] || {
    echo "Controller and Agent must run on different runner instances" >&2
    return 1
  }
  token_response="${SECRETS}/enrollment-token-response.json"
  compose exec -T control-plane curl --fail --silent --show-error \
    --header 'Content-Type: application/json' \
    --data "$(jq -cn --arg workspace "${WORKSPACE_ID}" --arg endpoint "${endpoint}" \
      '{workspace_id:$workspace,environment:"development",expected_node_name:"github-hosted-node",expected_endpoint_id:$endpoint,ttl_seconds:600,reason:"cross-VM real Iroh enrollment smoke"}')" \
    http://127.0.0.1:8080/api/v1/enrollment-tokens >"${token_response}"
  mkdir -p "${OUTBOX}/enrollment-token"
  jq -er '.token | select(type == "string" and length == 43)' "${token_response}" \
    >"${OUTBOX}/enrollment-token/enrollment-token"
  chmod 0600 "${OUTBOX}/enrollment-token/enrollment-token"
  printf '%s\n' "${endpoint}" >"${STATE}/agent-endpoint-id"
}

verify_enrollment() {
  local result_dir="${1:?enrollment result directory is required}"
  local node_id endpoint observed
  node_id="$(<"${result_dir}/node-id")"
  endpoint="$(<"${STATE}/agent-endpoint-id")"
  [[ "${node_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
    echo "Agent returned an invalid UUIDv7 node ID" >&2
    return 1
  }
  observed="$(compose exec -T postgres psql -At -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    -c "SELECT n.id || ':' || n.status || ':' || encode(k.endpoint_id,'hex') FROM nodes n JOIN node_endpoint_keys k ON k.node_id=n.id WHERE n.id='${node_id}'")"
  [[ "${observed}" == "${node_id}:pending:${endpoint}" ]] || {
    echo "Controller did not persist the expected pending EndpointID binding" >&2
    return 1
  }
  printf 'cross_vm_runner=PASS\nreal_transportd=PASS\nreal_iroh_enrollment=PASS\nnode_state=pending\n' \
    >"${ARTIFACT_DIR}/result.txt"
}

collect_diagnostics() {
  local leaked=0 hit
  mkdir -p "${ARTIFACT_DIR}"
  compose ps --all >"${ARTIFACT_DIR}/compose-ps.txt" 2>&1 || true
  compose logs --no-color postgres migrate control-plane transportd \
    >"${ARTIFACT_DIR}/controller-services.log" 2>&1 || true
  docker system df >"${ARTIFACT_DIR}/docker-storage.txt" 2>&1 || true
  for secret in "${SECRETS}/enrollment-token-response.json" \
    "${OUTBOX}/enrollment-token/enrollment-token"; do
    [[ -s "${secret}" ]] || continue
    while IFS= read -r hit; do
      rm -f -- "${hit}"
      leaked=1
    done < <(grep -RIlF -f "${secret}" "${ARTIFACT_DIR}" 2>/dev/null || true)
  done
  if grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' "${ARTIFACT_DIR}" >/dev/null 2>&1; then
    while IFS= read -r hit; do rm -f -- "${hit}"; done \
      < <(grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' "${ARTIFACT_DIR}" || true)
    leaked=1
  fi
  ((leaked == 0)) || {
    echo "a diagnostic file contained secret material and was removed" >&2
    return 1
  }
}

cleanup_controller() {
  local status=0
  compose --profile bootstrap down --volumes --remove-orphans --rmi local || status=$?
  rm -rf -- "${WORK}" "${RUNNER_TEMP:-/tmp}/real-e2e-agent-endpoint" \
    "${RUNNER_TEMP:-/tmp}/real-e2e-enrollment-result" || status=$?
  return "${status}"
}

case "${1:-}" in
  start) start_controller ;;
  issue-token) issue_enrollment_token "${2:-}" ;;
  verify-enrollment) verify_enrollment "${2:-}" ;;
  diagnostics) collect_diagnostics ;;
  cleanup) cleanup_controller ;;
  *) echo "usage: $0 {start|issue-token DIR|verify-enrollment DIR|diagnostics|cleanup}" >&2; exit 2 ;;
esac
