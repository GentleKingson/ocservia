#!/usr/bin/env bash
# Shared helpers for the two failure-domain sides of the G6 HA/PITR harness.
# Sourced by scripts/g6-ha-pitr-fd-a.sh and scripts/g6-ha-pitr-fd-b.sh; never
# executed directly.

g6_ha_init_environment() {
  G6HA_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  # Phases run as separate processes; the pinned toolchain (cargo for the
  # tunnel build) must be on PATH in every one of them, not only in steps
  # that sourced env.sh explicitly.
  # shellcheck source=scripts/env.sh disable=SC1091
  source "${G6HA_ROOT}/scripts/env.sh"
  RUN_ID="${RUN_ID:?RUN_ID is required}"
  FD_ID="${FD_ID:?FD_ID is required (fd-a or fd-b)}"
  FD_ALIAS="${FD_ALIAS:?FD_ALIAS is required (fd-alpha or fd-beta)}"
  ARTIFACT_DIR="${ARTIFACT_DIR:-${RUNNER_TEMP:?RUNNER_TEMP is required}/artifacts/g6-ha-pitr-${FD_ID}}"
  # The environment binding must satisfy the frozen verifier pattern
  # ^g6-[a-z0-9]{8,32}$ and be identical on both failure domains, so it is
  # derived from the shared run identity (RUN_ID minus the fd suffix) instead
  # of being a per-job constant.
  local shared_run="${RUN_ID%-fd-[a-b]}"
  G6HA_ENVIRONMENT_ID="${G6HA_ENVIRONMENT_ID:-g6-$(printf '%s' "${shared_run}" | openssl dgst -sha256 -r | cut -c1-16)}"
  if [[ ! "${G6HA_ENVIRONMENT_ID}" =~ ^g6-[a-z0-9]{8,32}$ ]]; then
    echo "environment id ${G6HA_ENVIRONMENT_ID} violates the frozen g6-[a-z0-9]{8,32} pattern" >&2
    return 1
  fi
  G6HA_CANDIDATE_SHA="${G6HA_CANDIDATE_SHA:?G6HA_CANDIDATE_SHA is required}"
  case "${RUN_ID}${FD_ID}${FD_ALIAS}" in
    *[!a-zA-Z0-9._-]*)
      echo "run or failure-domain IDs contain unsafe characters" >&2
      return 1
      ;;
  esac
  [[ "${FD_ID}" == "fd-a" || "${FD_ID}" == "fd-b" ]] || {
    echo "FD_ID must be fd-a or fd-b" >&2
    return 1
  }
  G6HA_WORK="${RUNNER_TEMP}/g6-ha-${RUN_ID}"
  G6HA_STATE="${G6HA_WORK}/state"
  G6HA_SECRETS="${G6HA_WORK}/secrets"
  G6HA_OUTBOX="${G6HA_WORK}/outbox"
  G6HA_ARCHIVE="${G6HA_WORK}/pgarchive"
  G6HA_BASEBACKUP="${G6HA_WORK}/basebackup"
  G6HA_RESTORE="${G6HA_WORK}/restore"
  G6HA_LOGS="${G6HA_WORK}/logs"
  G6HA_TUNNEL_BIN="${G6HA_ROOT}/rust/target/release/ocservia-g6-tunnel"
  COMPOSE_PROJECT="ocservia-g6-ha-${RUN_ID}"
  COMPOSE_FILE="${G6HA_ROOT}/deploy/g6-ha-pitr/compose.yaml"
  export COMPOSE_PROJECT COMPOSE_FILE RUN_ID FD_ID FD_ALIAS
  umask 077
  mkdir -p "${G6HA_STATE}" "${G6HA_SECRETS}" "${G6HA_OUTBOX}" \
    "${G6HA_ARCHIVE}" "${G6HA_BASEBACKUP}" "${G6HA_RESTORE}" \
    "${G6HA_LOGS}" "${ARTIFACT_DIR}"
  chmod 0700 "${G6HA_WORK}" "${G6HA_SECRETS}"
}

g6_ha_compose() {
  # Every phase runs as its own process, so the variables the compose file
  # substitutes must be present even for diagnostics and cleanup steps that
  # never sourced them explicitly. Secrets survive across steps in the runner
  # temp state.
  if [[ -z "${G6_OWNER_PASSWORD:-}" || -z "${G6_APP_PASSWORD:-}" ||
    -z "${G6_REPLICATION_PASSWORD:-}" || -z "${G6_FD_ID:-}" ]]; then
    g6_ha_export_common_env || g6_ha_placeholder_env
  fi
  docker compose --project-name "${COMPOSE_PROJECT}" --file "${COMPOSE_FILE}" "$@"
}

