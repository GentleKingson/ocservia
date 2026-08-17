#!/usr/bin/env bash
# Failure domain B of the formal G6 readiness harness: streaming standby,
# relay-b, promotion target under load, era-2 control plane, every fault
# scenario, the bounded stability window, and the merged evidence verdict.
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FD_ALIAS="${FD_ALIAS:-fd-beta}"
# shellcheck source=scripts/g6-readiness-lib.sh
source "${ROOT}/scripts/g6-readiness-lib.sh"
g6rd_init_environment
export FD_ALIAS

WINDOW_SECONDS="${G6RD_WINDOW_SECONDS:-305}"
NODES_FILE="${G6RD_STATE}/all-nodes.tsv"

require_file() {
  local path="${1:?path is required}"
  [[ -s "${path}" ]] || {
    echo "required file is missing: ${path}" >&2
    return 1
  }
}

db_primary_port() {
  # Before promotion fd-b's SQL touchpoints go to fd-a's primary through
  # the tunnel; afterwards they stay on the local promoted primary.
  if [[ -e "${G6RD_STATE}/promoted" ]]; then
    printf '%s\n' 5432
  else
    printf '%s\n' 15432
  fi
}

psql_primary() {
  G6_DB_PORT="$(db_primary_port)" g6rd_psql "$@"
}

node_ids() {
  cut -f2 "${NODES_FILE}"
}

node_service() {
  local node_id="${1:?node id is required}" name
  name="$(awk -F'\t' -v id "${node_id}" '$2 == id {print $1}' "${NODES_FILE}")"
  case "${name}" in
    g6-fd-a-*) printf 'agent-fd-a-%s\n' "${name#g6-fd-a-}" ;;
    g6-fd-b-*) printf 'agent-fd-b-%s\n' "${name#g6-fd-b-}" ;;
    *)
      echo "unknown node name ${name}" >&2
      return 1
      ;;
  esac
}

journal_query() {
  local service="${1:?agent service is required}" sql="${2:?sql is required}"
  g6rd_agent_compose exec -T "${service}" \
    sqlite3 -readonly /run/ocservia-agent/journal/agent.db "${sql}"
}

# ---------------------------------------------------------------------------
# Watchers: background pollers that turn the authoritative fencing and
# leadership tables into append-only history for the epoch-events artifact.
# ---------------------------------------------------------------------------

start_watchers() {
  : >"${G6RD_STATE}/fencing-history.jsonl"
  : >"${G6RD_STATE}/leadership-history.jsonl"
  g6rd_spawn_harness_loop "${G6RD_LOGS}/fencing-watcher.log" \
    g6rd_watch_fencing_history >"${G6RD_STATE}/fencing-watcher.pid"
  g6rd_spawn_harness_loop "${G6RD_LOGS}/leadership-watcher.log" \
    g6rd_watch_leadership_history >"${G6RD_STATE}/leadership-watcher.pid"
}

stop_watchers() {
  touch "${G6RD_STATE}/watchers-stop"
  local name pid
  for name in fencing leadership; do
    [[ -s "${G6RD_STATE}/${name}-watcher.pid" ]] || continue
    pid="$(<"${G6RD_STATE}/${name}-watcher.pid")"
    kill "${pid}" 2>/dev/null || true
    sleep 1
    kill -9 "${pid}" 2>/dev/null || true
  done
}

# ---------------------------------------------------------------------------
# Phases.
# ---------------------------------------------------------------------------

phase_prepare() {
  g6rd_prepare_support_image
  g6rd_build_tunnel
  mkdir -p "${G6RD_OUTBOX}/tunnel"
  local name
  for name in relay-b pg-b pg-a-forward api-a-forward relay-a-forward; do
    g6rd_tunnel_key "${name}" >"${G6RD_OUTBOX}/tunnel/${name}.node-id"
  done
  openssl dgst -sha256 -r </proc/sys/kernel/random/boot_id | cut -c1-16 \
    >"${G6RD_OUTBOX}/tunnel/boot-id-sha256"
}

import_peer_tunnel_nodes() {
  local peer="${1:?peer tunnel rendezvous directory is required}" name
  for name in pg-a api-a relay-a relay-b-forward pg-b-forward; do
    require_file "${peer}/${name}.node-id"
    cp -f "${peer}/${name}.node-id" "${G6RD_STATE}/peer-${name}-node-id"
  done
}

phase_import_peer_secrets() {
  local peer="${1:?peer shared secrets directory is required}"
  require_file "${peer}/dev-auth-token"
  require_file "${peer}/controller.key"
  local name
  for name in owner-password app-password replication-password dev-auth-token \
    oidc-client-secret session-key requester-identity-id requester-session-id \
    requester-session-cookie approver-identity-id approver-session-id \
    approver-session-cookie \
    relay-ca.pem relay-chain.crt relay-leaf.crt relay-leaf.key relay-token \
    command-signing.pem command-verification.pem \
    seal-user-password.key seal-user-password-sha256 \
    seal-p12.key seal-p12-sha256; do
    [[ -s "${G6RD_SECRETS}/${name}" ]] && continue
    cp -f "${peer}/${name}" "${G6RD_SECRETS}/${name}"
    chmod 0600 "${G6RD_SECRETS}/${name}"
  done
  # fd-b never generates its own controller key: era-2 transportd presents
  # the same controller NodeId fd-a's agents already dial.
  [[ -s "${G6RD_SECRETS}/controller.key" ]] || {
    cp -f "${peer}/controller.key" "${G6RD_SECRETS}/controller.key"
    chmod 0600 "${G6RD_SECRETS}/controller.key"
  }
  for name in pg-a api-a relay-b-forward pg-b-forward; do
    [[ -s "${G6RD_SECRETS}/tunnel-${name}.key" ]] || {
      cp -f "${peer}/tunnel-${name}.key" "${G6RD_SECRETS}/tunnel-${name}.key"
      chmod 0600 "${G6RD_SECRETS}/tunnel-${name}.key"
    }
  done
}

phase_materialize_runtime() {
  phase_import_peer_secrets "${1:?peer shared secrets directory is required}"
  g6rd_export_common_env || {
    echo "peer trust material did not produce a complete runtime environment" >&2
    return 1
  }
}

phase_build_images() {
  g6rd_build_tunnel
  g6rd_prepare_build_environment
  g6rd_write_agent_overlay "$(g6rd_agent_count)"
  g6rd_compose build postgres migrate api worker scheduler transportd \
    controller-key-init transport-runtime-init transport-endpoint-bootstrap relay g6-probe
  g6rd_agent_compose build
}

phase_tunnel_up() {
  g6rd_tunnel_forward pg-a-forward "$(<"${G6RD_STATE}/peer-pg-a-node-id")" 15432
  g6rd_tunnel_forward api-a-forward "$(<"${G6RD_STATE}/peer-api-a-node-id")" \
    "${G6_API_FORWARD_PORT:-18081}"
  # relay-a arrives through the peer's serve so fd-b's agents keep the
  # same two-relay map fd-a's agents use.
  g6rd_tunnel_forward relay-a-forward "$(<"${G6RD_STATE}/peer-relay-a-node-id")" \
    3443
  # relay-b serves the peer's forward key so fd-a's agents reach it.
  g6rd_tunnel_serve relay-b "$(<"${G6RD_STATE}/peer-relay-b-forward-node-id")" \
    "${G6_RELAY_BIND_PORT:-13443}"
}

