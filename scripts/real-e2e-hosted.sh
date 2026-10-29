#!/usr/bin/env bash
# Hosted-only coordination. Raw logs and decrypted tokens never leave RUNNER_TEMP.
set -Eeuo pipefail
umask 077
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROLE="${REAL_E2E_ROLE:?}"
[[ "$ROLE" == controller || "$ROLE" == node || "$ROLE" == relay ]]
: "${RUN_ID:?}" "${RUNNER_TEMP:?}" "${GITHUB_RUN_ID:?}" "${GITHUB_RUN_ATTEMPT:?}"
[[ "$RUN_ID" == "${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}" ]]
PRIVATE="$RUNNER_TEMP/real-e2e-private-$RUN_ID-$ROLE"
EVIDENCE="$RUNNER_TEMP/real-e2e-evidence-$ROLE"
WORK="$RUNNER_TEMP/ocservia-real-e2e-$ROLE-$RUN_ID"
mkdir -p "$PRIVATE" "$EVIDENCE"
chmod 0700 "$PRIVATE" "$EVIDENCE"
export ARTIFACT_DIR="$PRIVATE/diagnostics"

private_run() {
  local phase="$1"
  shift
  if timeout --signal=TERM --kill-after=30s 35m "$@" >"$PRIVATE/$phase.log" 2>&1; then
    echo "completed private phase: $phase"
  else
    local status=$?
    python3 - "$PRIVATE/$phase.log" "$EVIDENCE/$phase-failure.json" "$status" <<'FAILURE' || echo 'safe failure classification unavailable' >&2
import json, pathlib, re, sys
log = pathlib.Path(sys.argv[1]).read_text(errors='replace').lower()
patterns = {
    'storage': ('no space left on device', 'disk quota exceeded'),
    'memory': ('out of memory', 'cannot allocate memory', 'signal: killed', 'exit code: 137'),
    'permission': ('permission denied', 'operation not permitted'),
    'network': ('connection refused', 'connection reset', 'network is unreachable',
                'i/o timeout', 'context deadline exceeded', 'temporary failure in name resolution',
                'failed to resolve source metadata'),
}
online = {}
for value in re.findall(r'^native_online=(\{[^\n]+\})$', log, re.M):
    candidate = json.loads(value)
    if (set(candidate) == {'active', 'online', 'fresh', 'heartbeat', 'ocserv_version', 'agent_version'}
            and all(type(item) is bool for item in candidate.values())):
        online = candidate
pathlib.Path(sys.argv[2]).write_text(json.dumps({
    'exit_code': int(sys.argv[3]),
    'signals': [name for name, values in patterns.items() if any(value in log for value in values)],
    'build_targets': [name for name in ('controller-key-init', 'migrate', 'gateway', 'transportd',
                                       'transport-endpoint-bootstrap') if f'target {name}:' in log],
    'build_locations': [{'file': name, 'line': int(line)} for name, line in sorted(set(re.findall(
        r'\b(gateway\.dockerfile|transportd\.dockerfile|dockerfile):([0-9]+)\b', log)))],
    'python_locations': [{'file': name, 'line': int(line)} for name, line in re.findall(
        r'file "[^"\n]*/scripts/(real-e2e-business\.py|release-business-api\.py|real-e2e-native-business\.py)", line ([0-9]+)', log)],
    'wait_timeouts': [name for name in ('native agent online with fresh heartbeat',
                                       'cross-vm configuration plan', 'cross-vm configuration apply',
                                       'single relay owner lease invalidation', 'node offline during relay outage',
                                       'fresh agent session after relay recovery', 'same operation after relay recovery')
                      if 'timeout: ' + name in log],
    'native_online': online,
}) + '\n')
FAILURE
    echo "private phase $phase failed (exit $status); raw log withheld" >&2
    return "$status"
  fi
}