g6_ha_placeholder_env() {
  export G6_FD_ID="${FD_ID}"
  export G6_OWNER_PASSWORD="${G6_OWNER_PASSWORD:-harness-placeholder}"
  export G6_APP_PASSWORD="${G6_APP_PASSWORD:-harness-placeholder}"
  export G6_REPLICATION_PASSWORD="${G6_REPLICATION_PASSWORD:-harness-placeholder}"
  export G6_ARCHIVE_DIR="${G6_ARCHIVE_DIR:-${G6HA_ARCHIVE}}"
  export G6_BASEBACKUP_DIR="${G6_BASEBACKUP_DIR:-${G6HA_BASEBACKUP}}"
  export G6_DB_HOST="${G6_DB_HOST:-postgres}"
  export G6_DB_PORT="${G6_DB_PORT:-5432}"
}

g6_ha_export_common_env() {
  local owner_password app_password replication_password secret
  # Called from an || fallback, so set -e is suppressed inside: fail the
  # guard explicitly or an aborted-before-prepare cleanup silently exports
  # empty passwords that compose variable substitution rejects.
  for secret in owner-password app-password replication-password; do
    [[ -s "${G6HA_SECRETS}/${secret}" ]] || return 1
  done
  owner_password="$(g6_ha_secret owner-password)" || return 1
  app_password="$(g6_ha_secret app-password)" || return 1
  replication_password="$(g6_ha_secret replication-password)" || return 1
  export G6_FD_ID="${FD_ID}"
  export G6_OWNER_PASSWORD="${owner_password}"
  export G6_APP_PASSWORD="${app_password}"
  export G6_REPLICATION_PASSWORD="${replication_password}"
  export G6_ARCHIVE_DIR="${G6HA_ARCHIVE}"
  export G6_BASEBACKUP_DIR="${G6HA_BASEBACKUP}"
  export G6_DB_HOST="${G6_DB_HOST:-postgres}"
  export G6_DB_PORT="${G6_DB_PORT:-5432}"
  if [[ -s "${G6HA_STATE}/controller-endpoint-id" ]]; then
    OCSERV_CONTROLLER_ENDPOINT_ID="$(<"${G6HA_STATE}/controller-endpoint-id")"
    export OCSERV_CONTROLLER_ENDPOINT_ID
  fi
}

g6_ha_secret() {
  local name="${1:?secret name is required}"
  local path="${G6HA_SECRETS}/${name}"
  [[ -s "${path}" ]] || return 1
  cat -- "${path}"
}

g6_ha_generate_secrets() {
  local name
  for name in owner-password app-password replication-password; do
    [[ -s "${G6HA_SECRETS}/${name}" ]] && continue
    openssl rand -hex 24 >"${G6HA_SECRETS}/${name}"
    chmod 0600 "${G6HA_SECRETS}/${name}"
  done
}

# fd-b's own randomly generated cluster credentials are useless: the standby
# clone carries fd-a's roles and passwords, so every later client on fd-b
# (roles, promotion, reconciliation) must use the peer's cluster credentials.
# Only the three password files are replaced; the local tunnel key stays.
g6_ha_import_peer_cluster_credentials() {
  local peer_dir="${1:?peer primary-up directory is required}"
  local name source
  for name in owner-password app-password replication-password; do
    source="${peer_dir}/${name}"
    [[ -s "${source}" ]] || {
      echo "peer cluster credential ${name} is missing" >&2
      return 1
    }
    install -m 0600 "${source}" "${G6HA_SECRETS}/${name}.imported"
    mv -f "${G6HA_SECRETS}/${name}.imported" "${G6HA_SECRETS}/${name}"
  done
  chmod 0600 "${G6HA_SECRETS}"/{owner,app,replication}-password
}

g6_ha_owner_dsn_local() {
  printf 'postgres://ocservia_owner:%s@postgres:5432/ocservia?sslmode=disable' \
    "$(g6_ha_secret owner-password)"
}

# Runs psql inside the local postgres service container.
g6_ha_psql() {
  # The exec transport appends carriage returns to query output on some
  # hosted compose versions; grep-based checks tolerate them but strict JSON
  # assembly and timestamp math do not, so every row leaves CR-free.
  g6_ha_compose exec -T postgres psql -v ON_ERROR_STOP=1 -U ocservia_owner -d ocservia "$@" \
    | tr -d '\r'
}

