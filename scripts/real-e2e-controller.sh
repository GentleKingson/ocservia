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
TLS_DIR="${WORK}/tls"
OUTBOX="${WORK}/outbox"
export REAL_E2E_RELAY_DIR="${WORK}/relay"
WORKSPACE_ID="019d0000-0000-7000-8000-000000000001"
export T07_WORK="${WORK}" T07_WORKSPACE="${WORKSPACE_ID}"
export OCSERV_CONTROLLER_PUBLIC_URL=https://localhost:8443

case "${RUN_ID}${COMPOSE_PROJECT}" in
  *[!a-zA-Z0-9._-]*) echo "run or Compose project ID contains unsafe characters" >&2; exit 2 ;;
esac
umask 077

OCSERV_CONTROLLER_ENDPOINT_ID="$(printf '0%.0s' {1..64})"
[[ ! -s "${STATE}/controller-endpoint-id" ]] || OCSERV_CONTROLLER_ENDPOINT_ID="$(<"${STATE}/controller-endpoint-id")"
export OCSERV_CONTROLLER_ENDPOINT_ID

compose() {
  export REAL_E2E_RELAY_URL REAL_E2E_BUSINESS_DIR="${SECRETS}" REAL_E2E_TLS_DIR="${TLS_DIR}"
  REAL_E2E_RELAY_URL="$(<"${REAL_E2E_RELAY_DIR}/url")" || return 1
  local overlay=()
  if [[ -f "${REAL_E2E_RELAY_DIR}/compose.json" ]]; then
    overlay=(--file "${REAL_E2E_RELAY_DIR}/compose.json")
  fi
  docker compose --project-name "${COMPOSE_PROJECT}" --file "${COMPOSE_FILE}" "${overlay[@]}" "$@"
}

require_endpoint() {
  local value="${1:-}"
  [[ "${value}" =~ ^[0-9a-f]{64}$ ]] || {
    echo "invalid EndpointID" >&2
    return 1
  }
}