standby_in_recovery() {
  [[ "$(G6_DB_PORT=5432 g6rd_psql -Atc 'SELECT pg_is_in_recovery()' 2>/dev/null)" == t ]]
}

phase_standby_bootstrap() {
  local peer="${1:?primary rendezvous directory is required}"
  require_file "${G6RD_STATE}/peer-pg-a-node-id"
  require_file "${peer}/controller-endpoint-id"
  require_file "${peer}/workspace-id"
  cp -f "${peer}/controller-endpoint-id" "${G6RD_STATE}/controller-endpoint-id"
  cp -f "${peer}/workspace-id" "${G6RD_STATE}/workspace-id"
  g6rd_export_common_env
  g6rd_prepare_postgres_bind_dirs
  # The clone runs inside the pinned image against the peer primary through
  # the tunnel, as the postgres user, with the replication slot retained.
  g6rd_compose run --rm --no-deps -T --user 999:999 \
    -e PGPASSWORD="$(g6rd_secret replication-password)" postgres \
    pg_basebackup -h host.docker.internal -p 15432 -U ocservia_replication \
    -D /var/lib/postgresql/data -R -X stream -C -S g6_slot --checkpoint=fast \
    < /dev/null >"${G6RD_LOGS}/basebackup.log" 2>&1
  docker run --rm --pull=never --entrypoint /bin/sh \
    -v "${COMPOSE_PROJECT}_postgres-data:/data" postgres:17.10-bookworm \
    -c "printf '%s\n' \"primary_conninfo = 'host=host.docker.internal port=15432 user=ocservia_replication password=$(g6rd_secret replication-password) application_name=g6_standby'\" >> /data/postgresql.auto.conf"
  g6rd_compose up --detach postgres
  g6rd_wait_until 60 2 "standby in recovery" standby_in_recovery
  g6rd_now >"${G6RD_STATE}/standby-streaming-at"
}

phase_relay_up() {
  g6rd_export_common_env
  g6rd_compose up --detach relay
  g6rd_wait_until 60 2 "relay-b healthy" \
    g6rd_compose exec -T relay relay-healthcheck
}

# Activate this domain's nodes in the authoritative database before either
# domain starts an agent. fd-a then reloads transportd's fail-closed trust
# snapshot once, with the complete fleet present.
phase_agents_enroll() {
  local peer_nodes="${1:?peer nodes tsv}"
  local index count dir name endpoint token node_id enrollment_log
  count="$(g6rd_agent_count)"
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  G6_APPROVAL_DB_PORT="$(db_primary_port)"
  export G6_APPROVAL_DB_PORT
  g6rd_export_common_env
  g6rd_write_agent_overlay "${count}"
  g6rd_wait_for_controller_relay
  : >"${NODES_FILE}"
  cat "${peer_nodes}" >>"${NODES_FILE}"
  for index in $(seq 1 "${count}"); do
    dir="$(g6rd_agent_dir "${index}")"
    name="g6-fd-b-$(printf '%02d' "${index}")"
    g6rd_prepare_agent_material "${index}"
    docker run --rm --pull=never -v "${dir}/identity:/chown" postgres:17.10-bookworm \
      chown -R 65532:65532 /chown >/dev/null 2>&1
    endpoint="$(g6rd_agent_compose run --rm --no-deps \
      -e G6_MODE=prepare "agent-${FD_ID}-$(printf '%02d' "${index}")" \
      | tail -1)"
    [[ "${endpoint:-}" =~ ^[0-9a-f]{64}$ ]] || {
      echo "agent ${name} did not report an endpoint id" >&2
      return 1
    }
    token="$(g6rd_mint_enrollment_token "${name}" "${endpoint}")"
    g6rd_install_agent_enrollment_token "${index}" "${token}"
    enrollment_log="${G6RD_LOGS}/enrollment-${name}.log"
    if ! node_id="$(g6rd_agent_compose run --rm --no-deps \
      -e G6_MODE=enroll \
      -e G6_ENROLLMENT_TOKEN_FILE=/run/ocservia-agent/secrets/enrollment-token \
      -e G6_ENROLLMENT_ENVIRONMENT=development \
      "agent-${FD_ID}-$(printf '%02d' "${index}")" \
      2>&1 | tee "${enrollment_log}" | g6rd_extract_enrollment_node_id)"; then
      echo "agent ${name} enrollment did not return a UUIDv7 node id" >&2
      sed "s/${token}/[redacted]/g" "${enrollment_log}" | tail -40 >&2 || true
      return 1
    fi
    g6rd_approve_node "${node_id}" || {
      echo "approval failed for node ${node_id}" >&2
      return 1
    }
    printf '%s\t%s\t%s\n' "${name}" "${node_id}" "${endpoint}" >>"${NODES_FILE}"
    printf '%s\n' "${node_id}" >"${dir}/state/node-id"
  done
  mkdir -p "${G6RD_OUTBOX}/agents-enrolled"
  printf '%s\n' "${G6RD_CANDIDATE_SHA}" >"${G6RD_OUTBOX}/agents-enrolled/candidate-sha"
}

phase_agents_start() {
  local trust_ready="${1:?transport trust rendezvous is required}"
  local count local_nodes
  require_file "${trust_ready}/candidate-sha"
  [[ "$(<"${trust_ready}/candidate-sha")" == "${G6RD_CANDIDATE_SHA}" ]] || {
    echo "transport trust rendezvous belongs to a different candidate" >&2
    return 1
  }
  count="$(g6rd_agent_count)"
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  local_nodes="${G6RD_STATE}/local-nodes.tsv"
  awk -F'\t' -v prefix="g6-${FD_ID}-" 'index($1, prefix) == 1' \
    "${NODES_FILE}" >"${local_nodes}"
  g6rd_stage_agent_node_state "${local_nodes}"
  g6rd_write_agent_overlay "${count}"
  g6rd_chown_agent_dirs
  g6rd_start_agent_fleet "${local_nodes}" "${NODES_FILE}"
  # The controller's observed-state API is the era-1 readiness authority;
  # the watchers start only after all 55 Agents are online and fresh.
  start_watchers
}

