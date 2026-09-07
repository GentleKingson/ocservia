#!/usr/bin/env bash
# One real API/Controller/Iroh/Agent chain, not a formal G6 verdict.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export RUN_ID="${RUN_ID:?use a new run identity}" RUNNER_TEMP="${RUNNER_TEMP:?}"
export ARTIFACT_DIR="$RUNNER_TEMP/artifacts/$RUN_ID"
[[ ! -e "$RUNNER_TEMP/g6-readiness-$RUN_ID" ]] || { echo 'run identity already used' >&2; exit 2; }
export FD_ID=fd-a FD_ALIAS=fd-alpha G6_AUTHORITY=engineering G6_AGENTS_A=1
G6RD_CANDIDATE_SHA="$(git -C "$ROOT" rev-parse HEAD)"
export G6RD_CANDIDATE_SHA
export G6RD_CONTROL_PLANE_IMAGE=ocservia-relay-recovery:control
export G6RD_TRANSPORTD_IMAGE=ocservia-relay-recovery:transportd
export G6RD_AGENT_IMAGE=ocservia-relay-recovery:agent
export G6RD_PROBE_IMAGE=ocservia-relay-recovery:probe
export G6RD_RELAY_IMAGE=ocservia-relay-recovery:relay
# Reuse the real enrollment, approval, roles and Agent principal split, but
# no HA/promotion/load/window phases or formal authority claims.
eval "$(sed '/^case "${1:-}" in/,$d' "$ROOT/scripts/g6-readiness-fd-a.sh")"
source "$ROOT/scripts/g6-relay-diagnostics.sh"
for function in capture_relay_dispatch_proof capture_successful_command_proof wait_commands_settled; do
  eval "$(sed -n "/^${function}() {/,/^}/p" "$ROOT/scripts/g6-readiness-fd-b.sh")"
done
g6rd_relay_url_a() { printf 'https://relay-b:3443/\n'; }
g6rd_relay_url_b() { printf 'https://relay-a:3443/\n'; }
export G6_LOCAL_RELAY_HOST=relay-b G6_REMOTE_RELAY_HOST=relay-a
OVERRIDE="$G6RD_WORK/experiment.yaml"
AGENT_OVERRIDE="$G6RD_WORK/agent-network.yaml"
EVIDENCE="$ARTIFACT_DIR/relay-recovery"
mkdir -p "$EVIDENCE" "$G6RD_WORK/relay-loss"
chmod 0777 "$G6RD_WORK/relay-loss"
g6rd_compose() {
  timeout --signal=TERM --kill-after=5s "${G6RD_COMPOSE_TIMEOUT_SECONDS:-120}s" \
    docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" -f "$G6RD_RELEASE_COMPOSE" -f "$OVERRIDE" "$@"
}
g6rd_agent_compose() {
  timeout --signal=TERM --kill-after=5s "${G6RD_COMPOSE_TIMEOUT_SECONDS:-120}s" \
    docker compose -p "$COMPOSE_PROJECT" -f "$COMPOSE_FILE" -f "$G6RD_RELEASE_COMPOSE" \
    -f "$OVERRIDE" -f "$G6RD_AGENT_COMPOSE" -f "$AGENT_OVERRIDE" "$@"
}
psql_primary_probe() { G6_DB_PORT=5432 G6RD_PSQL_TIMEOUT_SECONDS=10 g6rd_psql "$@"; }
relay_probe_relay_b() { G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 g6rd_probe_node_connection relay "$1" > "$2"; }
finish() {
  local status=$?
  trap - EXIT
  set +e
  printf '%s\n' "$status" > "$EVIDENCE/exit-status"
  g6rd_compose logs --no-color --no-log-prefix transportd > "$EVIDENCE/transportd.jsonl" 2> "$EVIDENCE/transport-log-error"
  g6rd_compose logs --no-color worker > "$EVIDENCE/worker.log" 2>&1
  g6rd_agent_compose logs --no-color agent-fd-a-01 > "$EVIDENCE/agent.log" 2>&1
  cp "$G6RD_WORK/relay-loss/target.dropped" "$EVIDENCE/" 2>/dev/null
  if [[ -n "${node:-}" && -n "${key:-}" ]]; then
    capture_relay_agent_exit_diagnostics "$key" agent-fd-a-01 "$EVIDENCE/agent-exit-diagnostics"
    capture_relay_command_snapshot "$key" "$node" "$EVIDENCE/final-database.jsonl" 2> "$EVIDENCE/final-database-error"
    g6rd_probe_node_connection relay "$node" > "$EVIDENCE/final-session.json" 2> "$EVIDENCE/final-session-error"
  fi
  g6rd_agent_compose down --volumes --remove-orphans > "$EVIDENCE/cleanup.log" 2>&1
  cleanup_status=$?
  [[ "$status" != 0 || "$cleanup_status" == 0 ]] || status=$cleanup_status
  exit "$status"
}
trap finish EXIT
g6rd_generate_secrets
g6rd_export_common_env
g6rd_prepare_release_images
cat > "$OVERRIDE" <<EOF
services:
  relay:
    networks:
      default:
        aliases: [relay-b]
      agent-only:
        aliases: [relay-b]
  transport-endpoint-bootstrap:
    extra_hosts: !reset []
  transportd:
    extra_hosts: !reset []
    environment:
      OCSERVIA_TEST_DROP_RESULT_TARGET: /run/relay-loss/target
    volumes:
      - $G6RD_WORK/relay-loss:/run/relay-loss
  g6-probe:
    extra_hosts: !reset []
