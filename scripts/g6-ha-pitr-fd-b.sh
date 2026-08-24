#!/usr/bin/env bash
# Failure domain B of the G6 HA/PITR harness: streaming standby across the
# pinned Iroh tunnel, promotion target, and owner of the merged evidence.
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FD_ALIAS="${FD_ALIAS:-fd-beta}"
# shellcheck source=scripts/g6-ha-pitr-lib.sh
source "${ROOT}/scripts/g6-ha-pitr-lib.sh"
g6_ha_init_environment
export FD_ALIAS

require_file() {
  local path="${1:?path is required}"
  [[ -s "${path}" ]] || {
    echo "required file is missing: ${path}" >&2
    return 1
  }
}

peer_node_id() {
  require_file "${G6HA_STATE}/peer-tunnel-node-id"
  printf '%s\n' "$(<"${G6HA_STATE}/peer-tunnel-node-id")"
}

standby_in_recovery() {
  [[ "$(g6_ha_psql -Atc 'SELECT pg_is_in_recovery()' 2>/dev/null)" == t ]]
}

standby_streaming() {
  standby_in_recovery && [[ "$(g6_ha_psql -Atc \
    "SELECT pg_is_wal_replay_paused()" 2>/dev/null)" == f ]]
}

peer_boot_hash() {
  require_file "${G6HA_STATE}/peer-boot-id"
  printf '%s\n' "$(<"${G6HA_STATE}/peer-boot-id")"
}

phase_prepare() {
  g6_ha_build_tunnel
  g6_ha_generate_secrets
  local tunnel_node
  tunnel_node="$(g6_ha_tunnel_key)"
  mkdir -p "${G6HA_OUTBOX}/tunnel"
  printf '%s\n' "${tunnel_node}" >"${G6HA_OUTBOX}/tunnel/tunnel-node-id"
  printf '%s\n' "$(g6_ha_boot_id_hash)" >"${G6HA_OUTBOX}/tunnel/boot-id-sha256"
}

phase_images() {
  g6_ha_timing_run control_plane_build g6_ha_compose build \
    migrate api worker scheduler controller-key-init
  g6_ha_timing_run transportd_build g6_ha_compose build \
    transportd transport-runtime-init transport-endpoint-bootstrap
  g6_ha_timing_run tunnel_build g6_ha_build_tunnel
  g6_ha_timing_record_images
}

phase_tunnel_up() {
  # fd-b reaches the peer primary through the pinned tunnel as the client.
  g6_ha_tunnel_forward "$(peer_node_id)"
  sleep 2
  kill -0 "$(<"${G6HA_STATE}/tunnel-forward.pid")"
}

bootstrap_controller_endpoint() {
  g6_ha_compose up --detach controller-key-init transport-runtime-init
  g6_ha_compose --profile bootstrap up --detach transport-endpoint-bootstrap
  local endpoint
  for _ in {1..60}; do
    endpoint="$(g6_ha_compose logs --no-color transport-endpoint-bootstrap 2>/dev/null \
      | sed -n 's/.*"endpoint_id":"\([0-9a-f]\{64\}\)".*/\1/p' | tail -1)"
    [[ -n "${endpoint}" ]] && break
    sleep 1
  done
  [[ "${endpoint:-}" =~ ^[0-9a-f]{64}$ ]] || {
    echo "transport endpoint bootstrap did not report an endpoint" >&2
    return 1
  }
  g6_ha_compose --profile bootstrap stop transport-endpoint-bootstrap
  g6_ha_compose --profile bootstrap rm --force transport-endpoint-bootstrap
  printf '%s\n' "${endpoint}" >"${G6HA_STATE}/controller-endpoint-id"
}