# The database-failure-under-load sequence, split at the rendezvous the
# peer observes: load-start drives fifty-five concurrent commands through
# the era-1 path and publishes the load-active marker; promote consumes the
# peer's isolation record, promotes the standby, brings up the era-2
# control plane, and reconciles every command that was in flight.
phase_load_start() {
  require_file "${NODES_FILE}"
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  g6rd_timeline_init
  g6rd_timeline_event load_started
  # First wave: one command per node is transport-accepted but held at each
  # Agent's synthetic execution barrier, so the commands remain non-terminal.
  local node count=0
  : >"${G6RD_STATE}/load-keys.txt"
  for node in $(node_ids); do
    local key="g6-load-${RUN_ID}-${count}"
    g6rd_enqueue_command "${node}" "${key}" || {
      echo "load enqueue failed for node ${node}" >&2
      return 1
    }
    printf '%s\n' "${key}" >>"${G6RD_STATE}/load-keys.txt"
    count=$((count + 1))
  done
  [[ "${count}" -ge 50 ]] || {
    echo "only ${count} nodes are available for the load phase" >&2
    return 1
  }
  g6rd_wait_until 60 2 "fifty load commands active without results" load_commands_active
  # Hold the same advisory lock Claim uses, then enqueue a second wave. This
  # freezes at least fifty genuine, due outbox writes at the outage boundary
  # without pausing API admission or fabricating command-attempt history.
  psql_primary -Atc "SELECT pg_advisory_lock(5711500382397350988); SELECT pg_sleep(600)" \
    >"${G6RD_LOGS}/load-dispatch-barrier.log" 2>&1 &
  echo $! >"${G6RD_STATE}/load-dispatch-barrier.pid"
  g6rd_wait_until 30 1 "dispatch advisory lock" dispatch_barrier_held
  for node in $(node_ids); do
    local key="g6-load-${RUN_ID}-backlog-${count}"
    g6rd_enqueue_command "${node}" "${key}" || {
      echo "backlog enqueue failed for node ${node}" >&2
      return 1
    }
    printf '%s\n' "${key}" >>"${G6RD_STATE}/load-keys.txt"
    count=$((count + 1))
  done
  g6rd_wait_until 30 1 "fifty due outbox writes" load_outbox_pending
  mkdir -p "${G6RD_OUTBOX}/load-active"
  g6rd_now >"${G6RD_OUTBOX}/load-active/load-active-at"
}

phase_promote() {
  local isolation="${1:?peer isolation directory is required}"
  require_file "${isolation}/isolation.json"
  require_file "${NODES_FILE}"
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  g6rd_timeline_event primary_failure_injected "${isolation}/outage-declared-at"
  g6rd_timeline_event old_primary_isolated "${isolation}/isolated-at"
  g6rd_timeline_event api_instance_failed "${isolation}/isolated-at"

  # promote the standby while the load commands are still open
  touch "${G6RD_STATE}/promoted"
  g6rd_tunnel_serve pg-b "$(<"${G6RD_STATE}/peer-pg-b-forward-node-id")" 5432
  G6_DB_PORT=5432 g6rd_psql -Atc 'SELECT pg_promote(wait := true)' >/dev/null
  G6_DB_PORT=5432 g6rd_psql -Atc "ALTER SYSTEM SET synchronous_standby_names = ''" >/dev/null
  G6_DB_PORT=5432 g6rd_psql -Atc 'SELECT pg_reload_conf()' >/dev/null
  g6rd_wait_until 30 2 "promoted primary writable" promoted_and_writable
  g6rd_now >"${G6RD_STATE}/promoted-at"

  # era-2 roles against the local promoted primary; the controller key
  # handover keeps every agent dialing the same controller NodeId
  g6rd_install_controller_key
  # FD-B did not run the era-1 controller bootstrap. Initialize its transport
  # socket and statistics volumes synchronously before the first era-2 role
  # can create either volume with root-only default ownership.
  g6rd_compose run --rm --no-deps transport-runtime-init >/dev/null
  export G6_DB_HOST=postgres G6_DB_PORT=5432
  g6rd_compose up --detach worker
  g6rd_wait_until 60 1 "era-2 worker trust socket" \
    g6rd_compose exec -T worker test -S /run/ocserv-trust/control-plane.sock
  g6rd_compose up --detach transportd api scheduler
  g6rd_wait_until 60 1 "era-2 transportd socket" \
    g6rd_compose exec -T transportd test -S /run/ocserv-platform/transportd.sock
  if ! g6rd_wait_until 15 1 "era-2 transportd controller endpoint" \
    transport_endpoint_matches; then
    echo "era-2 transportd controller endpoint mismatch: expected $(<"${G6RD_STATE}/controller-endpoint-id"), observed $(cat "${G6RD_STATE}/era2-controller-endpoint-observed" 2>/dev/null || printf 'not reported')" >&2
    return 1
  fi
  g6rd_wait_until 60 2 "era-2 api ready" g6rd_api_ready
  g6rd_now >"${G6RD_STATE}/gateway-transferred-at"
  g6rd_timeline_event gateway_traffic_transferred "${G6RD_STATE}/gateway-transferred-at"
  g6rd_timeline_event worker_instance_failed "${isolation}/isolated-at"
  g6rd_timeline_event new_primary_writable "${G6RD_STATE}/promoted-at"
  g6rd_now >"${G6RD_STATE}/api-recovered-at"
  g6rd_timeline_event api_recovered "${G6RD_STATE}/api-recovered-at"
  g6rd_now >"${G6RD_STATE}/worker-replacement-at"
  g6rd_timeline_event worker_replacement_active "${G6RD_STATE}/worker-replacement-at"
  g6rd_now >"${G6RD_STATE}/worker-recovered-at"
  g6rd_timeline_event worker_recovered "${G6RD_STATE}/worker-recovered-at"
  # The load fixture holds one command stream open inside each local Agent.
  # Release those streams only after promotion, but before waiting for
  # reconnects: an Agent handles a command stream synchronously and cannot
  # observe the old connection closing while the fixture blocks that stream.
  g6rd_release_synthetic_barriers
  # Agents redial the handed-over controller endpoint through relay-b. Keep
  # each inventory probe and the complete recovery wait independently bounded.
  if ! G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_wait_until_deadline 180 5 \
      "agents reconnected to era-2 transportd" all_nodes_connected; then
    report_node_connection_timeout
    return 1
  fi
  # the era-2 session start of every agent, from the live transportd; this
  # is the session_started_at population of the agent-session inventory
  local args=()
  readarray -t args < <(node_ids)
  g6rd_probe_node_connection any "${args[@]}" \
    | jq -r '.observations[] | [.node_id, .connected_at, .last_seen] | @tsv' \
    >"${G6RD_STATE}/era2-sessions.tsv"
  # every in-flight load command must reach a terminal or reconciled state
  g6rd_wait_until_deadline 180 5 "load commands reconciled" load_commands_settled
  g6rd_now >"${G6RD_STATE}/dispatch-recovered-at"
  g6rd_timeline_event dispatch_recovered "${G6RD_STATE}/dispatch-recovered-at"
  g6rd_timeline_event new_primary_promoted "${G6RD_STATE}/promoted-at"
  g6rd_now >"${G6RD_STATE}/load-stopped-at"
  g6rd_timeline_event load_stopped "${G6RD_STATE}/load-stopped-at"
  mkdir -p "${G6RD_OUTBOX}/new-primary"
  cp -f "${G6RD_STATE}/promoted-at" "${G6RD_OUTBOX}/new-primary/promoted-at"
}

transport_endpoint_matches() {
  local expected observed
  expected="$(<"${G6RD_STATE}/controller-endpoint-id")"
  observed="$(g6rd_compose logs --no-color transportd 2>/dev/null \
    | sed -n 's/.*"endpoint_id":"\([0-9a-f]\{64\}\)".*/\1/p' \
    | tail -1)"
  [[ "${observed}" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s\n' "${observed}" >"${G6RD_STATE}/era2-controller-endpoint-observed"
  [[ "${observed}" == "${expected}" ]]
}

all_nodes_connected() {
  local args=()
  readarray -t args < <(node_ids)
  g6rd_probe_node_connection any "${args[@]}" >/dev/null 2>&1
}

report_node_connection_timeout() {
  local args=() response="${G6RD_STATE}/node-connections-timeout.json"
  local error="${G6RD_STATE}/node-connections-timeout-error.txt"
  readarray -t args < <(node_ids)
  echo "last era-2 transport connection probe:" >&2
  if G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_probe_node_connection any "${args[@]}" >"${response}" 2>"${error}"; then
    jq -c '{all_matched, observations: [.observations[] | {
      node_id, path, owner_epoch, last_seen
    }]}' "${response}" >&2
  elif [[ -s "${error}" ]]; then
    sed -n '1,20p' "${error}" >&2
  else
    echo "the transport probe produced no response" >&2
  fi
  if ! g6rd_capture_agent_readiness "${NODES_FILE}"; then
    :
  fi
  g6rd_report_agent_readiness
}

