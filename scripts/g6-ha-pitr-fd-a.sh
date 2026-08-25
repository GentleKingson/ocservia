#!/usr/bin/env bash
# Failure domain A of the G6 HA/PITR harness: first PostgreSQL primary, victim
# of the failover, PITR restore host, and rejoined standby at the end.
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FD_ALIAS="${FD_ALIAS:-fd-alpha}"
# shellcheck source=scripts/g6-ha-pitr-lib.sh
source "${ROOT}/scripts/g6-ha-pitr-lib.sh"
g6_ha_init_environment
export FD_ALIAS

PITR_RESTORE_PORT=55432
MARKER_COUNT=12
PITR_TARGET_NAME=g6_pitr_target

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

postgres_healthy() {
  g6_ha_compose exec -T postgres pg_isready -U ocservia_owner -d ocservia >/dev/null 2>&1
}

rejoined_in_recovery() {
  [[ "$(g6_ha_psql -Atc 'SELECT pg_is_in_recovery()' 2>/dev/null)" == t ]]
}

# Write probes use the host-local write path (published loopback port) from a
# host-network container, so a success proves the probe itself is valid and a
# failure after isolation proves the write path is fenced. Each probe writes a
# unique id with an upsert, so a stale row from an earlier probe can never
# make a writable primary look fenced through a duplicate-key error: success
# is decided by writability alone.
attempt_primary_write_raw() {
  local probe_id="${1:?probe id is required}"
  # The id is inlined as a literal (after a strict charset guard) instead of a
  # psql variable: -v argument passing through `docker compose exec` differs
  # across compose versions on the hosted runners, and psql passes an
  # undefined variable reference through raw to the server.
  [[ "${probe_id}" =~ ^[A-Za-z0-9._-]+$ ]] || {
    echo "probe id contains unexpected characters: ${probe_id}" >&2
    return 1
  }
  docker run --rm --network host --log-driver none \
    -e PGPASSWORD="$(g6_ha_secret owner-password)" postgres:17.10-bookworm \
    psql "host=127.0.0.1 port=5432 user=ocservia_owner dbname=ocservia sslmode=disable" \
    -v ON_ERROR_STOP=1 \
    -c "INSERT INTO g6_ha_markers(id, txid, phase) VALUES ('${probe_id}', txid_current()::text, 'probe') ON CONFLICT (id) DO UPDATE SET written_at = now()"
}

attempt_primary_write() {
  attempt_primary_write_raw "${1:?probe id is required}" >/dev/null 2>&1
}

write_outbox_file() {
  local name="${1:?outbox name is required}" source="${2:?source file is required}"
  mkdir -p "${G6HA_OUTBOX}/${name}"
  cp -f "${source}" "${G6HA_OUTBOX}/${name}/"
}

# Repeated client-side write attempts against the local former primary,
# timestamped on this failure domain's clock. Every attempt must fail: while
# fenced (postgres stopped) connections are refused, and after rejoin the
# instance is a read-only standby that rejects the INSERT itself. The frozen
# verifier enforces closed {at, accepted} keys per probe, so the unique probe
# id is recorded only in the markers table, not in the evidence entries.
run_primary_write_probes() {
  local count="${1:?probe count is required}"
  local out_file="${2:?output jsonl path is required}"
  local phase_label="${3:?probe phase label is required}"
  local attempt at accepted
  : >"${out_file}"
  for attempt in $(seq 1 "${count}"); do
    at="$(g6_ha_now)"
    accepted=false
    if attempt_primary_write "g6-probe-${RUN_ID}-${phase_label}-${attempt}"; then
      accepted=true
    fi
    jq -cn --arg at "${at}" --argjson accepted "${accepted}" \
      '{at:$at, accepted:$accepted}' >>"${out_file}"
    sleep 1
  done
  jq -s --argjson expected "${count}" \
    '(length) == $expected and all(.accepted == false)' "${out_file}"
}

