#!/usr/bin/env bash
# Disposable Relay VM. No token, private key or raw log enters an artifact.
set -Eeuo pipefail
umask 077
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
: "${RUNNER_TEMP:?}" "${RUN_ID:?}" "${GITHUB_RUN_ID:?}" "${GITHUB_RUN_ATTEMPT:?}"
[[ "$RUN_ID" == "$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT" && "$RUN_ID" =~ ^[0-9]+-[0-9]+$ ]]
WORK="$RUNNER_TEMP/real-e2e-relay-$RUN_ID"
EVIDENCE="$RUNNER_TEMP/real-e2e-evidence-relay"
NAME="real-e2e-relay-$RUN_ID"
mkdir -p "$WORK" "$EVIDENCE"

restore_relay() {
  [[ -f "$WORK/stop-requested" ]] || return 0
  [[ "$(docker inspect --format '{{.Id}}' "$NAME")" == "$(<"$WORK/container-id")" ]] || return 1
  docker start "$NAME" >"$WORK/restore.log" || return 1
  local ip
  ip="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["ip"])' "$WORK/ready/ready.json")" || return 1
  for _ in {1..20}; do
    if curl --fail --silent --show-error --noproxy '*' --connect-timeout 2 --max-time 4 \
      --cacert "$WORK/ready/ca.pem" --resolve "relay.ocservia.test:3443:$ip" \
      https://relay.ocservia.test:3443/ >"$WORK/health.log" 2>&1; then break; fi
    sleep 1
  done
  [[ "$(docker inspect --format '{{.State.Running}}' "$NAME")" == true ]] || return 1
  curl --fail --silent --show-error --noproxy '*' --connect-timeout 3 --max-time 5 \
    --cacert "$WORK/ready/ca.pem" --resolve "relay.ocservia.test:3443:$ip" \
    https://relay.ocservia.test:3443/ >"$WORK/health.log" 2>&1 || return 1
  mkdir -p "$WORK/restored" || return 1
  python3 - "$WORK/container-id" "$WORK/restored/result.json" <<'RESTORED' || return 1
import json,pathlib,re,sys
container=pathlib.Path(sys.argv[1]).read_text().strip()
assert re.fullmatch('[0-9a-f]{64}',container)
pathlib.Path(sys.argv[2]).write_text(json.dumps({'container_id':container,'running':True,'reachable':True})+'\n')
RESTORED
  cp "$WORK/restored/result.json" "$EVIDENCE/recovery-restored.json" || return 1
}

case "${1:-}" in
  start)
    ip="$(tailscale ip -4)"
    python3 - "$ip" <<'PY'
import ipaddress, sys
assert ipaddress.ip_address(sys.argv[1]) in ipaddress.ip_network('100.64.0.0/10')
PY
    mkdir -p "$WORK/secrets" "$WORK/ready"
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=ocservia-ci-ca \
      -addext basicConstraints=critical,CA:TRUE -keyout "$WORK/ca.key" -out "$WORK/ready/ca.pem" >"$WORK/tls.log" 2>&1
    openssl req -new -newkey rsa:2048 -nodes -subj /CN=relay.ocservia.test \
      -keyout "$WORK/secrets/relay.key" -out "$WORK/relay.csr" >>"$WORK/tls.log" 2>&1
    printf 'basicConstraints=critical,CA:FALSE\nsubjectAltName=DNS:relay.ocservia.test\nextendedKeyUsage=serverAuth\n' >"$WORK/leaf.ext"
    openssl x509 -req -days 1 -in "$WORK/relay.csr" -CA "$WORK/ready/ca.pem" \
      -CAkey "$WORK/ca.key" -CAserial "$WORK/ca.srl" -CAcreateserial -extfile "$WORK/leaf.ext" \
      -out "$WORK/secrets/relay.crt" >>"$WORK/tls.log" 2>&1
    printf '%s' "${REAL_E2E_RELAY_TOKEN:?}" >"$WORK/secrets/relay-token"
    unset REAL_E2E_RELAY_TOKEN
    sudo chown 65532:65532 "$WORK/secrets/relay.key" "$WORK/secrets/relay-token"
    chmod 0755 "$WORK/secrets"
    chmod 0444 "$WORK/secrets/relay.crt"
    sed 's/enable_quic_addr_discovery = true/enable_quic_addr_discovery = false/' \
      "$ROOT/deploy/test-fixtures/relay.toml" >"$WORK/relay.toml"
    chmod 0444 "$WORK/relay.toml"
    timeout --kill-after=30s 30m docker build -f "$ROOT/deploy/production/relay.Dockerfile" \
      -t "$NAME" "$ROOT" >"$WORK/build.log" 2>&1
    docker run -d --name "$NAME" --read-only --cap-drop ALL --security-opt no-new-privileges \
      --tmpfs /tmp:rw,noexec,nosuid,size=16m --publish "$ip:3443:3443/tcp" \
      --mount "type=bind,src=$WORK/secrets,dst=/run/relay-secrets,readonly" \
      --mount "type=bind,src=$WORK/relay.toml,dst=/etc/iroh-relay/relay.toml,readonly" \
      -e IROH_RELAY_ACCESS_TOKEN_FILE=/run/relay-secrets/relay-token \
      "$NAME" --config-path /etc/iroh-relay/relay.toml >"$WORK/container-id"
    for _ in {1..30}; do
      if curl --fail --silent --show-error --noproxy '*' --connect-timeout 2 --max-time 4 \
        --cacert "$WORK/ready/ca.pem" --resolve "relay.ocservia.test:3443:$ip" \
        https://relay.ocservia.test:3443/ >"$WORK/health.log" 2>&1; then break; fi
      sleep 2
    done
    curl --fail --silent --show-error --noproxy '*' --connect-timeout 5 --max-time 10 \
      --cacert "$WORK/ready/ca.pem" --resolve "relay.ocservia.test:3443:$ip" \
      https://relay.ocservia.test:3443/ >"$WORK/health.log" 2>&1
    python3 - "$WORK/ready" "$EVIDENCE/environment.json" "$ip" <<'PY'
