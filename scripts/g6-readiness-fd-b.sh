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
WINDOW_API_READY_TIMEOUT_SECONDS=15
WINDOW_PRE_DRAIN_TIMEOUT_SECONDS=15
WINDOW_COMMAND_SETTLE_TIMEOUT_SECONDS=110
WINDOW_POST_DRAIN_TIMEOUT_SECONDS=15
WINDOW_API_PREDICATE_OVERRUN_SECONDS=10
WINDOW_SQL_PREDICATE_OVERRUN_SECONDS=10
WINDOW_DRIVER_OVERRUN_SECONDS=21
WINDOW_DIAGNOSTIC_MAX_SECONDS=10
WINDOW_SAMPLER_STOP_MAX_SECONDS=10
WINDOW_WORKFLOW_TIMEOUT_SECONDS=660
WINDOW_MINIMUM_OUTER_MARGIN_SECONDS=60
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

psql_primary_probe() {
  G6RD_PSQL_TIMEOUT_SECONDS=10 psql_primary "$@"
}

capture_local_database_clock() {
  G6_DB_PORT=5432 G6RD_PSQL_TIMEOUT_SECONDS=10 g6rd_psql -qAtc \
    "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')"
}

node_ids() {
  cut -f2 "${NODES_FILE}"
}

managed_node_count() {
  local configured actual
  require_file "${NODES_FILE}" || return 1
  configured="$(g6rd_total_agent_count)" || return 1
  if ! actual="$(awk -F'\t' '
    NF != 3 || $1 == "" || $2 == "" || $3 == "" { exit 2 }
    { count++ }
    END { if (count > 0) print count; else exit 2 }
  ' "${NODES_FILE}")"; then
    echo "the managed-node inventory is malformed or empty" >&2
    return 1
  fi
  [[ "${actual}" == "${configured}" ]] || {
    echo "the managed-node inventory has ${actual} rows; expected the configured full population ${configured}" >&2
    return 1
  }
  printf '%s\n' "${actual}"
}

local_node_id() {
  local ordinal="${1:?local node ordinal is required}"
  [[ "${ordinal}" =~ ^[1-9][0-9]*$ ]] || return 2
  awk -F'\t' -v prefix="g6-${FD_ID}-" -v wanted="${ordinal}" '
    index($1, prefix) == 1 {
      seen++
      if (seen == wanted) {
        print $2
        exit
      }
    }
    END { if (seen < wanted) exit 1 }
  ' "${NODES_FILE}"
}

node_service() {
  local node_id="${1:?node id is required}" name
  name="$(awk -F'\t' -v id="${node_id}" '$2 == id {print $1}' "${NODES_FILE}")"
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
  g6rd_agent_journal_query "${service}" "${sql}"
}

# ---------------------------------------------------------------------------
# Watchers: background pollers that turn the authoritative fencing and
# leadership tables into append-only history for the epoch-events artifact.
# ---------------------------------------------------------------------------

start_watchers() {
  rm -f "${G6RD_STATE}/watchers-stop" \
    "${G6RD_STATE}/fencing-watcher-failed-at" \
    "${G6RD_STATE}/leadership-watcher-failed-at"
  : >"${G6RD_STATE}/fencing-history.jsonl"
  : >"${G6RD_STATE}/leadership-history.jsonl"
  g6rd_spawn_harness_loop "${G6RD_LOGS}/fencing-watcher.log" \
    g6rd_watch_fencing_history >"${G6RD_STATE}/fencing-watcher.pid"
  g6rd_spawn_harness_loop "${G6RD_LOGS}/leadership-watcher.log" \
    g6rd_watch_leadership_history >"${G6RD_STATE}/leadership-watcher.pid"
}

stop_watchers() {
  touch "${G6RD_STATE}/watchers-stop"
  local name failure status=0
  for name in fencing leadership; do
    g6rd_stop_harness_loop "${G6RD_STATE}/${name}-watcher.pid" || status=1
    failure="${G6RD_STATE}/${name}-watcher-failed-at"
    if [[ -e "${failure}" ]]; then
      echo "${name} authority watcher failed closed at $(<"${failure}")" >&2
      status=1
    fi
  done
  return "${status}"
}

# ---------------------------------------------------------------------------
# Phases.
# ---------------------------------------------------------------------------

phase_prepare() {
  g6rd_prepare_support_image
  g6rd_verify_tunnel
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
  g6rd_verify_tunnel
  g6rd_prepare_build_environment
  g6rd_write_agent_overlay "$(g6rd_agent_count)"
  g6rd_prepare_release_images
  g6rd_prepare_agent_image
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
  g6rd_tunnel_serve pg-b "$(<"${G6RD_STATE}/peer-pg-b-forward-node-id")" 5432
  g6rd_now >"${G6RD_STATE}/standby-streaming-at"
}

relay_b_healthy() {
  G6RD_COMPOSE_TIMEOUT_SECONDS=5 g6rd_compose exec -T relay relay-healthcheck
}

phase_relay_up() {
  g6rd_export_common_env
  g6rd_compose up --detach relay
  g6rd_wait_until_deadline 120 2 "relay-b healthy" relay_b_healthy
}

relay_b_stopped() {
  local running
  running="$(timeout --signal=TERM --kill-after=5s 15s docker container inspect \
    --format '{{.State.Running}}' "${COMPOSE_PROJECT}-relay-1" 2>/dev/null)" || return 1
  [[ "${running}" == false ]]
}

validate_relay_a_only_readiness() {
  local readiness="${1:?relay topology readiness is required}"
  require_file "${readiness}/candidate-sha"
  require_file "${readiness}/node-id"
  require_file "${readiness}/prior-connection-id"
  require_file "${readiness}/prior-owner-epoch"
  require_file "${readiness}/relay-a-only-readiness.json"
  [[ "$(<"${readiness}/candidate-sha")" == "${G6RD_CANDIDATE_SHA}" ]] || {
    echo "relay observation readiness belongs to a different candidate" >&2
    return 1
  }
  [[ "$(<"${readiness}/prior-connection-id")" =~ ^[0-9a-f]{32}$ ]] || {
    echo "relay observation readiness has an invalid prior connection" >&2
    return 1
  }
  [[ "$(<"${readiness}/prior-owner-epoch")" =~ ^[1-9][0-9]*$ ]] || {
    echo "relay observation readiness has an invalid prior owner epoch" >&2
    return 1
  }
  jq -e --arg environment "${G6RD_ENVIRONMENT_ID}" \
    --arg candidate "${G6RD_CANDIDATE_SHA}" \
    --arg node "$(<"${readiness}/node-id")" '
      keys == ["agent_default_network_connected","agent_service","candidate_sha",
        "environment_id","network_internal","network_name","node_id",
        "relay_alias","schema_version","topology_ready_at"]
      and .schema_version == "ocservia.g6-relay-topology.v1"
      and .environment_id == $environment and .candidate_sha == $candidate
      and .node_id == $node and .agent_service == "agent-fd-a-01"
      and (.network_name | endswith("_relay-a-only"))
      and .network_internal == true
      and .agent_default_network_connected == false
      and .relay_alias == "relay-a"
      and (.topology_ready_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]{1,9})?Z$"))
    ' "${readiness}/relay-a-only-readiness.json" >/dev/null
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
  g6rd_verify_agent_journal_observer_principals "agent-${FD_ID}-01"
  # The controller's observed-state API is the era-1 readiness authority;
  # the watchers start only after the complete 50-Agent fleet is online and fresh.
  start_watchers
}

# The database-failure-under-load sequence, split at the rendezvous the
# peer observes: load-start drives fifty concurrent commands through
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

phase_smoke_session() {
  require_file "${NODES_FILE}"
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  g6rd_timeline_init
  g6rd_timeline_event smoke_session_started
  # Before promotion the active transportd and API live in FD-A. The FD-B API
  # port is the authenticated tunnel to that controller, while FD-B has no
  # local transport socket yet. Use the controller's durable node view here;
  # the post-promotion phase uses the new local transport socket directly.
  if ! g6rd_wait_until_deadline 90 3 "four smoke Agents connected" \
    g6rd_capture_agent_readiness "${NODES_FILE}"; then
    g6rd_report_agent_readiness
    return 1
  fi
  local node key="g6-load-${RUN_ID}-smoke" out="${G6RD_OUTBOX}/smoke-session"
  mkdir -p "${out}"
  cp -f "${G6RD_STATE}/agent-readiness-last.json" "${out}/connections.json"
  node="$(awk -F'\t' '$1 == "g6-fd-b-01" {print $2}' "${NODES_FILE}")"
  [[ -n "${node}" ]] || { echo "cross-FD smoke node is absent" >&2; return 1; }
  g6rd_enqueue_command "${node}" "${key}"
  # A relay delivery can cross the Worker's ordinary-send ambiguity window.
  # Do not treat the intermediate `unknown` state as the smoke success point;
  # wait through reconciliation for the one durable successful Agent result.
  g6rd_wait_until_deadline 120 2 "cross-FD smoke command result" \
    smoke_command_succeeded "${key}" "${node}"
  capture_relay_command_proof "${key}" "${node}" "${out}/command-proof.json"
  cp -f "${NODES_FILE}" "${out}/nodes.tsv"
  printf '%s\n' "${G6RD_CANDIDATE_SHA}" >"${out}/candidate-sha"
}

smoke_command_succeeded() {
  local key="${1:?idempotency key is required}" node="${2:?node id is required}"
  [[ "$(psql_primary_probe -Atc \
    "SELECT count(*) FROM commands AS command
     JOIN agent_command_results AS result ON result.command_id=command.id
     WHERE command.idempotency_key='${key}' AND command.node_id='${node}'
       AND command.state='succeeded' AND result.state='succeeded'")" == 1 ]]
}

phase_smoke_evidence() {
  local out="${G6RD_OUTBOX}/smoke-final" args=()
  require_file "${G6RD_STATE}/promoted-at"
  mkdir -p "${out}/evidence"
  readarray -t args < <(node_ids)
  g6rd_probe_node_connection any "${args[@]}" >"${out}/evidence/post-promotion-connections.json"
  cp -f "${G6RD_STATE}/era2-sessions.tsv" "${out}/evidence/era2-sessions.tsv"
  cp -f "${G6RD_STATE}/promoted-at" "${out}/evidence/promoted-at"
  stop_watchers
  jq -cn --arg candidate "${G6RD_CANDIDATE_SHA}" --arg environment "${G6RD_ENVIRONMENT_ID}" \
    --arg domain "${FD_ID}" --argjson agents "$(managed_node_count)" \
    '{schema_version:"ocservia.g6-smoke-observations.v1",profile:"smoke",candidate_sha:$candidate,environment_id:$environment,failure_domain:$domain,claims:{managed_agents:$agents,primary_promoted:true,post_promotion_sessions:true,raw_evidence_frozen:true}}' \
    >"${out}/smoke-observations.json"
  g6rd_now >"${out}/evidence/frozen-at"
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
  capture_local_database_clock >"${G6RD_STATE}/promoted-at"

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
  if ! g6rd_wait_until_deadline 180 5 "load commands reconciled" load_commands_settled; then
    report_load_command_timeout
    return 1
  fi
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
  g6rd_probe_node_connection any "${args[@]}" 2>/dev/null \
    | jq -e '
      .all_matched == true
      and (.observations | length > 0)
      and all(.observations[];
        .owner_epoch > 0
        and (.session_expires_at | type == "string" and length > 0)
        and (.negotiated_capabilities | index("ocserv.fencing.v2") != null))
    ' >/dev/null
}

report_node_connection_timeout() {
  local args=() response="${G6RD_STATE}/node-connections-timeout.json"
  local error="${G6RD_STATE}/node-connections-timeout-error.txt"
  readarray -t args < <(node_ids)
  echo "last era-2 transport connection probe:" >&2
  if G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_probe_node_connection any "${args[@]}" >"${response}" 2>"${error}"; then
    jq -c '{all_matched, observations: [.observations[] | {
      node_id, path, owner_epoch, authorization_revision,
      negotiated_capabilities, session_expires_at, last_seen
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

report_load_command_timeout() {
  echo "load command state matrix at reconciliation timeout:" >&2
  psql_primary -F $'\t' -Atc \
    "SELECT command.state,operation.state,
       outbox.published_at IS NOT NULL AS published,
       outbox.locked_by IS NOT NULL AS locked,
       lease.command_id IS NOT NULL AS leased,
       count(*)
     FROM commands AS command
     JOIN operations AS operation ON operation.id=command.operation_id
     JOIN outbox_events AS outbox ON outbox.command_id=command.id
     LEFT JOIN node_command_leases AS lease ON lease.command_id=command.id
     WHERE command.idempotency_key LIKE 'g6-load-${RUN_ID}-%'
     GROUP BY 1,2,3,4,5
     ORDER BY 1,2,3,4,5" >&2 || {
    echo "load command state matrix unavailable" >&2
  }
  echo "unsettled load command sample (node, command, operation, published, locked, leased, attempts, results, updated):" >&2
  psql_primary -F $'\t' -Atc \
    "SELECT command.node_id,command.state,operation.state,
       outbox.published_at IS NOT NULL,
       outbox.locked_by IS NOT NULL,
       lease.command_id IS NOT NULL,
       outbox.attempts,
       (SELECT count(*) FROM agent_command_results AS result WHERE result.command_id=command.id),
       command.updated_at
     FROM commands AS command
     JOIN operations AS operation ON operation.id=command.operation_id
     JOIN outbox_events AS outbox ON outbox.command_id=command.id
     LEFT JOIN node_command_leases AS lease ON lease.command_id=command.id
     WHERE command.idempotency_key LIKE 'g6-load-${RUN_ID}-%'
       AND command.state NOT IN ('succeeded','failed','rejected','unknown','expired','rolled_back','superseded')
     ORDER BY command.updated_at,command.id
     LIMIT 20" >&2 || {
    echo "unsettled load command sample unavailable" >&2
  }
}

promoted_and_writable() {
  [[ "$(G6_DB_PORT=5432 g6rd_psql -Atc 'SELECT pg_is_in_recovery()' 2>/dev/null)" == f ]]
}

