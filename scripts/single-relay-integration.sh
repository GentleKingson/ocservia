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
g6rd_compose() {
  timeout --signal=TERM --kill-after=5s "${G6RD_COMPOSE_TIMEOUT_SECONDS:-120}s" docker compose -p "$COMPOSE_PROJECT" \
    -f "$COMPOSE_FILE" -f "$G6RD_RELEASE_COMPOSE" -f "$OVERRIDE" "$@"
}
finish() {
  local status=$?
  trap - EXIT
  set +e
  printf '%s\n' "$status" >"$ARTIFACT_DIR/exit-status"
  for role in transportd worker api relay; do
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
      OCSERV_RELAY_URL_B: ""
    networks: !override [application, observability, relay-egress]
    extra_hosts: !override ["relay-a:$GATEWAY"]
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
phase_primary_up
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
  --network "$NODE_NETWORK" --add-host "relay-a:$GATEWAY" \
  --tmpfs /run --tmpfs /run/lock \
  -v "$ROOT:/source:ro" -v "$G6RD_SECRETS:/test-secrets:ro" \
  -v "$(dirname "$SINGLE_AGENT_ARCHIVE"):/payload:ro" \
  -v "$SINGLE_AGENT_PUBLIC_KEY:/test-release-key.pem:ro" \
  "$SINGLE_NODE_IMAGE" /sbin/init >/dev/null
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
    # that installed unit; a test-only drop-in selects one Relay and its CA.
    [[ "$(/usr/libexec/ocservia/ocservia-agent --version)" == "ocservia-agent 0.6.0" ]]
    sed -e '\''s/ --relay-url \$RELAY_URL_B//'\'' \
      -e '\''/^ExecStart=\/usr\/libexec\/ocservia\/ocservia-agent / s|$| --relay-ca-file /etc/ocservia-agent/test-relay-ca.pem|'\'' \
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
  --controller "$OCSERV_CONTROLLER_ENDPOINT_ID" --relay-mode custom --relay-url "$RELAY_URL" \
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
RELAY_URL_B=
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
python3 - "$ARTIFACT_DIR" "$RELAY_URL" <<'PY'
import json
from pathlib import Path
import sys
for role in ('transport', 'agent'):
    argv = (Path(sys.argv[1]) / f'{role}-argv.nul').read_bytes().rstrip(b'\0').decode().split('\0')
    assert argv.count('--relay-url') == 1
    assert argv[argv.index('--relay-url') + 1] == sys.argv[2]
    assert argv[argv.index('--relay-mode') + 1] == 'custom'
    (Path(sys.argv[1]) / f'{role}-argv.json').write_text(json.dumps(argv, indent=2) + '\n')
PY
echo 'Single-relay enrollment, independent approval and real Agent session passed'

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
approval="$(approve_reload)"
revision="$(g6rd_node_revision "$node")"
g6rd_api_session_curl requester "/api/v1/nodes/$node/service:reload" --fail-with-body -X POST \
  -H 'Content-Type: application/json' -H "Idempotency-Key: single-reload-$RUN_ID" \
  -H "If-Match: \"revision-$revision\"" -H "X-Approval-ID: $approval" \
  --data '{"reason":"single Relay test ocserv reload","ttl_seconds":300}' \
  >"$ARTIFACT_DIR/reload-operation.json"
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

# Freeze the deadline before fault injection: Agent backoff caps at 30s and
# the existing handshake timeout is bounded; 120s covers redial and telemetry.
RECOVERY_SECONDS=120
transport="$(g6rd_compose ps -q transportd)"
transport_started="$(docker inspect --format '{{.State.StartedAt}}' "$transport")"
agent_started="$(docker exec "$NODE_CONTAINER" systemctl show ocservia-agent -p ExecMainStartTimestampMonotonic --value)"
identity_before="$(docker exec "$NODE_CONTAINER" sha256sum /var/lib/ocservia-agent/identity/endpoint.key /var/lib/ocservia-agent/identity/controller.endpoint)"
approval="$(approve_reload)"
revision="$(g6rd_node_revision "$node")"
g6rd_compose stop relay
docker exec "$NODE_CONTAINER" python3 -c 'import socket,sys
try:
    socket.create_connection(("relay-a",23443),timeout=5)
except OSError:
    sys.exit(0)
sys.exit("Relay remained reachable during the fault")'
key="single-pending-reload-$RUN_ID"
g6rd_api_session_curl requester "/api/v1/nodes/$node/service:reload" --fail-with-body -X POST \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $key" \
  -H "If-Match: \"revision-$revision\"" -H "X-Approval-ID: $approval" \
  --data '{"reason":"single Relay outage ocserv reload","ttl_seconds":300}' \
  >"$ARTIFACT_DIR/pending-operation.json"
operation="$(jq -er .id "$ARTIFACT_DIR/pending-operation.json")"
command_id="$(jq -er .command_id "$ARTIFACT_DIR/pending-operation.json")"
sleep 10
g6rd_api_session_curl requester "/api/v1/operations/$operation" >"$ARTIFACT_DIR/during-outage.json"
jq -e '.state != "succeeded"' "$ARTIFACT_DIR/during-outage.json" >/dev/null
restore_start=$SECONDS
g6rd_compose start relay
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
echo 'Single-relay pending real command recovery and stable identities passed'

# Cold start both communication processes while their sole Relay is absent.
g6rd_compose stop relay transportd
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
g6rd_compose start relay
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
if grep -Fq 'redialing over the healthy standby' "$ARTIFACT_DIR/final-agent.log"; then
  echo 'Single Relay falsely reported an alternate-Relay failover' >&2
  exit 1
fi
echo 'Single-relay cold start and automatic recovery passed without process restart'
