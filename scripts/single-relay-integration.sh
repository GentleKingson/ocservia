#!/usr/bin/env bash
# Local functional integration only. This is not the multi-host G6 gate.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${RUN_ID:?}" "${RUNNER_TEMP:?}" "${ARTIFACT_DIR:?}"
: "${G6RD_CONTROL_PLANE_IMAGE:?}" "${G6RD_TRANSPORTD_IMAGE:?}"
: "${G6RD_RELAY_IMAGE:?}" "${G6RD_PROBE_IMAGE:?}" "${SINGLE_NODE_IMAGE:?}"
: "${SINGLE_AGENT_ARCHIVE:?}" "${SINGLE_AGENT_PUBLIC_KEY:?}" "${SINGLE_AGENT_KEY_SHA256:?}"
[[ ! -e "${RUNNER_TEMP}/g6-readiness-${RUN_ID}" ]] || exit 2
export FD_ID=fd-a FD_ALIAS=fd-alpha G6_AUTHORITY=engineering
G6RD_CANDIDATE_SHA="$(git -C "$ROOT" rev-parse HEAD)"
export G6RD_CANDIDATE_SHA
# shellcheck source=scripts/g6-readiness-fd-a.sh disable=SC1091
source "$ROOT/scripts/g6-readiness-fd-a.sh"
printf '%s\n' "$G6RD_CANDIDATE_SHA" >"$ARTIFACT_DIR/code-sha"
OVERRIDE="$G6RD_WORK/single.yaml"
NODE_CONTAINER="ocservia-single-node-$RUN_ID"
NODE_NETWORK="ocservia-single-node-$RUN_ID"
GATEWAY="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
RELAY_PORT=23443
RELAY_URL="https://relay-a:$RELAY_PORT"
RELAY_URL_B=""
RELAYS=(relay)
relay_args=(--relay-url "$RELAY_URL")
relay_endpoints=("relay-a:$RELAY_PORT")
node_hosts=(--add-host "relay-a:$GATEWAY")
transport_hosts="[\"relay-a:$GATEWAY\"]"
extra_compose=()
chain_name=Single-relay
if [[ -n "${SINGLE_INTEGRATED_PUBLIC_IP:-}" ]]; then
  : "${SINGLE_EDGE_IMAGE:?}" "${SINGLE_NETWORK_PROBE_IMAGE:?}"
  [[ "${SINGLE_LEGACY_SECOND_RELAY:-false}" != true ]] || exit 2
  python3 -c 'import ipaddress,sys; assert ipaddress.ip_address(sys.argv[1]).is_global' "$SINGLE_INTEGRATED_PUBLIC_IP"
  RELAY_PORT=443
  RELAY_URL=https://relay.p1.test
  relay_args=(--relay-url "$RELAY_URL")
  node_hosts=(--add-host "relay.p1.test:$SINGLE_INTEGRATED_PUBLIC_IP")
  transport_hosts="[\"relay.p1.test:$SINGLE_INTEGRATED_PUBLIC_IP\"]"
  chain_name='Integrated public Relay'
fi
if [[ "${SINGLE_LEGACY_SECOND_RELAY:-false}" == true ]]; then
  RELAY_URL_B=https://relay-b:23444
  RELAYS+=(legacy-relay)
  relay_args+=(--relay-url "$RELAY_URL_B")
  relay_endpoints+=(relay-b:23444)
  node_hosts+=(--add-host "relay-b:$GATEWAY")
  transport_hosts="[\"relay-a:$GATEWAY\",\"relay-b:$GATEWAY\"]"
  chain_name='Legacy two-Relay'