import hashlib, json, os, pathlib, sys
p = pathlib.Path(sys.argv[1])
env = json.loads(pathlib.Path(sys.argv[2]).read_text())
item = dict(url='https://relay.ocservia.test:3443', ip=sys.argv[3],
            ca_sha256=hashlib.sha256((p/'ca.pem').read_bytes()).hexdigest(),
            source_commit=env['source_commit'], boot_id=env['boot_id'],
            run_id=os.environ['GITHUB_RUN_ID'], attempt=os.environ['GITHUB_RUN_ATTEMPT'])
(p/'ready.json').write_text(json.dumps(item)+'\n')
PY
    cp "$WORK/ready/ready.json" "$EVIDENCE/relay-ready.json"
    ;;
  stop)
    [[ "$(docker inspect --format '{{.State.Running}}' "$NAME")" == true ]]
    touch "$WORK/stop-requested"
    docker stop "$NAME" >"$WORK/stop.log"
    [[ "$(docker inspect --format '{{.State.Running}}' "$NAME")" == false ]]
    ip="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["ip"])' "$WORK/ready/ready.json")"
    status=0
    curl --fail --silent --show-error --noproxy '*' --connect-timeout 3 --max-time 5 \
      --cacert "$WORK/ready/ca.pem" --resolve "relay.ocservia.test:3443:$ip" \
      https://relay.ocservia.test:3443/ >"$WORK/health.log" 2>&1 || status=$?
    [[ "$status" == 7 || "$status" == 28 ]]
    mkdir -p "$WORK/stopped"
    python3 - "$WORK/container-id" "$WORK/stopped/result.json" <<'STOPPED'
import json,pathlib,re,sys
container=pathlib.Path(sys.argv[1]).read_text().strip()
assert re.fullmatch('[0-9a-f]{64}',container)
pathlib.Path(sys.argv[2]).write_text(json.dumps({'container_id':container,'running':False,'reachable':False})+'\n')
STOPPED
    cp "$WORK/stopped/result.json" "$EVIDENCE/recovery-stopped.json"
    ;;
  restore)
    [[ -f "$WORK/stop-requested" ]]
    restore_relay
    ;;
  hold)
    # Wait for BOTH full jobs (including their cleanup and Action post steps).
    deadline=$((SECONDS + 6000))
    while ((SECONDS < deadline)); do
      [[ "$(docker inspect --format '{{.State.Running}}' "$NAME")" == true ]]
      curl --fail --silent --show-error --connect-timeout 5 --max-time 20 \
        -H "Authorization: Bearer ${GITHUB_TOKEN:?}" -H 'Accept: application/vnd.github+json' \
        "${GITHUB_API_URL:?}/repos/${GITHUB_REPOSITORY:?}/actions/runs/$GITHUB_RUN_ID/attempts/$GITHUB_RUN_ATTEMPT/jobs?per_page=100" >"$WORK/jobs.json"
      ip="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["ip"])' "$WORK/ready/ready.json")"
      curl --fail --silent --show-error --noproxy '*' --connect-timeout 5 --max-time 10 \
        --cacert "$WORK/ready/ca.pem" --resolve "relay.ocservia.test:3443:$ip" \
        https://relay.ocservia.test:3443/ >"$WORK/health.log" 2>&1
      state="$(python3 - "$WORK/jobs.json" <<'PY'
import json,sys
jobs=json.load(open(sys.argv[1]))['jobs']
peers=[]
for name in ('Cross-VM Controller','Cross-VM Agent'):
 matches=[j for j in jobs if j['name']==name or j['name'].endswith(' / '+name)]
 if len(matches)!=1: raise ValueError('missing or ambiguous peer')
 peers.extend(matches)
print('running' if any(j['status']!='completed' for j in peers) else
      'success' if all(j['conclusion']=='success' for j in peers) else 'failure')
PY
)"
      if [[ "$state" != running ]]; then
        [[ "$state" == success ]]
        printf 'RELAY_ALIVE_UNTIL_PEERS_COMPLETED=PASS\n' >"$EVIDENCE/lifetime.txt"
        exit 0
      fi
      sleep 10
    done
    echo 'Relay peer completion deadline exceeded' >&2
    exit 1
    ;;
  cleanup)
    status=0
    # A failed/cancelled recovery restores the same Relay before resource removal.
    if [[ -f "$WORK/stop-requested" ]]; then restore_relay || status=1; fi
    # Do not hide daemon/API errors as an absent container/image.
    containers="$(docker ps -aq --filter "name=^/$NAME$")" || status=1
    if [[ -n "$containers" ]]; then docker rm -f "$NAME" >/dev/null || status=1; fi
    images="$(docker image ls -q "$NAME")" || status=1
    if [[ -n "$images" ]]; then docker image rm "$NAME" >/dev/null || status=1; fi
    remaining="$(docker ps -aq --filter "name=^/$NAME$")" || status=1
    [[ -z "$remaining" ]] || status=1
    rm -rf -- "$WORK" || status=1
    [[ ! -e "$WORK" ]] || status=1
    ((status == 0))
    printf 'CLEANUP=PASS\n' >"$EVIDENCE/cleanup.txt"
    ;;
  *) exit 2 ;;
esac