# Runs psql through the local postgres image against an arbitrary reachable
# host:port, used for peer and PITR instances.
g6_ha_psql_tunneled() {
  local conn="${1:?connection string is required}"
  g6_ha_compose run --rm --no-deps postgres psql -v ON_ERROR_STOP=1 "${conn}" "$@"
}

g6_ha_primary_psql() {
  # Primary before failover lives on fd-a; fd-b reaches it through the tunnel.
  if [[ "${FD_ID}" == "fd-a" ]]; then
    g6_ha_psql "$@"
  else
    g6_ha_psql_tunneled "postgres://ocservia_owner:$(g6_ha_secret owner-password)@host.docker.internal:15432/ocservia?sslmode=disable" "$@"
  fi
}

# The tunnel binary must exist before the prepare phase derives the node id
# for the rendezvous artifact, so prepare builds it (cargo is incremental;
# the later images phase rebuilds nothing).
g6_ha_build_tunnel() {
  (cd "${G6HA_ROOT}/rust" && cargo build --release --package ocservia-g6-tunnel)
  [[ -x "${G6HA_TUNNEL_BIN}" ]] || {
    echo "tunnel binary was not produced at ${G6HA_TUNNEL_BIN}" >&2
    return 1
  }
}

g6_ha_tunnel_key() {
  "${G6HA_TUNNEL_BIN}" node-id --key-file "${G6HA_SECRETS}/tunnel.key"
}

g6_ha_boot_id_hash() {
  local boot_id
  boot_id="$(</proc/sys/kernel/random/boot_id)"
  printf '%s' "${boot_id}" | openssl dgst -sha256 -r | cut -d' ' -f1
}

# Containers reach the host through host.docker.internal, which the compose
# files map via extra_hosts host-gateway to the DEFAULT bridge (docker0)
# gateway — not the compose network's own gateway. The forwarded listener
# must bind the address clients actually dial.
g6_ha_host_gateway_address() {
  local gateway
  gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
  [[ "${gateway}" =~ ^[0-9.]+$ ]] || {
    echo "default bridge gateway unavailable" >&2
    return 1
  }
  printf '%s\n' "${gateway}"
}

# Bind-mounted PostgreSQL directories must be owned by the container's
# postgres user; ownership is granted through the already-pinned image so the
# harness needs no host sudo.
g6_ha_chown_pg_dir() {
  local dir="${1:?directory is required}"
  docker run --rm --entrypoint /bin/sh \
    -v "${dir}:/mnt" postgres:17.10-bookworm \
    -c 'chown -R 999:999 /mnt'
}

# The counterpart for bind dirs the harness itself must read back (base
# backups copied into the PITR restore tree) after PostgreSQL wrote them.
g6_ha_chown_dir_to_runner() {
  local dir="${1:?directory is required}"
  docker run --rm --entrypoint /bin/sh \
    -v "${dir}:/mnt" postgres:17.10-bookworm \
    -c "chown -R $(id -u):$(id -g) /mnt"
}

# WAL segments land in a directory the postgres user owns with mode 0700, so
# the runner itself cannot stat them; presence checks run through the pinned
# image instead of the host filesystem.
g6_ha_archive_has_segment() {
  local segment="${1:?WAL segment name is required}"
  docker run --rm --entrypoint /bin/sh \
    -v "${G6HA_ARCHIVE}:/archive:ro" postgres:17.10-bookworm \
    -c "test -f /archive/${segment}"
}

# Exactly one tunnel process runs per failure domain per era, both sides
# pinned to each other's node id: before the failover fd-a serves its local
# primary and fd-b forwards a gateway listener to it; after the promotion the
# roles flip (fd-b serves, fd-a forwards). Running serve and forward from one
# key simultaneously registers two endpoints with the same id and the relay
# drops the second ("Another endpoint connected with the same endpoint id").
g6_ha_tunnel_serve() {
  local peer_node="${1:?peer node ID is required}"
  g6_ha_tunnel_stop
  g6_ha_export_common_env
  nohup "${G6HA_TUNNEL_BIN}" serve \
    --key-file "${G6HA_SECRETS}/tunnel.key" \
    --peer-node "${peer_node}" \
    --forward 127.0.0.1:5432 >"${G6HA_LOGS}/tunnel-serve.log" 2>&1 &
  echo $! >"${G6HA_STATE}/tunnel-serve.pid"
}

g6_ha_tunnel_forward() {
  local peer_node="${1:?peer node ID is required}"
  g6_ha_tunnel_stop
  g6_ha_export_common_env
  local gateway
  gateway="$(g6_ha_host_gateway_address)"
  nohup "${G6HA_TUNNEL_BIN}" forward \
    --key-file "${G6HA_SECRETS}/tunnel.key" \
    --peer-node "${peer_node}" \
    --listen "${gateway}:15432" >"${G6HA_LOGS}/tunnel-forward.log" 2>&1 &
  echo $! >"${G6HA_STATE}/tunnel-forward.pid"
}

