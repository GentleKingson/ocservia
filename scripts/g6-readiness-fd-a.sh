#!/usr/bin/env bash
# Failure domain A of the formal G6 readiness harness: first PostgreSQL
# primary, relay-a, era-1 control plane and transportd #1, PITR restore
# host, and the rejoined standby at the end.
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FD_ALIAS="${FD_ALIAS:-fd-alpha}"
# shellcheck source=scripts/g6-readiness-lib.sh
source "${ROOT}/scripts/g6-readiness-lib.sh"
g6rd_init_environment
export FD_ALIAS

MARKER_TABLE=g6_readiness_markers
PITR_RESTORE_PORT=55432

require_file() {
  local path="${1:?path is required}"
  [[ -s "${path}" ]] || {
    echo "required file is missing: ${path}" >&2
    return 1
  }
}

peer_node_id() {
  require_file "${G6RD_STATE}/peer-tunnel-node-id"
  printf '%s\n' "$(<"${G6RD_STATE}/peer-tunnel-node-id")"
}

import_peer_tunnel_nodes() {
  local peer="${1:?peer tunnel rendezvous directory is required}" name
  for name in pg-b relay-b pg-a-forward api-a-forward relay-a-forward; do
    require_file "${peer}/${name}.node-id"
    cp -f "${peer}/${name}.node-id" "${G6RD_STATE}/peer-${name}-node-id"
  done
}

postgres_healthy() {
  g6rd_compose exec -T postgres pg_isready -U ocservia_owner -d ocservia >/dev/null 2>&1
}

phase_prepare() {
  g6rd_prepare_support_image
  g6rd_build_tunnel
  g6rd_generate_secrets
  mkdir -p "${G6RD_OUTBOX}/tunnel"
  local name
  for name in pg-a api-a relay-a relay-b-forward pg-b-forward; do
    g6rd_tunnel_key "${name}" >"${G6RD_OUTBOX}/tunnel/${name}.node-id"
  done
  openssl dgst -sha256 -r </proc/sys/kernel/random/boot_id | cut -c1-16 \
    >"${G6RD_OUTBOX}/tunnel/boot-id-sha256"
}

# The shared-trust rendezvous: everything fd-b needs to run the standby,
# relay-b, the era-2 transportd (same controller key), and the probes.
phase_publish_shared_secrets() {
  g6rd_export_common_env
  mkdir -p "${G6RD_OUTBOX}/shared"
  local name
  for name in owner-password app-password replication-password dev-auth-token \
    oidc-client-secret session-key requester-identity-id requester-session-id \
    requester-session-cookie approver-identity-id approver-session-id \
    approver-session-cookie; do
    cp -f "${G6RD_SECRETS}/${name}" "${G6RD_OUTBOX}/shared/"
  done
  cp -f "${G6RD_SECRETS}/relay-ca.pem" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/relay-chain.crt" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/relay-leaf.crt" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/relay-leaf.key" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/relay-token" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/command-signing.pem" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/command-verification.pem" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/seal-user-password.key" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/seal-user-password-sha256" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/seal-p12.key" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/seal-p12-sha256" "${G6RD_OUTBOX}/shared/"
  cp -f "${G6RD_SECRETS}/controller.key" "${G6RD_OUTBOX}/shared/"
  # tunnel keys so fd-b can serve its own forwards with stable NodeIds
  cp -f "${G6RD_SECRETS}"/tunnel-*.key "${G6RD_OUTBOX}/shared/"
  chmod 0600 "${G6RD_OUTBOX}/shared/"*
}

phase_import_peer_secrets() {
  local peer="${1:?peer shared secrets directory is required}"
  require_file "${peer}/dev-auth-token"
  for name in owner-password app-password replication-password dev-auth-token \
    oidc-client-secret session-key requester-identity-id requester-session-id \
    requester-session-cookie approver-identity-id approver-session-id \
    approver-session-cookie \
    relay-ca.pem relay-leaf.crt relay-leaf.key relay-token \
    command-signing.pem command-verification.pem \
    seal-user-password.key seal-user-password-sha256 \
    seal-p12.key seal-p12-sha256; do
    [[ -s "${G6RD_SECRETS}/${name}" ]] && continue
    cp -f "${peer}/${name}" "${G6RD_SECRETS}/${name}"
    chmod 0600 "${G6RD_SECRETS}/${name}"
  done
  require_file "${G6RD_STATE}/controller.key"
}