runner_instance_id() {
  local boot_id
  boot_id="$(< /proc/sys/kernel/random/boot_id)"
  [[ "${boot_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
    echo "runner boot ID is unavailable or invalid" >&2
    return 1
  }
  printf '%s\n' "${boot_id}"
}

start_controller() {
  local endpoint
  mkdir -p "${STATE}" "${SECRETS}" "${TLS_DIR}" "${WORK}/private" "${OUTBOX}/controller-ready" "${ARTIFACT_DIR}"
  chmod 0700 "${WORK}"
  chmod 0755 "${SECRETS}" "${TLS_DIR}"
  openssl genpkey -algorithm ED25519 -out "${SECRETS}/command.key.pem"
  openssl pkey -in "${SECRETS}/command.key.pem" -outform DER | tail -c 32 >"${SECRETS}/controller.key"
  openssl pkey -in "${SECRETS}/command.key.pem" -pubout -out "${OUTBOX}/controller-ready/command.pub.pem"
  sudo install -m 0440 "${OUTBOX}/controller-ready/command.pub.pem" "${SECRETS}/command.pub.pem"
  sudo chown 0:65532 "${SECRETS}/command.pub.pem"
  openssl rand -hex 32 >"${SECRETS}/session-key"
  openssl rand -hex 32 >"${SECRETS}/audit-checkpoint-key"
  for role in requester approver; do
    openssl rand -hex 32 >"${WORK}/private/${role}-password"
    sudo install -m 0400 "${WORK}/private/${role}-password" "${SECRETS}/${role}-password"
    sudo chown 65534:65532 "${SECRETS}/${role}-password"
  done
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=real-e2e-ca \
    -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign \
    -keyout "${SECRETS}/ca.key" -out "${WORK}/ca.crt" >/dev/null 2>&1
  openssl req -new -newkey rsa:2048 -nodes -subj /CN=localhost \
    -keyout "${TLS_DIR}/tls_private_key" -out "${TLS_DIR}/tls.csr" >/dev/null 2>&1
  printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' >"${TLS_DIR}/tls.ext"
  openssl x509 -req -days 1 -in "${TLS_DIR}/tls.csr" -CA "${WORK}/ca.crt" \
    -CAkey "${SECRETS}/ca.key" -CAcreateserial -extfile "${TLS_DIR}/tls.ext" \
    -out "${TLS_DIR}/tls_certificate" >/dev/null 2>&1
  chmod 0400 "${SECRETS}/command.key.pem" "${SECRETS}/session-key" \
    "${SECRETS}/audit-checkpoint-key" "${SECRETS}/controller.key" \
    "${TLS_DIR}/tls_private_key" "${SECRETS}/ca.key"
  sudo chown 65534:65532 "${SECRETS}/command.key.pem" "${SECRETS}/session-key" \
    "${SECRETS}/audit-checkpoint-key"
  sudo chown 0:0 "${TLS_DIR}/tls_private_key"
  chmod 0444 "${TLS_DIR}/tls_certificate" "${WORK}/ca.crt"
  # Only the token is secret inside the read-only transportd mount. Docker
  # mounts the directory directly; the host WORK ancestor remains private.
  sudo chown 65532:65532 "${REAL_E2E_RELAY_DIR}/token"
  chmod 0755 "${REAL_E2E_RELAY_DIR}"
  chmod 0444 "${REAL_E2E_RELAY_DIR}/ca.pem"
  OCSERV_CONTROLLER_ENDPOINT_ID="$(printf '0%.0s' {1..64})"
  export OCSERV_CONTROLLER_ENDPOINT_ID

  compose build controller-key-init migrate gateway
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
  if [[ -f "${REAL_E2E_RELAY_DIR}/compose.json" ]]; then
    compose exec -T transportd /usr/local/bin/ocservia-relay-network-probe \
      "$(<"${REAL_E2E_RELAY_DIR}/url")" /run/real-e2e-relay/ca.pem /run/real-e2e-relay/token --tcp-only
    printf 'TRANSPORTD_CONTAINER_RELAY_AUTH=PASS\nTRANSPORTD_WRONG_TOKEN_REJECTED=PASS\n' >"${RUNNER_TEMP}/real-e2e-evidence-controller/relay.txt"
  fi
  compose exec -T control-plane curl --fail --silent --show-error \
    http://127.0.0.1:8080/readyz >/dev/null
  compose exec -T postgres psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    -c "INSERT INTO workspaces(id,name,slug,created_at,updated_at) VALUES('${WORKSPACE_ID}','Cross-VM Real E2E','cross-vm-real-e2e',now(),now()),('00000000-0000-7000-8000-000000000072','Other','cross-vm-other',now(),now()) ON CONFLICT (id) DO NOTHING" >/dev/null
  compose run --rm --no-deps \
    -e OCSERV_LOCAL_BOOTSTRAP_USERNAME=t07-requester \
    -e "OCSERV_LOCAL_BOOTSTRAP_WORKSPACE_ID=${WORKSPACE_ID}" \
    -e OCSERV_LOCAL_BOOTSTRAP_PASSWORD_FILE=/run/real-e2e-business/requester-password \
    -e OCSERV_LOCAL_BOOTSTRAP_APPROVER_USERNAME=t07-approver \
    -e OCSERV_LOCAL_BOOTSTRAP_APPROVER_PASSWORD_FILE=/run/real-e2e-business/approver-password \
    control-plane --bootstrap-local-admin
  compose up --detach gateway
  for _ in {1..30}; do
    if curl --fail --silent --show-error --max-time 3 --cacert "${WORK}/ca.crt" https://localhost:8443/readyz >/dev/null; then
      break
    fi
    sleep 1
  done
  curl --fail --silent --show-error --max-time 3 --cacert "${WORK}/ca.crt" https://localhost:8443/readyz >/dev/null
  python3 "${ROOT}/scripts/real-e2e-business.py" login

  printf '%s\n' "${endpoint}" >"${OUTBOX}/controller-ready/controller-endpoint-id"
  runner_instance_id >"${OUTBOX}/controller-ready/runner-instance"
  printf '%s\n' "${WORKSPACE_ID}" >"${OUTBOX}/controller-ready/workspace-id"
}

issue_enrollment_token() {
  local peer_dir="${1:?agent endpoint directory is required}"
  local endpoint peer_runner
  endpoint="$(<"${peer_dir}/agent-endpoint-id")"
  peer_runner="$(<"${peer_dir}/runner-instance")"
  require_endpoint "${endpoint}"
  [[ -n "${peer_runner}" && "${peer_runner}" != "$(runner_instance_id)" ]] || {
    echo "Controller and Agent must run on different runner instances" >&2
    return 1
  }
  printf '%s\n' "${endpoint}" >"${STATE}/agent-endpoint-id"
  printf '%s\n' "${peer_runner}" >"${STATE}/agent-runner-instance"
  python3 "${ROOT}/scripts/real-e2e-business.py" issue-token "${endpoint}"
}

verify_enrollment() {
  local result_dir="${1:?enrollment result directory is required}"
  local node_id endpoint observed token_id token_observed
  node_id="$(<"${result_dir}/node-id")"
  endpoint="$(<"${STATE}/agent-endpoint-id")"
  [[ "$(<"${result_dir}/runner-instance")" == "$(<"${STATE}/agent-runner-instance")" ]]
  require_endpoint "${endpoint}"
  token_id="$(<"${STATE}/token-id")"
  [[ "$token_id" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]
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
  token_observed="$(compose exec -T postgres psql -At -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia \
    -c "SELECT consumed_node_id || ':' || encode(expected_endpoint_id,'hex') FROM enrollment_tokens WHERE id='${token_id}' AND consumed_at IS NOT NULL AND workspace_id='${WORKSPACE_ID}' AND expected_environment='development'")"
  [[ "$token_observed" == "$node_id:$endpoint" ]]
  printf '%s\n' "${node_id}" >"${STATE}/node-id"
  mkdir -p "$ARTIFACT_DIR"
  jq -n --arg node "$node_id" --arg endpoint "$endpoint" \
    --arg controller "$OCSERV_CONTROLLER_ENDPOINT_ID" --arg token "$token_id" \
    '{node_id:$node,agent_endpoint_id:$endpoint,controller_endpoint_id:$controller,token_id:$token,status:"pending",token_consumed:true}' \
    >"$ARTIFACT_DIR/binding.json"
  printf 'cross_vm_runner=PASS\nreal_transportd=PASS\nreal_iroh_enrollment=PASS\nnode_state=pending\n' \
    >"${ARTIFACT_DIR}/result.txt"
}

collect_diagnostics() {
  local leaked=0 hit
  mkdir -p "${ARTIFACT_DIR}"
  compose ps --all >"${ARTIFACT_DIR}/compose-ps.txt" 2>&1 || true
  compose logs --no-color postgres migrate control-plane transportd gateway \
    >"${ARTIFACT_DIR}/controller-services.log" 2>&1 || true
  docker system df >"${ARTIFACT_DIR}/docker-storage.txt" 2>&1 || true
  for secret in "${OUTBOX}/enrollment-token/enrollment-token" \
    "${WORK}/private/privd-credential" "${WORK}/private/smoke-vpn-password"; do
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
  # Cleanup must also work when configuration failed before any service start.
  REAL_E2E_RELAY_URL=https://unconfigured.invalid \
    REAL_E2E_RELAY_DIR="${REAL_E2E_RELAY_DIR}" \
    REAL_E2E_BUSINESS_DIR="${SECRETS}" REAL_E2E_TLS_DIR="${TLS_DIR}" docker compose \
    --project-name "${COMPOSE_PROJECT}" --file "${COMPOSE_FILE}" \
    --profile bootstrap down --volumes --remove-orphans --rmi local || status=$?
  rm -rf -- "${WORK}" "${RUNNER_TEMP:-/tmp}/real-e2e-agent-endpoint" \
    "${RUNNER_TEMP:-/tmp}/real-e2e-enrollment-result" || status=$?
  return "${status}"
}

case "${1:-}" in
  start)
    mkdir -p "${RUNNER_TEMP:-/tmp}/real-e2e-evidence-controller"
    # Preserve the failure location without exporting commands or secret arguments.
    trap 'printf "exit=%s file=%s line=%s callers=%s\n" "$?" "${BASH_SOURCE[0]##*/}" "$LINENO" "${BASH_LINENO[*]}" >> "${RUNNER_TEMP:-/tmp}/real-e2e-evidence-controller/startup-failure.txt"' ERR
    start_controller ;;
  sql) compose exec -T postgres psql -XAt -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia ;;
  issue-token) issue_enrollment_token "${2:-}" ;;
  verify-enrollment) verify_enrollment "${2:-}" ;;
  authority) python3 "${ROOT}/scripts/real-e2e-business.py" authority "${2:-}" ;;
  register-privd) python3 "${ROOT}/scripts/real-e2e-business.py" register-privd "${2:-}" ;;
  business) python3 "${ROOT}/scripts/real-e2e-business.py" run "${2:-}" "${3:-}" ;;
  verify-business) python3 "${ROOT}/scripts/real-e2e-business.py" verify "${2:-}" ;;
  diagnostics) collect_diagnostics ;;
  cleanup) cleanup_controller ;;
  *) echo "usage: $0 {start|issue-token DIR|verify-enrollment DIR|authority DIR|register-privd DIR|business READY_DIR PEER_DIR|verify-business DIR|diagnostics|cleanup}" >&2; exit 2 ;;
esac