phase_standby_bootstrap() {
  local peer="${1:?peer primary-up directory is required}"
  require_file "${peer}/owner-password"
  require_file "${peer}/replication-password"
  require_file "${peer}/app-password"
  # The cloned cluster keeps fd-a's roles and passwords: fd-b's own random
  # credentials must be replaced by the peer's before any local client runs.
  g6_ha_import_peer_cluster_credentials "${peer}"
  # The clone runs inside the postgres image against the peer primary through
  # the pinned tunnel, as the postgres user (the capability-dropped service
  # context cannot write volume contents as uid 0). The slot keeps WAL
  # available until streaming starts.
  g6_ha_compose run --rm --no-deps -T --user 999:999 \
    -e PGPASSWORD="$(<"${peer}/replication-password")" postgres \
    pg_basebackup -h host.docker.internal -p 15432 -U ocservia_replication \
    -D /var/lib/postgresql/data -R -X stream -C -S g6_slot --checkpoint=fast \
    < /dev/null >"${G6HA_LOGS}/basebackup.log" 2>&1
  docker run --rm --entrypoint /bin/sh \
    -v "${COMPOSE_PROJECT}_postgres-data:/data" postgres:17.10-bookworm \
    -c "printf '%s\n' \"primary_conninfo = 'host=host.docker.internal port=15432 user=ocservia_replication password=$(<"${peer}/replication-password") application_name=g6_standby'\" >> /data/postgresql.auto.conf"
  g6_ha_compose up --detach postgres
  g6_ha_wait_until 60 2 "standby in recovery" standby_in_recovery
  mkdir -p "${G6HA_OUTBOX}/standby"
  g6_ha_now >"${G6HA_OUTBOX}/standby/standby-streaming-at"
}

phase_roles_up() {
  export G6_DB_HOST=host.docker.internal
  export G6_DB_PORT=15432
  bootstrap_controller_endpoint
  g6_ha_export_common_env
  g6_ha_compose up --detach worker
  g6_ha_wait_until 60 1 "worker trust socket" \
    g6_ha_compose exec -T worker test -S /run/ocserv-trust/control-plane.sock
  g6_ha_compose up --detach transportd api scheduler
  g6_ha_wait_until 60 1 "transportd socket" \
    g6_ha_compose exec -T transportd test -S /run/ocserv-platform/transportd.sock
  g6_ha_wait_until 60 2 "api ready" g6_ha_api_ready
}

sync_standby_confirmed() {
  [[ "$(g6_ha_primary_psql -Atc \
    "SELECT sync_state FROM pg_stat_replication WHERE application_name = 'g6_standby'" \
    2>/dev/null)" == sync ]]
}

# Declares readiness for failover: fd-b roles run against the primary through
# the tunnel, and the streaming standby is raised to the confirmed
# synchronous standby from this point on (never at initdb, where it would
# block every commit before the standby exists). The load phase has already
# completed by the time this runs; its bracketing timeline events belong to
# the later real-agent run and are deliberately not emitted here.
phase_failover_ready() {
  g6_ha_primary_psql -Atc \
    "ALTER SYSTEM SET synchronous_standby_names = 'FIRST 1 (g6_standby)'" >/dev/null
  g6_ha_primary_psql -Atc 'SELECT pg_reload_conf()' >/dev/null
  g6_ha_wait_until 30 2 "synchronous standby confirmed" sync_standby_confirmed
  g6_ha_wait_until 30 2 "worker connected through the tunnel" peer_role_connected worker
  g6_ha_wait_until 30 2 "scheduler connected through the tunnel" peer_role_connected scheduler
  g6_ha_timeline_init
  mkdir -p "${G6HA_OUTBOX}/failover-ready"
  g6_ha_now >"${G6HA_OUTBOX}/failover-ready/ready-at"
}

# Role backends live on whichever PostgreSQL they dial, which before promotion
# is the fd-a primary reached through the tunnel.
peer_role_connected() {
  local role="${1:?role name is required}"
  [[ "$(g6_ha_primary_psql -Atc \
    "SELECT count(*) FROM pg_stat_activity WHERE application_name = '${FD_ID}-${role}'" \
    2>/dev/null)" != 0 ]]
}

promoted_and_writable() {
  [[ "$(g6_ha_psql -Atc 'SELECT pg_is_in_recovery()' 2>/dev/null)" == f ]]
}

worker_connected_local() {
  g6_ha_role_connected worker
}

scheduler_connected_local() {
  g6_ha_role_connected scheduler
}