seed_authenticated_approval_fixtures() {
  local workspace_id="${1:?workspace id is required}"
  local requester_identity requester_session approver_identity approver_session
  local requester_binding approver_binding
  requester_identity="$(g6rd_secret requester-identity-id)"
  requester_session="$(g6rd_secret requester-session-id)"
  approver_identity="$(g6rd_secret approver-identity-id)"
  approver_session="$(g6rd_secret approver-session-id)"
  requester_binding="$(g6rd_uuidv7)"
  approver_binding="$(g6rd_uuidv7)"
  # The test IdP fixture bootstraps two independent SecurityAdmin subjects;
  # every high-risk mutation still traverses real authentication, RBAC,
  # content-bound approval, audit, and approval consumption paths.
  g6rd_psql -c "
    INSERT INTO identities(id,issuer,subject,display_name,created_at,updated_at)
      VALUES('${requester_identity}','https://oidc.g6.invalid','g6-requester','G6 requester',now(),now()),
            ('${approver_identity}','https://oidc.g6.invalid','g6-approver','G6 approver',now(),now());
    INSERT INTO auth_sessions(id,identity_id,expires_at,created_at)
      VALUES('${requester_session}','${requester_identity}',now()+interval '7 hours',now()),
            ('${approver_session}','${approver_identity}',now()+interval '7 hours',now());
    INSERT INTO role_bindings(id,identity_id,workspace_id,role_name,resource_type,resource_id,created_by,created_at)
      VALUES('${requester_binding}','${requester_identity}','${workspace_id}','SecurityAdmin','workspace',NULL,'${requester_identity}',now()),
            ('${approver_binding}','${approver_identity}','${workspace_id}','SecurityAdmin','workspace',NULL,'${requester_identity}',now());" \
    >/dev/null
}

phase_images() {
  g6rd_build_tunnel
  g6rd_export_common_env
  g6rd_write_agent_overlay "$(g6rd_agent_count)"
  g6rd_compose build postgres migrate api worker scheduler transportd \
    controller-key-init transport-runtime-init transport-endpoint-bootstrap relay g6-probe
  g6rd_agent_compose build
}

# fd-a serves its primary, API, and relay-a to the pinned peer (the peer's
# forwards dial these serve NodeIds), and forwards the peer's relay-b so
# local agents keep a two-relay map.
phase_tunnel_up() {
  g6rd_tunnel_serve pg-a "$(<"${G6RD_STATE}/peer-pg-a-forward-node-id")" 5432
  g6rd_tunnel_serve api-a "$(<"${G6RD_STATE}/peer-api-a-forward-node-id")" 18080
  g6rd_tunnel_serve relay-a "$(<"${G6RD_STATE}/peer-relay-a-forward-node-id")" \
    "${G6_RELAY_BIND_PORT:-3443}"
  g6rd_tunnel_forward relay-b-forward "$(<"${G6RD_STATE}/peer-relay-b-node-id")" \
    "${G6_RELAY_B_FORWARD_PORT:-3445}"
}

bootstrap_controller_endpoint() {
  g6rd_compose up --detach controller-key-init transport-runtime-init
  g6rd_compose --profile bootstrap up --detach transport-endpoint-bootstrap
  local endpoint
  for _ in {1..60}; do
    endpoint="$(g6rd_compose logs --no-color transport-endpoint-bootstrap 2>/dev/null \
      | sed -n 's/.*"endpoint_id":"\([0-9a-f]\{64\}\)".*/\1/p' | tail -1)"
    [[ -n "${endpoint}" ]] && break
    sleep 1
  done
  [[ "${endpoint:-}" =~ ^[0-9a-f]{64}$ ]] || {
    echo "transport endpoint bootstrap did not report an endpoint" >&2
    return 1
  }
  g6rd_compose --profile bootstrap stop transport-endpoint-bootstrap
  g6rd_compose --profile bootstrap rm --force transport-endpoint-bootstrap
  printf '%s\n' "${endpoint}" >"${G6RD_STATE}/controller-endpoint-id"
  export OCSERV_CONTROLLER_ENDPOINT_ID="${endpoint}"
}