fi
g6rd_compose() {
  timeout --signal=TERM --kill-after=5s "${G6RD_COMPOSE_TIMEOUT_SECONDS:-120}s" docker compose -p "$COMPOSE_PROJECT" \
    -f "$COMPOSE_FILE" -f "$G6RD_RELEASE_COMPOSE" -f "$OVERRIDE" "${extra_compose[@]}" "$@"
}
finish() {
  local status=$?
  trap - EXIT
  set +e
  printf '%s\n' "$status" >"$ARTIFACT_DIR/exit-status"
  if [[ -n "${SINGLE_INTEGRATED_PUBLIC_IP:-}" ]]; then
    g6rd_compose logs --no-color edge >"$ARTIFACT_DIR/edge.log" 2>&1
    docker rm -f "p1-network-$RUN_ID" >/dev/null 2>&1
  fi
  for role in transportd worker api "${RELAYS[@]}"; do
    g6rd_compose logs --no-color "$role" >"$ARTIFACT_DIR/$role.log" 2>&1
  done
  g6rd_psql -c 'SELECT id,attempts,last_error FROM outbox_events; SELECT id,state FROM commands;' \
    >"$ARTIFACT_DIR/dispatch-state.txt" 2>&1
  docker exec "$NODE_CONTAINER" journalctl --no-pager -u ocservia-agent -u ocservia-privd -u ocserv \
    >"$ARTIFACT_DIR/node.log" 2>&1
  docker exec "$NODE_CONTAINER" bash -c '
    dpkg-query -W ocserv systemd openssl
    /usr/sbin/ocserv --version
    for binary in ocservia-agent ocservia-privd ocservia-upgrader; do
      "/usr/libexec/ocservia/$binary" --version
    done
  ' >"$ARTIFACT_DIR/node-versions.txt" 2>&1
  g6rd_psql -c 'SELECT version();' >"$ARTIFACT_DIR/postgres-version.txt" 2>&1
  docker rm -f "$NODE_CONTAINER" >"$ARTIFACT_DIR/cleanup-node.log" 2>&1
  docker network rm "$NODE_NETWORK" >>"$ARTIFACT_DIR/cleanup-node.log" 2>&1
  g6rd_compose down --volumes --remove-orphans >"$ARTIFACT_DIR/cleanup-compose.log" 2>&1
  exit "$status"
}
trap finish EXIT
g6rd_generate_secrets
if [[ -n "${SINGLE_INTEGRATED_PUBLIC_IP:-}" ]]; then
  openssl req -new -key "$G6RD_SECRETS/relay-leaf.key" -subj /CN=relay.p1.test \
    -out "$G6RD_SECRETS/p1.csr"
  openssl x509 -req -in "$G6RD_SECRETS/p1.csr" -CA "$G6RD_SECRETS/relay-ca.pem" \
    -CAkey "$G6RD_SECRETS/relay-ca.key" -CAcreateserial -days 1 \
    -extfile <(printf 'subjectAltName=DNS:relay.p1.test\nextendedKeyUsage=serverAuth\n') \
    -out "$G6RD_SECRETS/relay-leaf.crt"
  cat "$G6RD_SECRETS/relay-leaf.crt" "$G6RD_SECRETS/relay-ca.pem" >"$G6RD_SECRETS/relay-chain.crt"
fi
g6rd_export_common_env
g6rd_prepare_release_images
cat >"$OVERRIDE" <<EOF
services:
  relay:
    ports: !override ["$GATEWAY:$RELAY_PORT:3443"]
    networks: !override [relay-boundary]
  transportd:
    entrypoint: [/usr/local/libexec/ocservia-transportd-relays]
    environment:
      OCSERV_RELAY_URL_A: "$RELAY_URL"
      OCSERV_RELAY_URL_B: "$RELAY_URL_B"
    networks: !override [application, observability, relay-egress]
    extra_hosts: !override $transport_hosts
    command: !override
      - --socket
      - /run/ocserv-platform/transportd.sock
      - --trust-socket
      - /run/ocserv-trust/control-plane.sock
      - --key-file
      - /run/ocservia-secrets/controller.key
      - --control-plane-uid
      - "65534"
      - --control-plane-gid
      - "65532"
      - --relay-token-file
      - /run/relay-secrets/relay-token
      - --relay-ca-file
      - /run/relay-secrets/relay-ca.pem
      - --controller-verification-key-file
      - /run/ocservia-secrets/command-verification.pem
      - --require-fencing
  g6-probe:
    entrypoint: [/usr/local/bin/ocservia-g6-probe]
    user: "65534:65532"
networks:
  application:
    internal: true
  observability:
    internal: true
  relay-egress: {}
  relay-boundary: {}
EOF
if [[ -n "${SINGLE_INTEGRATED_PUBLIC_IP:-}" ]]; then
  sed 's/0.0.0.0:3443/0.0.0.0:8443/' "$ROOT/deploy/g6-readiness/relay.toml" >"$G6RD_WORK/p1-relay.toml"
  cat >"$G6RD_WORK/p1-edge.yaml" <<EOF