g6_ha_tunnel_stop() {
  local pid_file pid status=0
  for pid_file in "${G6HA_STATE}/tunnel-serve.pid" "${G6HA_STATE}/tunnel-forward.pid"; do
    [[ -s "${pid_file}" ]] || continue
    pid="$(<"${pid_file}")"
    kill "${pid}" 2>/dev/null || status=1
    for _ in {1..20}; do
      kill -0 "${pid}" 2>/dev/null || break
      sleep 0.5
    done
    kill -9 "${pid}" 2>/dev/null || true
    rm -f "${pid_file}"
  done
  return "${status}"
}

g6_ha_now() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

g6_ha_slo_limit() {
  local metric="${1:?metric name is required}" limit
  limit="$(awk -v m="  ${metric}:" '
    index($0, m) == 1 { inside = 1; next }
    inside && /limit:/ { print $2; exit }
    inside && /^  [a-z]/ { inside = 0 }
  ' "${G6HA_ROOT}/docs/acceptance/g6-slo.yaml")"
  [[ "${limit}" =~ ^[0-9]+(\.[0-9]+)?$ ]] || {
    echo "g6-slo.yaml limit for ${metric} is missing" >&2
    return 1
  }
  printf '%s\n' "${limit}"
}

g6_ha_seconds_between() {
  local start="${1:?start RFC3339 is required}"
  local end="${2:?end RFC3339 is required}"
  jq -nr --arg s "${start}" --arg e "${end}" \
    '($e | fromdateiso8601) - ($s | fromdateiso8601)'
}

# Timeline records are owned by fd-b, which stamps each event when it observes
# it, so sequence and timestamps stay monotonic across the two runner clocks.
g6_ha_timeline_init() {
  : >"${G6HA_OUTBOX}/timeline.jsonl"
  echo 0 >"${G6HA_STATE}/timeline-sequence"
  g6_ha_now >"${G6HA_STATE}/timeline-last"
}

g6_ha_timeline_event() {
  local event_id="${1:?event id is required}" sequence last stamp
  sequence="$(( $(<"${G6HA_STATE}/timeline-sequence") + 1 ))"
  last="$(<"${G6HA_STATE}/timeline-last")"
  stamp="$(g6_ha_now)"
  stamp="$(jq -nr --arg l "${last}" --arg s "${stamp}" \
    'if ($s | fromdateiso8601) <= ($l | fromdateiso8601)
     then (($l | fromdateiso8601) + 1 | todateiso8601)
     else $s end')"
  jq -cn --argjson sequence "${sequence}" --arg timestamp "${stamp}" \
    --arg environment_id "${G6HA_ENVIRONMENT_ID}" \
    --arg candidate_sha "${G6HA_CANDIDATE_SHA}" \
    --arg event_id "${event_id}" \
    '{sequence:$sequence,timestamp:$timestamp,environment_id:$environment_id,candidate_sha:$candidate_sha,event_id:$event_id}' \
    >>"${G6HA_OUTBOX}/timeline.jsonl"
  echo "${sequence}" >"${G6HA_STATE}/timeline-sequence"
  echo "${stamp}" >"${G6HA_STATE}/timeline-last"
}

g6_ha_wait_until() {
  local attempts="${1:?attempts is required}"
  local interval="${2:?interval seconds is required}"
  local description="${3:?description is required}"
  shift 3
  local _
  for _ in $(seq 1 "${attempts}"); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep "${interval}"
  done
  echo "timed out waiting for ${description}" >&2
  return 1
}

g6_ha_api_ready() {
  g6_ha_compose exec -T api curl --fail --silent http://127.0.0.1:8080/readyz >/dev/null
}

g6_ha_role_connected() {
  local role="${1:?role name is required}"
  g6_ha_psql -Atc "SELECT count(*) FROM pg_stat_activity WHERE application_name = '${FD_ID}-${role}'" \
    | grep -qv '^0$'
}