preflight() {
  [[ "$GITHUB_ACTIONS" == true && "$RUNNER_ENVIRONMENT" == github-hosted ]]
  # shellcheck source=/dev/null
  source /etc/os-release
  [[ "$ID" == ubuntu && "$VERSION_ID" == 24.04 && "$(uname -m)" == x86_64 ]]
  [[ "$(cat /proc/1/comm)" == systemd ]]
  [[ ! -e /.dockerenv && ! -e /run/.containerenv ]]
  for command in docker jq openssl curl unzip python3 ip timeout; do command -v "$command" >/dev/null; done
  systemctl show --property=Version >/dev/null
  docker info >/dev/null
  docker compose version >/dev/null
  [[ "$(git -C "$ROOT" rev-parse HEAD)" == "${GITHUB_SHA:?}" ]]
  python3 - "$EVIDENCE/environment.json" "$ROLE" <<'PY'
import json, os, pathlib, socket, subprocess, sys
p = pathlib.Path
out = {'role':sys.argv[2], 'boot_id':p('/proc/sys/kernel/random/boot_id').read_text().strip(),
       'hostname':socket.gethostname(), 'pid1':p('/proc/1/comm').read_text().strip(),
       'network_namespace':os.readlink('/proc/self/ns/net'),
       'default_routes':json.loads(subprocess.check_output(['ip','-json','route','show','default'])),
       'runner_os':'ubuntu-24.04', 'runner_environment':os.environ['RUNNER_ENVIRONMENT'],
       'runner_arch':subprocess.check_output(['uname','-m'],text=True).strip(),
       'run_id':os.environ['RUN_ID'],
       'source_commit':subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip(),
       'docker':subprocess.check_output(['docker','version','--format','{{.Server.Version}}'],text=True).strip(),
       'compose':subprocess.check_output(['docker','compose','version','--short'],text=True).strip()}
p(sys.argv[1]).write_text(json.dumps(out,indent=2)+'\n')
PY
}

case "${1:-}" in
  preflight) preflight ;;
  consume-relay)
    ip="$(python3 "$ROOT/scripts/real-e2e-tailnet.py" consume "$PRIVATE/relay-ready" "$EVIDENCE/environment.json")"
    printf '%s relay.ocservia.test\n' "$ip" | sudo tee -a /etc/hosts >/dev/null
    export REAL_E2E_RELAY_URL=https://relay.ocservia.test:3443
    REAL_E2E_RELAY_CA="$(<"$PRIVATE/relay-ready/ca.pem")"$'\n'
    export REAL_E2E_RELAY_CA
    bash "$0" configure-relay
    if [[ "$ROLE" == controller ]]; then
      python3 - "$WORK/relay/compose.json" "$ip" <<'OVERLAY'
import json, sys
services = {name: {'extra_hosts': ['relay.ocservia.test:' + sys.argv[2]]}
            for name in ('transportd', 'transport-endpoint-bootstrap')}
services['transportd']['build'] = {'target': 'real-e2e-probe'}
open(sys.argv[1], 'w').write(json.dumps({'services': services}))
OVERLAY
    fi
    ;;
  configure-relay)
    python3 "$ROOT/scripts/real-e2e-relay.py" "$WORK/relay"
    python3 - "$EVIDENCE/environment.json" "$WORK/relay/metadata.json" <<'PY'
import json, pathlib, sys
path = pathlib.Path(sys.argv[1])
item = json.loads(path.read_text())
item['relay'] = json.loads(pathlib.Path(sys.argv[2]).read_text())
path.write_text(json.dumps(item, indent=2) + '\n')
PY
    ;;
  tailnet-path)
    ip="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["relay_vm"]["ip"])' "$EVIDENCE/environment.json")"
    # Keep a compact path observation, not a full tailnet peer inventory.
    tailscale status --json >"$PRIVATE/tailscale-status.json"
    python3 - "$PRIVATE/tailscale-status.json" "$ip" "$EVIDENCE/tailnet-path.json" <<'PATHINFO'