services:
  edge:
    image: $SINGLE_EDGE_IMAGE
    user: "65532:65532"
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    tmpfs: [/tmp:size=16m,mode=1777]
    environment:
      OCSERV_PUBLIC_HOST: controller.p1.test
      OCSERV_RELAY_PUBLIC_HOST: relay.p1.test
    ports: ["0.0.0.0:443:8443/tcp"]
    networks: [relay-boundary]
  relay:
    ports: !override ["0.0.0.0:7842:7842/udp"]
    volumes:
      - $G6RD_WORK/p1-relay.toml:/etc/iroh-relay/relay.toml:ro
EOF
  extra_compose=(-f "$G6RD_WORK/p1-edge.yaml")
  g6rd_compose up -d --no-deps edge
fi
if [[ -n "$RELAY_URL_B" ]]; then
  # Preserve v0.6.0's two-Relay requirement with two real authenticated
  # services, not duplicate URLs. Both still share this one disposable host.
  cat >"$G6RD_WORK/legacy-relay.yaml" <<EOF
services:
  legacy-relay:
    extends:
      file: "$COMPOSE_FILE"
      service: relay
    image: "$G6RD_RELAY_IMAGE"
    ports: !override ["$GATEWAY:23444:3443"]
    networks: !override [relay-boundary]
EOF
  extra_compose=(-f "$G6RD_WORK/legacy-relay.yaml")
fi
phase_primary_up
network_probe_status=0
if [[ -n "${SINGLE_INTEGRATED_PUBLIC_IP:-}" ]]; then
  timeout --kill-after=5s 70s docker run --rm --name "p1-network-$RUN_ID" \
    --user 65534:65532 --network "${COMPOSE_PROJECT}_relay-egress" \
    --add-host "relay.p1.test:$SINGLE_INTEGRATED_PUBLIC_IP" \
    -v "$G6RD_RELAY_DIR/probe:/probe:ro" "$SINGLE_NETWORK_PROBE_IMAGE" \
    "$RELAY_URL" /probe/relay-ca.pem /probe/relay-token \
    >"$ARTIFACT_DIR/relay-network.log" 2>&1 || network_probe_status=$?
  printf '%s\n' "$network_probe_status" >"$ARTIFACT_DIR/relay-network-status"
fi
if [[ -n "$RELAY_URL_B" ]]; then
  g6rd_compose up -d --no-build --no-deps legacy-relay
fi
for role in requester approver; do
  identity="$(g6rd_secret "$role-identity-id")"
  binding="$(g6rd_uuidv7)"
  g6rd_psql -c "INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at)
    VALUES('$binding','$identity','$G6RD_WORKSPACE_ID','Operator','workspace',NULL,'$identity',now())" >/dev/null
done
g6rd_compose config --format json | jq '{transportd:.services.transportd,networks:.networks}' \
  >"$ARTIFACT_DIR/transport-topology.json"
docker network create "$NODE_NETWORK" >/dev/null
docker run -d --name "$NODE_CONTAINER" --privileged --cgroupns private \
  --network "$NODE_NETWORK" "${node_hosts[@]}" \
  --tmpfs /run --tmpfs /run/lock \
  -v "$ROOT:/source:ro" -v "$G6RD_SECRETS:/test-secrets:ro" \
  -v "$(dirname "$SINGLE_AGENT_ARCHIVE"):/payload:ro" \
  -v "$SINGLE_AGENT_PUBLIC_KEY:/test-release-key.pem:ro" \
  "$SINGLE_NODE_IMAGE" /sbin/init >/dev/null
if [[ -n "${SINGLE_INTEGRATED_PUBLIC_IP:-}" ]]; then
  # Block direct QUIC only in this disposable node namespace; TCP Relay stays available.
  node_pid="$(docker inspect --format '{{.State.Pid}}' "$NODE_CONTAINER")"
  nsenter -t "$node_pid" -n iptables -A OUTPUT -p udp ! -d 127.0.0.0/8 -j REJECT
  nsenter -t "$node_pid" -n ip6tables -A OUTPUT -p udp -j REJECT
  nsenter -t "$node_pid" -n iptables-save >"$ARTIFACT_DIR/node-direct-udp-block.txt"
  nsenter -t "$node_pid" -n ip6tables-save >>"$ARTIFACT_DIR/node-direct-udp-block.txt"