load_commands_settled() {
  local unsettled
  unsettled="$(psql_primary -Atc \
    "SELECT count(*) FROM commands WHERE idempotency_key LIKE 'g6-load-${RUN_ID}-%' AND state NOT IN ('succeeded','failed','rejected','unknown','expired','rolled_back','superseded')")"
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
# prove the replacement's higher epoch and a real fenced maintenance commit,
# then prove the old exact term cannot write the same durable marker.
phase_scenario_scheduler() {
  local leader
  leader="$(G6RD_PSQL_TIMEOUT_SECONDS=5 psql_primary -Atc \
    "SELECT instance_id||':'||incarnation||':'||epoch FROM scheduler_leadership WHERE id=1")"
  local old_instance old_incarnation old_epoch
  IFS=: read -r old_instance old_incarnation old_epoch <<<"${leader}"
  SCHEDULER_OLD_EPOCH="${old_epoch}"
  export SCHEDULER_OLD_EPOCH
  g6rd_compose stop scheduler
  g6rd_timeline_event scheduler_a_paused
  g6rd_wait_until_deadline 120 2 "old scheduler lease lapsed" \
    scheduler_lease_lapsed
  g6rd_compose up --detach scheduler
  g6rd_wait_until_deadline 120 2 \
    "replacement scheduler acquired leadership" scheduler_replaced
  g6rd_timeline_event scheduler_b_acquired
  local replacement_term
  replacement_term="$(G6RD_PSQL_TIMEOUT_SECONDS=5 psql_primary -Atc \
    "SELECT instance_id||':'||incarnation||':'||epoch FROM scheduler_leadership WHERE id=1")"
  IFS=: read -r SCHEDULER_REPLACEMENT_INSTANCE \
    SCHEDULER_REPLACEMENT_INCARNATION SCHEDULER_REPLACEMENT_EPOCH \
    <<<"${replacement_term}"
  export SCHEDULER_REPLACEMENT_INSTANCE SCHEDULER_REPLACEMENT_INCARNATION \
    SCHEDULER_REPLACEMENT_EPOCH
  [[ -n "${SCHEDULER_REPLACEMENT_INSTANCE}" \
    && "${SCHEDULER_REPLACEMENT_INCARNATION}" =~ ^[1-9][0-9]*$ \
    && "${SCHEDULER_REPLACEMENT_EPOCH}" =~ ^[1-9][0-9]*$ \
    && "${SCHEDULER_REPLACEMENT_EPOCH}" -gt "${old_epoch}" ]] || {
    echo "replacement scheduler term is malformed" >&2
    return 1
  }
  printf '%s\n' "${replacement_term}" >"${G6RD_STATE}/scheduler-replacement-term"
  rm -f -- "${G6RD_STATE}/scheduler-maintenance-observation.json"
  g6rd_wait_until_deadline 60 1 \
    "replacement scheduler completed exact-term fenced maintenance" \
    scheduler_maintenance_completed
  g6rd_timeline_event scheduler_a_resumed

  # Exercise the durable recorder through the restricted runtime role. A
  # predicate-only check cannot prove that the stale transaction itself is
  # rejected.
  local stale_log="${G6RD_LOGS}/stale-scheduler-maintenance.log"
  if G6RD_PSQL_TIMEOUT_SECONDS=10 psql_primary -v ON_ERROR_STOP=1 -c \
    "SET ROLE ocservia_app; SELECT public.g6_record_scheduler_maintenance(\
      '${old_instance}'::uuid,${old_incarnation},${old_epoch});" \
    >"${stale_log}" 2>&1; then
    echo "the expired scheduler term committed a maintenance marker" >&2
    return 1
  fi
  grep -qF 'scheduler maintenance term is not the exact live leader' \
    "${stale_log}" || {
    echo "the stale scheduler maintenance transaction failed for an unexpected reason" >&2
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
  [[ "$(G6RD_PSQL_TIMEOUT_SECONDS=5 psql_primary -Atc \
    'SELECT (lease_until <= clock_timestamp())::text FROM scheduler_leadership WHERE id=1')" == "t" ]]
}

scheduler_replaced() {
  local epoch
  epoch="$(G6RD_PSQL_TIMEOUT_SECONDS=5 psql_primary -Atc \
    'SELECT epoch FROM scheduler_leadership WHERE id=1')"
  [[ "${epoch}" =~ ^[0-9]+$ ]] && ((epoch > SCHEDULER_OLD_EPOCH))
}

scheduler_maintenance_completed() {
  local output="${G6RD_STATE}/scheduler-maintenance-observation.json"
  local temporary="${output}.tmp.$$"
  [[ ! -e "${output}" ]] || return 0
  rm -f -- "${temporary}"
  if ! G6RD_PSQL_TIMEOUT_SECONDS=5 psql_primary_probe -qAtc \
    "WITH marker AS MATERIALIZED (
       SELECT maintenance_id,instance_id,incarnation,epoch,completed_at
       FROM g6_scheduler_maintenance_history
       WHERE instance_id='${SCHEDULER_REPLACEMENT_INSTANCE}'
         AND incarnation=${SCHEDULER_REPLACEMENT_INCARNATION}
         AND epoch=${SCHEDULER_REPLACEMENT_EPOCH}
       ORDER BY maintenance_id
       LIMIT 1
     ), observed AS MATERIALIZED (
       SELECT clock_timestamp() AS at
     )
     SELECT jsonb_build_object(
       'maintenance_id',marker.maintenance_id::text,
       'instance_id',marker.instance_id::text,
       'incarnation',marker.incarnation::text,
       'epoch',marker.epoch,
       'marker_completed_at',to_char(marker.completed_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),
       'committed_observed_at',to_char(observed.at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')
     )
     FROM marker CROSS JOIN observed
     WHERE marker.completed_at<=observed.at" >"${temporary}"; then
    rm -f -- "${temporary}"
    return 1
  fi
  if ! jq -e \
    --arg instance "${SCHEDULER_REPLACEMENT_INSTANCE}" \
    --arg incarnation "${SCHEDULER_REPLACEMENT_INCARNATION}" \
    --argjson epoch "${SCHEDULER_REPLACEMENT_EPOCH}" \
    'keys == ["committed_observed_at","epoch","incarnation","instance_id","maintenance_id","marker_completed_at"]
     and (.maintenance_id | type == "string" and test("^[1-9][0-9]*$"))
     and .instance_id == $instance
     and .incarnation == $incarnation
     and .epoch == $epoch
     and (.marker_completed_at | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{6}Z$"))
     and (.committed_observed_at | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{6}Z$"))
     and .marker_completed_at <= .committed_observed_at' \
    "${temporary}" >/dev/null; then
    rm -f -- "${temporary}"
    return 1
  fi
  mv -f -- "${temporary}" "${output}"
}

capture_live_owner_terms() {
  local all_file="${G6RD_STATE}/owner-all-terms.tsv"
  local selected_file="${G6RD_STATE}/owner-a-terms.tsv"
  local all_tmp selected_tmp node node_hex sql_nodes="" separator=""
  local instance incarnation connection epoch lease_us extra sample_count=0 expected_count
  local snapshot_valid=1
  local -a managed_nodes=()
  local -A expected_nodes=() seen_nodes=() owner_rows=()
  expected_count="$(managed_node_count)" || return 1
  mapfile -t managed_nodes < <(node_ids)
  [[ "${#managed_nodes[@]}" == "${expected_count}" ]] || {
    echo "the owner scenario must cover exactly ${expected_count} managed nodes, found ${#managed_nodes[@]}" >&2
    return 1
  }
  for node in "${managed_nodes[@]}"; do
    [[ "${node}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
      echo "invalid managed node id in owner scenario: ${node}" >&2
      return 1
    }
    node_hex="${node//-/}"
    [[ -z "${expected_nodes[${node_hex}]:-}" ]] || {
      echo "duplicate managed node id in owner scenario: ${node}" >&2
      return 1
    }
    expected_nodes["${node_hex}"]=1
    sql_nodes+="${separator}'${node_hex}'"
    separator=,
  done
  all_tmp="$(mktemp "${G6RD_STATE}/owner-all-terms.XXXXXX")"
  selected_tmp="$(mktemp "${G6RD_STATE}/owner-a-terms.XXXXXX")"
  if ! psql_primary_probe -F $'\t' -Atc \
    "WITH cut AS MATERIALIZED (SELECT clock_timestamp() AS at)
     SELECT encode(node_id,'hex'),owner_instance_id,owner_incarnation,
       encode(connection_id,'hex'),owner_epoch,
       floor(extract(epoch FROM lease_until)*1000000)::bigint
     FROM connection_owner_fencing CROSS JOIN cut
     WHERE lease_until>cut.at
       AND encode(node_id,'hex') IN (${sql_nodes})
     ORDER BY encode(node_id,'hex')" >"${all_tmp}"; then
    rm -f -- "${all_tmp}" "${selected_tmp}"
    echo "failed to freeze the live managed owner population" >&2
    return 1
  fi
  while IFS=$'\t' read -r node_hex instance incarnation connection epoch lease_us extra; do
    [[ "${node_hex}" =~ ^[0-9a-f]{32}$ \
      && "${instance}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ \
      && "${incarnation}" =~ ^[1-9][0-9]*$ \
      && "${connection}" =~ ^[0-9a-f]{32}$ \
      && "${epoch}" =~ ^[1-9][0-9]*$ \
      && "${lease_us}" =~ ^[1-9][0-9]*$ && -z "${extra}" \
      && -n "${expected_nodes[${node_hex}]:-}" \
      && -z "${seen_nodes[${node_hex}]:-}" ]] || {
      snapshot_valid=0
      break
    }
    seen_nodes["${node_hex}"]=1
    owner_rows["${node_hex}"]="${node_hex}:${instance}:${incarnation}:${connection}:${epoch}"
  done <"${all_tmp}"
  ((snapshot_valid == 1)) || {
    rm -f -- "${all_tmp}" "${selected_tmp}"
    echo "the live owner snapshot is malformed, duplicated, or outside the managed population" >&2
    return 1
  }
  [[ "${#seen_nodes[@]}" == "${#managed_nodes[@]}" ]] || {
    rm -f -- "${all_tmp}" "${selected_tmp}"
    echo "the live owner snapshot does not cover every managed node" >&2
    return 1
  }
  while IFS= read -r node; do
    node_hex="${node//-/}"
    [[ -n "${owner_rows[${node_hex}]:-}" ]] || {
      rm -f -- "${all_tmp}" "${selected_tmp}"
      echo "local stale-probe node has no frozen owner term: ${node}" >&2
      return 1
    }
    printf '%s\n' "${owner_rows[${node_hex}]}" >>"${selected_tmp}"
    sample_count=$((sample_count + 1))
    ((sample_count == 5)) && break
  done < <(awk -F'\t' -v prefix="g6-${FD_ID}-" \
    'index($1, prefix) == 1 {print $2}' "${NODES_FILE}")
  ((sample_count == 5)) || {
    rm -f -- "${all_tmp}" "${selected_tmp}"
    echo "fewer than five local authoritative owners are available for stale probes" >&2
    return 1
  }
  mv -f -- "${all_tmp}" "${all_file}"
  mv -f -- "${selected_tmp}" "${selected_file}"
}

owner_replacement_values() {
  local node_hex _instance _incarnation connection old_epoch lease_us extra
  local values="" separator=""
  [[ -s "${G6RD_STATE}/owner-all-terms.tsv" ]] || return 1
  while IFS=$'\t' read -r node_hex _instance _incarnation connection old_epoch lease_us extra; do
    [[ "${node_hex}" =~ ^[0-9a-f]{32}$ \
      && "${connection}" =~ ^[0-9a-f]{32}$ \
      && "${old_epoch}" =~ ^[1-9][0-9]*$ \
      && "${lease_us}" =~ ^[1-9][0-9]*$ && -z "${extra}" ]] || return 1
    values+="${separator}(decode('${node_hex}','hex'),${old_epoch}::bigint,decode('${connection}','hex'))"
    separator=,
  done <"${G6RD_STATE}/owner-all-terms.tsv"
  [[ -n "${values}" ]] || return 1
  printf '%s\n' "${values}"
}

owner_expiry_values() {
  local node_hex _instance _incarnation connection old_epoch lease_us extra
  local values="" separator=""
  [[ -s "${G6RD_STATE}/owner-all-terms.tsv" ]] || return 1
  while IFS=$'\t' read -r node_hex _instance _incarnation connection old_epoch lease_us extra; do
    [[ "${node_hex}" =~ ^[0-9a-f]{32}$ \
      && "${connection}" =~ ^[0-9a-f]{32}$ \
      && "${old_epoch}" =~ ^[1-9][0-9]*$ \
      && "${lease_us}" =~ ^[1-9][0-9]*$ && -z "${extra}" ]] || return 1
    values+="${separator}(decode('${node_hex}','hex'),${old_epoch}::bigint,decode('${connection}','hex'),${lease_us}::bigint)"
    separator=,
  done <"${G6RD_STATE}/owner-all-terms.tsv"
  [[ -n "${values}" ]] || return 1
  printf '%s\n' "${values}"
}

owner_leases_lapsed() {
  local values expired expected_count
  values="$(owner_expiry_values)" || return 1
  expected_count="$(managed_node_count)" || return 1
  [[ "$(wc -l <"${G6RD_STATE}/owner-all-terms.tsv" | tr -d '[:space:]')" == "${expected_count}" ]] || return 1
  expired="$(psql_primary_probe -Atc \
    "WITH expected(node_id,old_epoch,old_connection_id,frozen_lease_us) AS (VALUES ${values})
     SELECT count(*)
     FROM expected
     JOIN connection_owner_fencing AS current USING (node_id)
     WHERE current.owner_epoch=expected.old_epoch
       AND current.connection_id=expected.old_connection_id
       AND floor(extract(epoch FROM current.lease_until)*1000000)::bigint>=expected.frozen_lease_us
       AND current.lease_until<=clock_timestamp()")" || return 1
  [[ "${expired}" == "${expected_count}" ]]
}

owner_replaced() {
  local values advanced expected_count
  values="$(owner_replacement_values)" || return 1
  expected_count="$(managed_node_count)" || return 1
  [[ "$(wc -l <"${G6RD_STATE}/owner-all-terms.tsv" | tr -d '[:space:]')" == "${expected_count}" ]] || return 1
  advanced="$(psql_primary_probe -Atc \
    "WITH expected(node_id,old_epoch,old_connection_id) AS (VALUES ${values})
     SELECT count(*)
     FROM expected
     JOIN connection_owner_fencing AS current USING (node_id)
     WHERE current.owner_epoch>expected.old_epoch
       AND current.connection_id<>expected.old_connection_id
       AND current.lease_until>clock_timestamp()")" || return 1
  [[ "${advanced}" == "${expected_count}" ]]
}

capture_owner_replacement_sessions() {
  local values expected_count terms_output terms_tmp sessions_output sessions_tmp
  local boundary_file="${G6RD_STATE}/owner-b-acquired-at"
  local -a args=()
  values="$(owner_replacement_values)" || return 1
  expected_count="$(managed_node_count)" || return 1
  terms_output="${G6RD_STATE}/owner-b-terms.tsv"
  sessions_output="${G6RD_STATE}/owner-replacement-sessions.json"
  terms_tmp="$(mktemp "${G6RD_STATE}/owner-b-terms.XXXXXX")"
  sessions_tmp="$(mktemp "${G6RD_STATE}/owner-replacement-sessions.XXXXXX")"
  if ! psql_primary_probe -F $'\t' -Atc \
    "WITH expected(node_id,old_epoch,old_connection_id) AS (VALUES ${values})
     SELECT encode(current.node_id,'hex'),current.owner_instance_id,
       current.owner_incarnation,encode(current.connection_id,'hex'),
       current.owner_epoch,
       to_char(acquired.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')
     FROM expected
     JOIN connection_owner_fencing AS current USING (node_id)
     JOIN LATERAL (
       SELECT history.updated_at
       FROM g6_connection_owner_history AS history
       WHERE history.node_id=current.node_id
         AND history.owner_instance_id=current.owner_instance_id
         AND history.owner_incarnation=current.owner_incarnation
         AND history.connection_id=current.connection_id
         AND history.owner_epoch=current.owner_epoch
       ORDER BY history.history_id
       LIMIT 1
     ) AS acquired ON true
     WHERE current.owner_epoch>expected.old_epoch
       AND current.connection_id<>expected.old_connection_id
       AND current.lease_until>clock_timestamp()
     ORDER BY encode(current.node_id,'hex')" >"${terms_tmp}"; then
    rm -f -- "${terms_tmp}" "${sessions_tmp}"
    echo "could not freeze replacement owner acquisition terms" >&2
    return 1
  fi
  [[ "$(wc -l <"${terms_tmp}" | tr -d '[:space:]')" == "${expected_count}" ]] || {
    rm -f -- "${terms_tmp}" "${sessions_tmp}"
    echo "replacement owner terms do not cover the exact managed population" >&2
    return 1
  }
  mapfile -t args < <(node_ids)
  [[ "${#args[@]}" == "${expected_count}" ]] || {
    rm -f -- "${terms_tmp}" "${sessions_tmp}"
    return 1
  }
  if ! G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=30 \
    g6rd_probe_node_connection any "${args[@]}" >"${sessions_tmp}"; then
    rm -f -- "${terms_tmp}" "${sessions_tmp}"
    echo "could not freeze replacement-owner transport sessions" >&2
    return 1
  fi
  g6rd_atomic_now "${boundary_file}"
  if ! jq -e --arg boundary "$(<"${boundary_file}")" \
    --argjson expected_count "${expected_count}" \
    --rawfile managed_tsv "${NODES_FILE}" --rawfile terms_tsv "${terms_tmp}" '
      def stamp_key:
        capture("^(?<whole>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $stamp
        | $stamp.whole + "." + ((($stamp.fraction // "") + "000000000")[0:9]);
      ($managed_tsv | split("\n") | map(select(length > 0) | split("\t"))
        | map(select(length == 3) | {key: .[1], value: .[2]}) | from_entries) as $managed
      | ($terms_tsv | split("\n") | map(select(length > 0) | split("\t"))
        | map(select(length == 6) | {key: .[0], value: {
          instance: .[1], incarnation: .[2], connection: .[3],
          epoch: (.[4] | tonumber), registered_at: .[5]}}) | from_entries) as $terms
      | ($managed | keys | sort) as $managed_nodes
      | .mode == "node_connection" and .expected_path == "any"
        and .all_matched == true
        and ($managed_nodes | length) == $expected_count
        and ($terms | length) == $expected_count
        and (.observations | type == "array" and length == $expected_count)
        and ([.observations[].node_id] | sort) == $managed_nodes
        and ([.observations[].node_id] | unique | length) == $expected_count
        and all(.observations[];
          (.node_id | gsub("-"; "")) as $node_hex
          | .found == true and .matched == true
          and .endpoint_id == $managed[.node_id]
          and (.owner_fence_id | type == "string" and test("^[0-9a-f]{32}$"))
          and .owner_instance_id == $terms[$node_hex].instance
          and .owner_incarnation == $terms[$node_hex].incarnation
          and .connection_id == $terms[$node_hex].connection
          and .owner_epoch == $terms[$node_hex].epoch
          and (.negotiated_capabilities | type == "array"
            and index("ocserv.fencing.v2") != null)
          and ((.connected_at | stamp_key) >= ($terms[$node_hex].registered_at | stamp_key))
          and ((.connected_at | stamp_key) <= ($boundary | stamp_key)))
    ' "${sessions_tmp}" >/dev/null; then
    rm -f -- "${terms_tmp}" "${sessions_tmp}" "${boundary_file}"
    echo "replacement-owner sessions are incomplete or not bound to durable acquisition terms" >&2
    return 1
  fi
  mv -f -- "${terms_tmp}" "${terms_output}"
  mv -f -- "${sessions_tmp}" "${sessions_output}"
}

report_owner_replacement_timeout() {
  local values
  values="$(owner_replacement_values)" || {
    echo "the frozen owner population is unavailable for timeout diagnostics" >&2
    return 1
  }
  echo "managed owner terms that did not advance after the transport bounce:" >&2
  if ! psql_primary_probe -F $'\t' -Atc \
    "WITH expected(node_id,old_epoch,old_connection_id) AS (VALUES ${values})
     SELECT encode(expected.node_id,'hex'),expected.old_epoch,
       COALESCE(current.owner_epoch,0),
       COALESCE(encode(current.connection_id,'hex'),'missing'),
       COALESCE(to_char(current.lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),'missing'),
       to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')
     FROM expected
     LEFT JOIN connection_owner_fencing AS current USING (node_id)
     WHERE current.node_id IS NULL OR current.owner_epoch<=expected.old_epoch
       OR current.connection_id=expected.old_connection_id
       OR current.lease_until<=clock_timestamp()
     ORDER BY expected.node_id" >&2; then
    echo "owner replacement timeout diagnostics were unavailable" >&2
    return 1
  fi
}

validate_reconnect_sessions() {
  local sessions_file="${1:?session inventory is required}"
  local bulk_disconnect_file="${2:?bulk disconnect stamp is required}"
  local bulk_disconnect expected_count
  [[ -s "${sessions_file}" && -s "${bulk_disconnect_file}" ]] || return 1
  bulk_disconnect="$(<"${bulk_disconnect_file}")"
  expected_count="$(managed_node_count)" || return 1
  jq -e --arg bulk_disconnect "${bulk_disconnect}" \
    --argjson expected_count "${expected_count}" --rawfile managed_tsv "${NODES_FILE}" '
      def stamp_key:
        capture("^(?<whole>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $stamp
        | $stamp.whole + "." + ((($stamp.fraction // "") + "000000000")[0:9]);
      ($managed_tsv | split("\n") | map(select(length > 0) | split("\t"))
        | map(select(length == 3) | {key: .[1], value: .[2]}) | from_entries) as $managed
      | ($managed | keys | sort) as $managed_nodes
      | .all_matched == true
        and .expected_path == "any"
        and ($managed_nodes | length) == $expected_count
        and (.observations | type == "array" and length == $expected_count)
        and ([.observations[].node_id] | sort) == $managed_nodes
        and ([.observations[].node_id] | unique | length) == $expected_count
        and all(.observations[];
          .found == true and .matched == true
          and (.node_id | type == "string")
          and (.endpoint_id == $managed[.node_id])
          and (.agent_instance_id | type == "string" and test("^[0-9a-f]{32}$"))
          and (.owner_fence_id | type == "string" and test("^[0-9a-f]{32}$"))
          and (.owner_instance_id | type == "string"
            and test("^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"))
          and (.owner_incarnation | type == "string" and test("^[1-9][0-9]*$"))
          and (.connection_id | type == "string" and test("^[0-9a-f]{32}$"))
          and (.owner_epoch | type == "number" and floor == . and . > 0)
          and (.authorization_revision | type == "number" and floor == . and . >= 0)
          and (.negotiated_capabilities | type == "array"
            and index("ocserv.fencing.v2") != null)
          and (.path == "direct" or .path == "relay")
          and ((.connected_at | stamp_key) > ($bulk_disconnect | stamp_key))
          and ((.owner_lease_until | stamp_key) > (.last_seen | stamp_key))
          and ((.session_expires_at | stamp_key) > (.last_seen | stamp_key)))
    ' "${sessions_file}" >/dev/null
}

capture_reconnect_sessions() {
  local bulk_disconnect_file="${1:?bulk disconnect stamp is required}"
  local output="${G6RD_STATE}/reconnect-sessions.json" temporary expected_count
  local -a args=()
  expected_count="$(managed_node_count)" || return 1
  mapfile -t args < <(node_ids)
  [[ "${#args[@]}" == "${expected_count}" ]] || {
    echo "the reconnect inventory must cover exactly ${expected_count} managed nodes" >&2
    return 1
  }
  temporary="$(mktemp "${G6RD_STATE}/reconnect-sessions.XXXXXX")"
  if ! G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=30 \
    g6rd_probe_node_connection any "${args[@]}" >"${temporary}"; then
    rm -f -- "${temporary}"
    echo "the post-storm transport inventory could not be captured" >&2
    if ! report_node_connection_timeout; then
      echo "post-storm connection diagnostics were unavailable" >&2
    fi
    return 1
  fi
  if ! validate_reconnect_sessions "${temporary}" "${bulk_disconnect_file}"; then
    echo "the post-storm transport inventory is incomplete or not causally after the disconnect" >&2
    jq -c '{all_matched, observations: [.observations[]? | {
      node_id, endpoint_id, connected_at, last_seen, owner_epoch,
      owner_instance_id, owner_incarnation, connection_id,
      owner_lease_until, session_expires_at
    }]}' "${temporary}" >&2 || echo "the invalid reconnect inventory is not JSON" >&2
    rm -f -- "${temporary}"
    return 1
  fi
  mv -f -- "${temporary}" "${output}"
  g6rd_now >"${G6RD_STATE}/reconnect-sessions-captured-at"
}

phase_scenario_owner() {
  capture_live_owner_terms
  g6rd_export_common_env
  # Crash the owner process without running its graceful shutdown path. The
  # frozen terms must remain in PostgreSQL until their leases expire naturally
  # so the replacement proves lease-expiry takeover for every managed Agent.
  G6RD_COMPOSE_TIMEOUT_SECONDS=15 g6rd_compose kill --signal KILL worker
  g6rd_timeline_event owner_a_paused
  g6rd_wait_until_deadline 60 1 "all frozen owner leases expired" \
    owner_leases_lapsed
  g6rd_compose up --detach worker
  g6rd_wait_until_deadline 30 1 "replacement worker trust socket" \
    g6rd_compose exec -T worker test -S /run/ocserv-trust/control-plane.sock
  # A worker restart alone does not sever existing Iroh sessions. Bounce the
  # one active controller endpoint after every frozen lease expires so every
  # local and peer Agent must register a higher owner epoch. This bounce is
  # separate from the later measured reconnect storm and stale-Agent probe.
  g6rd_compose stop transportd
  g6rd_compose up --detach transportd
  g6rd_wait_until_deadline 30 1 "transportd ready for owner replacement" \
    g6rd_compose exec -T transportd test -S /run/ocserv-platform/transportd.sock
  if ! g6rd_wait_until_deadline 60 1 \
    "all managed owners registered higher epochs" owner_replaced; then
    if ! report_owner_replacement_timeout; then
      echo "owner replacement timeout diagnostics were unavailable" >&2
    fi
    return 1
  fi
  if ! g6rd_wait_until_deadline 60 2 \
    "all Agents connected through replacement owners" all_nodes_connected; then
    if ! report_node_connection_timeout; then
      echo "replacement-owner connection diagnostics were unavailable" >&2
    fi
    return 1
  fi
  capture_owner_replacement_sessions
  g6rd_timeline_event owner_b_acquired "${G6RD_STATE}/owner-b-acquired-at"
  g6rd_timeline_event owner_a_resumed
  # enforcement point 1: transportd returns Stale with the retained epoch
  local first epoch target_node target_endpoint
  first="$(head -1 "${G6RD_STATE}/owner-a-terms.tsv")"
  epoch="$(printf '%s' "${first}" | cut -d: -f5)"
  target_node="$(node_from_fencing_hex "$(printf '%s' "${first}" | cut -d: -f1)")"
  target_endpoint="$(awk -F'\t' -v id="${target_node}" '$2 == id {print $3; exit}' "${NODES_FILE}")"
  [[ "${target_endpoint}" =~ ^[0-9a-f]{64}$ ]] || {
    echo "agent endpoint for owner-fencing target ${target_node} is invalid" >&2
    return 1
  }
  local retained
  retained="$(psql_primary -Atc \
    "SELECT owner_epoch FROM connection_owner_fencing WHERE node_id=decode('$(printf '%s' "${first}" | cut -d: -f1)','hex')")"
  g6rd_compose --profile probe run --rm --no-deps g6-probe uds-stale-fence \
    --socket /run/ocserv-platform/transportd.sock \
    --signing-key-file /run/ocservia-signing/command-signing.pem \
    --node-id "${target_node}" \
    --endpoint-id "${target_endpoint}" \
    --owner-instance-id "$(printf '%s' "${first}" | cut -d: -f2)" \
    --owner-incarnation "$(printf '%s' "${first}" | cut -d: -f3)" \
    --stale-epoch "${epoch}" \
    --expect-retained-epoch "${retained}" \
    >"${G6RD_STATE}/stale-transport-probe.json"
  jq -e '.status == "rejected"' "${G6RD_STATE}/stale-transport-probe.json" >/dev/null
  g6rd_timeline_event stale_transport_rejected
  # enforcement point 2 + the reconnect storm: stop transportd, let the
  # probe hold the controller endpoint for one bounded window, restart
  local bulk_disconnect_file="${G6RD_STATE}/bulk-disconnect-at"
  g6rd_atomic_now "${bulk_disconnect_file}"
  g6rd_compose stop transportd
  g6rd_timeline_event bulk_disconnect_injected "${bulk_disconnect_file}"
  g6rd_compose --profile probe run --rm --no-deps \
    -e RUST_LOG=info g6-probe agent-stale-command \
    --signing-key-file /run/ocservia-signing/command-signing.pem \
    --controller-key-file /run/ocservia-controller/controller.key \
    --node-id "${target_node}" \
    --endpoint-id "${target_endpoint}" \
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
  if ! g6rd_wait_until_deadline 180 5 \
    "all agents reconnected after the storm" all_nodes_connected; then
    if ! report_node_connection_timeout; then
      echo "post-storm connection diagnostics were unavailable" >&2
    fi
    return 1
  fi
  capture_reconnect_sessions "${bulk_disconnect_file}"
  capture_database_clock >"${G6RD_STATE}/reconnect-completed-at"
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
phase_relay_pre_fault() {
  local readiness="${1:?relay observation readiness is required}"
  local cross_vm_node out observation before key command_id disabled_at temporary
  validate_relay_a_only_readiness "${readiness}" || {
    echo "relay-a-only readiness is invalid or substituted" >&2
    return 1
  }
  cross_vm_node="$(<"${readiness}/node-id")"
  [[ "$(awk -F'\t' -v node="${cross_vm_node}" \
    '$1 == "g6-fd-a-01" && $2 == node {print $2; exit}' "${NODES_FILE}")" == "${cross_vm_node}" ]] || {
    echo "the controlled relay node is not the selected managed fd-a Agent" >&2
    return 1
  }
  out="${G6RD_OUTBOX}/relay-pre-fault"
  observation="${out}/relay-a-observation.json"
  before="${out}/relay-a-before-command.json"
  mkdir -p "${out}"
  rm -f -- "${observation}" "${before}"
  G6RD_COMPOSE_TIMEOUT_SECONDS=30 g6rd_compose stop relay
  g6rd_wait_until_deadline 30 1 "relay-b stopped before relay-a proof" relay_b_stopped
  disabled_at="$(capture_database_clock)"
  temporary="${out}/relay-b-disabled.json.$$"
  jq -cn --arg environment "${G6RD_ENVIRONMENT_ID}" \
    --arg candidate "${G6RD_CANDIDATE_SHA}" --arg node "${cross_vm_node}" \
    --arg disabled_at "${disabled_at}" '{
      schema_version:"ocservia.g6-relay-state.v1",
      environment_id:$environment,candidate_sha:$candidate,node_id:$node,
      relay:"relay-b",state:"stopped",disabled_at:$disabled_at
    }' >"${temporary}"
  mv -f -- "${temporary}" "${out}/relay-b-disabled.json"
  cp -f "${readiness}/relay-a-only-readiness.json" \
    "${out}/relay-a-only-readiness.json"
  G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_wait_until_deadline 90 5 "cross-VM session through relay-a before fault" \
    relay_probe_named relay-a "${cross_vm_node}" "${before}"
  key="g6-relay-pre-fault-${RUN_ID}"
  g6rd_enqueue_command "${cross_vm_node}" "${key}" \
    "${out}/relay-a-command-enqueue.jsonl"
  command_id="$(command_id_of_key "${key}")"
  [[ "${command_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
    echo "pre-fault relay command id is missing" >&2
    return 1
  }
  g6rd_wait_until_deadline 120 2 "pre-fault relay command result" \
    wait_commands_settled "${key}"
  capture_relay_dispatch_proof "${command_id}" "${cross_vm_node}" "${before}" \
    "${out}/relay-a-dispatch-proof.json" relay-a
  capture_relay_command_proof "${key}" "${cross_vm_node}" \
    "${out}/relay-a-command-proof.json"
  G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_wait_until_deadline 30 2 \
    "same relay-a session after authenticated command" \
    relay_probe_named relay-a "${cross_vm_node}" "${observation}"
  require_file "${observation}"
  relay_observations_same_session "${before}" "${observation}"
  capture_database_clock >"${out}/observed-at"
  printf '%s\n' "${cross_vm_node}" >"${out}/node-id"
  printf '%s\n' "${G6RD_CANDIDATE_SHA}" >"${out}/candidate-sha"
}

phase_scenario_relay() {
  local peer_ready="${1:?failure-domain A readiness directory is required}"
  local relay_failed_at="${peer_ready}/relay-a-failed-at"
  require_file "${relay_failed_at}"
  require_file "${peer_ready}/relay-fault-cut.json"
  local cross_vm_node observation_file before_file node_file temporary
  local key command_id proof_file dispatch_file active_at_file started_at started_file
  require_file "${G6RD_OUTBOX}/relay-pre-fault/relay-a-observation.json"
  require_file "${G6RD_OUTBOX}/relay-pre-fault/observed-at"
  require_file "${G6RD_OUTBOX}/relay-pre-fault/relay-a-only-readiness.json"
  require_file "${G6RD_OUTBOX}/relay-pre-fault/relay-b-disabled.json"
  cross_vm_node="$(<"${G6RD_OUTBOX}/relay-pre-fault/node-id")"
  [[ -n "${cross_vm_node}" ]] || {
    echo "no cross-failure-domain agent is available for the relay scenario" >&2
    return 1
  }
  observation_file="${G6RD_STATE}/relay-b-observation.json"
  before_file="${G6RD_STATE}/relay-b-before-command.json"
  node_file="${G6RD_STATE}/relay-b-node-id"
  temporary="${node_file}.$$"
  printf '%s\n' "${cross_vm_node}" >"${temporary}"
  mv -f -- "${temporary}" "${node_file}"
  rm -f -- "${observation_file}" "${before_file}"
  jq -e --arg environment "${G6RD_ENVIRONMENT_ID}" \
    --arg candidate "${G6RD_CANDIDATE_SHA}" --arg node "${cross_vm_node}" \
    --slurpfile topology "${G6RD_OUTBOX}/relay-pre-fault/relay-a-only-readiness.json" \
    --slurpfile cut "${peer_ready}/relay-fault-cut.json" '
      def stamp_key:
        capture("^(?<whole>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $stamp
        | $stamp.whole + "." + ((($stamp.fraction // "") + "000000000")[0:9]);
      keys == ["candidate_sha","disabled_at","environment_id","node_id",
        "relay","schema_version","state"]
      and .schema_version == "ocservia.g6-relay-state.v1"
      and .environment_id == $environment and .candidate_sha == $candidate
      and .node_id == $node and .relay == "relay-b" and .state == "stopped"
      and ($topology | length) == 1 and ($cut | length) == 1
      and $topology[0].candidate_sha == $candidate
      and $topology[0].node_id == $node
      and (($topology[0].topology_ready_at | stamp_key) <= (.disabled_at | stamp_key))
      and ((.disabled_at | stamp_key) < ($cut[0].cut_at | stamp_key))
    ' "${G6RD_OUTBOX}/relay-pre-fault/relay-b-disabled.json" >/dev/null || {
    echo "relay-b was not durably disabled for the controlled pre-fault proof" >&2
    return 1
  }
  relay_b_stopped || {
    echo "relay-b restarted before the fault artifact rendezvous completed" >&2
    return 1
  }
  g6rd_timeline_event relay_a_failed "${relay_failed_at}"
  phase_relay_up
  started_at="$(capture_database_clock)"
  jq -en --arg started_at "${started_at}" \
    --slurpfile cut "${peer_ready}/relay-fault-cut.json" '
      def stamp_key:
        capture("^(?<whole>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $stamp
        | $stamp.whole + "." + ((($stamp.fraction // "") + "000000000")[0:9]);
      ($cut | length) == 1 and (($started_at | stamp_key) > ($cut[0].cut_at | stamp_key))
    ' >/dev/null || {
    echo "relay-b did not start strictly after the promoted-database fault cut" >&2
    return 1
  }
  started_file="${G6RD_STATE}/relay-b-started.json"
  temporary="${started_file}.$$"
  jq -cn --arg environment "${G6RD_ENVIRONMENT_ID}" \
    --arg candidate "${G6RD_CANDIDATE_SHA}" --arg node "${cross_vm_node}" \
    --arg started_at "${started_at}" '{
      schema_version:"ocservia.g6-relay-state.v1",
      environment_id:$environment,candidate_sha:$candidate,node_id:$node,
      relay:"relay-b",state:"healthy",started_at:$started_at
    }' >"${temporary}"
  mv -f -- "${temporary}" "${started_file}"
  G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_wait_until_deadline 90 5 "cross-VM session through relay-b" \
    relay_probe_relay_b "${cross_vm_node}" "${before_file}"
  key="g6-relay-failover-${RUN_ID}"
  g6rd_enqueue_command "${cross_vm_node}" "${key}" \
    "${G6RD_STATE}/relay-command-enqueue.jsonl"
  command_id="$(command_id_of_key "${key}")"
  [[ "${command_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
    echo "relay failover command id is missing" >&2
    return 1
  }
  g6rd_wait_until_deadline 120 2 "relay failover command result" \
    wait_commands_settled "${key}"
  dispatch_file="${G6RD_STATE}/relay-dispatch-proof.json"
  capture_relay_dispatch_proof \
    "${command_id}" "${cross_vm_node}" "${before_file}" "${dispatch_file}" relay-b
  proof_file="${G6RD_STATE}/relay-command-proof.json"
  capture_relay_command_proof "${key}" "${cross_vm_node}" "${proof_file}"
  G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_wait_until_deadline 30 2 "same relay-b session after authenticated command" \
    relay_probe_relay_b "${cross_vm_node}" "${observation_file}"
  require_file "${observation_file}"
  relay_observations_same_session "${before_file}" "${observation_file}"
  active_at_file="${G6RD_STATE}/relay-b-active-at"
  capture_database_clock >"${active_at_file}"
  g6rd_timeline_event relay_b_active "${active_at_file}"
}

capture_relay_dispatch_proof() {
  local command_id="${1:?command id is required}" node="${2:?node id is required}"
  local observation="${3:?relay observation is required}"
  local output="${4:?dispatch proof destination is required}"
  local relay="${5:?relay name is required}"
  local logs="${output}.logs.$$" temporary="${output}.$$"
  local command_hex="${command_id//-/}" node_hex="${node//-/}"
  if ! G6RD_COMPOSE_TIMEOUT_SECONDS=15 g6rd_compose logs --no-color \
    --no-log-prefix transportd >"${logs}"; then
    rm -f -- "${logs}" "${temporary}"
    return 1
  fi
  if ! jq -eRsc --arg command "${command_hex}" --arg node "${node_hex}" '
    [split("\n")[] | fromjson? |
      select(.fields.event_type == "command_frame_written"
        and .fields.command_id == $command
        and .fields.node_id == $node) | .fields] as $matches
    | if ($matches | length) == 1 then $matches[0] else false end
  ' "${logs}" >"${temporary}"; then
    rm -f -- "${logs}" "${temporary}"
    return 1
  fi
  rm -f -- "${logs}"
  if ! jq -e --slurpfile observation "${observation}" --arg relay "${relay}" '
    .path == "relay"
    and (.path_detail | contains($relay))
    and (.owner_fence_id | test("^[0-9a-f]{32}$"))
    and (.connection_id | test("^[0-9a-f]{32}$"))
    and (.owner_epoch | type == "number" and floor == . and . > 0)
    and .owner_fence_id == ($observation[0].observations[0].owner_fence_id | gsub("-"; ""))
    and .connection_id == ($observation[0].observations[0].connection_id | gsub("-"; ""))
    and .owner_epoch == $observation[0].observations[0].owner_epoch
  ' "${temporary}" >/dev/null; then
    rm -f -- "${temporary}"
    echo "transportd did not log one exact relay frame write for the command" >&2
    return 1
  fi
  mv -f -- "${temporary}" "${output}"
}

relay_probe_named() {
  local relay="${1:?relay name is required}"
  local node="${2:?node id is required}"
  local output="${3:?relay observation destination is required}"
  local expected_endpoint temporary error_file error_temporary
  expected_endpoint="$(awk -F'\t' -v node="${node}" '$2 == node {print $3; exit}' "${NODES_FILE}")"
  [[ "${expected_endpoint}" =~ ^[0-9a-f]{64}$ ]] || return 1
  temporary="${output}.$$"
  error_file="${output}.last-error.log"
  error_temporary="${error_file}.$$"
  if ! g6rd_probe_node_connection relay "${node}" \
    >"${temporary}" 2>"${error_temporary}"; then
    mv -f -- "${error_temporary}" "${error_file}"
    rm -f -- "${temporary}"
    return 1
  fi
  if ! jq -e --arg node "${node}" --arg endpoint "${expected_endpoint}" \
    --arg relay "${relay}" '
    .mode == "node_connection"
    and .expected_path == "relay"
    and .all_matched == true
    and (.observations | length == 1)
    and (.observations[0] |
      .node_id == $node
      and .endpoint_id == $endpoint
      and .found == true and .matched == true
      and .path == "relay"
      and (.path_detail | type == "string" and contains($relay))
      and (.owner_fence_id | type == "string" and test("^[0-9a-f]{32}$"))
      and (.owner_instance_id | type == "string" and length == 36)
      and (.owner_incarnation | type == "string" and test("^[1-9][0-9]*$"))
      and (.connection_id | type == "string" and test("^[0-9a-f]{32}$"))
      and (.owner_epoch | type == "number" and floor == . and . > 0)
      and (.negotiated_capabilities | type == "array"
        and index("ocserv.fencing.v2") != null))
  ' "${temporary}" >/dev/null; then
    {
      printf '%s\n' "node connection probe returned a non-matching observation"
      cat "${error_temporary}"
    } >"${error_file}"
    mv -f -- "${temporary}" "${output}.last-invalid.json"
    rm -f -- "${error_temporary}"
    return 1
  fi
  rm -f -- "${error_temporary}" "${error_file}" "${output}.last-invalid.json"
  mv -f -- "${temporary}" "${output}"
}

relay_probe_relay_b() {
  relay_probe_named relay-b "$@"
}

relay_observations_same_session() {
  local before="${1:?before observation is required}"
  local after="${2:?after observation is required}"
  jq -e --slurpfile before "${before}" '
    def tuple: {
      node_id,endpoint_id,owner_fence_id,owner_instance_id,
      owner_incarnation,connection_id,owner_epoch,path,path_detail
    };
    (.observations | length) == 1
    and ($before[0].observations | length) == 1
    and (.observations[0] | tuple) == ($before[0].observations[0] | tuple)
  ' "${after}" >/dev/null
}

capture_relay_command_proof() {
  local key="${1:?idempotency key is required}"
  local node="${2:?node id is required}"
  local output="${3:?proof destination is required}"
  local temporary="${output}.$$"
  psql_primary_probe -Atc \
    "SELECT jsonb_build_object(
       'observed_at',to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),
       'command_id',command.id::text,'idempotency_key',command.idempotency_key,
       'node_id',command.node_id::text,'command_state',command.state,
       'result_count',count(result.event_id),
       'result_state',min(result.state::text),
       'agent_result_completed_at',to_char(max(coalesce(result.completed_at,result.created_at)) AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),
       'result_observed_at',to_char(command.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'))
     FROM commands AS command
     JOIN agent_command_results AS result ON result.command_id=command.id
     WHERE command.idempotency_key='${key}' AND command.node_id='${node}'
     GROUP BY command.id,command.idempotency_key,command.node_id,command.state,command.updated_at" \
    >"${temporary}"
  jq -e --arg key "${key}" --arg node "${node}" '
    .idempotency_key == $key and .node_id == $node
    and .command_state == "succeeded" and .result_count == 1
    and .result_state == "succeeded"
    and (.command_id | test("^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"))
    and (.observed_at | test("^[0-9]{4}-.*Z$"))
    and (.agent_result_completed_at | test("^[0-9]{4}-.*Z$"))
    and (.result_observed_at | test("^[0-9]{4}-.*Z$"))
  ' "${temporary}" >/dev/null || {
    mv -f -- "${temporary}" "${output}.failed.json"
    echo "relay failover command lacks one successful durable result" >&2
    jq -c . "${output}.failed.json" >&2 || true
    return 1
  }
  mv -f -- "${temporary}" "${output}"
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
  # Keep the local relay reachable on the isolated bridge while removing the
  # Agent's shared bridge path to transportd. Otherwise the fault injection
  # would sever both the direct and relay paths and could not prove fallback.
  docker network connect --alias relay-b "${isolated_network}" \
    "${COMPOSE_PROJECT}-relay-1" >/dev/null
  G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_wait_until_deadline 60 5 "agent-01 session on the direct path" \
    g6rd_probe_node_connection direct "${node}"
  g6rd_timeline_event direct_path_active
  docker network connect "${isolated_network}" "${COMPOSE_PROJECT}-${service}-1" >/dev/null
  docker network disconnect "${COMPOSE_PROJECT}_default" "${COMPOSE_PROJECT}-${service}-1"
  g6rd_timeline_event direct_path_failed
  G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_wait_until_deadline 120 5 "agent-01 session moved to the relay path" \
    g6rd_probe_node_connection relay "${node}"
  g6rd_timeline_event relay_path_active
  docker network connect "${COMPOSE_PROJECT}_default" "${COMPOSE_PROJECT}-${service}-1" >/dev/null
  docker network disconnect "${isolated_network}" "${COMPOSE_PROJECT}-${service}-1"
  docker network disconnect "${isolated_network}" "${COMPOSE_PROJECT}-relay-1"
  docker network rm "${isolated_network}" >/dev/null
  G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_wait_until_deadline 180 5 "agent-01 session recovered the direct path" \
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
  unsettled="$(psql_primary_probe -Atc \
    "SELECT count(*) FROM commands WHERE idempotency_key LIKE '${keys_prefix}%' AND state NOT IN ('succeeded','failed','rejected','unknown','expired','rolled_back','superseded')")"
  [[ "${unsettled}" == 0 ]]
}

outbox_row_claimed() {
  local claimed
  claimed="$(psql_primary_probe -Atc \
    "SELECT count(*)
     FROM outbox_events AS outbox
     JOIN commands AS command ON command.id=outbox.command_id
     JOIN node_command_leases AS lease
       ON lease.command_id=command.id AND lease.node_id=command.node_id
     JOIN command_attempts AS attempt
       ON attempt.command_id=command.id AND attempt.outbox_event_id=outbox.id
      AND attempt.worker_id=lease.worker_id
     WHERE command.idempotency_key='${CLAIM_KEY:?}'
       AND outbox.published_at IS NULL
       AND outbox.locked_by=lease.worker_id
       AND outbox.locked_until>clock_timestamp()
       AND lease.leased_until>clock_timestamp()
       AND attempt.attempt_number=outbox.attempts
       AND attempt.state='sending' AND attempt.finished_at IS NULL")"
  [[ "${claimed}" =~ ^[0-9]+$ ]] && ((claimed >= 1))
}

exact_post_send_attempt_id() {
  local command_id="${1:?command id is required}"
  psql_primary_probe -Atc \
    "SELECT attempt.id::text
     FROM commands AS command
     JOIN outbox_events AS outbox ON outbox.command_id=command.id
     JOIN node_command_leases AS lease
       ON lease.command_id=command.id AND lease.node_id=command.node_id
     JOIN command_attempts AS attempt
       ON attempt.command_id=command.id AND attempt.outbox_event_id=outbox.id
      AND attempt.worker_id=lease.worker_id
     WHERE command.id='${command_id}'
       AND outbox.published_at IS NULL
       AND outbox.locked_by=lease.worker_id
       AND outbox.locked_until>clock_timestamp()
       AND lease.leased_until>clock_timestamp()
       AND attempt.attempt_number=outbox.attempts
       AND attempt.state='sending' AND attempt.finished_at IS NULL"
}

exact_post_send_attempt_proof() {
  local command_id="${1:?command id is required}"
  local attempt_id="${2:?attempt id is required}"
  psql_primary_probe -F $'\t' -Atc \
    "SELECT attempt.state,attempt.finished_at IS NULL,
       command.state,operation.state,outbox.published_at IS NULL,
       outbox.locked_by=attempt.worker_id,
       COALESCE(outbox.locked_until>clock_timestamp(),false),
       attempt.attempt_number=outbox.attempts,
       lease.command_id IS NOT NULL,
       COALESCE(lease.leased_until>clock_timestamp(),false),
       NOT EXISTS (
         SELECT 1 FROM agent_command_results AS result
         WHERE result.command_id=command.id
       )
     FROM command_attempts AS attempt
     JOIN commands AS command ON command.id=attempt.command_id
     JOIN operations AS operation ON operation.id=command.operation_id
     JOIN outbox_events AS outbox
       ON outbox.id=attempt.outbox_event_id AND outbox.command_id=command.id
     LEFT JOIN node_command_leases AS lease
       ON lease.command_id=command.id AND lease.node_id=command.node_id
      AND lease.worker_id=attempt.worker_id
     WHERE command.id='${command_id}' AND attempt.id='${attempt_id}'"
}

report_exact_post_send_attempt_failure() {
  local command_id="${1:?command id is required}"
  local attempt_id="${2:?attempt id is required}"
  echo "send-before-MarkSent database state (target, attempts, results):" >&2
  psql_primary_probe -F $'\t' -Atc \
    "SELECT 'target',command.id::text,attempt.id::text,command.state,
       operation.state,outbox.published_at,outbox.locked_by,
       outbox.locked_until,outbox.attempts,attempt.attempt_number,
       attempt.state,attempt.started_at,attempt.finished_at,attempt.error_code,
       lease.worker_id,lease.leased_until,
       (SELECT count(*) FROM agent_command_results AS result
        WHERE result.command_id=command.id),clock_timestamp()
     FROM command_attempts AS attempt
     JOIN commands AS command ON command.id=attempt.command_id
     JOIN operations AS operation ON operation.id=command.operation_id
     JOIN outbox_events AS outbox
       ON outbox.id=attempt.outbox_event_id AND outbox.command_id=command.id
     LEFT JOIN node_command_leases AS lease
       ON lease.command_id=command.id AND lease.node_id=command.node_id
     WHERE command.id='${command_id}' AND attempt.id='${attempt_id}';
     SELECT 'attempt',attempt.id::text,attempt.attempt_number,attempt.state,
       attempt.worker_id::text,attempt.started_at,attempt.finished_at,
       attempt.error_code
     FROM command_attempts AS attempt
     WHERE attempt.command_id='${command_id}'
     ORDER BY attempt.attempt_number;
     SELECT 'result',result.event_id::text,result.state,result.error_code,
       result.accepted_at,result.completed_at,result.created_at
     FROM agent_command_results AS result
     WHERE result.command_id='${command_id}'
     ORDER BY result.created_at,result.event_id" >&2 || {
    echo "send-before-MarkSent database state unavailable" >&2
    return 1
  }
}

pre_send_barrier_disarm() {
  rm -f -- "${G6RD_PRE_SEND_BARRIER}/arm" \
    "${G6RD_PRE_SEND_BARRIER}/received" \
    "${G6RD_PRE_SEND_BARRIER}/release" \
    "${G6RD_PRE_SEND_BARRIER}/post-send-arm" \
    "${G6RD_PRE_SEND_BARRIER}/post-send-received" \
    "${G6RD_PRE_SEND_BARRIER}/post-send-release"
}

pre_send_barrier_arm() {
  local command_id="${1:?command id is required}"
  pre_send_barrier_disarm
  printf '%s\n' "${command_id}" >"${G6RD_PRE_SEND_BARRIER}/arm"
  chmod 0644 "${G6RD_PRE_SEND_BARRIER}/arm"
}

pre_send_barrier_reached() {
  local command_id="${1:?command id is required}"
  [[ -s "${G6RD_PRE_SEND_BARRIER}/received" ]] || return 1
  [[ "$(sed -n '1p' "${G6RD_PRE_SEND_BARRIER}/received")" == "${command_id}" ]]
}

pre_send_barrier_release() {
  local command_id="${1:-}"
  [[ -n "${command_id}" ]] || return 0
  printf '%s\n' "${command_id}" >"${G6RD_PRE_SEND_BARRIER}/release"
  chmod 0644 "${G6RD_PRE_SEND_BARRIER}/release"
}

post_send_barrier_arm() {
  local command_id="${1:?command id is required}"
  rm -f -- "${G6RD_PRE_SEND_BARRIER}/post-send-arm" \
    "${G6RD_PRE_SEND_BARRIER}/post-send-received" \
    "${G6RD_PRE_SEND_BARRIER}/post-send-release"
  printf '%s\n' "${command_id}" >"${G6RD_PRE_SEND_BARRIER}/post-send-arm"
  chmod 0644 "${G6RD_PRE_SEND_BARRIER}/post-send-arm"
}

post_send_barrier_reached() {
  local command_id="${1:?command id is required}"
  [[ -s "${G6RD_PRE_SEND_BARRIER}/post-send-received" ]] || return 1
  [[ "$(sed -n '1p' "${G6RD_PRE_SEND_BARRIER}/post-send-received")" == "${command_id}" ]]
}

post_send_barrier_release() {
  local command_id="${1:-}"
  [[ -n "${command_id}" ]] || return 0
  printf '%s\n' "${command_id}" >"${G6RD_PRE_SEND_BARRIER}/post-send-release"
  chmod 0644 "${G6RD_PRE_SEND_BARRIER}/post-send-release"
}

release_armed_pre_send_barrier() {
  local command_id=""
  if [[ -s "${G6RD_PRE_SEND_BARRIER}/arm" ]]; then
    command_id="$(sed -n '1p' "${G6RD_PRE_SEND_BARRIER}/arm")"
  fi
  pre_send_barrier_release "${command_id}"
}

release_armed_post_send_barrier() {
  local command_id=""
  if [[ -s "${G6RD_PRE_SEND_BARRIER}/post-send-arm" ]]; then
    command_id="$(sed -n '1p' "${G6RD_PRE_SEND_BARRIER}/post-send-arm")"
  fi
  post_send_barrier_release "${command_id}"
}

release_armed_result_commit_barrier() {
  local command_id=""
  if [[ -s "${G6RD_RESULT_BARRIER}/arm" ]]; then
    command_id="$(sed -n '1p' "${G6RD_RESULT_BARRIER}/arm")"
  fi
  [[ -n "${command_id}" ]] || return 0
  printf '%s\n' "${command_id}" >"${G6RD_RESULT_BARRIER}/release"
  chmod 0666 "${G6RD_RESULT_BARRIER}/release"
}

scoped_socket_ready() {
  local service="${1:?service is required}" path="${2:?socket path is required}"
  G6RD_COMPOSE_TIMEOUT_SECONDS=10 g6rd_compose exec -T "${service}" test -S "${path}"
}

restart_worker_transport_unit() {
  local reason="${1:?restart reason is required}"
  g6rd_export_common_env
  g6rd_compose up --detach worker
  g6rd_wait_until_deadline 30 1 "${reason} replacement worker trust socket" \
    scoped_socket_ready worker /run/ocserv-trust/control-plane.sock
  g6rd_compose up --detach transportd
  g6rd_wait_until_deadline 30 1 "${reason} replacement transportd socket" \
    scoped_socket_ready transportd /run/ocserv-platform/transportd.sock
  if ! G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 \
    g6rd_wait_until_deadline 180 5 \
      "all Agents to establish fenced sessions after ${reason}" all_nodes_connected; then
    report_node_connection_timeout
    return 1
  fi
}

# The agent journal stores the envelope's binary command id, so lookups key
# on the database command uuid (hex, dashes stripped).
journal_has_command() {
  local service="${1:?service}" command_id="${2:?command id}" count
  if ! count="$(journal_query "${service}" \
    "SELECT count(*) FROM command_journal WHERE lower(hex(command_id))='${command_id//-/}'")"; then
    echo "Agent journal query failed for ${service}" >&2
    return 2
  fi
  count="$(tr -d '[:space:]' <<<"${count}")"
  [[ "${count}" =~ ^[0-9]+$ ]] || {
    echo "Agent journal query returned an invalid count for ${service}: ${count}" >&2
    return 2
  }
  printf '%s\n' "${count}"
}

journal_command_ready() {
  local service="${1:?service}" command_id="${2:?command id}" count
  if ! count="$(journal_has_command "${service}" "${command_id}")"; then
    return 2
  fi
  case "${count}" in
    1) return 0 ;;
    0) return 1 ;;
    *)
      echo "Agent journal contains duplicate command rows for ${service}: ${count}" >&2
      return 2
      ;;
  esac
}

wait_for_journal_command() {
  local timeout_seconds="${1:?timeout seconds is required}"
  local interval="${2:?interval seconds is required}"
  local description="${3:?description is required}"
  local service="${4:?service is required}" command_id="${5:?command id}"
  local deadline remaining query_timeout status
  [[ "${timeout_seconds}" =~ ^[0-9]+$ && "${timeout_seconds}" -ge 1 ]] || return 2
  [[ "${interval}" =~ ^[0-9]+$ && "${interval}" -ge 1 ]] || return 2
  deadline=$((SECONDS + timeout_seconds))
  while ((SECONDS < deadline)); do
    remaining=$((deadline - SECONDS))
    query_timeout="${remaining}"
    ((query_timeout > 10)) && query_timeout=10
    if G6RD_JOURNAL_QUERY_TIMEOUT_SECONDS="${query_timeout}" \
      journal_command_ready "${service}" "${command_id}"; then
      return 0
    else
      status=$?
    fi
    if ((status != 1)); then
      echo "failed while waiting for ${description}" >&2
      return "${status}"
    fi
    remaining=$((deadline - SECONDS))
    ((remaining > 0)) || break
    if ((remaining < interval)); then
      sleep "${remaining}"
    else
      sleep "${interval}"
    fi
  done
  echo "timed out waiting for ${description}" >&2
  return 1
}

agent_synthetic_receipt_reached() {
  local receipt="${1:?receipt file}" command_id="${2:?command id}"
  [[ -f "${receipt}" && ! -L "${receipt}" && -s "${receipt}" ]] || return 1
  [[ "$(sed -n '1p' "${receipt}")" == "${command_id}" ]]
}

journal_result_state() {
  local service="${1:?service}" command_id="${2:?command id}" state
  if ! state="$(journal_query "${service}" \
    "SELECT state FROM command_journal WHERE lower(hex(command_id))='${command_id//-/}'")"; then
    echo "Agent journal state query failed for ${service}" >&2
    return 2
  fi
  state="$(tr -d '[:space:]' <<<"${state}")"
  printf '%s\n' "${state}"
}

command_id_of_key() {
  psql_primary_probe -Atc "SELECT id FROM commands WHERE idempotency_key='${1:?key}'"
}

# Window 1: the exact command has returned from the committed Claim and is
# blocked by the test-only Worker hook immediately before SendCommand. Killing
# both replacement-unit processes at that signal proves no transport write can
# have been queued in the UDS kernel buffers.
phase_outbox_claim_before_send() {
  (
    local node key command_id completed=0
    trap 'if [[ "${completed}" != 1 ]]; then
      pre_send_barrier_release "${command_id:-}" || true
      docker unpause "${COMPOSE_PROJECT}-worker-1" >/dev/null 2>&1 || true
    fi' EXIT
    node="$(local_node_id 1)"
    [[ -n "${node}" ]] || {
      echo "the first local FD-B crash-window node is missing" >&2
      return 1
    }
    key="g6-crash1-${RUN_ID}"
    CLAIM_KEY="${key}"
    export CLAIM_KEY
    g6rd_wait_until_deadline 60 5 "outbox drained before claim-before-send" outbox_drained
    docker pause "${COMPOSE_PROJECT}-worker-1" >/dev/null
    g6rd_enqueue_command "${node}" "${key}"
    # shellcheck disable=SC2030  # phase state is intentionally subshell-local
    command_id="$(command_id_of_key "${key}")"
    [[ -n "${command_id}" ]] || {
      echo "claim-before-send command id is missing" >&2
      return 1
    }
    pre_send_barrier_arm "${command_id}"
    docker unpause "${COMPOSE_PROJECT}-worker-1" >/dev/null
    g6rd_wait_until_deadline 10 1 "exact pre-send Worker barrier" \
      pre_send_barrier_reached "${command_id}"
    g6rd_wait_until_deadline 5 1 "exact committed outbox claim" outbox_row_claimed
    g6rd_timeline_event outbox_claim_committed
    docker kill "${COMPOSE_PROJECT}-worker-1" >/dev/null
    g6rd_timeline_event worker_crashed_before_send
    docker kill "${COMPOSE_PROJECT}-transportd-1" >/dev/null
    pre_send_barrier_disarm
    restart_worker_transport_unit claim-before-send
    g6rd_wait_until_deadline 120 5 "claim-before-send command recovered" \
      wait_commands_settled "g6-crash1-${RUN_ID}"
    printf '%s\n' "${command_id}" >"${G6RD_STATE}/crash1-command-id"
    g6rd_timeline_event command_recovered
    completed=1
  )
}

# Window 2: the exact command stops at the post-Claim/pre-Send Worker hook while
# the harness arms a second exact hook immediately after SendCommand returns.
# That return proves the local transport write, not Agent receipt, so the Agent
# independently signals after decoding and fence verification. Only after both
# exact signals does the worker die before MarkSent can begin.
phase_outbox_send_before_mark() {
  (
    local node key command_id attempt_id attempt_proof
    local service synthetic_barrier synthetic_receipt completed=0
    node="$(local_node_id 2)"
    [[ -n "${node}" ]] || {
      echo "the second local FD-B crash-window node is missing" >&2
      return 1
    }
    service="$(node_service "${node}")"
    synthetic_barrier="${G6RD_AGENTS}/${service}/state/synthetic-barrier"
    synthetic_receipt="${synthetic_barrier}.received"
    # shellcheck disable=SC2031  # trap reads this phase's subshell-local id
    trap 'if [[ "${completed}" != 1 ]]; then
      pre_send_barrier_release "${command_id:-}" || true
      post_send_barrier_release "${command_id:-}" || true
      docker unpause "${COMPOSE_PROJECT}-worker-1" >/dev/null 2>&1 || true
    fi
    rm -f -- "${synthetic_barrier}"
    : >"${synthetic_receipt}" 2>/dev/null || true
    docker unpause "${COMPOSE_PROJECT}-transportd-1" >/dev/null 2>&1 || true' EXIT
    key="g6-crash2-${RUN_ID}"
    CLAIM_KEY="${key}"
    export CLAIM_KEY
    g6rd_wait_until_deadline 60 5 "outbox drained before send-before-MarkSent" outbox_drained
    docker pause "${COMPOSE_PROJECT}-worker-1" >/dev/null
    rm -f -- "${synthetic_barrier}"
    : >"${synthetic_receipt}"
    chmod 0666 "${synthetic_receipt}"
    g6rd_enqueue_command "${node}" "${key}"
    # shellcheck disable=SC2030  # phase state is intentionally subshell-local
    command_id="$(command_id_of_key "${key}")"
    [[ -n "${command_id}" ]] || {
      echo "send-before-MarkSent command id is missing" >&2
      return 1
    }
    printf '%s\n' "${command_id}" >"${synthetic_barrier}"
    chmod 0644 "${synthetic_barrier}"
    pre_send_barrier_arm "${command_id}"
    post_send_barrier_arm "${command_id}"
    docker unpause "${COMPOSE_PROJECT}-worker-1" >/dev/null
    g6rd_wait_until_deadline 10 1 "exact pre-send Worker barrier" \
      pre_send_barrier_reached "${command_id}"
    g6rd_wait_until_deadline 5 1 "send-before-MarkSent strict outbox claim" outbox_row_claimed
    pre_send_barrier_release "${command_id}"
    g6rd_wait_until_deadline 15 1 "exact post-send Worker barrier" \
      post_send_barrier_reached "${command_id}"
    # A transient pre-Send failure may leave an older failed attempt. Bind the
    # crash proof to the live attempt that reached this exact post-Send hook.
    attempt_id="$(exact_post_send_attempt_id "${command_id}")"
    [[ "${attempt_id}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
      echo "the exact post-send Worker barrier did not identify one live attempt" >&2
      return 1
    }
    g6rd_timeline_event transport_send_accepted
    g6rd_wait_until_deadline 15 1 "exact Agent receipt before worker crash" \
      agent_synthetic_receipt_reached "${synthetic_receipt}" "${command_id}"
    docker kill "${COMPOSE_PROJECT}-worker-1" >/dev/null
    g6rd_timeline_event worker_crashed_before_mark_sent
    # Keep the Agent before Journal acceptance until transportd is gone. This
    # prevents a terminal response from racing the pre-MarkSent database proof.
    docker kill "${COMPOSE_PROJECT}-transportd-1" >/dev/null
    rm -f -- "${synthetic_barrier}"
    wait_for_journal_command 15 1 "exact Agent journal receipt after worker crash" \
      "${service}" "${command_id}"
    pre_send_barrier_disarm
    attempt_proof="$(exact_post_send_attempt_proof "${command_id}" "${attempt_id}")"
    [[ "${attempt_proof}" == $'sending\tt\tqueued\tqueued\tt\tt\tt\tt\tt\tt\tt' ]] || {
      echo "the exact send-before-MarkSent attempt changed before the crash: command=${command_id} attempt=${attempt_id} proof=${attempt_proof:-missing}" >&2
      report_exact_post_send_attempt_failure "${command_id}" "${attempt_id}" || true
      return 1
    }
    printf '%s\n' "${command_id}" >"${G6RD_STATE}/crash2-command-id"
    restart_worker_transport_unit send-before-MarkSent
    g6rd_wait_until_deadline 120 5 "send-before-MarkSent command reconciled" \
      wait_commands_settled "${key}"
    g6rd_timeline_event command_reconciled
    completed=1
  )
}

# Window 3: the ingress transaction validates and applies the Agent result,
# signals the harness while the transaction is still open, and blocks before
# commit. Killing the worker at that barrier proves the database saw no
# terminal result until the replacement reconciled the retained Agent result.
phase_outbox_result_before_commit() {
  (
    local node key command_id kill_file="${G6RD_STATE}/crash3-kill-at"
    local barrier="${G6RD_RESULT_BARRIER}" completed=0
    # A failed setup must not leave a frozen worker or an ingress transaction
    # waiting forever. The exact release is ignored unless this command armed
    # the barrier and reached it.
    # shellcheck disable=SC2031  # trap reads this phase's subshell-local id
    trap 'if [[ "${completed}" != 1 ]]; then
      if [[ -n "${command_id:-}" ]]; then
        printf "%s\n" "${command_id}" >"${barrier}/release" 2>/dev/null || true
        chmod 0666 "${barrier}/release" 2>/dev/null || true
      fi
      docker unpause "${COMPOSE_PROJECT}-worker-1" >/dev/null 2>&1 || true
    fi' EXIT
    node="$(local_node_id 3)"
    [[ -n "${node}" ]] || {
      echo "the third local FD-B crash-window node is missing" >&2
      return 1
    }
    key="g6-crash3-${RUN_ID}"
    g6rd_wait_until_deadline 60 5 "outbox drained before result-before-commit" outbox_drained
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
    # The worker creates received as uid 65532 with mode 0600. Pre-creating it
    # keeps the exact signal readable by the runner without widening the bind.
    : >"${barrier}/received"
    chmod 0666 "${barrier}/arm" "${barrier}/received"
    docker unpause "${COMPOSE_PROJECT}-worker-1" >/dev/null
    g6rd_wait_until_deadline 60 1 "ingress result commit barrier" test -s "${barrier}/received"
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
    docker kill "${COMPOSE_PROJECT}-transportd-1" >/dev/null
    [[ "$(psql_primary -Atc "SELECT count(*) FROM agent_command_results WHERE command_id='${command_id}'")" == 0 ]] || {
      echo "the killed ingress transaction committed a command result" >&2
      return 1
    }
    rm -f "${barrier}/arm" "${barrier}/received"
    restart_worker_transport_unit result-before-commit
    g6rd_wait_until_deadline 120 5 "crash3 result reconciled" wait_commands_settled "${key}"
    [[ "$(psql_primary -Atc "SELECT count(*) FROM agent_command_results WHERE command_id='${command_id}'")" == 1 ]] || {
      echo "the replacement ingress did not reconcile exactly one command result" >&2
      return 1
    }
    printf '%s\n' "${command_id}" >"${G6RD_STATE}/crash3-command-id"
    g6rd_timeline_event result_received "${G6RD_STATE}/crash3-result-received-at"
    g6rd_timeline_event ingress_crashed_before_commit "${kill_file}"
    g6rd_timeline_event result_reconciled
    completed=1
  )
}

journal_result_ready() {
  local service="${1:?service}" command_id="${2:?command id}" state
  if ! state="$(journal_result_state "${service}" "${command_id}")"; then
    return 2
  fi
  [[ -n "${state}" ]]
}

# ---------------------------------------------------------------------------
# The bounded stability window: continuous 3-second resource sampling on
# this failure domain's clock while the HTTP driver records read and
# enqueue observations against the recovered control plane.
# ---------------------------------------------------------------------------

phase_window_barrier_arm() {
  g6rd_arm_synthetic_barriers
  g6rd_synthetic_barriers_armed || {
    echo "failure domain B did not arm its complete local Agent population" >&2
    return 1
  }
  mkdir -p "${G6RD_OUTBOX}/window-barrier-arm-request"
  printf '%s\n' "${G6RD_CANDIDATE_SHA}" \
    >"${G6RD_OUTBOX}/window-barrier-arm-request/candidate-sha"
}

phase_resource_preflight() {
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  local output="${G6RD_STATE}/resource-preflight.csv" temporary
  local header='timestamp,component,instance,rss_bytes,fd_count,tasks,queue_depth,db_connections,environment_id,candidate_sha'
  temporary="$(mktemp "${G6RD_STATE}/resource-preflight.XXXXXX")"
  printf '%s\n' "${header}" >"${temporary}"
  if ! G6RD_SAMPLER_COMPOSE_TIMEOUT_SECONDS=8 \
    G6RD_SAMPLER_PSQL_TIMEOUT_SECONDS=8 \
    g6rd_sampler_tick "${temporary}"; then
    rm -f -- "${temporary}"
    echo "bounded resource preflight could not collect a complete real sampler tick" >&2
    return 1
  fi
  if ! g6rd_validate_sampler_batch "${temporary}"; then
    sed -n '1,12p' "${temporary}" >&2
    rm -f -- "${temporary}"
    return 1
  fi
  mv -f -- "${temporary}" "${output}"
}

outbox_drained() {
  [[ "$(psql_primary_probe -Atc \
    'SELECT count(*) FROM outbox_events WHERE published_at IS NULL')" == 0 ]]
}

psql_window_probe() {
  # Five seconds plus g6rd_psql's five-second TERM-to-KILL grace gives each
  # observation-window SQL predicate a ten-second hard process bound.
  G6RD_PSQL_TIMEOUT_SECONDS=5 psql_primary "$@"
}

window_outbox_drained() {
  [[ "$(psql_window_probe -Atc \
    'SELECT count(*) FROM outbox_events WHERE published_at IS NULL')" == 0 ]]
}

window_commands_settled() {
  local keys_prefix="${1:?key prefix}" unsettled
  unsettled="$(psql_window_probe -Atc \
    "SELECT count(*) FROM commands WHERE idempotency_key LIKE '${keys_prefix}%' AND state NOT IN ('succeeded','failed','rejected','unknown','expired','rolled_back','superseded')")"
  [[ "${unsettled}" == 0 ]]
}

window_inner_budget_seconds() {
  printf '%s\n' "$((
    WINDOW_SECONDS
    + WINDOW_API_READY_TIMEOUT_SECONDS
    + WINDOW_PRE_DRAIN_TIMEOUT_SECONDS
    + WINDOW_COMMAND_SETTLE_TIMEOUT_SECONDS
    + WINDOW_POST_DRAIN_TIMEOUT_SECONDS
    + WINDOW_API_PREDICATE_OVERRUN_SECONDS
    + (3 * WINDOW_SQL_PREDICATE_OVERRUN_SECONDS)
    + WINDOW_DRIVER_OVERRUN_SECONDS
    + WINDOW_DIAGNOSTIC_MAX_SECONDS
    + WINDOW_SAMPLER_STOP_MAX_SECONDS
  ))"
}

validate_window_timeout_budget() {
  local value budget
  for value in "${WINDOW_SECONDS}" "${WINDOW_API_READY_TIMEOUT_SECONDS}" \
    "${WINDOW_PRE_DRAIN_TIMEOUT_SECONDS}" "${WINDOW_COMMAND_SETTLE_TIMEOUT_SECONDS}" \
    "${WINDOW_POST_DRAIN_TIMEOUT_SECONDS}" "${WINDOW_API_PREDICATE_OVERRUN_SECONDS}" \
    "${WINDOW_SQL_PREDICATE_OVERRUN_SECONDS}" "${WINDOW_DRIVER_OVERRUN_SECONDS}" \
    "${WINDOW_DIAGNOSTIC_MAX_SECONDS}" "${WINDOW_SAMPLER_STOP_MAX_SECONDS}" \
    "${WINDOW_WORKFLOW_TIMEOUT_SECONDS}" "${WINDOW_MINIMUM_OUTER_MARGIN_SECONDS}"; do
    [[ "${value}" =~ ^[1-9][0-9]*$ ]] || {
      echo "observation-window timeout budgets must be positive integers" >&2
      return 2
    }
  done
  budget="$(window_inner_budget_seconds)"
  ((budget + WINDOW_MINIMUM_OUTER_MARGIN_SECONDS < WINDOW_WORKFLOW_TIMEOUT_SECONDS)) || {
    echo "observation-window inner budget ${budget}s leaves less than the required outer margin" >&2
    return 2
  }
}

report_window_command_timeout() {
  local keys_prefix="${1:?key prefix}"
  echo "observation-window command state matrix at settlement timeout:" >&2
  psql_window_probe -F $'\t' -Atc \
    "SELECT 'matrix',command.state,operation.state,
       outbox.published_at IS NOT NULL,outbox.locked_by IS NOT NULL,
       lease.command_id IS NOT NULL,
       COALESCE(lease.leased_until>clock_timestamp(),false),count(*)
     FROM commands AS command
     JOIN operations AS operation ON operation.id=command.operation_id
     JOIN outbox_events AS outbox ON outbox.command_id=command.id
     LEFT JOIN node_command_leases AS lease ON lease.command_id=command.id
     WHERE command.idempotency_key LIKE '${keys_prefix}%'
     GROUP BY 2,3,4,5,6,7 ORDER BY 2,3,4,5,6,7;
     SELECT 'unsettled',command.id::text,command.node_id::text,command.state,
       operation.state,outbox.published_at IS NOT NULL,
       outbox.locked_by IS NOT NULL,lease.command_id IS NOT NULL,
       COALESCE(lease.leased_until>clock_timestamp(),false),lease.leased_until,
       outbox.attempts,
       COALESCE((SELECT string_agg(attempt.attempt_number::text||':'||attempt.state,',' ORDER BY attempt.attempt_number)
         FROM command_attempts AS attempt WHERE attempt.command_id=command.id),'none'),
       COALESCE((SELECT string_agg(result.state,',' ORDER BY result.created_at)
         FROM agent_command_results AS result WHERE result.command_id=command.id),'none'),
       command.updated_at
     FROM commands AS command
     JOIN operations AS operation ON operation.id=command.operation_id
     JOIN outbox_events AS outbox ON outbox.command_id=command.id
     LEFT JOIN node_command_leases AS lease ON lease.command_id=command.id
     WHERE command.idempotency_key LIKE '${keys_prefix}%'
       AND command.state NOT IN ('succeeded','failed','rejected','unknown','expired','rolled_back','superseded')
     ORDER BY command.updated_at,command.id LIMIT 20" >&2 ||
    echo "observation-window command state matrix unavailable" >&2
}

report_window_outbox_timeout() {
  echo "observation-window unpublished outbox state at drain timeout:" >&2
  psql_window_probe -F $'\t' -Atc \
    "SELECT 'summary',count(*),count(*) FILTER (WHERE locked_by IS NOT NULL),
       min(available_at),max(attempts)
     FROM outbox_events WHERE published_at IS NULL;
     SELECT 'pending',outbox.id::text,COALESCE(command.idempotency_key,''),
       outbox.attempts,outbox.locked_by,outbox.locked_until,outbox.available_at
     FROM outbox_events AS outbox
     LEFT JOIN commands AS command ON command.id=outbox.command_id
     WHERE outbox.published_at IS NULL
     ORDER BY outbox.available_at,outbox.id LIMIT 20" >&2 ||
    echo "observation-window unpublished outbox state unavailable" >&2
}

wait_for_window_enqueue_wave() {
  local pid status=0
  (($# > 0)) || {
    echo "the observation-window enqueue wave has no child processes" >&2
    return 2
  }
  for pid in "$@"; do
    if ! wait "${pid}"; then
      echo "observation-window enqueue child ${pid} failed" >&2
      status=1
    fi
  done
  return "${status}"
}

window_opening_commands_active() {
  local expected prefix observed
  expected="$(managed_node_count)" || return 1
  prefix="g6-window-${RUN_ID}-opening-"
  observed="$(psql_window_probe -F $'\t' -Atc \
    "WITH opening AS (
       SELECT command.id,command.node_id,command.state
       FROM commands AS command
       WHERE command.idempotency_key LIKE '${prefix}%'
     )
     SELECT count(*),count(DISTINCT opening.node_id),
       count(*) FILTER (WHERE opening.state IN ('dispatched','accepted','running')),
       count(result.command_id)
     FROM opening
     LEFT JOIN agent_command_results AS result ON result.command_id=opening.id")" || return 1
  [[ "${observed}" == "${expected}"$'\t'"${expected}"$'\t'"${expected}"$'\t0' ]]
}

capture_window_opening_active() {
  local expected prefix output temporary
  expected="$(managed_node_count)" || return 1
  prefix="g6-window-${RUN_ID}-opening-"
  output="${G6RD_STATE}/window-opening-active.json"
  temporary="${output}.$$"
  if ! psql_window_probe -Atc \
    "WITH opening AS (
       SELECT command.id,command.node_id,command.state
       FROM commands AS command
       WHERE command.idempotency_key LIKE '${prefix}%'
     )
     SELECT jsonb_build_object(
       'captured_at',to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),
       'expected_count',${expected},
       'commands',coalesce(jsonb_agg(jsonb_build_object(
         'command_id',opening.id::text,'node_id',opening.node_id::text,
         'state',opening.state) ORDER BY opening.node_id),'[]'::jsonb),
       'result_count',count(result.command_id))
     FROM opening
     LEFT JOIN agent_command_results AS result ON result.command_id=opening.id" \
    >"${temporary}"; then
    rm -f -- "${temporary}"
    return 1
  fi
  if ! jq -e --argjson expected "${expected}" '
    .expected_count == $expected
    and .result_count == 0
    and (.commands | length) == $expected
    and ([.commands[].node_id] | unique | length) == $expected
    and all(.commands[]; (.state | IN("dispatched","accepted","running")))
  ' "${temporary}" >/dev/null; then
    rm -f -- "${temporary}"
    echo "the frozen opening-wave proof is not the exact active managed population" >&2
    return 1
  fi
  mv -f -- "${temporary}" "${output}"
}

record_window_opening_proof() {
  local marker="window-opening-proof-${RUN_ID}"
  psql_window_probe -v marker_id="${marker}" -Atc \
    "INSERT INTO g6_readiness_markers(id,txid,phase)
     VALUES (:'marker_id',txid_current()::text,'window_opening_proof')
     RETURNING id" \
    | grep -Fx -- "${marker}" >/dev/null
}

phase_window() (
  local peer_armed="${1:?peer window-barrier acknowledgement is required}"
  local completed=0
  trap 'g6rd_release_synthetic_barriers || :
    if ((completed == 0)); then g6rd_stop_sampler || :; fi' EXIT
  require_file "${peer_armed}/candidate-sha"
  [[ "$(<"${peer_armed}/candidate-sha")" == "${G6RD_CANDIDATE_SHA}" ]] || {
    echo "peer window-barrier acknowledgement belongs to a different candidate" >&2
    return 1
  }
  g6rd_synthetic_barriers_armed || {
    echo "failure domain B lost an Agent barrier before the observation window" >&2
    return 1
  }
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  validate_window_timeout_budget
  g6rd_wait_until_deadline "${WINDOW_API_READY_TIMEOUT_SECONDS}" 2 \
    "api ready before the window" g6rd_api_ready
  if ! g6rd_wait_until_deadline "${WINDOW_PRE_DRAIN_TIMEOUT_SECONDS}" 5 \
    "outbox drained before the window" window_outbox_drained; then
    report_window_outbox_timeout
    return 1
  fi
  g6rd_now >"${G6RD_STATE}/window-started-at"
  g6rd_start_sampler "${G6RD_STATE}/resource-samples.csv"
  local node total count=0 window_deadline _
  local -a enqueue_pids=()
  mapfile -t node_list < <(node_ids)
  total="${#node_list[@]}"
  window_deadline=$((SECONDS + WINDOW_SECONDS))
  : >"${G6RD_STATE}/read-log.jsonl"
  : >"${G6RD_STATE}/enqueue-log.jsonl"
  [[ "${total}" == "$(managed_node_count)" ]] || {
    echo "the observation window did not load the exact managed population" >&2
    return 1
  }
  # Arm both failure domains before admission, then prove exactly one active,
  # result-free production command for every managed Agent before releasing
  # either half of the fleet. This is the raw max-inflight witness.
  for node in "${node_list[@]}"; do
    g6rd_enqueue_command "${node}" "g6-window-${RUN_ID}-opening-${count}" &
    enqueue_pids+=("$!")
    count=$((count + 1))
  done
  if ! wait_for_window_enqueue_wave "${enqueue_pids[@]}"; then
    return 1
  fi
  if [[ -e "${G6RD_STATE}/sampler-failed-at" ]]; then
    echo "resource sampler failed during the all-fleet opening command wave" >&2
    return 1
  fi
  g6rd_wait_until_deadline 60 1 \
    "exact fifty-command production inflight proof" \
    window_opening_commands_active
  capture_window_opening_active
  record_window_opening_proof
  g6rd_release_synthetic_barriers

  # Retain the second production-path wave after the held population is
  # released so the remainder of the bounded workload continues at full
  # fleet breadth without weakening the exact opening proof.
  enqueue_pids=()
  for node in "${node_list[@]}"; do
    g6rd_enqueue_command "${node}" "g6-window-${RUN_ID}-continuation-${count}" &
    enqueue_pids+=("$!")
    count=$((count + 1))
  done
  wait_for_window_enqueue_wave "${enqueue_pids[@]}"
  while ((SECONDS < window_deadline)); do
    if [[ -e "${G6RD_STATE}/sampler-failed-at" ]]; then
      echo "resource sampler failed during the bounded observation window" >&2
      g6rd_stop_sampler || :
      return 1
    fi
    g6rd_read_nodes "${G6RD_STATE}/read-log.jsonl" || true
    ((SECONDS < window_deadline)) || break
    g6rd_read_nodes "${G6RD_STATE}/read-log.jsonl" || true
    ((SECONDS < window_deadline)) || break
    node="${node_list[$((count % total))]}"
    if [[ -n "${node}" ]]; then
      g6rd_enqueue_command "${node}" "g6-window-${RUN_ID}-${count}" || true
      ((SECONDS < window_deadline)) || break
      g6rd_enqueue_command "${node}" "g6-window-${RUN_ID}-b${count}" || true
      ((SECONDS < window_deadline)) || break
    fi
    count=$((count + 1))
    sleep 0.5
  done
  CLAIM_KEY="g6-window-${RUN_ID}-"
  export CLAIM_KEY
  if ! g6rd_wait_until_deadline "${WINDOW_COMMAND_SETTLE_TIMEOUT_SECONDS}" 5 \
    "window commands settled" window_commands_settled "${CLAIM_KEY}"; then
    report_window_command_timeout "${CLAIM_KEY}"
    g6rd_stop_sampler || true
    return 1
  fi
  if ! g6rd_wait_until_deadline "${WINDOW_POST_DRAIN_TIMEOUT_SECONDS}" 5 \
    "outbox drained after the window" window_outbox_drained; then
    report_window_outbox_timeout
    g6rd_stop_sampler || true
    return 1
  fi
  g6rd_stop_sampler
  g6rd_timeline_event resource_sampling_stopped \
    "${G6RD_STATE}/sampler-complete-at"
  g6rd_now >"${G6RD_STATE}/window-ended-at"
  g6rd_timeline_event api_slo_measured "${G6RD_STATE}/window-ended-at"
  completed=1
)

# ---------------------------------------------------------------------------
# Evidence collection and assembly. Collection runs while the recovered
# stack is still live: it freezes the authoritative database tables, the
# per-agent durable journals, the final session inventory, and this failure
# domain's container inventory into run state. Assembly turns the frozen
# records into the verifier's structured artifacts and runs the independent
# verifier against them.
# ---------------------------------------------------------------------------

quiesce_control_plane_writers() {
  # Stop new public mutations. Worker and scheduler renewal must remain live
  # until the atomic authority cut; pausing either before the session probe can
  # consume its 30s/15s lease TTL and manufacture an expired final snapshot.
  docker pause "${COMPOSE_PROJECT}-api-1" >/dev/null
}

quiesce_transport_ingress() {
  docker pause "${COMPOSE_PROJECT}-transportd-1" >/dev/null
}

quiesce_authority_renewers() {
  # Once transport ingress is frozen and the DB-clock cut is captured, no
  # later owner or scheduler renewal may leak past the watcher/evidence cut.
  docker pause "${COMPOSE_PROJECT}-worker-1" >/dev/null
  docker pause "${COMPOSE_PROJECT}-scheduler-1" >/dev/null
}

capture_database_clock() {
  psql_primary_probe -qAtc \
    "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')"
}

capture_final_authority_cut() {
  local out="${G6RD_STATE}/final-authority-cut.json" expected
  expected="$(node_ids | wc -l | tr -d '[:space:]')"
  psql_primary_probe -qAtc \
    "WITH cut AS MATERIALIZED (
       SELECT clock_timestamp() AS at
     ), owners AS (
       SELECT fencing.*
       FROM connection_owner_fencing AS fencing CROSS JOIN cut
       WHERE fencing.lease_until>cut.at
     ), leader AS (
       SELECT leadership.*
       FROM scheduler_leadership AS leadership CROSS JOIN cut
       WHERE leadership.id=1 AND leadership.lease_until>cut.at
     ), owner_journal AS MATERIALIZED (
       SELECT COALESCE(jsonb_agg(
         history.history_id::text||':'||encode(history.node_id,'hex')||':'||history.owner_instance_id||':'||history.owner_incarnation||':'||encode(history.connection_id,'hex')||':'||history.owner_epoch||':'||to_char(history.lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')||':'||to_char(history.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')
         ORDER BY history.history_id
       ),'[]'::jsonb) AS entries
       FROM g6_connection_owner_history AS history
     ), scheduler_journal AS MATERIALIZED (
       SELECT COALESCE(jsonb_agg(
         history.history_id::text||':'||history.instance_id||':'||history.incarnation||':'||history.epoch||':'||to_char(history.lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')||':'||to_char(history.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')
         ORDER BY history.history_id
       ),'[]'::jsonb) AS entries
       FROM g6_scheduler_leadership_history AS history
     ), maintenance_journal AS MATERIALIZED (
       SELECT COALESCE(jsonb_agg(
         history.maintenance_id::text||':'||history.instance_id||':'||history.incarnation||':'||history.epoch||':'||to_char(history.completed_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')
         ORDER BY history.maintenance_id
       ),'[]'::jsonb) AS entries
       FROM g6_scheduler_maintenance_history AS history
     )
     SELECT jsonb_build_object(
       'cut_at',to_char(cut.at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),
       'owners',COALESCE((
         SELECT jsonb_agg(jsonb_build_object(
           'node_hex',encode(owner.node_id,'hex'),
           'owner_instance_id',owner.owner_instance_id::text,
           'owner_incarnation',owner.owner_incarnation::text,
           'connection_id',encode(owner.connection_id,'hex'),
           'owner_epoch',owner.owner_epoch,
           'lease_until',to_char(owner.lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),
           'history',encode(owner.node_id,'hex')||':'||owner.owner_instance_id||':'||owner.owner_incarnation||':'||encode(owner.connection_id,'hex')||':'||owner.owner_epoch||':'||to_char(owner.lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')||':'||to_char(owner.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')
         ) ORDER BY owner.node_id) FROM owners AS owner
       ),'[]'::jsonb),
       'leader',(
         SELECT jsonb_build_object(
           'instance_id',entry.instance_id::text,
           'incarnation',entry.incarnation::text,
           'epoch',entry.epoch,
           'lease_until',to_char(entry.lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),
           'history',entry.instance_id||':'||entry.incarnation||':'||entry.epoch||':'||to_char(entry.lease_until AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')||':'||to_char(entry.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')
         ) FROM leader AS entry
       ),
       'owner_history',owner_journal.entries,
       'scheduler_history',scheduler_journal.entries,
       'scheduler_maintenance_history',maintenance_journal.entries
     ) FROM cut CROSS JOIN owner_journal CROSS JOIN scheduler_journal
       CROSS JOIN maintenance_journal" >"${out}"
  jq -e --argjson expected "${expected}" \
    '.cut_at | type == "string"
     and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{6}Z$")' \
    "${out}" >/dev/null
  jq -e --argjson expected "${expected}" \
    '(.owners | length) == $expected
     and ([.owners[].node_hex] | unique | length) == $expected
     and all(.owners[];
       (.node_hex | type == "string" and test("^[0-9a-f]{32}$"))
       and (.owner_instance_id | type == "string" and length == 36)
       and (.owner_incarnation | type == "string" and test("^[1-9][0-9]*$"))
       and (.connection_id | type == "string" and test("^[0-9a-f]{32}$"))
       and (.owner_epoch | type == "number" and . > 0)
       and (.lease_until | type == "string"))
     and (.leader.instance_id | type == "string" and length > 0)
     and (.leader.incarnation | type == "string" and test("^[1-9][0-9]*$"))
     and (.leader.epoch | type == "number" and . > 0)
     and (.leader.lease_until | type == "string")
     and (.owner_history | type == "array")
     and (.owner_history | length > 0)
     and all(.owner_history[];
       type == "string"
       and test("^[1-9][0-9]*:[0-9a-f]{32}:[0-9a-f-]{36}:[0-9]+:[0-9a-f]{32}:[1-9][0-9]*:[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{6}Z:[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{6}Z$"))
     and (.scheduler_history | type == "array")
     and (.scheduler_history | length > 0)
     and all(.scheduler_history[];
       type == "string"
       and test("^[1-9][0-9]*:[0-9a-f-]{36}:[0-9]+:[1-9][0-9]*:[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{6}Z:[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{6}Z$"))
     and (.scheduler_maintenance_history | type == "array")
     and (.scheduler_maintenance_history | length > 0)
     and all(.scheduler_maintenance_history[];
       type == "string"
       and test("^[1-9][0-9]*:[0-9a-f-]{36}:[0-9]+:[1-9][0-9]*:[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\\.[0-9]{6}Z$"))' \
    "${out}" >/dev/null || {
    echo "the final authority cut does not contain every live owner and leader lease" >&2
    return 1
  }
}

assert_final_session_authority() {
  local before="${G6RD_STATE}/evidence/final-sessions-before.json"
  local after="${G6RD_STATE}/evidence/final-sessions-after.json"
  local final="${G6RD_STATE}/evidence/final-sessions.json"
  local cut="${G6RD_STATE}/final-authority-cut.json" expected sessions
  local before_terms after_terms session_authority authority_terms
  expected="$(node_ids | wc -l | tr -d '[:space:]')"
  before_terms="${G6RD_STATE}/final-session-terms-before.tsv"
  after_terms="${G6RD_STATE}/final-session-terms-after.tsv"
  session_authority="${G6RD_STATE}/final-session-authority.tsv"
  authority_terms="${G6RD_STATE}/final-authority-terms.tsv"
  for sessions in "${before}" "${after}"; do
    jq -e --argjson expected "${expected}" --arg cut_at "$(jq -r '.cut_at' "${cut}")" '
      def stamp_key:
        capture("^(?<whole>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?:\\.(?<fraction>[0-9]{1,9}))?Z$") as $stamp
        | $stamp.whole + "." + ((($stamp.fraction // "") + "000000000")[0:9]);
      .all_matched == true
      and (.observations | length) == $expected
      and ([.observations[].node_id] | unique | length) == $expected
      and all(.observations[];
        .found == true
        and (.endpoint_id | type == "string" and test("^[0-9a-f]{64}$"))
        and (.agent_instance_id | type == "string" and test("^[0-9a-f]{32}$"))
        and (.owner_fence_id | type == "string" and test("^[0-9a-f]{32}$"))
        and (.owner_instance_id | type == "string" and length == 36)
        and (.owner_incarnation | type == "string" and test("^[1-9][0-9]*$"))
        and (.connection_id | type == "string" and test("^[0-9a-f]{32}$"))
        and (.owner_epoch | type == "number" and . > 0)
        and (.connected_at | stamp_key) <= ($cut_at | stamp_key)
        and (.owner_lease_until | stamp_key) > ($cut_at | stamp_key)
        and (.session_expires_at | stamp_key) > ($cut_at | stamp_key))' \
      "${sessions}" >/dev/null || {
      echo "a bracketed final session inventory is incomplete or not live at the authority cut" >&2
      return 1
    }
  done
  jq -r '.observations[] | [
      (.node_id | gsub("-"; "") | ascii_downcase),
      .endpoint_id,.agent_instance_id,.connected_at,.session_expires_at,
      .owner_fence_id,(.owner_instance_id | ascii_downcase),
      .owner_incarnation,.connection_id,(.owner_epoch | tostring),
      (.authorization_revision | tostring),(.negotiated_capabilities | tojson)
    ] | @tsv' "${before}" | sort >"${before_terms}"
  jq -r '.observations[] | [
      (.node_id | gsub("-"; "") | ascii_downcase),
      .endpoint_id,.agent_instance_id,.connected_at,.session_expires_at,
      .owner_fence_id,(.owner_instance_id | ascii_downcase),
      .owner_incarnation,.connection_id,(.owner_epoch | tostring),
      (.authorization_revision | tostring),(.negotiated_capabilities | tojson)
    ] | @tsv' "${after}" | sort >"${after_terms}"
  if ! cmp -s "${before_terms}" "${after_terms}"; then
    echo "a transport connection term changed across the DB authority cut" >&2
    diff -u "${before_terms}" "${after_terms}" >&2 || true
    return 1
  fi
  jq -r '.observations[] | [
      (.node_id | gsub("-"; "") | ascii_downcase),
      (.owner_instance_id | ascii_downcase),.owner_incarnation,
      .connection_id,(.owner_epoch | tostring)
    ] | @tsv' "${after}" | sort >"${session_authority}"
  jq -r '.owners[] | [
      .node_hex,(.owner_instance_id | ascii_downcase),.owner_incarnation,
      .connection_id,(.owner_epoch | tostring)
    ] | @tsv' "${cut}" | sort >"${authority_terms}"
  if ! cmp -s "${session_authority}" "${authority_terms}"; then
    echo "the final transport sessions do not match the authoritative owner terms" >&2
    diff -u "${authority_terms}" "${session_authority}" >&2 || true
    return 1
  fi
  cp -f "${after}" "${final}"
}

append_final_history_snapshot() {
  local cut="${G6RD_STATE}/final-authority-cut.json"
  local owner_tmp="${G6RD_STATE}/fencing-history.final.$$"
  local scheduler_tmp="${G6RD_STATE}/leadership-history.final.$$"
  local maintenance_tmp="${G6RD_STATE}/scheduler-maintenance-history.final.$$"
  local file label line history_id previous count
  require_file "${cut}"
  if ! jq -er '.owner_history[]' "${cut}" >"${owner_tmp}" \
    || ! jq -er '.scheduler_history[]' "${cut}" >"${scheduler_tmp}" \
    || ! jq -er '.scheduler_maintenance_history[]' "${cut}" >"${maintenance_tmp}"; then
    rm -f "${owner_tmp}" "${scheduler_tmp}" "${maintenance_tmp}"
    echo "the final authority cut is missing its frozen journal arrays" >&2
    return 1
  fi
  for file in "${owner_tmp}" "${scheduler_tmp}" "${maintenance_tmp}"; do
    label=fencing
    [[ "${file}" == "${scheduler_tmp}" ]] && label=leadership
    [[ "${file}" == "${maintenance_tmp}" ]] && label=scheduler-maintenance
    previous=0
    count=0
    while IFS= read -r line; do
      history_id="${line%%:*}"
      if [[ ! "${history_id}" =~ ^[1-9][0-9]*$ ]] \
        || (( history_id <= previous )); then
        rm -f "${owner_tmp}" "${scheduler_tmp}" "${maintenance_tmp}"
        echo "frozen ${label} journal is not in strict history-id order" >&2
        return 1
      fi
      previous="${history_id}"
      count=$((count + 1))
    done <"${file}"
    if (( count == 0 )); then
      rm -f "${owner_tmp}" "${scheduler_tmp}" "${maintenance_tmp}"
      echo "frozen ${label} journal is empty" >&2
      return 1
    fi
  done
  # These arrays and the live authority rows came from one SQL statement and
  # one MVCC snapshot. Replacing, rather than appending to, the live mirrors
  # prevents any renewal committed after cut_at from entering the evidence.
  mv -f "${owner_tmp}" "${G6RD_STATE}/fencing-history.jsonl"
  mv -f "${scheduler_tmp}" "${G6RD_STATE}/leadership-history.jsonl"
  mv -f "${maintenance_tmp}" "${G6RD_STATE}/scheduler-maintenance-history.jsonl"
}

phase_evidence_collect() {
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  local dir="${G6RD_STATE}/evidence" index service
  mkdir -p "${dir}/effects"
  # frozen database views, one JSON object per line; to_char pins every
  # timestamp to strict RFC 3339 with an explicit UTC offset
  psql_primary -Atc "SELECT jsonb_build_object('id',c.id::text,'idempotency_key',c.idempotency_key,'node_id',c.node_id::text,'state',c.state,'created_at',to_char(c.created_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),'updated_at',to_char(c.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')) FROM commands c WHERE c.idempotency_key LIKE 'g6-%' ORDER BY c.created_at, c.id" \
    >"${dir}/commands.jsonl"
  psql_primary -Atc "SELECT jsonb_build_object('command_id',a.command_id::text,'attempt_number',a.attempt_number,'state',a.state,'started_at',to_char(a.started_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),'finished_at',CASE WHEN a.finished_at IS NULL THEN '' ELSE to_char(a.finished_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') END) FROM command_attempts a JOIN commands c ON c.id=a.command_id WHERE c.idempotency_key LIKE 'g6-%' ORDER BY a.started_at, a.id" \
    >"${dir}/attempts.jsonl"
  psql_primary -Atc "SELECT jsonb_build_object('command_id',o.command_id::text,'created_at',to_char(o.created_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),'available_at',to_char(o.available_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),'published_at',CASE WHEN o.published_at IS NULL THEN '' ELSE to_char(o.published_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') END,'locked',CASE WHEN o.locked_by IS NULL THEN false ELSE true END) FROM outbox_events o JOIN commands c ON c.id=o.command_id WHERE c.idempotency_key LIKE 'g6-%' ORDER BY o.created_at, o.id" \
    >"${dir}/outbox.jsonl"
  psql_primary -Atc "SELECT jsonb_build_object('command_id',e.command_id::text,'request_id',e.request_id,'result',e.result,'occurred_at',to_char(e.occurred_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')) FROM audit_events e WHERE e.command_id IS NOT NULL AND EXISTS(SELECT 1 FROM commands c WHERE c.id=e.command_id AND c.idempotency_key LIKE 'g6-%') ORDER BY e.occurred_at, e.id" \
    >"${dir}/audit.jsonl"
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

  # All slow collection reads above run while the watchers remain live. Stop
  # public writes, then bracket one DB-clock authority cut with two complete
  # transport inventories. The immutable signed owner tuple and connection
  # identity must match on both sides, so a disconnect or replacement cannot
  # hide in the probe-to-cut interval. Authority renewers stay live throughout
  # the bracket; their lease timestamp may advance without changing the term.
  quiesce_control_plane_writers
  psql_primary -Atc "SELECT jsonb_build_object('agent_id',n.name,'last_telemetry_at',to_char(s.last_heartbeat_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')) FROM node_observed_snapshots s JOIN nodes n ON n.id=s.node_id WHERE n.name LIKE 'g6-fd-%' ORDER BY n.name" \
    >"${dir}/telemetry.jsonl"
  local args=()
  readarray -t args < <(node_ids)
  g6rd_probe_node_connection any "${args[@]}" >"${dir}/final-sessions-before.json"
  capture_database_clock >"${dir}/final-sessions-before-complete-at"
  capture_final_authority_cut
  stop_watchers
  append_final_history_snapshot
  capture_database_clock >"${dir}/final-sessions-after-start-at"
  g6rd_probe_node_connection any "${args[@]}" >"${dir}/final-sessions-after.json"
  assert_final_session_authority
  jq -er '.cut_at' "${G6RD_STATE}/final-authority-cut.json" \
    >"${dir}/final-session-observed-at"
  quiesce_transport_ingress
  quiesce_authority_renewers
  local history
  for history in fencing leadership scheduler-maintenance; do
    require_file "${G6RD_STATE}/${history}-history.jsonl"
    cp -f "${G6RD_STATE}/${history}-history.jsonl" \
      "${G6RD_OUTBOX}/${history}-history.jsonl"
  done
  jq -er '.cut_at' "${G6RD_STATE}/final-authority-cut.json" \
    >"${dir}/snapshot-taken-at"
}

phase_final_freeze() {
  require_file "${G6RD_STATE}/window-ended-at"
  require_file "${G6RD_STATE}/evidence/final-sessions.json"
  mkdir -p "${G6RD_OUTBOX}/final-freeze"
  g6rd_now >"${G6RD_OUTBOX}/final-freeze/final-freeze-at"
}

phase_runtime_result() {
  local job_status="${1:?job status is required}"
  local out="${G6RD_OUTBOX}/fd-b-final" source relative_path
  local -a paths=(
    state/read-log.jsonl
    state/enqueue-log.jsonl
    state/resource-samples.csv
    state/era2-sessions.tsv
    state/reconnect-sessions.json
    state/owner-a-terms.tsv
    state/owner-b-terms.tsv
    state/owner-replacement-sessions.json
    state/scheduler-replacement-term
    state/scheduler-maintenance-observation.json
    state/relay-b-node-id
    state/relay-b-before-command.json
    state/relay-b-observation.json
    state/relay-b-active-at
    state/relay-b-started.json
    state/relay-command-proof.json
    state/relay-dispatch-proof.json
    state/all-nodes.tsv
    state/promoted-at
    state/window-ended-at
    state/final-authority-cut.json
    state/window-opening-active.json
    state/stale-transport-probe.json
    state/stale-agent-probe.json
    state/stale-scheduler-term
    state/evidence
    outbox/relay-pre-fault
    outbox/timeline.jsonl
    outbox/fencing-history.jsonl
    outbox/leadership-history.jsonl
    outbox/scheduler-maintenance-history.jsonl
  )
  mkdir -p "${out}"
  for relative_path in "${paths[@]}"; do
    source="${G6RD_WORK}/${relative_path}"
    [[ -e "${source}" ]] || continue
    mkdir -p "$(dirname "${out}/${relative_path}")"
    cp -R -- "${source}" "${out}/${relative_path}"
  done
  g6rd_write_runtime_result "${out}" "${job_status}" \
    "$([[ "${job_status}" == success ]] && printf runtime_complete || printf unknown)"
}

unpause_scoped_container() {
  local container="${1:?container is required}" paused
  paused="$(timeout --foreground --signal=TERM --kill-after=2s 8s \
    docker container inspect --format '{{.State.Paused}}' "${container}" 2>/dev/null || true)"
  [[ "${paused}" == true ]] || return 0
  timeout --foreground --signal=TERM --kill-after=2s 8s \
    docker unpause "${container}" >/dev/null
}

phase_cleanup_prelude() {
  local status=0
  # Stop detached watchers while their sentinel still exists. Removing the run
  # directory first would let an orphaned loop recreate failed Docker clients
  # forever because its stop path disappeared.
  stop_watchers || status=1
  release_armed_pre_send_barrier || status=1
  release_armed_post_send_barrier || status=1
  release_armed_result_commit_barrier || status=1
  g6rd_release_synthetic_barriers || status=1
  local service
  for service in transportd api scheduler worker; do
    unpause_scoped_container "${COMPOSE_PROJECT}-${service}-1" || status=1
  done
  return "${status}"
}

phase_cleanup() {
  local status=0 prelude_status=0
  timeout --foreground --signal=TERM --kill-after=5s 45s \
    "${BASH_SOURCE[0]}" cleanup-prelude || prelude_status=$?
  if [[ "${prelude_status}" != 0 ]]; then
    echo "G6 cleanup prelude failed or exceeded its 45-second hard limit" >&2
    status="${prelude_status}"
  fi
  g6rd_cleanup_bounded || status=$?
  return "${status}"
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
  smoke-session) phase_smoke_session ;;
  promote) phase_promote "${2:?peer isolation directory}" ;;
  merge-peer-evidence) phase_merge_peer_evidence "${2:?peer evidence root}" ;;
scenario-scheduler) phase_scenario_scheduler ;;
scenario-owner) phase_scenario_owner ;;
relay-pre-fault) phase_relay_pre_fault "${2:?relay observation readiness directory}" ;;
scenario-relay) phase_scenario_relay "${2:?failure-domain A readiness directory}" ;;
scenario-path) phase_scenario_path ;;
outbox-claim-before-send) phase_outbox_claim_before_send ;;
outbox-send-before-mark) phase_outbox_send_before_mark ;;
  outbox-result-before-commit) phase_outbox_result_before_commit ;;
  window-barrier-arm) phase_window_barrier_arm ;;
  resource-preflight) phase_resource_preflight ;;
  window) phase_window "${2:?peer window-barrier acknowledgement is required}" ;;
  evidence-collect) phase_evidence_collect ;;
  final-freeze) phase_final_freeze ;;
  smoke-evidence) phase_smoke_evidence ;;
  runtime-result) phase_runtime_result "${2:?job status is required}" ;;
  diagnostics) g6rd_diagnostics ;;
  cleanup-prelude) phase_cleanup_prelude ;;
  cleanup) phase_cleanup ;;
*)
  echo "usage: $0 <prepare|materialize-runtime|import-peer-tunnel-nodes|build-images|tunnel-up|standby-bootstrap|relay-up|agents-enroll|agents-start|load-start|promote|merge-peer-evidence|scenario-scheduler|scenario-owner|relay-pre-fault|scenario-relay|scenario-path|outbox-claim-before-send|outbox-send-before-mark|outbox-result-before-commit|window-barrier-arm|resource-preflight|window|evidence-collect|final-freeze|runtime-result|diagnostics|cleanup-prelude|cleanup>" >&2
  exit 2
  ;;
esac