import json, sys
state=json.load(open(sys.argv[1]))
peers=[p for p in (state.get('Peer') or {}).values() if sys.argv[2] in p.get('TailscaleIPs', [])]
# Path may not be selected until the real TLS/iroh exchange; report honestly.
json.dump({'relay_ip':sys.argv[2], 'paths':[
    {k:p.get(k) for k in ('CurAddr','Relay','PeerRelay','Active')} for p in peers]}, open(sys.argv[3],'w'))
PATHINFO
    ;;
  relay-check)
    # Test TLS with the configured trust, then an unrelated CA. Only curl's
    # certificate-verification error counts; timeouts or DNS failures do not.
    mkdir -p "$PRIVATE/empty-ca"
    private_run relay-tls curl --silent --show-error --connect-timeout 5 --max-time 20 \
      --proto '=https' --cacert "$WORK/relay/ca.pem" --capath "$PRIVATE/empty-ca" \
      --output /dev/null "$(<"$WORK/relay/url")"
    private_run wrong-ca-generate openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
      -subj /CN=unrelated-real-e2e-ca -keyout "$PRIVATE/wrong-ca.key" -out "$PRIVATE/wrong-ca.pem"
    status=0
    curl --silent --show-error --connect-timeout 5 --max-time 20 --proto '=https' \
      --cacert "$PRIVATE/wrong-ca.pem" --capath "$PRIVATE/empty-ca" \
      --output /dev/null "$(<"$WORK/relay/url")" >"$PRIVATE/wrong-ca.log" 2>&1 || status=$?
    [[ "$status" == 60 ]] || { echo 'RELAY: wrong CA did not produce a certificate rejection' >&2; exit 1; }
    private_run relay-auth "$RUNNER_TEMP/ocservia-real-e2e-target-$RUN_ID/release/ocservia-relay-network-probe" \
      "$(<"$WORK/relay/url")" "$WORK/relay/ca.pem" "$WORK/relay/token" --tcp-only
    if [[ -f "$PRIVATE/relay-ready/ready.json" ]]; then bash "$0" tailnet-path; fi
    printf 'TLS=PASS\nWRONG_CA_REJECTED=PASS\nTOKEN_AUTH=PASS\nWRONG_TOKEN_REJECTED=PASS\n' >"$EVIDENCE/relay.txt"
    ;;
  wait-download)
    # Confine even helper failure/timeout scratch archives to our private tree.
    RUNNER_TEMP="$PRIVATE" timeout --kill-after=10s 42m bash "$ROOT/scripts/real-e2e-artifact.sh" \
      wait-download "${2:?artifact name}" "$PRIVATE/${3:?destination}" 2400 "${4:?peer job}"
    ;;
  start)
    private_run start bash "$ROOT/scripts/real-e2e-controller.sh" start
    cp "$EVIDENCE/environment.json" "$WORK/outbox/controller-ready/environment.json"
    ;;
  build)
    private_run bootstrap bash "$ROOT/scripts/bootstrap.sh" native-packages
    # shellcheck source=scripts/env.sh
    source "$ROOT/scripts/env.sh"
    private_run build bash "$ROOT/scripts/real-e2e-node.sh" build
    ;;
  prepare)
    python3 "$ROOT/scripts/real-e2e-evidence.py" "$EVIDENCE/environment.json" "$PRIVATE/controller-ready/environment.json"
    private_run prepare bash "$ROOT/scripts/real-e2e-node.sh" prepare "$PRIVATE/controller-ready"
    cp "$EVIDENCE/environment.json" "$WORK/outbox/agent-endpoint/environment.json"
    ;;
  issue-token)
    python3 "$ROOT/scripts/real-e2e-evidence.py" "$EVIDENCE/environment.json" "$PRIVATE/agent-endpoint/environment.json"
    cp "$PRIVATE/agent-endpoint/environment.json" "$EVIDENCE/peer-environment.json"
    private_run issue-token bash "$ROOT/scripts/real-e2e-controller.sh" issue-token "$PRIVATE/agent-endpoint"
    mkdir -p "$PRIVATE/encrypted-token"
    openssl pkeyutl -encrypt -pubin -inkey "$PRIVATE/agent-endpoint/exchange.pub" \
      -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 \
      -in "$WORK/outbox/enrollment-token/enrollment-token" -out "$PRIVATE/encrypted-token/token.oaep"
    ;;
  enroll)
    mkdir -p "$PRIVATE/decrypted-token"
    openssl pkeyutl -decrypt -inkey "$WORK/exchange.key" \
      -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 \
      -in "$PRIVATE/encrypted-token/token.oaep" -out "$PRIVATE/decrypted-token/enrollment-token"
    private_run enroll bash "$ROOT/scripts/real-e2e-node.sh" enroll "$PRIVATE/decrypted-token"
    rm -f "$PRIVATE/decrypted-token/enrollment-token"
    ;;
  verify)
    private_run verify bash "$ROOT/scripts/real-e2e-controller.sh" verify-enrollment "$PRIVATE/enrollment-result"
    if [[ -f "$PRIVATE/relay-ready/ready.json" ]]; then bash "$0" tailnet-path; fi
    cp "$ARTIFACT_DIR/result.txt" "$EVIDENCE/enrollment.txt"
    cp "$ARTIFACT_DIR/binding.json" "$EVIDENCE/binding.json"
    docker ps --filter "label=com.docker.compose.project=$COMPOSE_PROJECT" --format '{{.ID}}' >"$PRIVATE/container-ids"
    while read -r container; do
      docker inspect --format '{{json .State.Pid}} {{json .NetworkSettings.Networks}}' "$container"
    done <"$PRIVATE/container-ids" >"$EVIDENCE/controller-process-network.txt"
    ;;
  authority)
    private_run authority bash "$ROOT/scripts/real-e2e-controller.sh" authority "$PRIVATE/agent-endpoint"
    ;;
  activate)
    private_run activate bash "$ROOT/scripts/real-e2e-node.sh" activate "$PRIVATE/business-authority"
    ;;
  register-privd)
    private_run register-privd bash "$ROOT/scripts/real-e2e-controller.sh" register-privd "$PRIVATE/business-node-ready"
    ;;
  start-native)
    private_run start-native bash "$ROOT/scripts/real-e2e-node.sh" start-native "$PRIVATE/business-authority-registered"
    ;;
  business)
    private_run business bash "$ROOT/scripts/real-e2e-controller.sh" business \
      "$PRIVATE/business-node-ready" "$PRIVATE/agent-endpoint"
    ;;
  native-result)
    private_run native-result bash "$ROOT/scripts/real-e2e-node.sh" verify-business \
      "$PRIVATE/business-controller-done"
    ;;
  verify-business)
    private_run verify-business bash "$ROOT/scripts/real-e2e-controller.sh" verify-business \
      "$PRIVATE/business-node-result"
    cp "$ARTIFACT_DIR/business.txt" "$EVIDENCE/business.txt"
    cp "$ARTIFACT_DIR/business.json" "$EVIDENCE/business.json"
    ;;
  recovery-controller-baseline|recovery-controller-queued|recovery-controller-done|recovery-controller-verify)
    phase="${1#recovery-controller-}"
    private_run "$1" bash "$ROOT/scripts/real-e2e-controller.sh" "recovery-$phase" "$PRIVATE/recovery-node-result"
    if [[ "$phase" == verify ]]; then
      cp "$ARTIFACT_DIR/recovery.txt" "$ARTIFACT_DIR/recovery.json" "$EVIDENCE/"
    else
      cp "$WORK/outbox/recovery-controller-$phase/result.json" "$EVIDENCE/recovery-$phase.json"
    fi
    ;;
  recovery-node-baseline|recovery-node-queued|recovery-node-result)
    phase="${1#recovery-node-}"
    input="$phase"
    [[ "$phase" != result ]] || input=done
    private_run "$1" bash "$ROOT/scripts/real-e2e-node.sh" "recovery-$phase" "$PRIVATE/recovery-controller-$input"
    cp "$WORK/outbox/recovery-node-$phase/result.json" "$EVIDENCE/recovery-$phase.json"
    ;;
  tailnet-cleanup)
    trap 'rm -rf -- "$PRIVATE"' EXIT
    status=0
    if command -v tailscale >/dev/null; then
      sudo tailscale logout || status=1
      tailscale status --json | jq -e '.BackendState == "NeedsLogin"' >/dev/null || status=1
      # v4.3.0 stores the sudo parent PID here; statedir defaults to memory.
      pid_file="${XDG_RUNTIME_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}}/tailscaled.pid"
      pid="$(cat "$pid_file")" || status=1
      if [[ "$pid" =~ ^[0-9]+$ ]]; then
        sudo pkill -P "$pid" || status=1
      else
        status=1
      fi
      for _ in {1..10}; do
        if ! pgrep -x tailscaled >/dev/null; then break; fi
        sleep 1
      done
      if pgrep -x tailscaled >/dev/null; then status=1; fi
      sudo tailscaled --cleanup || status=1
      rm -f -- "$pid_file" "$HOME/tailscaled.log" || status=1
    fi
    sudo sed -i '/ relay\.ocservia\.test$/d' /etc/hosts || status=1
    # cleanup may already have removed PRIVATE; this phase recreated it.
    rm -rf -- "$PRIVATE" || status=1
    ((status == 0))
    printf 'LOCAL_TAILNET_LOGOUT=PASS\nLOCAL_TAILSCALED_STOPPED=PASS\nSERVER_DEVICE_DELETION=NOT_INDEPENDENTLY_VERIFIED\n' >"$EVIDENCE/tailnet-cleanup.txt"
    ;;
  cleanup)
    trap 'rm -rf -- "$PRIVATE"' EXIT
    trap 'exit 143' TERM
    status=0
    if [[ "$ROLE" == node && -f "$WORK/native-owned" ]]; then
      # Export fixed health categories, never journal text or environment files.
      timeout --kill-after=5s 20s python3 - "$EVIDENCE/native-health.json" <<'HEALTH' || echo 'safe native health unavailable' >&2