fi
archive_name="$(basename "$SINGLE_AGENT_ARCHIVE")"
docker exec -e "ARCHIVE=/payload/$archive_name" -e "KEY_SHA=$SINGLE_AGENT_KEY_SHA256" \
  "$NODE_CONTAINER" bash -euo pipefail -c '
  root=$(AGENT_TRUSTED_KEY_SHA256="$KEY_SHA" bash /source/scripts/verify-agent-package.sh "$ARCHIVE" "$ARCHIVE.sha256" "$ARCHIVE.sha256.sig" /test-release-key.pem)
  INSTALL_PRODUCTION_RELAYS=true bash "$root/scripts/install-agent.sh"
  install -o root -g ocserv-agent -m 0640 /test-secrets/relay-token /etc/ocservia-agent/relay-access-token
  install -o root -g ocserv-agent -m 0640 /test-secrets/command-verification.pem /etc/ocservia-agent/controller-command-verification-key.pem
  install -o root -g root -m 0600 /test-secrets/seal-user-password.key /etc/ocservia-agent/user-password-seal-private.pem
  install -o root -g root -m 0600 /test-secrets/seal-p12.key /etc/ocservia-agent/p12-password-seal-private.pem
  install -o root -g ocserv-agent -m 0640 /test-secrets/relay-ca.pem /etc/ocservia-agent/test-relay-ca.pem
  # The runtime uses WebPKI roots, not the OS trust store. This test-only
  # copy adds the existing explicit CA option; it never disables TLS checks.
  install -d /etc/systemd/system/ocservia-agent.service.d
  if [[ -x /usr/libexec/ocservia/ocservia-agent-relays ]]; then
    sed '\''s|exec /usr/libexec/ocservia/ocservia-agent "\$@"|exec /usr/libexec/ocservia/ocservia-agent "$@" --relay-ca-file /etc/ocservia-agent/test-relay-ca.pem|'\'' \
      /usr/libexec/ocservia/ocservia-agent-relays >/usr/libexec/ocservia/test-agent-relays
    chmod 0755 /usr/libexec/ocservia/test-agent-relays
    printf "[Service]\nExecStart=\nExecStart=/usr/libexec/ocservia/test-agent-relays\n" \
      >/etc/systemd/system/ocservia-agent.service.d/99-test-ca.conf
  else
    # v0.6.0 shipped a direct ExecStart, not the later launcher. Preserve
    # that installed unit; a test-only drop-in adds the private CA.
    [[ "$(/usr/libexec/ocservia/ocservia-agent --version)" == "ocservia-agent 0.6.0" ]]
    sed '\''/^ExecStart=\/usr\/libexec\/ocservia\/ocservia-agent / s|$| --relay-ca-file /etc/ocservia-agent/test-relay-ca.pem|'\'' \
      /usr/lib/systemd/system/ocservia-agent.service.d/10-production-relays.conf \
      >/etc/systemd/system/ocservia-agent.service.d/99-test-ca.conf
  fi
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=ocserv.single.test \
    -keyout /etc/ocserv/test.key -out /etc/ocserv/test.crt >/dev/null 2>&1
  chmod 0600 /etc/ocserv/test.key
  install -m 0600 /dev/null /etc/ocserv/ocpasswd
  ' >"$ARTIFACT_DIR/agent-install.log" 2>&1
docker exec -i "$NODE_CONTAINER" bash -c 'cat >/etc/ocserv/ocserv.conf' <<'EOF'
auth = "plain[passwd=/etc/ocserv/ocpasswd]"
tcp-port = 44443
udp-port = 0
listen-host = 127.0.0.1
run-as-user = nobody
run-as-group = nogroup
socket-file = /run/ocserv.socket
server-cert = /etc/ocserv/test.crt
server-key = /etc/ocserv/test.key
isolate-workers = false
max-clients = 4
max-same-clients = 2
device = vpns
ipv4-network = 192.168.199.0
ipv4-netmask = 255.255.255.0
use-occtl = true
EOF
endpoint="$(docker exec -u ocserv-agent "$NODE_CONTAINER" /usr/libexec/ocservia/ocservia-agent \
  --controller "$OCSERV_CONTROLLER_ENDPOINT_ID" --prepare-enrollment)"
