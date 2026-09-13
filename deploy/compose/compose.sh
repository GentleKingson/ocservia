#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
backend="${OCSERV_DATABASE_BACKEND:-postgres}"
case "${backend}" in
  postgres)
    compose_file="${ROOT}/deploy/compose/compose.yaml"
    ;;
  mysql)
    export OCSERV_DATABASE_IMAGE="${OCSERV_DATABASE_IMAGE:-mysql:8.4.10}"
    compose_file="${ROOT}/deploy/compose/compose.mysql-compatible.yaml"
    ;;
  mariadb)
    export OCSERV_DATABASE_IMAGE="${OCSERV_DATABASE_IMAGE:-mariadb:12.3.2}"
    compose_file="${ROOT}/deploy/compose/compose.mysql-compatible.yaml"
    ;;
  *)
    echo "OCSERV_DATABASE_BACKEND must be postgres, mysql or mariadb" >&2
    exit 2
    ;;
esac
exec docker compose -f "${compose_file}" "$@"