import json, pathlib, re, subprocess, sys
health = {}
patterns = ('permission denied', 'operation not permitted', 'no such file or directory',
            'privd read-only snapshot failed', 'initial read-only snapshot collected',
            'agent session accepted', 'agent telemetry delivered', 'controller connection ended',
            'connection refused', 'timed out', 'invalid peer certificate',
            'fixed read operation failed', 'fixed local resource unavailable',
            'ocserv output format unsupported', 'controller refused agent handshake',
            'controller session grant is required', 'controller session grant response mismatch',
            'controller mutation session omitted connection fencing',
            'controller connection fence epoch is not newer')
operations = ('service_status', 'ocserv_version', 'session_list', 'ip_ban_list',
              'config_fingerprint', 'user_list', 'group_list')
for service in ('ocserv', 'ocservia-agent', 'ocservia-privd'):
    def read(*args):
        return subprocess.run(args, capture_output=True, text=True, timeout=15).stdout.lower()
    values = dict(line.split('=', 1) for line in read('systemctl', 'show', service,
                  '-p', 'ActiveState,SubState,ExecMainStatus,NRestarts').splitlines() if '=' in line)
    log = read('sudo', 'journalctl', '--no-pager', '-o', 'cat', '-n', '200', '-u', service)
    health[service] = {
        'active': values.get('activestate') == 'active',
        'running': values.get('substate') == 'running',
        'exit_status': int(values.get('execmainstatus', '-1')),
        'restarts': int(values.get('nrestarts', '0')),
        'signals': [value for value in patterns if value in log],
        'failed_reads': [name for name in operations if name + ':' in log],
    }
    if service == 'ocservia-agent':
        results = []
        codes = {'', 'privd_rejected', 'privd_receipt_missing_or_malformed', 'privd_outcome_unknown',
                 'privd_transport_unknown', 'capacity_exceeded', 'capability_rejected',
                 'outcome_requires_reconciliation', 'revision_mismatch', 'session_grant_expired',
                 'command_authorization_capability_mismatch', 'config_plan_invalid'}
        for line in log.splitlines():
            try:
                event = json.loads(line).get('fields', {})
            except (ValueError, AttributeError):
                continue
            if not isinstance(event, dict) or event.get('event_type') != 'command_delivery_result':
                continue
            if not isinstance(event.get('command_id'), str) or not re.fullmatch('[0-9a-f]{32}', event['command_id']):
                continue
            if type(event.get('state')) is not int or event['state'] not in range(5):
                continue
            code = event.get('error_code', '')
            results.append({'command_id': event['command_id'], 'state': event['state'],
                            'error_code': code if isinstance(code, str) and code in codes else 'unclassified'})
        health[service]['command_results'] = results
    if service == 'ocservia-privd':
        errors = []
        for line in log.splitlines():
            try:
                event = json.loads(line).get('fields', {})
            except (ValueError, AttributeError):
                continue
            if not isinstance(event, dict) or event.get('message') != 'privd operation failed':
                continue
            if event.get('operation') not in ('config_plan', 'config_apply', 'service_reload'):
                continue
            if type(event.get('error_kind')) is not int or event['error_kind'] not in range(9):
                continue
            code = event.get('exit_code')
            errors.append({'operation':event['operation'], 'error_kind':event['error_kind'],
                           'exit_code':code if type(code) is int else None})
        health[service]['operation_errors'] = errors
        response = read('sudo', 'sqlite3', '-readonly', '/var/lib/ocservia-privd/desired-effects.sqlite3',
                        'SELECT hex(response) FROM authorized_effects ORDER BY updated_at DESC LIMIT 8;')
        replies = b''.join(bytes.fromhex(line) for line in response.splitlines()).lower()
        health[service]['root_reply_signals'] = [value for value in (
            'fixed resource path invalid', 'fixed read operation failed', 'ocserv output format unsupported',
            'permission denied', 'operation not permitted', 'command authorization required')
            if value.encode() in replies]