g6rd_mint_enrollment_token single-relay-node "$endpoint" >"$G6RD_SECRETS/enrollment-token"
docker exec "$NODE_CONTAINER" install -o root -g ocserv-agent -m 0640 \
  /test-secrets/enrollment-token /etc/ocservia-agent/enrollment-token
docker exec -u ocserv-agent "$NODE_CONTAINER" /usr/libexec/ocservia/ocservia-agent \
  --controller "$OCSERV_CONTROLLER_ENDPOINT_ID" --relay-mode custom "${relay_args[@]}" \
  --relay-token-file /etc/ocservia-agent/relay-access-token \
  --relay-ca-file /etc/ocservia-agent/test-relay-ca.pem \
  --enrollment-token-file /etc/ocservia-agent/enrollment-token --enrollment-environment development \
  --user-password-seal-key-id g6-user-seal-v1 \
  --user-password-seal-public-key-sha256 "$(g6rd_secret seal-user-password-sha256)" \
  --p12-password-seal-key-id g6-p12-seal-v1 \
  --p12-password-seal-public-key-sha256 "$(g6rd_secret seal-p12-sha256)" \
  >"$ARTIFACT_DIR/enrollment.log" 2>&1
node="$(g6rd_extract_enrollment_node_id <"$ARTIFACT_DIR/enrollment.log")"
printf '%s\n' "$node" >"$ARTIFACT_DIR/node-id"
g6rd_approve_node "$node"
docker exec -i "$NODE_CONTAINER" bash -euo pipefail -c 'cat >/etc/ocservia-agent/agent.env; chown root:ocserv-agent /etc/ocservia-agent/agent.env; chmod 0640 /etc/ocservia-agent/agent.env' <<EOF
CONTROLLER_ENDPOINT_ID=$OCSERV_CONTROLLER_ENDPOINT_ID
NODE_ID=$node
CONTROLLER_COMMAND_VERIFICATION_KEY_FILE=/etc/ocservia-agent/controller-command-verification-key.pem
USER_PASSWORD_SEAL_KEY_ID=g6-user-seal-v1
USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256=$(g6rd_secret seal-user-password-sha256)
P12_PASSWORD_SEAL_KEY_ID=g6-p12-seal-v1
P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256=$(g6rd_secret seal-p12-sha256)
EOF
docker exec -i "$NODE_CONTAINER" bash -euo pipefail -c 'cat >/etc/ocservia-agent/relays.env; chown root:ocserv-agent /etc/ocservia-agent/relays.env; chmod 0640 /etc/ocservia-agent/relays.env' <<EOF
RELAY_URL_A=$RELAY_URL
RELAY_URL_B=$RELAY_URL_B
EOF
docker exec "$NODE_CONTAINER" systemctl daemon-reload
docker exec "$NODE_CONTAINER" systemctl start ocserv ocservia-privd
# Provision the real root-owned receipt key through the existing one-shot API.
g6rd_api_session_curl requester "/api/v1/nodes/$node/privd-attestation-credentials" --fail-with-body -X POST \
  -H 'Content-Type: application/json' --data '{"ttl_seconds":300,"reason":"single Relay test receipt key"}' \
  >"$G6RD_SECRETS/privd-credential.json"
docker exec "$NODE_CONTAINER" /usr/libexec/ocservia/ocservia-privd attestation-registration \
  /var/lib/ocservia-privd/attestation.key "$node" \
  "$(jq -er .controller_nonce_hex "$G6RD_SECRETS/privd-credential.json")" \
  "$(jq -er .credential_context_sha256_hex "$G6RD_SECRETS/privd-credential.json")" \
  >"$G6RD_SECRETS/privd-proof.json"
jq --slurpfile credential "$G6RD_SECRETS/privd-credential.json" \
  '. + {credential:$credential[0].credential}' "$G6RD_SECRETS/privd-proof.json" \
  >"$G6RD_SECRETS/privd-registration.json"