g6_ha_diagnostics() {
  mkdir -p "${ARTIFACT_DIR}"
  g6_ha_compose ps --all >"${ARTIFACT_DIR}/compose-ps-${FD_ID}.txt" 2>&1 || true
  g6_ha_compose logs --no-color postgres api worker scheduler transportd \
    >"${ARTIFACT_DIR}/services-${FD_ID}.log" 2>&1 || true
  docker system df >"${ARTIFACT_DIR}/docker-storage-${FD_ID}.txt" 2>&1 || true
  cp -f "${G6HA_LOGS}"/tunnel-*.log "${ARTIFACT_DIR}/" 2>/dev/null || true
  # The harness's own phase logs (basebackup, verifybackup, rewind, pitr)
  # explain silent redirected failures; credentials inside connection
  # strings are redacted before anything leaves the runner.
  for log in "${G6HA_LOGS}"/*.log; do
    [[ -f "${log}" ]] || continue
    sed -E 's#(postgres(ql)?://[^:/]+:)[^@]+@#\1[redacted]@#g' \
      "${log}" >"${ARTIFACT_DIR}/$(basename "${log}")" || true
  done
  printf 'fd=%s alias=%s boot_id_sha256=%s\n' \
    "${FD_ID}" "${FD_ALIAS}" "$(g6_ha_boot_id_hash)" \
    >"${ARTIFACT_DIR}/failure-domain-${FD_ID}.txt"
  g6_ha_strip_secrets_from_artifacts
}

# Generated harness credentials must never reach a public-safe artifact.
g6_ha_strip_secrets_from_artifacts() {
  local leaked=0 secret hit
  for secret in owner-password app-password replication-password; do
    [[ -s "${G6HA_SECRETS}/${secret}" ]] || continue
    while IFS= read -r hit; do
      [[ -z "${hit}" ]] && continue
      rm -f -- "${hit}"
      leaked=1
    done < <(grep -RIlF -f "${G6HA_SECRETS}/${secret}" "${ARTIFACT_DIR}" 2>/dev/null || true)
  done
  if grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' \
    "${ARTIFACT_DIR}" >/dev/null 2>&1; then
    while IFS= read -r hit; do rm -f -- "${hit}"; done \
      < <(grep -RIlE -- 'BEGIN ([A-Z ]+ )?PRIVATE KEY|ocpasswd|session[_ -]?cookie' "${ARTIFACT_DIR}" || true)
    leaked=1
  fi
  ((leaked == 0)) || {
    echo "an artifact file contained secret material and was removed" >&2
    return 1
  }
}

# Postgres writes into the archive, basebackup, and restore bind mounts as
# uid 999, so the runner user cannot remove them. Hand the directory back
# through a short-lived root container before the rm.
g6_ha_reclaim_directory() {
  local dir="${1:?directory is required}"
  [[ -d "${dir}" ]] || return 0
  docker run --rm -v "${dir}:/reclaim" postgres:17.10-bookworm \
    chown -R "$(id -u):$(id -g)" /reclaim >/dev/null 2>&1 || {
      echo "cleanup: ownership reclaim failed for ${dir}" >&2
      return 1
    }
}

g6_ha_cleanup() {
  local status=0 pitr_container="${COMPOSE_PROJECT}-pitr"
  if docker container inspect "${pitr_container}" >/dev/null 2>&1; then
    docker rm -f "${pitr_container}" >/dev/null 2>&1 || {
      echo "cleanup: pitr container removal failed" >&2
      status=1
    }
  fi
  g6_ha_tunnel_stop || {
    echo "cleanup: tunnel stop failed" >&2
    status=1
  }
  if ! g6_ha_compose down --volumes --remove-orphans --rmi local \
    >"${G6HA_LOGS:-${RUNNER_TEMP}}/compose-down.log" 2>&1; then
    echo "cleanup: compose down failed; output follows:" >&2
    sed -n '1,40p' "${G6HA_LOGS:-${RUNNER_TEMP}}/compose-down.log" >&2 || true
    status=1
  fi
  for pg_dir in "${G6HA_ARCHIVE}" "${G6HA_BASEBACKUP}" "${G6HA_RESTORE}"; do
    g6_ha_reclaim_directory "${pg_dir}" || status=1
  done
  rm -rf -- "${G6HA_WORK}" "${RUNNER_TEMP}"/g6-ha-* || {
    echo "cleanup: work directory removal failed" >&2
    status=1
  }
  local volume image
  for volume in postgres-data controller-secrets transport-runtime trust-runtime; do
    if docker volume inspect "${COMPOSE_PROJECT}_${volume}" >/dev/null 2>&1; then
      echo "scoped volume cleanup failed: ${COMPOSE_PROJECT}_${volume}" >&2
      status=1
    fi
  done
  for image in $(docker images --format '{{.Repository}}:{{.Tag}}' \
    | grep -F "ocservia-g6-ha-${RUN_ID}-${FD_ID}" || true); do
    echo "scoped image cleanup failed: ${image}" >&2
    status=1
  done
  return "${status}"
}