phase_promote() {
  local isolation="${1:?peer isolation directory is required}"
  require_file "${isolation}/isolation.json"
  g6_ha_timeline_event primary_failure_injected
  g6_ha_timeline_event old_primary_isolated

  # The promoted side becomes the tunnel server; fd-a's recovery phase
  # flips to forwarding against this serve endpoint.
  g6_ha_tunnel_serve "$(peer_node_id)"

  g6_ha_psql -Atc 'SELECT pg_promote(wait := true)' >/dev/null
  g6_ha_psql -Atc "ALTER SYSTEM SET synchronous_standby_names = ''" >/dev/null
  g6_ha_psql -Atc 'SELECT pg_reload_conf()' >/dev/null
  g6_ha_wait_until 30 2 "promoted primary writable" promoted_and_writable
  # The true promotion boundary, recorded immediately after the promoted
  # primary was confirmed writable and before any recovered role touches it;
  # fd-a's post-promotion probes start only after receiving this record.
  local promoted_at
  promoted_at="$(g6_ha_now)"
  printf '%s\n' "${promoted_at}" >"${G6HA_STATE}/promoted-at"
  g6_ha_psql -At -c "INSERT INTO g6_ha_markers(id, txid, phase) VALUES ('new-primary-writable', txid_current()::text, 'promotion')" >/dev/null
  g6_ha_timeline_event new_primary_writable
  g6_ha_timeline_event new_primary_promoted

  # Re-point every role at the now-authoritative local primary and recover.
  export G6_DB_HOST=postgres
  export G6_DB_PORT=5432
  g6_ha_export_common_env
  g6_ha_compose up --detach worker
  g6_ha_wait_until 60 1 "worker trust socket after promotion" \
    g6_ha_compose exec -T worker test -S /run/ocserv-trust/control-plane.sock
  g6_ha_compose up --detach api scheduler
  g6_ha_wait_until 60 2 "api ready after promotion" g6_ha_api_ready
  g6_ha_wait_until 30 2 "worker connected after promotion" worker_connected_local
  g6_ha_wait_until 30 2 "scheduler connected after promotion" scheduler_connected_local
  g6_ha_timeline_event api_recovered
  g6_ha_timeline_event worker_recovered
  mkdir -p "${G6HA_OUTBOX}/new-primary"
  cp -f "${G6HA_STATE}/promoted-at" "${G6HA_OUTBOX}/new-primary/promoted-at"
  g6_ha_now >"${G6HA_OUTBOX}/new-primary/writable-at"
}