g6rd_api_curl "/api/v1/nodes/$node/privd-attestation-keys:register" --fail-with-body -X POST \
  -H 'Content-Type: application/json' --data-binary "@$G6RD_SECRETS/privd-registration.json" \
  >"$ARTIFACT_DIR/privd-key-registration.json"

# Match the existing local G6 fixture's initial trust-snapshot synchronization
# after approval and receipt-key provisioning. Never do this during a fault.
g6rd_compose restart transportd
docker exec "$NODE_CONTAINER" systemctl start ocservia-agent
G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 g6rd_wait_until_deadline 120 2 'single-relay Agent session' \
  g6rd_probe_node_connection relay "$node"
g6rd_probe_node_connection relay "$node" >"$ARTIFACT_DIR/session.json"
g6rd_api_session_curl requester "/api/v1/nodes/$node" >"$ARTIFACT_DIR/node-read.json"
docker exec "$(g6rd_compose ps -q transportd)" sh -c '
  for exe in /proc/[0-9]*/exe; do
    if [ "$(readlink "$exe")" = /usr/local/bin/ocservia-transportd ]; then
      cat "${exe%/exe}/cmdline"
    fi
  done' >"$ARTIFACT_DIR/transport-argv.nul"
docker exec "$NODE_CONTAINER" sh -c 'cat /proc/$(systemctl show ocservia-agent -p MainPID --value)/cmdline' \
  >"$ARTIFACT_DIR/agent-argv.nul"
python3 - "$ARTIFACT_DIR" "$RELAY_URL" "$RELAY_URL_B" <<'PY'
import json
from pathlib import Path
import sys
for role in ('transport', 'agent'):
    argv = (Path(sys.argv[1]) / f'{role}-argv.nul').read_bytes().rstrip(b'\0').decode().split('\0')
    expected = [url for url in sys.argv[2:] if url]
    assert [argv[i + 1] for i, arg in enumerate(argv) if arg == '--relay-url'] == expected
    assert argv[argv.index('--relay-mode') + 1] == 'custom'
    (Path(sys.argv[1]) / f'{role}-argv.json').write_text(json.dumps(argv, indent=2) + '\n')
PY
echo "$chain_name enrollment, independent approval and real Agent session passed"

approve_reload() {
  local approval hash
  g6rd_api_session_curl requester /api/v1/approval-requests --fail-with-body -X POST \
    -H 'Content-Type: application/json' -H "X-Workspace-ID: $G6RD_WORKSPACE_ID" \
    --data "$(jq -cn --arg node "$node" '{action:"service.reload",resource_type:"node",resource_id:$node,reason:"single Relay test ocserv reload",ttl_seconds:600}')" \
    >"$ARTIFACT_DIR/reload-approval.json" || return
  approval="$(jq -er .id "$ARTIFACT_DIR/reload-approval.json")" || return
  hash="$(jq -er .request_hash "$ARTIFACT_DIR/reload-approval.json")" || return
  g6rd_api_session_curl approver "/api/v1/approval-requests/$approval:approve" --fail-with-body -X POST \
    -H 'Content-Type: application/json' \
    --data "$(jq -cn --arg hash "$hash" '{reason:"independent test approval",expected_request_hash:$hash}')" \
    >"$ARTIFACT_DIR/reload-approval-decision.json" || return
  printf '%s\n' "$approval"
}
enqueue_reload() {
  local key="$1" reason="$2" output="$3" attempt status
  for attempt in 1 2 3; do
    revision="$(g6rd_node_revision "$node")" || return
    status="$(g6rd_api_session_curl requester "/api/v1/nodes/$node/service:reload" -X POST \
      -H 'Content-Type: application/json' -H "Idempotency-Key: $key" \
      -H "If-Match: \"revision-$revision\"" -H "X-Approval-ID: $approval" \
      --data "$(jq -cn --arg reason "$reason" '{reason:$reason,ttl_seconds:300}')" \
      --output "$output" --write-out '%{http_code}')" || return
    [[ "$status" != 202 ]] || return 0
    cp "$output" "$output.attempt-$attempt"
    # A session-owner transition can advance the revision during initial
    # admission. Refresh only this explicit pre-effect rejection, same key.
    if [[ "$status" != 409 ]] || ! jq -e '.type == "https://ocservia.dev/problems/stale-revision"' "$output" >/dev/null; then
      cat "$output" >&2
      return 1
    fi
    sleep 1
  done
  echo 'reload revision remained stale after three attempts' >&2
  return 1
}
approval="$(approve_reload)"
enqueue_reload "single-reload-$RUN_ID" 'single Relay test ocserv reload' "$ARTIFACT_DIR/reload-operation.json"
operation="$(jq -er .id "$ARTIFACT_DIR/reload-operation.json")"
operation_succeeded() {
  g6rd_api_session_curl requester "/api/v1/operations/$operation" >"$ARTIFACT_DIR/reload-result.json"
  jq -e '.state == "succeeded"' "$ARTIFACT_DIR/reload-result.json" >/dev/null
}
g6rd_wait_until_deadline 90 2 'real ocserv reload result' operation_succeeded
docker exec "$NODE_CONTAINER" journalctl --no-pager -u ocserv >"$ARTIFACT_DIR/ocserv-reload.log"
grep -Eiq 'reload|SIGHUP' "$ARTIFACT_DIR/ocserv-reload.log"
echo 'Real ocserv reload command and result passed'
cp "$ARTIFACT_DIR/reload-result.json" "$ARTIFACT_DIR/initial-reload-result.json"
if [[ -n "${SINGLE_INTEGRATED_PUBLIC_IP:-}" ]]; then
  echo 'Integrated public Relay-only real Agent command passed; recovery scenarios not requested here'
  exit "$network_probe_status"
