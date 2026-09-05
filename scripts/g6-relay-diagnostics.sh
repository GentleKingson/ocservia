#!/usr/bin/env bash

# Read-only snapshots for one synthetic relay command. Never export envelopes,
# result bytes, credentials, or unrelated command rows.
g6_relay_public_keys() {
  jq -c 'walk(if type == "object" and (.idempotency_key? | type == "string") then
    if (.idempotency_key | test("^[0-9a-fA-F]{32}$")) then
      .idempotency_key = ("g6-journal-key-" + (.idempotency_key | ascii_downcase))
    else . end else . end)'
}

capture_relay_agent_proof() {
  local command="${1:?}" service="${2:?}" output="${3:?}"
  [[ "$command" =~ ^[0-9a-f]{32}$ && "$service" =~ ^agent-fd-[ab]-[0-9]{2}$ ]] || return 2
  g6rd_agent_journal_query "$service" \
    "SELECT json_object('command_id',hex(j.command_id),'idempotency_key',hex(j.idempotency_key),'state',j.state,'effect_executed_at',e.executed_at,'total_executions',(SELECT executions FROM synthetic_effect_counter WHERE singleton=1),'effect_rows',(SELECT count(*) FROM synthetic_effects)) FROM command_journal j LEFT JOIN synthetic_effects e ON e.idempotency_key=j.idempotency_key WHERE j.command_id=X'$command'" >"${output}.journal" || return 1
  G6RD_COMPOSE_TIMEOUT_SECONDS=15 g6rd_agent_compose logs --no-color --no-log-prefix "$service" \
    | jq -Rsc --arg command "$command" '[split("\n")[] | fromjson? | .fields | select(.command_id == $command)]' >"${output}.events" || return 1
  jq -n --slurpfile journal "${output}.journal" --slurpfile events "${output}.events" \
    '{journal: $journal, events: $events[0]}' | g6_relay_public_keys >"$output" || return 1
  rm -f -- "${output}.journal" "${output}.events"
}

capture_relay_command_snapshot() {
  local key="${1:?}" node="${2:?}" output="${3:?}"
  [[ "$key" =~ ^[a-zA-Z0-9._-]+$ && "$node" =~ ^[0-9a-f-]{36}$ ]] || return 2
  psql_primary_probe -Atc "SELECT jsonb_build_object(
    'observed_at',clock_timestamp(),
    'command_id',c.id,'operation_id',c.operation_id,'node_id',c.node_id,
    'command_state',c.state,'operation_state',o.state,
    'idempotency_key',c.idempotency_key,'expected_version',c.expected_version,
    'outbox',(SELECT to_jsonb(b)-'payload' FROM outbox_events b WHERE b.command_id=c.id),
    'attempts',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.attempt_number),'[]')
      FROM command_attempts a WHERE a.command_id=c.id),
    'operation_events',(SELECT coalesce(jsonb_agg(to_jsonb(e) ORDER BY e.sequence),'[]')
      FROM operation_events e WHERE e.operation_id=c.operation_id),
    'results',(SELECT coalesce(jsonb_agg(jsonb_build_object(
      'event_id',r.event_id,'state',r.state,'replayed',r.replayed,
      'idempotency_key',replace(r.idempotency_key::text,'-',''),
      'payload_sha256',encode(r.payload_sha256,'hex'),
      'result_sha256',encode(sha256(r.result),'hex'),
      'accepted_at',r.accepted_at,'completed_at',r.completed_at)), '[]')
      FROM agent_command_results r WHERE r.command_id=c.id))
    FROM commands c JOIN operations o ON o.id=c.operation_id
    WHERE c.idempotency_key='$key' AND c.node_id='$node'
      AND c.payload_type='synthetic_noop'" | g6_relay_public_keys >> "$output"
}

relay_settled_with_snapshot() {
  capture_relay_command_snapshot "$1" "$2" "$3" 2>> "${3}.errors" \
    || printf 'snapshot failed\n' >> "${3}.errors"
  wait_commands_settled "$1"
}

capture_relay_exit_diagnostics() {
  local status="$1" key="$2" node="$3" before="$4"
  local dir="${G6RD_STATE}/relay-diagnostics" command_id
  mkdir -p "$dir"
  printf '%s\n' "$status" > "$dir/scenario-exit-status"
  if ! capture_relay_command_snapshot "$key" "$node" "$dir/database.jsonl" 2> "$dir/database-error.log"; then
    printf 'database snapshot failed\n' >> "$dir/failures.log"
  fi
  command_id="$(jq -sr 'map(.command_id) | last // empty' "$dir/database.jsonl")"
  if [[ "$command_id" =~ ^[0-9a-f-]{36}$ ]]; then
    # Keep complete matching JSON events, including response diagnostics, even
    # when the proof rejects their number. No other service log is copied.
    if ! G6RD_COMPOSE_TIMEOUT_SECONDS=10 g6rd_compose logs --no-color --no-log-prefix transportd \
      | jq -Rc --arg command "${command_id//-/}" 'fromjson? | select(.fields.command_id == $command)' \
      | g6_relay_public_keys \
      > "$dir/transport-events.jsonl"; then
      printf 'transport log capture failed\n' >> "$dir/failures.log"
    fi
  else
    printf 'target command ID unavailable\n' >> "$dir/failures.log"
  fi
  if ! capture_successful_command_proof "$key" "$node" "$dir/durable-proof.json" 2> "$dir/durable-proof-error.log"; then
    printf 'durable success proof unavailable\n' >> "$dir/failures.log"
  fi
  cp -f "$before" "$dir/session-before.json" 2>/dev/null || printf 'before session missing\n' >> "$dir/failures.log"
  if ! G6RD_NODE_CONNECTION_TIMEOUT_SECONDS=5 relay_probe_relay_b "$node" "$dir/session-after.json"; then
    printf 'after session observation failed\n' >> "$dir/failures.log"
  fi
}