networks:
  agent-only:
    internal: true
EOF
cat > "$AGENT_OVERRIDE" <<'EOF'
services:
  agent-fd-a-01:
    extra_hosts: !reset []
    networks: !override [default]
EOF
git -C "$ROOT" diff --binary > "$EVIDENCE/source.patch"
tar -C "$ROOT" -czf "$EVIDENCE/source-inputs.tar.gz" \
  rust/crates/agent/src/main.rs rust/crates/transportd/src/lib.rs rust/crates/transportd/Cargo.toml \
  control-plane/internal/operations/service.go scripts/g6-relay-diagnostics.sh \
  scripts/g6-readiness-fd-a.sh scripts/g6-readiness-fd-b.sh scripts/relay-recovery-experiment.sh \
  scripts/g6-relay-proof.mjs scripts/build-g6-evidence.mjs scripts/test-g6-relay-proof.mjs \
  scripts/relay-recovery-build.sh scripts/verify-relay-recovery-experiment.mjs \
  deploy/g6-readiness/relay-recovery.Dockerfile
git -C "$ROOT" rev-parse HEAD 'HEAD^{tree}' > "$EVIDENCE/base-identity"
phase_primary_up
# Enrollment (unlike the later mutation session) still uses public discovery.
# Retry only its pre-connection lookup failure, never a command or fault case.
for setup_attempt in 1 2 3; do
  set +e
  (set -e; phase_agents_enroll) > "$EVIDENCE/enrollment-$setup_attempt.log" 2>&1
  setup_status=$?
  set -e
  cat "$EVIDENCE/enrollment-$setup_attempt.log"
  [[ "$setup_status" != 0 ]] || break
  if [[ "$setup_attempt" == 3 ]] || ! grep -q '^Error: No addressing information available$' "$EVIDENCE/enrollment-$setup_attempt.log"; then
    exit "$setup_status"
  fi
  sleep 5
done
g6rd_compose restart transportd
cat > "$AGENT_OVERRIDE" <<'EOF'
services:
  agent-fd-a-01:
    extra_hosts: !reset []
    networks: !override [agent-only]
EOF
phase_agents_start
NODES_FILE="$G6RD_OUTBOX/agents/nodes.tsv"
node="$(awk 'NR==1 {print $2}' "$NODES_FILE")"
g6rd_wait_until_deadline 60 2 'one real relay-b session' g6rd_probe_node_connection relay "$node"
g6rd_release_synthetic_barriers
for scenario in control loss; do
  dir="$EVIDENCE/$scenario"
  mkdir -p "$dir"
  key="relay-recovery-$scenario-$RUN_ID"
  g6rd_probe_node_connection relay "$node" > "$dir/session-before.json"
  if [[ "$scenario" == loss ]]; then g6rd_arm_synthetic_barriers; fi
  g6rd_enqueue_command "$node" "$key" "$dir/enqueue.jsonl"
  command_id="$(psql_primary_probe -Atc "SELECT id FROM commands WHERE node_id='$node' AND idempotency_key='$key'")"
  if [[ "$scenario" == loss ]]; then
    printf '%s\n' "${command_id//-/}" > "$G6RD_WORK/relay-loss/target"
    chmod 0644 "$G6RD_WORK/relay-loss/target"
    g6rd_release_synthetic_barriers
  fi
  g6rd_wait_until_deadline 90 1 'real result converged' \
    relay_settled_with_snapshot "$key" "$node" "$dir/database.jsonl"
  capture_successful_command_proof "$key" "$node" "$dir/durable-proof.json"
  g6rd_probe_node_connection relay "$node" > "$dir/session-after.json"
  g6rd_compose logs --no-color --no-log-prefix transportd \
    | jq -Rc --arg command "${command_id//-/}" 'fromjson? | select(.fields.command_id == $command)' > "$dir/transport-events.jsonl"
  g6rd_agent_journal_query agent-fd-a-01 \
    "SELECT json_object('command_id',hex(j.command_id),'idempotency_key',hex(j.idempotency_key),'state',j.state,'effect_executed_at',e.executed_at,'total_executions',(SELECT executions FROM synthetic_effect_counter WHERE singleton=1)) FROM command_journal j LEFT JOIN synthetic_effects e ON e.idempotency_key=j.idempotency_key WHERE j.command_id=X'${command_id//-/}'" > "$dir/journal.jsonl"
  capture_relay_agent_proof "${command_id//-/}" agent-fd-a-01 "$dir/agent-proof.json"
  # Exercise the same runtime proof and remote-journal check as formal G6.
  if capture_relay_dispatch_proof "$command_id" "$node" "$dir/session-before.json" "$dir/existing-gate-proof.json" relay-b; then
    printf 'accepted\n' > "$dir/existing-gate-result"
    node "$ROOT/scripts/g6-relay-proof.mjs" "$dir/existing-gate-proof.json" "$dir/session-before.json" "$dir/agent-proof.json"
  else
    printf 'rejected\n' > "$dir/existing-gate-result"
    capture_relay_exit_diagnostics 1 "$key" "$node" "$dir/session-before.json"
    cp -R "$G6RD_STATE/relay-diagnostics" "$dir/failure-diagnostics"
  fi
done
g6rd_compose logs --no-color worker > "$EVIDENCE/worker-live.log"
g6rd_agent_compose logs --no-color --no-log-prefix agent-fd-a-01 > "$EVIDENCE/agent-live.log"
node "$ROOT/scripts/verify-relay-recovery-experiment.mjs" "$EVIDENCE"