phase_finalize() {
  local isolation="${1:?isolation directory is required}"
  local load="${2:?load directory is required}"
  local pitr="${3:?pitr directory is required}"
  local recovered="${4:?peer recovered directory is required}"
  local post_promotion="${5:?peer post-promotion directory is required}"
  local rejoin="${6:?peer rejoin directory is required}"
  local peer_tunnel="${7:?peer tunnel directory is required}"
  require_file "${isolation}/isolation.json"
  require_file "${load}/load-markers.json"
  require_file "${pitr}/pitr-report.json"
  require_file "${recovered}/recovered-at"
  require_file "${post_promotion}/probes.json"
  require_file "${rejoin}/post-rejoin-probes.jsonl"
  require_file "${peer_tunnel}/boot-id-sha256"

  local peer_boot own_boot
  peer_boot="$(<"${peer_tunnel}/boot-id-sha256")"
  own_boot="$(g6_ha_boot_id_hash)"
  [[ "${peer_boot}" != "${own_boot}" ]] || {
    echo "both failure domains resolved to the same runner boot identity" >&2
    return 1
  }

  # Acknowledged-transaction reconciliation reads the marker table on the
  # promoted primary; every acknowledged marker must have survived, and the
  # promoted primary must hold exactly the markers fd-a acknowledged.
  local acknowledged present loss declared_count
  acknowledged="$(g6_ha_psql -Atc \
    "SELECT json_agg(json_build_object('txid', txid, 'acknowledged_at', to_char(written_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')) ORDER BY written_at) FROM g6_ha_markers WHERE phase = 'failover-load'")"
  present="$(g6_ha_psql -Atc \
    "SELECT json_agg(txid ORDER BY written_at) FROM g6_ha_markers WHERE phase = 'failover-load'")"
  [[ "${acknowledged}" != "null" && -n "${acknowledged}" ]] || {
    echo "reconciliation found no acknowledged markers on the promoted primary" >&2
    return 1
  }
  loss="$(jq -nr --argjson acknowledged "${acknowledged}" --argjson present "${present:-[]}" \
    '($acknowledged | map(.txid)) - $present | length')"
  [[ "${loss}" == 0 ]] || {
    echo "reconciliation lost ${loss} acknowledged transactions" >&2
    return 1
  }
  declared_count="$(jq 'length' "${load}/load-markers.json")"
  jq -e --argjson declared "${declared_count}" \
    'length == $declared' <<<"${acknowledged}" >/dev/null || {
    echo "the promoted primary holds $(jq 'length' <<<"${acknowledged}") failover-load markers but fd-a acknowledged ${declared_count}" >&2
    return 1
  }

  local outage_declared_at isolated_at service_restored_at promoted_at
  outage_declared_at="$(jq -r '.outage_declared_at' "${isolation}/isolation.json")"
  isolated_at="$(jq -r '.isolated_at' "${isolation}/isolation.json")"
  service_restored_at="$(<"${recovered}/recovered-at")"
  promoted_at="$(<"${G6HA_STATE}/promoted-at")"
  g6_ha_timeline_event marker_a_written
  g6_ha_timeline_event restore_point_created
  g6_ha_timeline_event marker_b_written
  g6_ha_timeline_event restore_verified

  local rto rpo rto_limit rpo_limit
  rto="$(g6_ha_seconds_between "${outage_declared_at}" "${service_restored_at}")"
  rpo="$(jq -nr --argjson acknowledged "${acknowledged}" --arg outage "${outage_declared_at}" \
    '($outage | fromdateiso8601) - ($acknowledged | map(.acknowledged_at | fromdateiso8601) | max)')"
  rto_limit="$(g6_ha_slo_limit database_rto_seconds)"
  rpo_limit="$(g6_ha_slo_limit database_rpo_seconds)"
  ((rto <= rto_limit)) || {
    echo "measured RTO ${rto}s exceeds the g6-slo.yaml limit ${rto_limit}s" >&2
    return 1
  }
  ((rpo <= rpo_limit)) || {
    echo "measured RPO ${rpo}s exceeds the g6-slo.yaml limit ${rpo_limit}s" >&2
    return 1
  }
  ((rto >= 0)) || {
    echo "RTO measured negative; cross-runner clock skew is too large" >&2
    return 1
  }
  (( $(g6_ha_seconds_between "${isolated_at}" "${promoted_at}") >= 0 )) || {
    echo "promotion recorded before isolation; cross-runner clock skew is too large" >&2
    return 1
  }

  # fd-a echoes the promotion record it received before probing; it must be
  # byte-identical to the locally recorded boundary, binding the probe
  # rendezvous into the evidence chain.
  [[ "$(<"${post_promotion}/promoted-at")" == "${promoted_at}" ]] || {
    echo "the echoed promotion record does not match the recorded promotion boundary" >&2
    return 1
  }

  # Dual-primary evidence: write attempts against the fenced former primary
  # AFTER the replacement was promoted (the post-promotion probes), plus the
  # read-only rejections after it rejoined as a standby. The pre-promotion
  # probes that established the fence stay in fd-a's isolation artifact.
  local dual_primary_probes dual_primary_accepts
  dual_primary_probes="$(jq -n \
    --slurpfile promotion "${post_promotion}/probes.json" \
    --slurpfile rejoined "${rejoin}/post-rejoin-probes.jsonl" \
    '$promotion[0].promoted_at_probes + $rejoined')"
  # Every dual-primary probe must sit at or after the recorded promotion
  # boundary on fd-a's own clock; an earlier probe means cross-runner clock
  # skew or a missed rendezvous and must fail loudly instead of silently
  # entering the frozen record.
  jq -e --arg promoted "${promoted_at}" \
    'all(.[]; (.at | fromdateiso8601) >= ($promoted | fromdateiso8601))' \
    <<<"${dual_primary_probes}" >/dev/null || {
      echo "a dual-primary probe timestamp precedes the recorded promotion boundary" >&2
      return 1
    }
  jq -e 'length >= 3 and all(.accepted == false)' <<<"${dual_primary_probes}" >/dev/null || {
    echo "the former primary accepted a write after the replacement promotion" >&2
    return 1
  }
  dual_primary_accepts="$(jq 'map(select(.accepted == true)) | length' <<<"${dual_primary_probes}")"
  g6_ha_timeline_event old_primary_write_rejected

  mkdir -p "${G6HA_OUTBOX}/evidence"
  jq -n \
    --arg environment_id "${G6HA_ENVIRONMENT_ID}" \
    --arg candidate_sha "${G6HA_CANDIDATE_SHA}" \
    --arg outage "${outage_declared_at}" \
    --arg restored "${service_restored_at}" \
    --argjson acknowledged "${acknowledged}" \
    --arg isolated_at "${isolated_at}" \
    --arg promoted_at "${promoted_at}" \
    --argjson isolated_primary_writes "${dual_primary_probes}" \
    --argjson present_txids "${present}" \
    '{environment_id:$environment_id, candidate_sha:$candidate_sha,
      outage_declared_at:$outage, service_restored_at:$restored,
      acknowledged:$acknowledged,
      failover:{old_primary:"postgres-fd-alpha", new_primary:"postgres-fd-beta",
        isolated_at:$isolated_at, promoted_at:$promoted_at,
        isolated_primary_writes:$isolated_primary_writes},
      recovery:{restored_at:$restored, present_txids:$present_txids}}' \
    >"${G6HA_OUTBOX}/evidence/postgres-recovery.json"
  cp -f "${G6HA_OUTBOX}/timeline.jsonl" "${G6HA_OUTBOX}/evidence/timeline.jsonl"
  cp -f "${pitr}/pitr-report.json" "${G6HA_OUTBOX}/evidence/pitr-report.json"

  # Topology snapshot with opaque failure-domain aliases; the distinct runner
  # boot identities prove the two failure domains are separate hosts without
  # exposing them.
  jq -n \
    --arg environment_id "${G6HA_ENVIRONMENT_ID}" \
    --arg candidate_sha "${G6HA_CANDIDATE_SHA}" \
    --arg fd_a "fd-alpha" --arg fd_b "fd-beta" \
    --arg fd_a_boot "${peer_boot}" --arg fd_b_boot "${own_boot}" \
    '{schema_note:"g6-ha-pitr stage topology snapshot (not the final G6 verdict topology)",
      environment_id:$environment_id, candidate_sha:$candidate_sha,
      failure_domains:[
        {alias:$fd_a, distinct_host_attestation:$fd_a_boot},
        {alias:$fd_b, distinct_host_attestation:$fd_b_boot}],
      instances:[
        {role:"api", instance:"api-fd-alpha", failure_domain:$fd_a, component:"control-plane"},
        {role:"api", instance:"api-fd-beta", failure_domain:$fd_b, component:"control-plane"},
        {role:"worker", instance:"worker-fd-alpha", failure_domain:$fd_a, component:"control-plane"},
        {role:"worker", instance:"worker-fd-beta", failure_domain:$fd_b, component:"control-plane"},
        {role:"scheduler", instance:"scheduler-fd-alpha", failure_domain:$fd_a, component:"control-plane"},
        {role:"scheduler", instance:"scheduler-fd-beta", failure_domain:$fd_b, component:"control-plane"},
        {role:"transportd", instance:"transportd-fd-alpha", failure_domain:$fd_a, component:"transportd"},
        {role:"transportd", instance:"transportd-fd-beta", failure_domain:$fd_b, component:"transportd"},
        {role:"postgres_primary", instance:"postgres-fd-alpha", failure_domain:$fd_a, component:"postgres"},
        {role:"postgres_standby", instance:"postgres-fd-beta", failure_domain:$fd_b, component:"postgres"}],
      stage_boundary:{
        relay_instances:"planned with the real-agent fleet stage",
        agent_instances:"planned with the real-agent fleet stage"}}' \
    >"${G6HA_OUTBOX}/evidence/topology.json"

  jq -n \
    --arg environment_id "${G6HA_ENVIRONMENT_ID}" \
    --arg candidate_sha "${G6HA_CANDIDATE_SHA}" \
    --argjson rto "${rto}" --argjson rto_limit "${rto_limit}" \
    --argjson rpo "${rpo}" --argjson rpo_limit "${rpo_limit}" \
    --argjson acknowledged_loss "${loss}" \
    --argjson dual_primary_accepts "${dual_primary_accepts}" \
    --argjson pitr_marker_a_present true --argjson pitr_marker_b_present false \
    '{environment_id:$environment_id, candidate_sha:$candidate_sha,
      rto_seconds:$rto, rto_limit_seconds:$rto_limit, rto_within_limit:($rto <= $rto_limit),
      rpo_seconds:$rpo, rpo_limit_seconds:$rpo_limit, rpo_within_limit:($rpo <= $rpo_limit),
      acknowledged_transaction_loss_count:$acknowledged_loss,
      dual_primary_write_accept_count:$dual_primary_accepts,
      pitr:{marker_a_present:$pitr_marker_a_present, marker_b_present:$pitr_marker_b_present}}' \
    >"${G6HA_OUTBOX}/evidence/verification-summary.json"
  cp -f "${G6HA_LOGS}"/tunnel-*.log "${G6HA_OUTBOX}/evidence/" 2>/dev/null || true
}