pathlib.Path(sys.argv[1]).write_text(json.dumps(health, sort_keys=True) + '\n')
HEALTH
    fi
    if [[ "$ROLE" == controller && -f "$ARTIFACT_DIR/config-plan-status.json" ]]; then
      cp "$ARTIFACT_DIR/config-plan-status.json" "$EVIDENCE/config-plan-status.json" || status=1
    fi
    # An outer deadline prevents a broken Docker daemon from holding cleanup open.
    private_run cleanup bash "$ROOT/scripts/real-e2e-$ROLE.sh" cleanup || status=$?
    if [[ "$ROLE" == controller ]]; then
      remaining="$(docker ps -aq --filter "label=com.docker.compose.project=$COMPOSE_PROJECT")" || status=1
      [[ -z "$remaining" ]] || status=1
      remaining="$(docker volume ls -q --filter "label=com.docker.compose.project=$COMPOSE_PROJECT")" || status=1
      [[ -z "$remaining" ]] || status=1
      remaining="$(docker network ls -q --filter "label=com.docker.compose.project=$COMPOSE_PROJECT")" || status=1
      [[ -z "$remaining" ]] || status=1
    fi
    rm -rf -- "$PRIVATE"
    [[ ! -e "$WORK" && ! -e "$PRIVATE" ]] || status=1
    [[ "$ROLE" != node || ! -e "$RUNNER_TEMP/ocservia-real-e2e-target-$RUN_ID" ]] || status=1
    ((status == 0))
    printf 'CLEANUP=PASS\n' >"$EVIDENCE/cleanup.txt"
    ;;
  *) echo 'unknown hosted phase' >&2; exit 2 ;;
esac