# After rejoin the instance is up and the write path connects, so only the
# standby's own read-only rejection may fail a probe. Any other SQL failure
# (constraint, syntax, authentication) means the probe itself is broken, not
# that fencing held, and fails the phase instead of counting as a rejection.
run_readonly_rejection_probes() {
  local count="${1:?probe count is required}"
  local out_file="${2:?output jsonl path is required}"
  local phase_label="${3:?probe phase label is required}"
  local attempt at accepted output
  : >"${out_file}"
  for attempt in $(seq 1 "${count}"); do
    at="$(g6_ha_now)"
    accepted=false
    if output="$(attempt_primary_write_raw "g6-probe-${RUN_ID}-${phase_label}-${attempt}" 2>&1)"; then
      accepted=true
    elif ! grep -q 'cannot execute INSERT in a read-only transaction' <<<"${output}"; then
      echo "rejoin write probe g6-probe-${RUN_ID}-${phase_label}-${attempt} failed for a reason other than standby read-only rejection:" >&2
      printf '%s\n' "${output}" >&2
      return 1
    fi
    jq -cn --arg at "${at}" --argjson accepted "${accepted}" \
      '{at:$at, accepted:$accepted}' >>"${out_file}"
    sleep 1
  done
  jq -s --argjson expected "${count}" \
    '(length) == $expected and all(.accepted == false)' "${out_file}"
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
  # fd-a owns the first primary, so it serves it to the pinned peer.
  g6_ha_tunnel_serve "$(peer_node_id)"
  sleep 2
  kill -0 "$(<"${G6HA_STATE}/tunnel-serve.pid")"
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

phase_primary_up() {
  g6_ha_chown_pg_dir "${G6HA_ARCHIVE}"
  g6_ha_compose up --detach postgres
  g6_ha_wait_until 60 2 "postgres healthy" postgres_healthy
  g6_ha_compose run --rm migrate
  g6_ha_psql -c "CREATE TABLE IF NOT EXISTS g6_ha_markers (
    id text PRIMARY KEY,
    txid text NOT NULL,
    phase text NOT NULL,
    written_at timestamptz NOT NULL DEFAULT now()
  )"
  bootstrap_controller_endpoint
  g6_ha_export_common_env
  g6_ha_compose up --detach worker
  g6_ha_wait_until 60 1 "worker trust socket" \
    g6_ha_compose exec -T worker test -S /run/ocserv-trust/control-plane.sock
  g6_ha_compose up --detach transportd scheduler api
  g6_ha_wait_until 60 1 "transportd socket" \
    g6_ha_compose exec -T transportd test -S /run/ocserv-platform/transportd.sock
  g6_ha_wait_until 60 2 "api ready" g6_ha_api_ready
  g6_ha_wait_until 30 1 "worker connected" "g6_ha_role_connected" worker
  g6_ha_wait_until 30 1 "scheduler connected" "g6_ha_role_connected" scheduler

  mkdir -p "${G6HA_OUTBOX}/primary-up"
  g6_ha_secret owner-password >"${G6HA_OUTBOX}/primary-up/owner-password"
  g6_ha_secret app-password >"${G6HA_OUTBOX}/primary-up/app-password"
  g6_ha_secret replication-password >"${G6HA_OUTBOX}/primary-up/replication-password"
  cp -f "${G6HA_STATE}/controller-endpoint-id" "${G6HA_OUTBOX}/primary-up/"
  chmod 0600 "${G6HA_OUTBOX}/primary-up/"*-password
}