phase_primary_up() {
  g6rd_export_common_env
  g6rd_prepare_postgres_bind_dirs
  export G6_DB_HOST=postgres G6_DB_PORT=5432
  g6rd_compose up --detach postgres
  g6rd_wait_until 60 2 "postgres healthy" postgres_healthy
  g6rd_compose run --rm migrate
  local workspace_id
  workspace_id="$(g6rd_uuidv7)"
  g6rd_psql -c "INSERT INTO workspaces(id,name,slug,created_at,updated_at) \
    VALUES('${workspace_id}','G6 Readiness','g6-readiness',now(),now()) \
    ON CONFLICT (id) DO NOTHING" >/dev/null
  seed_authenticated_approval_fixtures "${workspace_id}"
  printf '%s\n' "${workspace_id}" >"${G6RD_STATE}/workspace-id"
  g6rd_psql -c "CREATE TABLE IF NOT EXISTS ${MARKER_TABLE} (
    id text PRIMARY KEY,
    txid text NOT NULL,
    phase text NOT NULL,
    written_at timestamptz NOT NULL DEFAULT now()
  )"
  bootstrap_controller_endpoint
  g6rd_compose up --detach relay
  g6rd_compose up --detach worker
  g6rd_wait_until 60 1 "worker trust socket" \
    g6rd_compose exec -T worker test -S /run/ocserv-trust/control-plane.sock
  g6rd_compose up --detach transportd api scheduler
  g6rd_wait_until 60 1 "transportd socket" \
    g6rd_compose exec -T transportd test -S /run/ocserv-platform/transportd.sock
  export G6RD_WORKSPACE_ID="${workspace_id}"
  g6rd_wait_until 60 2 "api ready" g6rd_api_ready
  mkdir -p "${G6RD_OUTBOX}/primary-up"
  cp -f "${G6RD_STATE}/controller-endpoint-id" "${G6RD_OUTBOX}/primary-up/"
  cp -f "${G6RD_STATE}/workspace-id" "${G6RD_OUTBOX}/primary-up/workspace-id"
  # The controller iroh key hands the controller NodeId to fd-b's era-2
  # transportd; the copy stays inside the 1-day rendezvous artifact.
  cp -f "${G6RD_SECRETS}/controller.key" "${G6RD_OUTBOX}/primary-up/controller.key"
  chmod 0600 "${G6RD_OUTBOX}/primary-up/"*
}