fi

# Freeze the deadline before fault injection: Agent backoff caps at 30s and
# the existing handshake timeout is bounded; 120s covers redial and telemetry.
RECOVERY_SECONDS=120
transport="$(g6rd_compose ps -q transportd)"
transport_started="$(docker inspect --format '{{.State.StartedAt}}' "$transport")"
agent_started="$(docker exec "$NODE_CONTAINER" systemctl show ocservia-agent -p ExecMainStartTimestampMonotonic --value)"
identity_before="$(docker exec "$NODE_CONTAINER" sha256sum /var/lib/ocservia-agent/identity/endpoint.key /var/lib/ocservia-agent/identity/controller.endpoint)"
approval="$(approve_reload)"
g6rd_compose stop "${RELAYS[@]}"
docker exec "$NODE_CONTAINER" python3 -c 'import socket,sys
for endpoint in sys.argv[1:]:
    host, port = endpoint.split(":")
    try:
        socket.create_connection((host,int(port)),timeout=5)
    except OSError:
        continue
    sys.exit("Relay remained reachable during the fault")' "${relay_endpoints[@]}"
key="single-pending-reload-$RUN_ID"
enqueue_reload "$key" 'single Relay outage ocserv reload' "$ARTIFACT_DIR/pending-operation.json"
operation="$(jq -er .id "$ARTIFACT_DIR/pending-operation.json")"
command_id="$(jq -er .command_id "$ARTIFACT_DIR/pending-operation.json")"
sleep 10
g6rd_api_session_curl requester "/api/v1/operations/$operation" >"$ARTIFACT_DIR/during-outage.json"
jq -e '.state != "succeeded"' "$ARTIFACT_DIR/during-outage.json" >/dev/null
restore_start=$SECONDS
g6rd_compose start "${RELAYS[@]}"
g6rd_wait_until_deadline "$RECOVERY_SECONDS" 2 'pending ocserv command after Relay recovery' operation_succeeded
printf '%s\n' "$((SECONDS - restore_start))" >"$ARTIFACT_DIR/recovery-seconds"
g6rd_probe_node_connection relay "$node" >"$ARTIFACT_DIR/recovered-session.json"
[[ "$transport_started" == "$(docker inspect --format '{{.State.StartedAt}}' "$transport")" ]]
[[ "$agent_started" == "$(docker exec "$NODE_CONTAINER" systemctl show ocservia-agent -p ExecMainStartTimestampMonotonic --value)" ]]
[[ "$identity_before" == "$(docker exec "$NODE_CONTAINER" sha256sum /var/lib/ocservia-agent/identity/endpoint.key /var/lib/ocservia-agent/identity/controller.endpoint)" ]]
docker exec "$NODE_CONTAINER" sqlite3 -readonly /var/lib/ocservia-agent/agent.db \
  "SELECT count(*),state,length(privileged_result_proof)>0 FROM command_journal WHERE hex(command_id)=upper('${command_id//-/}');" \
  >"$ARTIFACT_DIR/pending-journal.txt"