write_failover_markers() {
  local index payload row
  : >"${G6HA_WORK}/load-markers.ndjson"
  for index in $(seq 1 "${MARKER_COUNT}"); do
    payload="g6-ha-failover-marker-${RUN_ID}-${index}"
    # Inlined literal, never a psql variable: compose exec mangles -v passing
    # on some runner compose versions (an undefined variable reference reaches
    # the server raw and fails with a syntax error at the colon).
    [[ "${payload}" =~ ^[A-Za-z0-9._-]+$ ]] || {
      echo "marker payload contains unexpected characters: ${payload}" >&2
      return 1
    }
    row="$(g6_ha_psql -At \
      -c "INSERT INTO g6_ha_markers(id, txid, phase) VALUES ('${payload}', txid_current()::text, 'failover-load') RETURNING id || ' ' || txid || ' ' || to_char(written_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')")"
    # psql -At still prints the INSERT command tag on its own line after the
    # RETURNING tuple; keep only the tuple.
    row="${row%%$'\n'*}"
    # Bound the row to the exact shape the three fields can carry: a stray
    # control character from the exec transport must die here with its bytes
    # visible, never inside the evidence JSON.
    [[ "${row}" =~ ^[A-Za-z0-9._-]+[[:space:]][0-9]+[[:space:]][0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || {
      echo "marker row ${index} does not match id-txid-timestamp shape:" >&2
      printf '%s\n' "${row}" | od -c | head -5 >&2
      return 1
    }
    [[ "${row}" == "${payload} "* ]] || {
      echo "marker insert failed at index ${index}" >&2
      return 1
    }
    jq -cn --arg marker_id "${row%% *}" \
      --arg txid "$(printf '%s\n' "${row}" | awk '{print $2}')" \
      --arg acknowledged_at "$(printf '%s\n' "${row}" | awk '{print $3}')" \
      '{marker_id: $marker_id, txid: $txid, acknowledged_at: $acknowledged_at}' \
      >>"${G6HA_WORK}/load-markers.ndjson"
    sleep 1
  done
  jq -s '.' "${G6HA_WORK}/load-markers.ndjson" >"${G6HA_OUTBOX}/load-markers.json"
  jq -e 'length == 12' "${G6HA_OUTBOX}/load-markers.json" >/dev/null
}

phase_load() {
  write_failover_markers
  g6_ha_chown_pg_dir "${G6HA_BASEBACKUP}"
  # The service drops all capabilities, so uid 0 cannot write into the
  # postgres-owned directory; the clone must run as the postgres user.
  g6_ha_compose run --rm --no-deps -T --user 999:999 \
    -e PGPASSWORD="$(g6_ha_secret replication-password)" postgres \
    pg_basebackup -h postgres -p 5432 -U ocservia_replication \
    -D /var/lib/postgresql/basebackup -X stream --checkpoint=fast \
    < /dev/null >>"${G6HA_LOGS}/basebackup.log" 2>&1
  docker run --rm --entrypoint /bin/sh \
    -v "${G6HA_BASEBACKUP}:/verify" postgres:17.10-bookworm \
    -c 'pg_verifybackup /verify' >"${G6HA_LOGS}/pg-verifybackup.log" 2>&1
  g6_ha_chown_dir_to_runner "${G6HA_BASEBACKUP}"
  mkdir -p "${G6HA_OUTBOX}/load"
  cp -f "${G6HA_OUTBOX}/load-markers.json" "${G6HA_OUTBOX}/load/"
  sha256sum "${G6HA_LOGS}/pg-verifybackup.log" \
    | awk '{print $1}' >"${G6HA_OUTBOX}/load/basebackup-verified-sha256"
}

pitr_marker_row() {
  local label="${1:?marker label is required}" row
  [[ "${label}" =~ ^[a-z]+$ ]] || {
    echo "pitr marker label contains unexpected characters: ${label}" >&2
    return 1
  }
  row="$(g6_ha_psql -At \
    -c "INSERT INTO g6_ha_markers(id, txid, phase) VALUES ('pitr-${label}', txid_current()::text, 'pitr') RETURNING id || ' ' || txid || ' ' || to_char(written_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")"
  # Drop the psql INSERT command tag line; keep the RETURNING tuple.
  row="${row%%$'\n'*}"
  [[ "${row}" =~ ^pitr-[a-z]+[[:space:]][0-9]+[[:space:]][0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}Z$ ]] || {
    echo "pitr marker row ${label} does not match id-txid-timestamp shape:" >&2
    printf '%s\n' "${row}" | od -c | head -5 >&2
    return 1
  }
  printf '%s\n' "${row}"
}

phase_pitr() {
  local marker_a marker_b restore_point_created switch_target
  marker_a="$(pitr_marker_row a)"
  restore_point_created="$(g6_ha_psql -At -c \
    "SELECT pg_create_restore_point('${PITR_TARGET_NAME}')::text || ' ' || to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")"
  marker_b="$(pitr_marker_row b)"

  switch_target="$(g6_ha_psql -At -c 'SELECT pg_walfile_name(pg_switch_wal())')"
  for _ in {1..60}; do
    g6_ha_archive_has_segment "${switch_target}" && break
    sleep 1
  done
  g6_ha_archive_has_segment "${switch_target}" || {
    echo "switched WAL segment ${switch_target} never reached the archive" >&2
    return 1
  }

  rm -rf "${G6HA_RESTORE}/data"
  mkdir -p "${G6HA_RESTORE}/data"
  cp -a "${G6HA_BASEBACKUP}/." "${G6HA_RESTORE}/data/"
  rm -f "${G6HA_RESTORE}/data/backup_label.old"
  rm -rf "${G6HA_RESTORE}/data/pg_wal"
  mkdir -p "${G6HA_RESTORE}/data/pg_wal"
  printf '%s\n' "restore_command = 'cp /var/lib/postgresql/archive/%f %p'" \
    "recovery_target_name = '${PITR_TARGET_NAME}'" \
    "recovery_target_action = 'pause'" \
    >>"${G6HA_RESTORE}/data/postgresql.auto.conf"
  touch "${G6HA_RESTORE}/data/recovery.signal"
  g6_ha_chown_pg_dir "${G6HA_RESTORE}"

  local pitr_container="${COMPOSE_PROJECT}-pitr"
  docker run --detach --name "${pitr_container}" \
    --network host \
    -v "${G6HA_RESTORE}/data:/var/lib/postgresql/data" \
    -v "${G6HA_ARCHIVE}:/var/lib/postgresql/archive:ro" \
    postgres:17.10-bookworm \
    postgres -D /var/lib/postgresql/data -p "${PITR_RESTORE_PORT}" \
    >"${G6HA_LOGS}/pitr-run.log" 2>&1

  local paused=no
  for _ in {1..90}; do
    if docker exec "${pitr_container}" psql -U ocservia_owner -d ocservia \
      -p "${PITR_RESTORE_PORT}" -Atc \
      'SELECT pg_is_in_recovery() AND pg_is_wal_replay_paused()' 2>/dev/null \
      | grep -q t; then
      paused=yes
      break
    fi
    sleep 2
  done
  [[ "${paused}" == yes ]] || {
    echo "PITR instance never paused at the restore point" >&2
    docker logs "${pitr_container}" >"${G6HA_LOGS}/pitr-failure.log" 2>&1 || true
    return 1
  }

  local marker_a_present marker_b_present
  marker_a_present="$(docker exec "${pitr_container}" psql -U ocservia_owner -d ocservia \
    -p "${PITR_RESTORE_PORT}" -Atc \
    "SELECT count(*) FROM g6_ha_markers WHERE id = 'pitr-a' AND phase = 'pitr'")"
  marker_b_present="$(docker exec "${pitr_container}" psql -U ocservia_owner -d ocservia \
    -p "${PITR_RESTORE_PORT}" -Atc \
    "SELECT count(*) FROM g6_ha_markers WHERE id = 'pitr-b' AND phase = 'pitr'")"
  [[ "${marker_a_present}" == 1 ]] || {
    echo "PITR restore lost marker_a" >&2
    return 1
  }
  [[ "${marker_b_present}" == 0 ]] || {
    echo "PITR restore replayed past the restore point into marker_b" >&2
    return 1
  }
  local restored_at marker_a_id marker_b_id
  restored_at="$(g6_ha_now)"
  marker_a_id="$(printf '%s\n' "${marker_a}" | awk '{print $1}')"
  marker_b_id="$(printf '%s\n' "${marker_b}" | awk '{print $1}')"
  docker rm -f "${pitr_container}" >/dev/null

  mkdir -p "${G6HA_OUTBOX}/pitr"
  jq -n \
    --arg environment_id "${G6HA_ENVIRONMENT_ID}" \
    --arg candidate_sha "${G6HA_CANDIDATE_SHA}" \
    --arg marker_a_txid "$(printf '%s\n' "${marker_a}" | awk '{print $2}')" \
    --arg marker_a_at "$(printf '%s\n' "${marker_a}" | awk '{print $3}')" \
    --arg restore_point_at "$(printf '%s\n' "${restore_point_created}" | awk '{print $2}')" \
    --arg marker_b_txid "$(printf '%s\n' "${marker_b}" | awk '{print $2}')" \
    --arg marker_b_at "$(printf '%s\n' "${marker_b}" | awk '{print $3}')" \
    --arg restored_at "${restored_at}" \
    '{environment_id:$environment_id, candidate_sha:$candidate_sha,
      marker_a:{txid:$marker_a_txid, written_at:$marker_a_at},
      restore_point_created_at:$restore_point_at,
      marker_b:{txid:$marker_b_txid, written_at:$marker_b_at},
      restore:{restored_at:$restored_at, marker_a_present:true, marker_b_present:false}}' \
    >"${G6HA_OUTBOX}/pitr/pitr-report.json"
  [[ "${marker_a_id}" == 'pitr-a' && "${marker_b_id}" == 'pitr-b' ]] || {
    echo "unexpected PITR marker identifiers" >&2
    return 1
  }
  jq -e '.marker_a.txid != "" and .marker_b.txid != "" and
    (.marker_a.txid != .marker_b.txid) and
    (.marker_a.written_at < .restore_point_created_at) and
    (.restore_point_created_at < .marker_b.written_at) and
    (.restore.restored_at >= .marker_b.written_at)' \
    "${G6HA_OUTBOX}/pitr/pitr-report.json" >/dev/null || {
    echo "PITR report violates the frozen marker ordering contract" >&2
    return 1
  }
}

phase_isolate() {
  local outage_row isolated_at probes_file
  # Sanity probe: the same write path must succeed before isolation so the
  # later failures prove fencing rather than a broken probe.
  attempt_primary_write "g6-probe-${RUN_ID}-sanity" || {
    echo "write probe failed before isolation; probe path is invalid" >&2
    return 1
  }
  outage_row="$(g6_ha_psql -At -c \
    "INSERT INTO g6_ha_markers(id, txid, phase) VALUES ('outage-declared', txid_current()::text, 'outage') RETURNING to_char(written_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')")"
  # Keep only the RETURNING tuple; psql appends its INSERT command tag on a
  # second line, which would make the recovery artifact fail strict RFC 3339
  # verification even though jq's date parser accepts the prefix.
  outage_row="${outage_row%%$'\n'*}"
  [[ "${outage_row}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || {
    echo "outage declaration does not match the RFC 3339 timestamp shape" >&2
    return 1
  }
  g6_ha_compose stop api scheduler worker transportd
  g6_ha_compose stop postgres
  isolated_at="$(g6_ha_now)"

  # These probes establish the fence itself; the dual-primary evidence in the
  # merged postgres-recovery record comes from the post-promotion probes.
  probes_file="${G6HA_STATE}/isolated-probes.jsonl"
  run_primary_write_probes 3 "${probes_file}" isolated | grep -qx true || {
    echo "the isolated former primary accepted a write" >&2
    return 1
  }

  mkdir -p "${G6HA_OUTBOX}/isolation"
  jq -s \
    --arg outage_declared_at "${outage_row}" \
    --arg isolated_at "${isolated_at}" \
    --arg old_primary "postgres-fd-alpha" \
    --arg new_primary "postgres-fd-beta" \
    '{outage_declared_at:$outage_declared_at, isolated_at:$isolated_at,
      old_primary:$old_primary, new_primary:$new_primary,
      isolated_primary_writes:.}' \
    "${probes_file}" >"${G6HA_OUTBOX}/isolation/isolation.json"
}

# The dual-primary window starts at the peer's promotion: fd-a keeps its
# former primary fenced and proves, after receiving the promotion record,
# that it still refuses every write.
phase_post_promotion_probes() {
  local promoted_at="${1:?peer promotion timestamp file is required}"
  require_file "${promoted_at}"
  local probes_file="${G6HA_STATE}/post-promotion-probes.jsonl"
  run_primary_write_probes 3 "${probes_file}" post-promotion | grep -qx true || {
    echo "the fenced former primary accepted a write after promotion" >&2
    return 1
  }
  mkdir -p "${G6HA_OUTBOX}/post-promotion"
  cp -f "${promoted_at}" "${G6HA_OUTBOX}/post-promotion/promoted-at"
  jq -s '{promoted_at_probes:.}' "${probes_file}" \
    >"${G6HA_OUTBOX}/post-promotion/probes.json"
}

phase_recover_roles() {
  # fd-b now serves the promoted primary; this side becomes the forwarder.
  g6_ha_tunnel_forward "$(peer_node_id)"
  export G6_DB_HOST=host.docker.internal
  export G6_DB_PORT=15432
  g6_ha_export_common_env
  g6_ha_compose up --detach worker
  g6_ha_wait_until 60 1 "worker trust socket after failover" \
    g6_ha_compose exec -T worker test -S /run/ocserv-trust/control-plane.sock
  g6_ha_compose up --detach transportd api scheduler
  g6_ha_wait_until 60 2 "api ready after failover" g6_ha_api_ready
  g6_ha_psql_tunneled \
    "postgres://ocservia_app:$(g6_ha_secret app-password)@host.docker.internal:15432/ocservia?sslmode=disable" \
    -Atc "SELECT count(*) FROM pg_stat_activity WHERE application_name LIKE 'fd-a-%'" \
    | grep -qv '^0$'
  mkdir -p "${G6HA_OUTBOX}/recovered"
  g6_ha_now >"${G6HA_OUTBOX}/recovered/recovered-at"
}

phase_rejoin() {
  # Rewind must run as the postgres user: the service drops all capabilities,
  # so uid 0 could not rewrite the postgres-owned data volume.
  g6_ha_compose run --rm --no-deps -T --user 999:999 \
    -e PGPASSWORD="$(g6_ha_secret owner-password)" postgres \
    pg_rewind --target-pgdata=/var/lib/postgresql/data \
    --source-server="host=host.docker.internal port=15432 user=ocservia_owner dbname=ocservia sslmode=disable" \
    < /dev/null >"${G6HA_LOGS}/pg-rewind.log" 2>&1
  docker run --rm --entrypoint /bin/sh \
    -v "${COMPOSE_PROJECT}_postgres-data:/data" postgres:17.10-bookworm \
    -c "printf '%s\n' \"primary_conninfo = 'host=host.docker.internal port=15432 user=ocservia_replication password=$(g6_ha_secret replication-password) application_name=g6_rejoined'\" >> /data/postgresql.auto.conf && touch /data/standby.signal"
  g6_ha_compose up --detach postgres
  g6_ha_wait_until 60 2 "rejoined standby in recovery" rejoined_in_recovery
  # The rejoined instance must stay read-only: the write probe now connects
  # successfully, so only the standby's own read-only rejection can fail it.
  local rejoin_probes="${G6HA_STATE}/post-rejoin-probes.jsonl"
  run_readonly_rejection_probes 3 "${rejoin_probes}" post-rejoin | grep -qx true || {
    echo "the rejoined former primary accepted a write while a standby" >&2
    return 1
  }
  mkdir -p "${G6HA_OUTBOX}/rejoin"
  g6_ha_now >"${G6HA_OUTBOX}/rejoin/rejoined-at"
  cp -f "${rejoin_probes}" "${G6HA_OUTBOX}/rejoin/post-rejoin-probes.jsonl"
}

case "${1:-}" in
  prepare) g6_ha_timing_run prepare phase_prepare ;;
  images) g6_ha_timing_run compose_image_build phase_images ;;
  tunnel-up)
    cp -f "${2:?peer tunnel directory is required}/tunnel-node-id" \
      "${G6HA_STATE}/peer-tunnel-node-id"
    g6_ha_timing_run tunnel_up phase_tunnel_up
    ;;
  primary-up) g6_ha_timing_run primary_bootstrap phase_primary_up ;;
  load) g6_ha_timing_run basebackup phase_load ;;
  pitr) g6_ha_timing_run pitr_restore phase_pitr ;;
  isolate) g6_ha_timing_run failover phase_isolate ;;
  post-promotion-probes)
    g6_ha_timing_run post_promotion_probes phase_post_promotion_probes "${2:?peer promoted-at file is required}"
    ;;
  recover-roles) g6_ha_timing_run recover_roles phase_recover_roles ;;
  rejoin) g6_ha_timing_run rejoin phase_rejoin ;;
  diagnostics) g6_ha_diagnostics ;;
  cleanup) g6_ha_cleanup ;;
  outbox)
    [[ -d "${2:?outbox name is required}" ]] || {
      echo "unknown outbox payload: ${2}" >&2
      exit 2
    }
    ;;
  *)
    echo "usage: $0 {prepare|images|tunnel-up DIR|primary-up|load|pitr|isolate|post-promotion-probes FILE|recover-roles|rejoin|diagnostics|cleanup}" >&2
    exit 2
    ;;
esac