# Acknowledged markers and the WAL restore point for the later PITR proof,
# plus the base backup the restore replays from. All on fd-a's clock while
# the primary is still authoritative.
phase_pitr_prepare() {
  mkdir -p "${G6RD_OUTBOX}/pitr-prep"
  # A physical restore can only replay forward from its base backup. Take and
  # verify the backup before the target markers, then bind each marker's txid
  # and timestamp to the INSERT transaction that acknowledged it.
  docker run --rm --pull=never --network host --log-driver none \
    -e PGPASSWORD="$(g6rd_secret replication-password)" \
    -v "${G6RD_BASEBACKUP}:/backup" postgres:17.10-bookworm \
    sh -c 'pg_basebackup -h 127.0.0.1 -p 5432 -U ocservia_replication -D /backup -X stream --checkpoint=fast' \
    >"${G6RD_LOGS}/basebackup.log" 2>&1
  docker run --rm --pull=never -v "${G6RD_BASEBACKUP}:/backup:ro" postgres:17.10-bookworm \
    pg_verifybackup /backup >>"${G6RD_LOGS}/basebackup.log" 2>&1
  g6rd_reclaim_directory "${G6RD_BASEBACKUP}" || true
  local marker_output marker_row switch_target
  marker_output="$(g6rd_psql -Atc "INSERT INTO ${MARKER_TABLE}(id,txid,phase) \
    VALUES ('pitr-marker-a',txid_current()::text,'pitr_a') \
    RETURNING txid || ':' || to_char(written_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")"
  marker_row="$(g6rd_extract_pitr_marker_row "${marker_output}")"
  printf '%s\n' "${marker_row}" >"${G6RD_STATE}/pitr-marker-a"
  sed -n 's/^[^:]*://p' "${G6RD_STATE}/pitr-marker-a" >"${G6RD_OUTBOX}/pitr-prep/pitr-marker-a-at"
  sleep 1
  g6rd_psql -Atc "SELECT pg_create_restore_point('g6_pitr_target')" >/dev/null
  g6rd_psql -Atc "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')" \
    >"${G6RD_STATE}/pitr-restore-point-at"
  cp -f "${G6RD_STATE}/pitr-restore-point-at" "${G6RD_OUTBOX}/pitr-prep/restore-point-at"
  sleep 1
  marker_output="$(g6rd_psql -Atc "INSERT INTO ${MARKER_TABLE}(id,txid,phase) \
    VALUES ('pitr-marker-b',txid_current()::text,'pitr_b') \
    RETURNING txid || ':' || to_char(written_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")"
  marker_row="$(g6rd_extract_pitr_marker_row "${marker_output}")"
  printf '%s\n' "${marker_row}" >"${G6RD_STATE}/pitr-marker-b"
  sed -n 's/^[^:]*://p' "${G6RD_STATE}/pitr-marker-b" >"${G6RD_OUTBOX}/pitr-prep/pitr-marker-b-at"
  switch_target="$(g6rd_psql -Atc 'SELECT pg_walfile_name(pg_switch_wal())')"
  [[ "${switch_target}" =~ ^[0-9A-F]{24}$ ]] || {
    echo "pg_switch_wal returned an invalid segment name: ${switch_target}" >&2
    return 1
  }
  g6rd_wait_until 60 1 "PITR target WAL archived" \
    g6rd_archive_has_segment "${switch_target}"
  printf '%s\n' "${switch_target}" >"${G6RD_OUTBOX}/pitr-prep/archived-wal-segment"
}

# Prepare and durably activate the local managed nodes. The transport runtime
# is reloaded only after fd-b has activated its nodes too, so its fail-closed
# startup snapshot contains the complete cross-domain fleet.
phase_agents_enroll() {
  local index count dir name endpoint token node_id enrollment_log
  count="$(g6rd_agent_count)"
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  g6rd_write_agent_overlay "${count}"
  g6rd_wait_for_controller_relay
  mkdir -p "${G6RD_OUTBOX}/agents"
  : >"${G6RD_OUTBOX}/agents/nodes.tsv"
  for index in $(seq 1 "${count}"); do
    dir="$(g6rd_agent_dir "${index}")"
    name="g6-fd-a-$(printf '%02d' "${index}")"
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
    printf '%s\n' "${token}" >"${dir}/secrets/enrollment-token"
    chmod 0600 "${dir}/secrets/enrollment-token"
    docker run --rm --pull=never -v "${dir}/secrets:/fix" postgres:17.10-bookworm \
      chown 65532:65532 /fix/enrollment-token >/dev/null 2>&1
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
    printf '%s\t%s\t%s\n' "${name}" "${node_id}" "${endpoint}" \
      >>"${G6RD_OUTBOX}/agents/nodes.tsv"
    printf '%s\n' "${node_id}" >"${dir}/state/node-id"
  done
}

phase_transport_trust_reload() {
  local peer="${1:?fd-b enrollment rendezvous is required}"
  local container started_at
  require_file "${peer}/candidate-sha"
  [[ "$(<"${peer}/candidate-sha")" == "${G6RD_CANDIDATE_SHA}" ]] || {
    echo "fd-b enrollment rendezvous belongs to a different candidate" >&2
    return 1
  }
  g6rd_export_common_env
  g6rd_compose restart transportd
  container="$(g6rd_compose ps -q transportd)"
  [[ -n "${container}" ]] || {
    echo "transportd container is absent after trust snapshot reload" >&2
    return 1
  }
  started_at="$(docker inspect --format '{{.State.StartedAt}}' "${container}")"
  g6rd_wait_until 60 1 "transportd serving after trust snapshot reload" \
    transportd_serving_since "${container}" "${started_at}"
  g6rd_wait_until 60 2 "api ready after trust snapshot reload" g6rd_api_ready
  mkdir -p "${G6RD_OUTBOX}/trust-ready"
  printf '%s\n' "${G6RD_CANDIDATE_SHA}" >"${G6RD_OUTBOX}/trust-ready/candidate-sha"
}

transportd_serving_since() {
  local container="${1:?transportd container is required}"
  local started_at="${2:?transportd start time is required}"
  docker logs --since "${started_at}" "${container}" 2>&1 \
    | grep -q 'transportd serving'
}

phase_agents_start() {
  local count
  count="$(g6rd_agent_count)"
  G6RD_WORKSPACE_ID="$(<"${G6RD_STATE}/workspace-id")"
  export G6RD_WORKSPACE_ID
  g6rd_export_common_env
  g6rd_write_agent_overlay "${count}"
  g6rd_chown_agent_dirs
  g6rd_start_agent_fleet "${G6RD_OUTBOX}/agents/nodes.tsv"
}

# The database failure: fd-a's primary is stopped under active load, and
# with it the era-1 control plane. The record carries fd-a's own clock for
# outage declaration and isolation so the dual-primary probes taken on the
# same clock stay comparable.
phase_isolate() {
  local outage isolated
  g6rd_require_support_image
  mkdir -p "${G6RD_OUTBOX}/isolation"
  g6rd_psql -Atc "WITH active AS (
    SELECT c.id::text AS command_id,c.state AS command_state,a.state AS attempt_state,
      a.finished_at,s.last_heartbeat_at
    FROM commands c
    JOIN LATERAL (
      SELECT state,finished_at FROM command_attempts
      WHERE command_id=c.id ORDER BY attempt_number LIMIT 1
    ) a ON true
    LEFT JOIN node_observed_snapshots s ON s.node_id=c.node_id
    WHERE c.idempotency_key LIKE 'g6-load-${RUN_ID%-fd-a}-fd-b-%'
      AND c.idempotency_key NOT LIKE '%-backlog-%'
      AND c.state IN ('dispatched','accepted','running')
      AND NOT EXISTS (SELECT 1 FROM agent_command_results r WHERE r.command_id=c.id)
  ) SELECT jsonb_build_object(
    'captured_at',to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"'),
    'commands',coalesce(jsonb_agg(jsonb_build_object(
      'command_id',command_id,'command_state',command_state,
      'attempt_state',attempt_state,'attempt_finished',finished_at IS NOT NULL,
      'last_telemetry_at',to_char(last_heartbeat_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')) ORDER BY command_id),'[]'::jsonb)
    ,'queued_outbox_count',(SELECT count(*) FROM outbox_events o JOIN commands c ON c.id=o.command_id
      WHERE c.idempotency_key LIKE 'g6-load-${RUN_ID%-fd-a}-fd-b-backlog-%'
        AND o.published_at IS NULL AND o.available_at<=now())
  ) FROM active" >"${G6RD_OUTBOX}/isolation/active-load.json"
  jq -e '.queued_outbox_count >= 50 and (.commands | length >= 50 and all(
    (.command_state | IN("dispatched","accepted","running")) and
    .attempt_state == "sent" and .attempt_finished and
    (.last_telemetry_at | type == "string" and length > 0)))' \
    "${G6RD_OUTBOX}/isolation/active-load.json" >/dev/null || {
    echo "fewer than fifty real commands, outbox rows, and live telemetry producers are active at failure injection" >&2
    return 1
  }
  g6rd_now >"${G6RD_STATE}/outage-declared-at"
  g6rd_compose stop scheduler api worker transportd >/dev/null 2>&1
  g6rd_release_synthetic_barriers
  g6rd_compose stop postgres
  g6rd_now >"${G6RD_STATE}/isolated-at"
  outage="$(<"${G6RD_STATE}/outage-declared-at")"
  isolated="$(<"${G6RD_STATE}/isolated-at")"
  jq -cn --arg outage_declared_at "${outage}" --arg isolated_at "${isolated}" \
    --arg fd "${FD_ID}" \
    '{outage_declared_at:$outage_declared_at,isolated_at:$isolated_at,failure_domain:$fd}' \
    >"${G6RD_OUTBOX}/isolation/isolation.json"
  printf '%s\n' "${outage}" >"${G6RD_OUTBOX}/isolation/outage-declared-at"
  printf '%s\n' "${isolated}" >"${G6RD_OUTBOX}/isolation/isolated-at"
}

# Repeated write attempts against the stopped former primary, on fd-a's
# clock. Every attempt must fail while the instance is down.
phase_dual_primary_probes() {
  local promoted_dir="${1:?promoted primary directory is required}"
  require_file "${promoted_dir}/promoted-at"
  local promoted_at attempt at accepted
  promoted_at="$(<"${promoted_dir}/promoted-at")"
  : >"${G6RD_OUTBOX}/isolation/isolated-primary-writes.jsonl"
  for attempt in 1 2 3 4 5 6; do
    at="$(g6rd_now)"
    accepted=false
    if PGPASSWORD="$(g6rd_secret owner-password)" \
      docker run --rm --pull=never --network host --log-driver none \
      -e PGPASSWORD postgres:17.10-bookworm \
      psql "host=127.0.0.1 port=5432 user=ocservia_owner dbname=ocservia sslmode=disable" \
      -v ON_ERROR_STOP=1 -c "INSERT INTO ${MARKER_TABLE}(id,txid,phase) \
        VALUES ('dual-primary-probe-${attempt}',txid_current()::text,'probe')" \
      >/dev/null 2>&1; then
      accepted=true
    fi
    jq -cn --arg at "${at}" --argjson accepted "${accepted}" \
      '{at:$at,accepted:$accepted}' >>"${G6RD_OUTBOX}/isolation/isolated-primary-writes.jsonl"
    sleep 1
  done
  jq -s 'length == 6 and all(.accepted == false)' \
    "${G6RD_OUTBOX}/isolation/isolated-primary-writes.jsonl"
  jq -e --arg promoted "${promoted_at}" \
    'all((.at | fromdateiso8601) >= ($promoted | fromdateiso8601))' \
    "${G6RD_OUTBOX}/isolation/isolated-primary-writes.jsonl" >/dev/null || {
    echo "a dual-primary probe predates the replacement promotion" >&2
    return 1
  }
}

# PITR restore of fd-a's own base backup plus archived WAL up to the named
# restore point, into a scratch container. Marker A must return, marker B
# must not. Mirrors the stage-6 restore with the same two markers.
phase_pitr_restore() {
  require_file "${G6RD_STATE}/pitr-marker-a"
  require_file "${G6RD_STATE}/pitr-marker-b"
  require_file "${G6RD_STATE}/pitr-restore-point-at"
  local pitr_container="${COMPOSE_PROJECT}-pitr" restore_dir="${G6RD_RESTORE}"
  rm -rf -- "${restore_dir}"
  mkdir -p "${restore_dir}"
  cp -a "${G6RD_BASEBACKUP}/." "${restore_dir}/"
  docker run --rm --pull=never -v "${restore_dir}:/data" postgres:17.10-bookworm \
    sh -c "printf '%s\n' \"restore_command = 'cp /var/lib/postgresql/archive/%f %p'\" \
      \"recovery_target_name = 'g6_pitr_target'\" \
      \"recovery_target_action = 'promote'\" >> /data/postgresql.auto.conf && \
      touch /data/recovery.signal && chmod 700 /data" \
    >"${G6RD_LOGS}/pitr-prepare.log" 2>&1
  docker run -d --pull=never --name "${pitr_container}" \
    -p "127.0.0.1:${PITR_RESTORE_PORT}:5432" \
    -e POSTGRES_DB=ocservia -e POSTGRES_USER=ocservia_owner \
    -e POSTGRES_PASSWORD="$(g6rd_secret owner-password)" \
    -v "${restore_dir}:/var/lib/postgresql/data" \
    -v "${G6RD_ARCHIVE}:/var/lib/postgresql/archive:ro" \
    postgres:17.10-bookworm >/dev/null
  local marker_counts="" restored_at pitr_query
  pitr_query="host=127.0.0.1 port=${PITR_RESTORE_PORT} user=ocservia_owner dbname=ocservia sslmode=disable"
  for _ in {1..60}; do
    if marker_counts="$(PGPASSWORD="$(g6rd_secret owner-password)" docker run --rm --pull=never \
      --network host --log-driver none -e PGPASSWORD postgres:17.10-bookworm \
      psql "${pitr_query}" -Atc \
      "SELECT count(*) FILTER (WHERE id='pitr-marker-a') || ':' || count(*) FILTER (WHERE id='pitr-marker-b') FROM ${MARKER_TABLE}" \
      2>/dev/null)"; then
      break
    fi
    sleep 2
  done
  restored_at="$(g6rd_now)"
  docker rm -f "${pitr_container}" >/dev/null 2>&1 || true
  g6rd_reclaim_directory "${restore_dir}" || true
  [[ "${marker_counts}" == "1:0" ]] || {
    echo "PITR restore recovered marker counts '${marker_counts}', expected 1:0" >&2
    return 1
  }
  local marker_a marker_b
  marker_a="$(<"${G6RD_STATE}/pitr-marker-a")"
  marker_b="$(<"${G6RD_STATE}/pitr-marker-b")"
  mkdir -p "${G6RD_OUTBOX}/pitr"
  jq -cn --arg environment_id "${G6RD_ENVIRONMENT_ID}" \
    --arg candidate_sha "${G6RD_CANDIDATE_SHA}" \
    --arg a_txid "${marker_a%%:*}" --arg a_at "${marker_a#*:}" \
    --arg restore_point_at "$(cat "${G6RD_STATE}/pitr-restore-point-at")" \
    --arg b_txid "${marker_b%%:*}" --arg b_at "${marker_b#*:}" \
    --arg restored_at "${restored_at}" \
    '{environment_id:$environment_id,candidate_sha:$candidate_sha,
      marker_a:{txid:$a_txid,written_at:$a_at},
      restore_point_created_at:$restore_point_at,
      marker_b:{txid:$b_txid,written_at:$b_at},
      restore:{restored_at:$restored_at,marker_a_present:true,marker_b_present:false}}' \
    >"${G6RD_OUTBOX}/pitr/pitr-report.json"
}

rejoined_in_recovery() {
  [[ "$(g6rd_psql -Atc 'SELECT pg_is_in_recovery()' 2>/dev/null)" == t ]]
}

phase_rejoin_readonly_probes() {
  local attempt at accepted output file="${G6RD_OUTBOX}/post-rejoin-probes.jsonl"
  : >"${file}"
  for attempt in 1 2 3; do
    at="$(g6rd_now)"
    accepted=false
    if output="$(PGPASSWORD="$(g6rd_secret owner-password)" docker run --rm --pull=never \
      --network host --log-driver none -e PGPASSWORD postgres:17.10-bookworm \
      psql "host=127.0.0.1 port=5432 user=ocservia_owner dbname=ocservia sslmode=disable" \
      -v ON_ERROR_STOP=1 -c "INSERT INTO ${MARKER_TABLE}(id,txid,phase) VALUES ('post-rejoin-${attempt}',txid_current()::text,'probe')" 2>&1)"; then
      accepted=true
    elif ! grep -q 'cannot execute INSERT in a read-only transaction' <<<"${output}"; then
      echo "post-rejoin probe failed for a reason other than read-only rejection" >&2
      printf '%s\n' "${output}" >&2
      return 1
    fi
    jq -cn --arg at "${at}" --argjson accepted "${accepted}" \
      '{at:$at,accepted:$accepted}' >>"${file}"
    sleep 1
  done
  jq -s -e 'length == 3 and all(.accepted == false)' "${file}" >/dev/null
}

# Rejoin the former primary as a streaming standby of the promoted peer
# through the reversed tunnel forward, completing the distinct-failure-
# domain standby the G6 topology requires.
phase_rejoin() {
  require_file "${G6RD_STATE}/peer-pg-b-node-id"
  g6rd_tunnel_forward pg-b-forward "$(<"${G6RD_STATE}/peer-pg-b-node-id")" 15432
  local data_volume="${COMPOSE_PROJECT}_postgres-data"
  docker run --rm --pull=never -v "${data_volume}:/data" \
    -e PGPASSWORD="$(g6rd_secret replication-password)" postgres:17.10-bookworm \
    sh -c 'pg_rewind -D /data --source-server="host=host.docker.internal port=15432 user=ocservia_replication dbname=ocservia password=$PGPASSWORD" || (rm -rf /data/* && PGSSLMODE=disable pg_basebackup -h host.docker.internal -p 15432 -U ocservia_replication -D /data -R -X stream -C -S g6_rejoin_slot --checkpoint=fast)' \
    >"${G6RD_LOGS}/rejoin.log" 2>&1
  docker run --rm --pull=never -v "${data_volume}:/data" postgres:17.10-bookworm \
    sh -c "printf '%s\n' \"primary_conninfo = 'host=host.docker.internal port=15432 user=ocservia_replication password=$(g6rd_secret replication-password) application_name=g6_rejoin_standby'\" >> /data/postgresql.auto.conf && touch /data/standby.signal" \
    >>"${G6RD_LOGS}/rejoin.log" 2>&1
  export G6_DB_HOST=postgres G6_DB_PORT=5432
  g6rd_compose up --detach postgres
  g6rd_wait_until 60 2 "rejoined standby in recovery" rejoined_in_recovery
  g6rd_now >"${G6RD_OUTBOX}/rejoin-at"
  phase_rejoin_readonly_probes
}

phase_relay_a_stop() {
  g6rd_compose stop relay
  g6rd_now >"${G6RD_OUTBOX}/relay-a-failed-at"
}

copy_control_evidence() {
  local out="${1:?control evidence destination is required}"
  mkdir -p "${out}/isolation" "${out}/pitr-prep" "${out}/pitr"
  cp -f "${G6RD_OUTBOX}/isolation/isolation.json" "${out}/isolation/"
  cp -f "${G6RD_OUTBOX}/isolation/active-load.json" "${out}/isolation/"
  cp -f "${G6RD_OUTBOX}/isolation"/*.at "${out}/isolation/"
  cp -f "${G6RD_OUTBOX}/isolation/isolated-primary-writes.jsonl" "${out}/isolation/"
  cp -f "${G6RD_OUTBOX}/pitr-prep"/* "${out}/pitr-prep/"
  cp -f "${G6RD_OUTBOX}/pitr/pitr-report.json" "${out}/pitr/"
  cp -f "${G6RD_OUTBOX}/rejoin-at" "${out}/"
  cp -f "${G6RD_OUTBOX}/post-rejoin-probes.jsonl" "${out}/"
  cp -f "${G6RD_OUTBOX}/relay-a-failed-at" "${out}/"
}

# Publish the causal control-plane evidence needed by fd-b's scenarios while
# keeping all 28 Agents alive. Journals and container inventory are deliberately
# excluded until fd-b closes the bounded window and requests the final freeze.
phase_ready() {
  copy_control_evidence "${G6RD_OUTBOX}/fd-a-ready"
}

# Final failure-domain rendezvous for evidence assembly on fd-b. The freeze
# request is published only after fd-b has completed every scenario, closed the
# bounded window, and captured the 55-node final session inventory.
# No credentials enter this bundle.
phase_evidence() {
  local freeze="${1:?final-freeze directory is required}"
  require_file "${freeze}/final-freeze-at"
  local out="${G6RD_OUTBOX}/fd-a-final" name
  copy_control_evidence "${out}"
  mkdir -p "${out}/evidence/effects"
  cp -f "${freeze}/final-freeze-at" "${out}/"
  g6rd_now >"${out}/freeze-received-at"
  local index service
  for index in $(seq 1 "$(g6rd_agent_count)"); do
    service="agent-${FD_ID}-$(printf '%02d' "${index}")"
    g6rd_agent_compose exec -T "${service}" \
      sqlite3 -readonly /run/ocservia-agent/journal/agent.db \
      "SELECT hex(e.idempotency_key)||' '||hex(j.command_id)||' '||e.executed_at FROM synthetic_effects e JOIN command_journal j ON j.idempotency_key=e.idempotency_key" \
      >"${out}/evidence/effects/${service}.tsv"
  done
  : >"${out}/evidence/instances.tsv"
  docker ps -a --filter "label=com.docker.compose.project=${COMPOSE_PROJECT}" \
    --format '{{.Names}}' | sort -u | while read -r name; do
    [[ -n "${name}" ]] || continue
    docker inspect --format \
      '{{.Name}}	{{.Image}}	{{.State.StartedAt}}	{{.State.FinishedAt}}	{{index .Config.Labels "com.docker.compose.service"}}' \
      "${name}" 2>/dev/null || true
  done >>"${out}/evidence/instances.tsv"
  g6rd_now >"${out}/evidence/snapshot-taken-at"
  printf 'failure_domain=%s\nalias=%s\n' "${FD_ID}" "${FD_ALIAS}" >"${out}/evidence/failure-domain.txt"
}

case "${1:-}" in
prepare) phase_prepare ;;
publish-shared-secrets) phase_publish_shared_secrets ;;
import-peer-secrets) phase_import_peer_secrets "${2:?peer directory}" ;;
import-peer-tunnel-nodes) import_peer_tunnel_nodes "${2:?peer directory}" ;;
images) phase_images ;;
tunnel-up) phase_tunnel_up ;;
primary-up) phase_primary_up ;;
pitr-prepare) phase_pitr_prepare ;;
agents-enroll) phase_agents_enroll ;;
transport-trust-reload) phase_transport_trust_reload "${2:?fd-b enrollment rendezvous is required}" ;;
agents-start) phase_agents_start ;;
isolate) phase_isolate ;;
dual-primary-probes) phase_dual_primary_probes "${2:?promoted primary directory is required}" ;;
pitr-restore) phase_pitr_restore ;;
rejoin) phase_rejoin ;;
relay-a-stop) phase_relay_a_stop ;;
ready) phase_ready ;;
evidence) phase_evidence "${2:?final-freeze directory is required}" ;;
diagnostics) g6rd_diagnostics ;;
cleanup) g6rd_cleanup_bounded ;;
*)
  echo "usage: $0 <prepare|publish-shared-secrets|import-peer-secrets|import-peer-tunnel-nodes|images|tunnel-up|primary-up|pitr-prepare|agents-enroll|transport-trust-reload|agents-start|isolate|dual-primary-probes|pitr-restore|rejoin|relay-a-stop|ready|evidence|diagnostics|cleanup>" >&2
  exit 2
  ;;
esac