grep -Fxq '1|succeeded|1' "$ARTIFACT_DIR/pending-journal.txt"
docker exec "$NODE_CONTAINER" journalctl --no-pager -u ocserv >"$ARTIFACT_DIR/ocserv-final.log"
g6rd_api_session_curl requester "/api/v1/nodes/$node/service:reload" --fail-with-body -X POST \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $key" \
  -H "If-Match: \"revision-$revision\"" -H "X-Approval-ID: $approval" \
  --data '{"reason":"single Relay outage ocserv reload","ttl_seconds":300}' \
  >"$ARTIFACT_DIR/replayed-operation.json"
[[ "$operation" == "$(jq -er .id "$ARTIFACT_DIR/replayed-operation.json")" ]]
[[ "$command_id" == "$(jq -er .command_id "$ARTIFACT_DIR/replayed-operation.json")" ]]
sleep 3
docker exec "$NODE_CONTAINER" journalctl --no-pager -u ocserv >"$ARTIFACT_DIR/ocserv-final.log"
[[ "$(grep -c 'Reloaded ocserv.service' "$ARTIFACT_DIR/ocserv-final.log")" == 2 ]]
[[ "$(grep -c 'main: reloading configuration' "$ARTIFACT_DIR/ocserv-final.log")" == 2 ]]
echo "$chain_name pending real command recovery and stable identities passed"

# Cold start both communication processes while every configured Relay is absent.
g6rd_compose stop "${RELAYS[@]}" transportd
docker exec "$NODE_CONTAINER" systemctl stop ocservia-agent
g6rd_compose start transportd
docker exec "$NODE_CONTAINER" systemctl start ocservia-agent
sleep 10
if G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 g6rd_probe_node_connection relay "$node" \
  >"$ARTIFACT_DIR/cold-unavailable.json" 2>"$ARTIFACT_DIR/cold-unavailable.log"; then
  echo 'Cold-start probe unexpectedly reached the Agent without the Relay' >&2
  exit 1
fi
transport_started="$(docker inspect --format '{{.State.StartedAt}}' "$transport")"
agent_started="$(docker exec "$NODE_CONTAINER" systemctl show ocservia-agent -p ExecMainStartTimestampMonotonic --value)"
restore_start=$SECONDS
g6rd_compose start "${RELAYS[@]}"
G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 g6rd_wait_until_deadline "$RECOVERY_SECONDS" 2 'cold-start Relay recovery' \
  g6rd_probe_node_connection relay "$node"
printf '%s\n' "$((SECONDS - restore_start))" >"$ARTIFACT_DIR/cold-recovery-seconds"
g6rd_probe_node_connection relay "$node" >"$ARTIFACT_DIR/cold-recovered-session.json"
[[ "$transport_started" == "$(docker inspect --format '{{.State.StartedAt}}' "$transport")" ]]
[[ "$agent_started" == "$(docker exec "$NODE_CONTAINER" systemctl show ocservia-agent -p ExecMainStartTimestampMonotonic --value)" ]]
[[ "$identity_before" == "$(docker exec "$NODE_CONTAINER" sha256sum /var/lib/ocservia-agent/identity/endpoint.key /var/lib/ocservia-agent/identity/controller.endpoint)" ]]
g6rd_api_session_curl requester "/api/v1/nodes/$node" >"$ARTIFACT_DIR/final-node-read.json"
jq -e '.trust_status == "active" and .connection_state == "online" and .freshness == "fresh"' \
  "$ARTIFACT_DIR/final-node-read.json" >/dev/null
printf '%s\n' "$identity_before" >"$ARTIFACT_DIR/identity-hashes.txt"
docker exec "$NODE_CONTAINER" journalctl --no-pager -u ocservia-agent >"$ARTIFACT_DIR/final-agent.log"
if [[ -z "$RELAY_URL_B" ]] && grep -Fq 'redialing over the healthy standby' "$ARTIFACT_DIR/final-agent.log"; then
  echo 'Single Relay falsely reported an alternate-Relay failover' >&2
  exit 1
fi
echo "$chain_name cold start and automatic recovery passed without process restart"
