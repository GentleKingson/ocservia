#!/usr/bin/env bash
# Shared PostgreSQL 18 image pin and readiness helpers for database test scripts.

# shellcheck disable=SC2034 # Consumed by sourcing scripts.
readonly POSTGRES_18_IMAGE='postgres:18.7-bookworm@sha256:e742a2668255221388004f3a42b6150a2103f5054ab3a4a93ae9e4f01e551c35'

postgres_allocate_loopback_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

postgres_wait_tcp() {
  local container="$1" user="${2:-ocservia_owner}" database="${3:-ocservia}"
  local password="${4:-test-owner-only}" attempts="${5:-240}" interval="${6:-0.25}"
  # Probe over TCP: the entrypoint's initdb temporary server listens on the Unix
  # socket only, so a socket probe can report ready before the final server exists.
  local i
  for ((i = 0; i < attempts; i++)); do
    if docker exec -e PGPASSWORD="${password}" "${container}" psql -X -h 127.0.0.1 -U "${user}" -d "${database}" -Atc 'SELECT 1' >/dev/null 2>&1; then
      return 0
    fi
    sleep "${interval}"
  done
  return 1
}

postgres_wait_initdb_ready() {
  local container="$1" user="${2:-postgres}" database="${3:-ocservia}"
  local password="${4:?password required}" attempts="${5:-60}"
  local i
  for ((i = 0; i < attempts; i++)); do
    if docker logs "${container}" 2>&1 | grep -Fq 'PostgreSQL init process complete; ready for start up.' \
      && docker exec -e PGPASSWORD="${password}" "${container}" pg_isready -U "${user}" -d "${database}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

postgres_expected_migration_version() {
  local root="${1:-${ROOT:?ROOT is required}}"
  (
    cd "${root}/control-plane/migrations/sql" || return 1
    local file
    for file in [0-9]*_*.sql; do
      if [[ -f "${file}" && ! -L "${file}" ]]; then
        printf '%s\n' "${file}"
      fi
    done
  ) | sed -n 's/^\([0-9][0-9]*\)_.*/\1/p' | awk '{print $1 + 0}' | sort -n | tail -n 1
}