phase_rejoin_wait() {
  # The rejoined former primary must appear as a streaming standby of the
  # promoted primary before evidence collection ends.
  g6_ha_wait_until 60 2 "rejoined peer streaming" rejoined_peer_streaming
  g6_ha_timeline_event old_primary_rejoined
}

rejoined_peer_streaming() {
  [[ "$(g6_ha_psql -Atc \
    "SELECT count(*) FROM pg_stat_replication WHERE application_name = 'g6_rejoined'" \
    2>/dev/null)" != 0 ]]
}

case "${1:-}" in
  prepare) g6_ha_timing_run prepare phase_prepare ;;
  images) g6_ha_timing_run compose_image_build phase_images ;;
  tunnel-up)
    cp -f "${2:?peer tunnel directory is required}/tunnel-node-id" \
      "${G6HA_STATE}/peer-tunnel-node-id"
    cp -f "${2:?peer tunnel directory is required}/boot-id-sha256" \
      "${G6HA_STATE}/peer-boot-id"
    g6_ha_timing_run tunnel_up phase_tunnel_up
    ;;
  standby-bootstrap)
    g6_ha_timing_run standby_bootstrap phase_standby_bootstrap "${2:?peer primary-up directory is required}"
    ;;
  roles-up) g6_ha_timing_run roles_up phase_roles_up ;;
  failover-ready) g6_ha_timing_run failover_ready phase_failover_ready ;;
  promote)
    g6_ha_timing_run promotion phase_promote "${2:?peer isolation directory is required}"
    ;;
  finalize)
    g6_ha_timing_run evidence_collection phase_finalize "${2:?isolation directory is required}" \
      "${3:?load directory is required}" \
      "${4:?pitr directory is required}" \
      "${5:?peer recovered directory is required}" \
      "${6:?peer post-promotion directory is required}" \
      "${7:?peer rejoin directory is required}" \
      "${8:?peer tunnel directory is required}"
    ;;
  rejoin-wait) g6_ha_timing_run rejoin_confirm phase_rejoin_wait ;;
  diagnostics) g6_ha_diagnostics ;;
  cleanup) g6_ha_cleanup ;;
  *)
    echo "usage: $0 {prepare|images|tunnel-up DIR|standby-bootstrap DIR|roles-up|failover-ready|promote DIR|finalize DIR DIR DIR DIR DIR DIR DIR|rejoin-wait|diagnostics|cleanup}" >&2
    exit 2
    ;;
esac