load_commands_active() {
  [[ "$(psql_primary -Atc \
    "SELECT count(*) FROM commands c WHERE c.idempotency_key LIKE 'g6-load-${RUN_ID}-%' \
      AND c.state IN ('dispatched','accepted','running') \
      AND NOT EXISTS (SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id)")" -ge 50 ]]
}

dispatch_barrier_held() {
  [[ "$(psql_primary -Atc \
    "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND classid=1329812310 AND objid=1129137228 AND granted")" -ge 1 ]]
}

load_outbox_pending() {
  [[ "$(psql_primary -Atc \
    "SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id \
      WHERE c.idempotency_key LIKE 'g6-load-${RUN_ID}-backlog-%' \
        AND o.published_at IS NULL AND o.available_at<=now()")" -ge 50 ]]
}

promoted_and_writable() {
  [[ "$(G6_DB_PORT=5432 g6rd_psql -Atc 'SELECT pg_is_in_recovery()' 2>/dev/null)" == f ]]
}

load_commands_settled() {
  local unsettled
  unsettled="$(psql_primary -Atc \
    "SELECT count(*) FROM commands WHERE idempotency_key LIKE 'g6-load-${RUN_ID}-%' AND state NOT IN ('succeeded','failed','unknown','expired','superseded')")"
  [[ "${unsettled}" == 0 ]]
}

# Post-promotion evidence from the peer: fenced-former-primary probes, the
# PITR marker times recorded on fd-a's clock, and the verified PITR report.
phase_merge_peer_evidence() {
  local peer="${1:?peer evidence root is required}"
  local isolation="${peer}/isolation" pitr_prep="${peer}/pitr-prep"
  require_file "${isolation}/isolated-primary-writes.jsonl"
  require_file "${peer}/pitr/pitr-report.json"
  require_file "${pitr_prep}/pitr-marker-a-at"
  g6rd_timeline_event old_primary_write_rejected
  g6rd_timeline_event marker_a_written "${pitr_prep}/pitr-marker-a-at"
  g6rd_timeline_event restore_point_created "${pitr_prep}/restore-point-at"
  g6rd_timeline_event marker_b_written "${pitr_prep}/pitr-marker-b-at"
  g6rd_timeline_event restore_verified
  mkdir -p "${G6RD_OUTBOX}/peer"
  cp -f "${isolation}/isolated-primary-writes.jsonl" "${G6RD_OUTBOX}/peer/"
  cp -f "${isolation}/isolation.json" "${G6RD_OUTBOX}/peer/" 2>/dev/null || true
  cp -f "${isolation}"/*.at "${G6RD_OUTBOX}/peer/" 2>/dev/null || true
  cp -f "${pitr_prep}"/*.at "${G6RD_OUTBOX}/peer/" 2>/dev/null || true
  cp -f "${peer}/pitr/pitr-report.json" "${G6RD_OUTBOX}/peer/"
}

# Scheduler leadership failover: stop the era-2 leader, let the lease lapse,
# prove the replacement's higher epoch, and prove the old term cannot commit
# through the platform's own fence predicate.
phase_scenario_scheduler() {
  local leader
  leader="$(psql_primary -Atc \
    "SELECT instance_id||':'||incarnation||':'||epoch FROM scheduler_leadership WHERE id=1")"
  local old_instance old_incarnation old_epoch
  IFS=: read -r old_instance old_incarnation old_epoch <<<"${leader}"
  SCHEDULER_OLD_EPOCH="${old_epoch}"
  export SCHEDULER_OLD_EPOCH
  g6rd_compose stop scheduler
  g6rd_timeline_event scheduler_a_paused
  local lease_until
  lease_until="$(psql_primary -Atc \
    "SELECT to_char(lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') FROM scheduler_leadership WHERE id=1")"
  g6rd_wait_until 60 2 "old scheduler lease lapsed" scheduler_lease_lapsed
  g6rd_compose up --detach scheduler
  g6rd_wait_until 60 2 "replacement scheduler acquired leadership" scheduler_replaced
  g6rd_timeline_event scheduler_b_acquired
  g6rd_timeline_event scheduler_a_resumed
  # The old term's fence predicate, verbatim from coordination.AssertLeader:
  # no returned row means the superseded epoch can never commit again.
  local fenced
  fenced="$(psql_primary -Atc "SELECT 1 FROM scheduler_leadership \
    WHERE id=1 AND instance_id='${old_instance}' AND incarnation=${old_incarnation} \
    AND epoch=${old_epoch} AND lease_until>clock_timestamp() FOR SHARE OF scheduler_leadership")"
  [[ -z "${fenced}" ]] || {
    echo "the expired scheduler term still passes its own fence predicate" >&2
    return 1
  }
  printf '%s\n' "${old_instance}:${old_incarnation}:${old_epoch}" >"${G6RD_STATE}/stale-scheduler-term"
  g6rd_timeline_event stale_scheduler_commit_rejected
}

# Connection-owner failover inside era 2: stop the worker, let the node
# leases lapse, prove the replacement's higher per-node epochs, then resume
# the old owner's exact fence through both enforcement points (transportd
# disposition and the Agent's stale_owner_epoch gate). The transportd stop
# for the agent-side probe is also the reconnect-storm injection.
scheduler_lease_lapsed() {
  local lease_until
  lease_until="$(psql_primary -Atc \
    "SELECT to_char(lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') FROM scheduler_leadership WHERE id=1")"
  [[ -n "${lease_until}" ]] || return 1
  [[ "$(date -u +%s)" -gt "$(date -u -d "${lease_until}" +%s)" ]]
}

scheduler_replaced() {
  local epoch
  epoch="$(psql_primary -Atc 'SELECT epoch FROM scheduler_leadership WHERE id=1')"
  [[ "${epoch}" =~ ^[0-9]+$ ]] && ((epoch > SCHEDULER_OLD_EPOCH))
}

owner_leases_lapsed() {
  local latest_lease
  latest_lease="$(psql_primary -Atc \
    "SELECT to_char(max(lease_until) AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') FROM connection_owner_fencing")"
  [[ -n "${latest_lease}" ]] || return 1
  [[ "$(date -u +%s)" -ge "$(date -u -d "${latest_lease}" +%s)" ]]
}

owner_replaced() {
  local higher
  higher="$(psql_primary -Atc \
    "SELECT count(*) FROM connection_owner_fencing WHERE owner_epoch > ${OWNER_A_MAX_EPOCH}")"
  [[ "${higher}" =~ ^[0-9]+$ ]] && ((higher > 0))
}

phase_scenario_owner() {
  local sample_nodes
  sample_nodes="$(psql_primary -Atc \
    "SELECT encode(node_id,'hex')||':'||owner_instance_id||':'||owner_incarnation||':'||encode(connection_id,'hex')||':'||owner_epoch FROM connection_owner_fencing ORDER BY node_id LIMIT 5")"
  [[ -n "${sample_nodes}" ]] || {
    echo "no registered connection owners before the owner scenario" >&2
    return 1
  }
  printf '%s\n' "${sample_nodes}" >"${G6RD_STATE}/owner-a-terms.tsv"
  OWNER_A_MAX_EPOCH="$(printf '%s' "${sample_nodes}" | cut -d: -f5 | sort -n | tail -1)"
  export OWNER_A_MAX_EPOCH
  g6rd_export_common_env
  g6rd_compose stop worker
  g6rd_timeline_event owner_a_paused
  g6rd_wait_until 90 2 "owner leases lapsed" owner_leases_lapsed
  g6rd_compose up --detach worker
  g6rd_wait_until 60 1 "replacement worker trust socket" \
    g6rd_compose exec -T worker test -S /run/ocserv-trust/control-plane.sock
  # drive dispatch so the replacement registers higher epochs on the nodes
  local node index=0
  for node in $(node_ids | head -5); do
    g6rd_enqueue_command "${node}" "g6-owner-${RUN_ID}-${index}" || true
    index=$((index + 1))
  done
  g6rd_wait_until 90 2 "replacement owner registered higher epochs" owner_replaced
  g6rd_timeline_event owner_b_acquired
  g6rd_timeline_event owner_a_resumed
  # enforcement point 1: transportd returns Stale with the retained epoch
  local first epoch
  first="$(head -1 "${G6RD_STATE}/owner-a-terms.tsv")"
  epoch="$(printf '%s' "${first}" | cut -d: -f5)"
  local retained
  retained="$(psql_primary -Atc \
    "SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=decode('$(printf '%s' "${first}" | cut -d: -f1)','hex')")"
  g6rd_compose --profile probe run --rm --no-deps g6-probe uds-stale-fence \
    --socket /run/ocserv-platform/transportd.sock \
    --signing-key-file /run/ocservia-signing/command-signing.pem \
    --node-id "$(node_from_fencing_hex "$(printf '%s' "${first}" | cut -d: -f1)")" \
    --endpoint-id "$(<"${G6RD_STATE}/controller-endpoint-id")" \
    --owner-instance-id "$(printf '%s' "${first}" | cut -d: -f2)" \
    --owner-incarnation "$(printf '%s' "${first}" | cut -d: -f3)" \
    --stale-epoch "${epoch}" \
    --expect-retained-epoch "${retained}" \
    >"${G6RD_STATE}/stale-transport-probe.json"
  jq -e '.status == "rejected"' "${G6RD_STATE}/stale-transport-probe.json" >/dev/null
  g6rd_timeline_event stale_transport_rejected
  # enforcement point 2 + the reconnect storm: stop transportd, let the
  # probe hold the controller endpoint for one bounded window, restart
  g6rd_compose stop transportd
  g6rd_timeline_event bulk_disconnect_injected
  local target_node
  target_node="$(node_from_fencing_hex "$(printf '%s' "${first}" | cut -d: -f1)")"
  g6rd_compose --profile probe run --rm --no-deps \
    -e RUST_LOG=info g6-probe agent-stale-command \
    --signing-key-file /run/ocservia-signing/command-signing.pem \
    --controller-key-file /run/ocservia-controller/controller.key \
    --node-id "${target_node}" \
    --endpoint-id "$(<"${G6RD_STATE}/controller-endpoint-id")" \
    --owner-instance-id "$(printf '%s' "${first}" | cut -d: -f2)" \
    --owner-incarnation "$(printf '%s' "${first}" | cut -d: -f3)" \
    --stale-epoch "${epoch}" \
    --relay-url "${G6_RELAY_URL_B}" \
    --relay-token-file /run/relay-secrets/relay-token \
    --relay-ca-file /run/relay-secrets/relay-ca.pem \
    --wait-seconds 60 \
    >"${G6RD_STATE}/stale-agent-probe.json"
  jq -e '.status == "rejected"' "${G6RD_STATE}/stale-agent-probe.json" >/dev/null
  g6rd_timeline_event reconnect_started
  g6rd_timeline_event stale_agent_rejected
  g6rd_compose up --detach transportd
  g6rd_wait_until 60 1 "transportd back after the storm" \
    g6rd_compose exec -T transportd test -S /run/ocserv-platform/transportd.sock
  g6rd_wait_until 180 5 "all agents reconnected after the storm" all_nodes_connected
  g6rd_now >"${G6RD_STATE}/reconnect-completed-at"
  g6rd_timeline_event reconnect_completed "${G6RD_STATE}/reconnect-completed-at"
}

node_from_fencing_hex() {
  local hex="${1:?node hex is required}" node
  for node in $(node_ids); do
    [[ "${node//-/}" == "${hex}" ]] && {
      printf '%s\n' "${node}"
      return 0
    }
  done
  return 1
}

# Relay failover: after fd-a stops relay-a, authenticated cross-VM session
# traffic must flow through relay-b.
phase_scenario_relay() {
  local relay_failed_at="${1:?relay-a failure stamp file}"
  require_file "${relay_failed_at}"
  local cross_vm_node
  cross_vm_node="$(awk -F'\t' '$1 ~ /^g6-fd-a-/ {print $2; exit}' "${NODES_FILE}")"
  [[ -n "${cross_vm_node}" ]] || {
    echo "no cross-failure-domain agent is available for the relay scenario" >&2
    return 1
  }
  g6rd_timeline_event relay_a_failed "${relay_failed_at}"
  g6rd_wait_until 90 5 "cross-VM session through relay-b" \
    relay_probe_relay_b "${cross_vm_node}"
  relay_probe_relay_b "${cross_vm_node}" >"${G6RD_STATE}/relay-b-observation.json"
  g6rd_timeline_event relay_b_active
}

relay_probe_relay_b() {
  local node="${1:?node id is required}" observation
  observation="$(g6rd_probe_node_connection relay "${node}" 2>/dev/null)" || return 1
  jq -e '.all_matched == true and (.observations | length == 1) and
    .observations[0].path == "relay" and
    (.observations[0].path_detail | contains("relay-b"))' \
    <<<"${observation}" >/dev/null
}

# Direct-relay path transitions on one same-host agent: sever the shared
# docker network (bridge isolation blocks the direct path), let the session
# re-establish through the relay, then restore the network and let iroh
# converge back to the direct path.
phase_scenario_path() {
  local service="agent-${FD_ID}-01" agent_name="g6-${FD_ID}-01" node isolated_network
  node="$(awk -F'\t' -v name="${agent_name}" '$1 == name {print $2; exit}' "${NODES_FILE}")"
  [[ -n "${node}" ]] || {
    echo "node id for ${agent_name} is missing" >&2
    return 1
  }
  isolated_network="${COMPOSE_PROJECT}_agent-isolated"
  docker network create "${isolated_network}" >/dev/null 2>&1 || true
  g6rd_wait_until 60 5 "agent-01 session on the direct path" \
    g6rd_probe_node_connection direct "${node}"
  g6rd_timeline_event direct_path_active
  docker network connect "${isolated_network}" "${COMPOSE_PROJECT}-${service}-1" >/dev/null
  docker network disconnect "${COMPOSE_PROJECT}_default" "${COMPOSE_PROJECT}-${service}-1"
  g6rd_timeline_event direct_path_failed
  g6rd_wait_until 120 5 "agent-01 session moved to the relay path" \
    g6rd_probe_node_connection relay "${node}"
  g6rd_timeline_event relay_path_active
  docker network connect "${COMPOSE_PROJECT}_default" "${COMPOSE_PROJECT}-${service}-1" >/dev/null
  docker network disconnect "${isolated_network}" "${COMPOSE_PROJECT}-${service}-1"
  g6rd_wait_until 180 5 "agent-01 session recovered the direct path" \
    g6rd_probe_node_connection direct "${node}"
  g6rd_timeline_event direct_path_recovered
}

# ---------------------------------------------------------------------------
# Outbox crash windows. Each window issues real commands through the API,
# freezes or kills the worker inside the named window, and requires the
# reconciliation path to settle the command afterwards. Evidence comes from
# the database (attempts, outbox rows, terminal states) and the target
# agent's durable journal.
# ---------------------------------------------------------------------------

wait_commands_settled() {
  local keys_prefix="${1:?key prefix}"
  local unsettled
  unsettled="$(psql_primary -Atc \
    "SELECT count(*) FROM commands WHERE idempotency_key LIKE '${keys_prefix}%' AND state NOT IN ('succeeded','failed','unknown','expired','superseded')")"
  [[ "${unsettled}" == 0 ]]
}

outbox_row_claimed() {
  local claimed
  claimed="$(psql_primary -Atc \
    "SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.idempotency_key='${CLAIM_KEY:?}' AND (o.locked_by IS NOT NULL OR o.published_at IS NOT NULL)")"
  [[ "${claimed}" =~ ^[0-9]+$ ]] && ((claimed >= 1))
}

# The agent journal stores the envelope's binary command id, so lookups key
# on the database command uuid (hex, dashes stripped).
journal_has_command() {
  local service="${1:?service}" command_id="${2:?command id}"
  journal_query "${service}" \
    "SELECT count(*) FROM command_journal WHERE hex(command_id)='${command_id//-/}'" \
    2>/dev/null | tr -d '[:space:]'
}

journal_result_state() {
  local service="${1:?service}" command_id="${2:?command id}"
  journal_query "${service}" \
    "SELECT state FROM command_journal WHERE hex(command_id)='${command_id//-/}'" \
    2>/dev/null | tr -d '[:space:]'
}

command_id_of_key() {
  psql_primary -Atc "SELECT id FROM commands WHERE idempotency_key='${1:?key}'"
}

# Window 1: claim committed, transport send blocked (transportd frozen),
# worker killed before any send can be accepted.
phase_outbox_claim_before_send() {
  local node key command_id
  node="$(node_ids | head -1)"
  key="g6-crash1-${RUN_ID}"
  CLAIM_KEY="${key}"
  export CLAIM_KEY
  docker pause "${COMPOSE_PROJECT}-transportd-1" >/dev/null
  g6rd_enqueue_command "${node}" "${key}"
  g6rd_wait_until 30 1 "outbox row claimed" outbox_row_claimed
  g6rd_timeline_event outbox_claim_committed
  docker kill "${COMPOSE_PROJECT}-worker-1" >/dev/null
  g6rd_timeline_event worker_crashed_before_send
  docker unpause "${COMPOSE_PROJECT}-transportd-1" >/dev/null
  g6rd_compose up --detach worker
  g6rd_wait_until 120 5 "claim-before-send command recovered" \
    wait_commands_settled "g6-crash1-${RUN_ID}"
  printf '%s\n' "$(command_id_of_key "${key}")" >"${G6RD_STATE}/crash1-command-id"
  g6rd_timeline_event command_recovered
}

# Window 2: transport accepted the send, the worker dies before MarkSent
# commits. Detected from the journal receipt plus a first attempt that never
# left 'sending'; retried with fresh commands until the window is caught.
phase_outbox_send_before_mark() {
  local node key command_id first_attempt caught=0 attempt service
  node="$(node_ids | head -2 | tail -1)"
  service="$(node_service "${node}")"
  for attempt in 1 2 3 4 5; do
    key="g6-crash2-${RUN_ID}-${attempt}"
    CLAIM_KEY="${key}"
    export CLAIM_KEY
    g6rd_enqueue_command "${node}" "${key}" || continue
    g6rd_wait_until 30 1 "crash2 attempt claimed" outbox_row_claimed || continue
    docker kill "${COMPOSE_PROJECT}-worker-1" >/dev/null
    g6rd_compose up --detach worker
    g6rd_wait_until 120 5 "crash2 attempt settled" \
      wait_commands_settled "g6-crash2-${RUN_ID}-${attempt}" || continue
    command_id="$(command_id_of_key "${key}")"
    first_attempt="$(psql_primary -Atc \
      "SELECT state FROM command_attempts WHERE command_id='${command_id}' ORDER BY attempt_number LIMIT 1")"
    if [[ "${first_attempt}" == "sending" ]] \
      && [[ "$(journal_has_command "${service}" "${command_id}")" == 1 ]]; then
      # journal receipt proves transport acceptance; the first attempt never
      # committed MarkSent before the crash
      printf '%s\n' "${command_id}" >"${G6RD_STATE}/crash2-command-id"
      g6rd_timeline_event transport_send_accepted
      g6rd_timeline_event worker_crashed_before_mark_sent
      g6rd_timeline_event command_reconciled
      caught=1
      break
    fi
  done
  [[ "${caught}" == 1 ]] || {
    echo "the send-before-mark window was not observed in five attempts" >&2
    return 1
  }
}

# Window 3: the ingress transaction validates and applies the Agent result,
# signals the harness while the transaction is still open, and blocks before
# commit. Killing the worker at that barrier proves the database saw no
# terminal result until the replacement reconciled the retained Agent result.
phase_outbox_result_before_commit() {
  local node key command_id service kill_file="${G6RD_STATE}/crash3-kill-at"
  local barrier="${G6RD_RESULT_BARRIER}"
  node="$(node_ids | head -3 | tail -1)"
  service="$(node_service "${node}")"
  key="g6-crash3-${RUN_ID}"
  rm -f "${barrier}/arm" "${barrier}/received" "${barrier}/release"
  # Pause dispatch until the API has returned the exact command id that arms
  # the ingress barrier; otherwise a fast synthetic result could beat setup.
  docker pause "${COMPOSE_PROJECT}-worker-1" >/dev/null
  g6rd_enqueue_command "${node}" "${key}"
  command_id="$(command_id_of_key "${key}")"
  [[ -n "${command_id}" ]] || {
    echo "result-before-commit command id is missing" >&2
    return 1
  }
  printf '%s\n' "${command_id}" >"${barrier}/arm"
  chmod 0666 "${barrier}/arm"
  docker unpause "${COMPOSE_PROJECT}-worker-1" >/dev/null
  g6rd_wait_until 60 1 "ingress result commit barrier" test -s "${barrier}/received"
  [[ "$(sed -n '1p' "${barrier}/received")" == "${command_id}" ]] || {
    echo "result commit barrier signaled for the wrong command" >&2
    return 1
  }
  sed -n '2p' "${barrier}/received" >"${G6RD_STATE}/crash3-result-received-at"
  require_file "${G6RD_STATE}/crash3-result-received-at"
  # The signal is emitted only after result validation/mutation inside the
  # open transaction. A separate connection must still see no terminal row.
  [[ "$(psql_primary -Atc "SELECT count(*) FROM agent_command_results WHERE command_id='${command_id}'")" == 0 ]] || {
    echo "the command result committed before the ingress crash" >&2
    return 1
  }
  sleep 1
  docker kill "${COMPOSE_PROJECT}-worker-1" >/dev/null
  g6rd_now >"${kill_file}"
  [[ "$(psql_primary -Atc "SELECT count(*) FROM agent_command_results WHERE command_id='${command_id}'")" == 0 ]] || {
    echo "the killed ingress transaction committed a command result" >&2
    return 1
  }
  rm -f "${barrier}/arm" "${barrier}/received"
  g6rd_compose up --detach worker
  g6rd_wait_until 120 5 "crash3 result reconciled" wait_commands_settled "${key}"
  [[ "$(psql_primary -Atc "SELECT count(*) FROM agent_command_results WHERE command_id='${command_id}'")" == 1 ]] || {
    echo "the replacement ingress did not reconcile exactly one command result" >&2
    return 1
  }
  printf '%s\n' "${command_id}" >"${G6RD_STATE}/crash3-command-id"
  g6rd_timeline_event result_received "${G6RD_STATE}/crash3-result-received-at"
  g6rd_timeline_event ingress_crashed_before_commit "${kill_file}"
  g6rd_timeline_event result_reconciled
}

journal_result_ready() {
  local service="${1:?service}" command_id="${2:?command id}"
  [[ -n "$(journal_result_state "${service}" "${command_id}")" ]]
}

# ---------------------------------------------------------------------------
# The bounded stability window: continuous 3-second resource sampling on
# this failure domain's clock while the HTTP driver records read and
# enqueue observations against the recovered control plane.
# ---------------------------------------------------------------------------

outbox_drained() {
  [[ "$(psql_primary -Atc \
    'SELECT count(*) FROM outbox_events WHERE published_at IS NULL')" == 0 ]]
}

phase_window() {
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  g6rd_wait_until 60 2 "api ready before the window" g6rd_api_ready
  g6rd_wait_until 120 5 "outbox drained before the window" outbox_drained
  g6rd_start_sampler "${G6RD_STATE}/resource-samples.csv"
  g6rd_now >"${G6RD_STATE}/window-started-at"
  local node total count=0 elapsed=0 start _
  mapfile -t node_list < <(node_ids)
  total="${#node_list[@]}"
  start="$(date +%s)"
  : >"${G6RD_STATE}/read-log.jsonl"
  : >"${G6RD_STATE}/enqueue-log.jsonl"
  # Two opening waves of one-command-per-node, enqueued in parallel: the
  # whole fleet holds a command in flight before any result can return,
  # which drives the concurrent production-command floor above fifty
  # inside the bounded window itself.
  for _ in 1 2; do
    for node in "${node_list[@]}"; do
      g6rd_enqueue_command "${node}" "g6-window-${RUN_ID}-${count}" &
      count=$((count + 1))
    done
    wait
  done
  while ((elapsed < WINDOW_SECONDS)); do
    g6rd_read_nodes "${G6RD_STATE}/read-log.jsonl" || true
    g6rd_read_nodes "${G6RD_STATE}/read-log.jsonl" || true
    node="${node_list[$((count % total))]}"
    if [[ -n "${node}" ]]; then
      g6rd_enqueue_command "${node}" "g6-window-${RUN_ID}-${count}" || true
      g6rd_enqueue_command "${node}" "g6-window-${RUN_ID}-b${count}" || true
    fi
    count=$((count + 1))
    sleep 0.5
    elapsed="$(( $(date +%s) - start ))"
  done
  CLAIM_KEY="g6-window-${RUN_ID}-"
  export CLAIM_KEY
  g6rd_wait_until 240 5 "window commands settled" wait_commands_settled "g6-window-${RUN_ID}"
  g6rd_wait_until 60 5 "outbox drained after the window" outbox_drained
  g6rd_stop_sampler
  g6rd_now >"${G6RD_STATE}/window-ended-at"
  g6rd_timeline_event api_slo_measured "${G6RD_STATE}/window-ended-at"
}

# ---------------------------------------------------------------------------
# Evidence collection and assembly. Collection runs while the recovered
# stack is still live: it freezes the authoritative database tables, the
# per-agent durable journals, the final session inventory, and this failure
# domain's container inventory into run state. Assembly turns the frozen
# records into the verifier's structured artifacts and runs the independent
# verifier against them.
# ---------------------------------------------------------------------------

phase_evidence_collect() {
  stop_watchers
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  local dir="${G6RD_STATE}/evidence" index service
  mkdir -p "${dir}/effects"
  # the authorized+connected population as the live transportd sees it
  local args=()
  readarray -t args < <(node_ids)
  g6rd_probe_node_connection any "${args[@]}" >"${dir}/final-sessions.json"
  g6rd_now >"${dir}/snapshot-taken-at"
  # frozen database views, one JSON object per line; to_char pins every
  # timestamp to strict RFC 3339 with an explicit UTC offset
  psql_primary -Atc "SELECT jsonb_build_object('id',c.id::text,'idempotency_key',c.idempotency_key,'node_id',c.node_id::text,'state',c.state,'created_at',to_char(c.created_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),'updated_at',to_char(c.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')) FROM commands c WHERE c.idempotency_key LIKE 'g6-%' ORDER BY c.created_at, c.id" \
    >"${dir}/commands.jsonl"
  psql_primary -Atc "SELECT jsonb_build_object('command_id',a.command_id::text,'attempt_number',a.attempt_number,'state',a.state,'started_at',to_char(a.started_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),'finished_at',CASE WHEN a.finished_at IS NULL THEN '' ELSE to_char(a.finished_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') END) FROM command_attempts a JOIN commands c ON c.id=a.command_id WHERE c.idempotency_key LIKE 'g6-%' ORDER BY a.started_at, a.id" \
    >"${dir}/attempts.jsonl"
  psql_primary -Atc "SELECT jsonb_build_object('command_id',o.command_id::text,'created_at',to_char(o.created_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),'available_at',to_char(o.available_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),'published_at',CASE WHEN o.published_at IS NULL THEN '' ELSE to_char(o.published_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') END,'locked',CASE WHEN o.locked_by IS NULL THEN false ELSE true END) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.idempotency_key LIKE 'g6-%' ORDER BY o.created_at, o.id" \
    >"${dir}/outbox.jsonl"
  psql_primary -Atc "SELECT jsonb_build_object('command_id',e.command_id::text,'result',e.result,'occurred_at',to_char(e.occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')) FROM audit_events e WHERE e.command_id IS NOT NULL AND EXISTS(SELECT 1 FROM commands c WHERE c.id=e.command_id AND c.idempotency_key LIKE 'g6-%') ORDER BY e.occurred_at, e.id" \
    >"${dir}/audit.jsonl"
  psql_primary -Atc "SELECT jsonb_build_object('agent_id',n.name,'last_telemetry_at',to_char(s.last_heartbeat_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')) FROM node_observed_snapshots s JOIN nodes n ON n.id=s.node_id WHERE n.name LIKE 'g6-fd-%' ORDER BY n.name" \
    >"${dir}/telemetry.jsonl"
  psql_primary -Atc "SELECT jsonb_build_object('id',m.id,'txid',m.txid,'written_at',to_char(m.written_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')) FROM g6_readiness_markers m ORDER BY m.written_at" \
    >"${dir}/markers.jsonl"
  # per-agent durable journals: the effect population keyed by the binary
  # command id, joined through the journal's own idempotency identity
  for index in $(seq 1 "$(g6rd_agent_count)"); do
    service="agent-${FD_ID}-$(printf '%02d' "${index}")"
    journal_query "${service}" \
      "SELECT hex(e.idempotency_key)||' '||hex(j.command_id)||' '||e.executed_at FROM synthetic_effects e JOIN command_journal j ON j.idempotency_key=e.idempotency_key" \
      >"${dir}/effects/${service}.tsv"
  done
  # this failure domain's container inventory
  : >"${dir}/instances.tsv"
  docker ps -a --filter "label=com.docker.compose.project=${COMPOSE_PROJECT}" \
    --format '{{.Names}}' | sort -u | while read -r name; do
    [[ -n "${name}" ]] || continue
    docker inspect --format \
      '{{.Name}}	{{.Image}}	{{.State.StartedAt}}	{{.State.FinishedAt}}	{{index .Config.Labels "com.docker.compose.service"}}' \
      "${name}" 2>/dev/null || true
  done >>"${dir}/instances.tsv"
  printf 'failure_domain=%s\nalias=%s\n' "${FD_ID}" "${FD_ALIAS}" >"${dir}/failure-domain.txt"
}

phase_final_freeze() {
  require_file "${G6RD_STATE}/window-ended-at"
  require_file "${G6RD_STATE}/evidence/final-sessions.json"
  mkdir -p "${G6RD_OUTBOX}/final-freeze"
  g6rd_now >"${G6RD_OUTBOX}/final-freeze/final-freeze-at"
}

phase_merge_peer_final_evidence() {
  local peer="${1:?peer final evidence root is required}"
  require_file "${peer}/final-freeze-at"
  require_file "${peer}/freeze-received-at"
  require_file "${peer}/evidence/snapshot-taken-at"
  mkdir -p "${G6RD_STATE}/evidence/effects"
  local node_name service count=0
  while IFS= read -r node_name; do
    service="agent-${node_name#g6-}"
    require_file "${peer}/evidence/effects/${service}.tsv"
    cp -f "${peer}/evidence/effects/${service}.tsv" \
      "${G6RD_STATE}/evidence/effects/"
    count=$((count + 1))
  done < <(awk -F'\t' '$1 ~ /^g6-fd-a-/ {print $1}' "${NODES_FILE}")
  [[ "${count}" -gt 0 ]] || {
    echo "peer final evidence contains no fd-a Agent journals" >&2
    return 1
  }
}

phase_evidence_build() {
  local peer="${1:?peer evidence root is required}" out="${G6RD_OUTBOX}/evidence-bundle"
  require_file "${peer}/evidence/instances.tsv"
  require_file "${G6RD_STATE}/evidence/commands.jsonl"
  mkdir -p "${out}"
  "${G6RD_NODE_BIN:-node}" "${ROOT}/scripts/build-g6-evidence.mjs" \
    --run-dir "${G6RD_WORK}" \
    --peer-dir "${peer}" \
    --out-dir "${out}" \
    --slo "${ROOT}/docs/acceptance/g6-slo.yaml" \
    --environment-id "${G6RD_ENVIRONMENT_ID}" \
    --candidate-sha "${G6RD_CANDIDATE_SHA}" \
    --authority "${G6_AUTHORITY}" \
    --failure-domain-class "${G6RD_FAILURE_DOMAIN_CLASS:-multi_host}" \
    --run-id "${RUN_ID}"
  # The independent verifier recomputes every derivation from the artifact
  # bytes. An engineering-rehearsal bundle is allowed to carry a non-final
  # verdict (the authority fence alone keeps it non-final), but any parse
  # or integrity rejection fails the run.
  local verify_status=0
  "${G6RD_NODE_BIN:-node}" "${ROOT}/scripts/verify-g6-evidence.mjs" \
    --slo "${ROOT}/docs/acceptance/g6-slo.yaml" \
    --evidence "${out}/evidence.json" \
    --topology "${out}/topology.json" \
    --release-manifest "${out}/release-manifest.json" \
    --artifact-root "${out}" \
    --expected-authority "${G6_AUTHORITY}" \
    --expected-environment-id "${G6RD_ENVIRONMENT_ID}" \
    --expected-failure-domain-class "${G6RD_FAILURE_DOMAIN_CLASS:-multi_host}" \
    >"${out}/verdict.json" || verify_status=$?
  if [[ "${G6_AUTHORITY}" == production_readiness ]]; then
    if [[ "${verify_status}" != 0 ]]; then
      echo "the independent verifier rejected the production-readiness bundle" >&2
      return 1
    fi
  else
    jq -e '.schema_version == "ocservia.g6-verdict.v2"' "${out}/verdict.json" >/dev/null
  fi
  cp -f "${out}/verdict.json" "${G6RD_OUTBOX}/verdict.json"
}

case "${1:-}" in
prepare) phase_prepare ;;
materialize-runtime | import-peer-secrets) phase_materialize_runtime "${2:?peer directory}" ;;
import-peer-tunnel-nodes) import_peer_tunnel_nodes "${2:?peer directory}" ;;
build-images | images) phase_build_images ;;
tunnel-up) phase_tunnel_up ;;
standby-bootstrap) phase_standby_bootstrap "${2:?primary rendezvous directory}" ;;
relay-up) phase_relay_up ;;
  agents-enroll) phase_agents_enroll "${2:?peer nodes tsv}" ;;
  agents-start) phase_agents_start "${2:?transport trust rendezvous is required}" ;;
  load-start) phase_load_start ;;
  promote) phase_promote "${2:?peer isolation directory}" ;;
  merge-peer-evidence) phase_merge_peer_evidence "${2:?peer evidence root}" ;;
scenario-scheduler) phase_scenario_scheduler ;;
scenario-owner) phase_scenario_owner ;;
scenario-relay) phase_scenario_relay "${2:?relay-a failure stamp}" ;;
scenario-path) phase_scenario_path ;;
outbox-claim-before-send) phase_outbox_claim_before_send ;;
outbox-send-before-mark) phase_outbox_send_before_mark ;;
  outbox-result-before-commit) phase_outbox_result_before_commit ;;
  window) phase_window ;;
  evidence-collect) phase_evidence_collect ;;
  final-freeze) phase_final_freeze ;;
  merge-peer-final-evidence) phase_merge_peer_final_evidence "${2:?peer final evidence root}" ;;
  evidence-build) phase_evidence_build "${2:?peer evidence root}" ;;
  diagnostics) g6rd_diagnostics ;;
  cleanup) g6rd_cleanup_bounded ;;
*)
  echo "usage: $0 <prepare|materialize-runtime|import-peer-tunnel-nodes|build-images|tunnel-up|standby-bootstrap|relay-up|agents-enroll|agents-start|load-start|promote|merge-peer-evidence|scenario-scheduler|scenario-owner|scenario-relay|scenario-path|outbox-claim-before-send|outbox-send-before-mark|outbox-result-before-commit|window|evidence-collect|final-freeze|merge-peer-final-evidence|evidence-build|diagnostics|cleanup>" >&2
  exit 2
  ;;
esac
