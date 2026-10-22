#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
backend="${OCSERV_DATABASE_BACKEND:-postgres}"
case "${backend}" in
  postgres)
    compose_file="${ROOT}/deploy/compose/compose.yaml"
    ;;
  mysql)
    export OCSERV_DATABASE_IMAGE="${OCSERV_DATABASE_IMAGE:-mysql:8.4.11@sha256:0744ee5ef89ce6ccfa13de3e579fe6b9e27f93dd70da9c06d2c908b1b193fb8d}"
    compose_file="${ROOT}/deploy/compose/compose.mysql-compatible.yaml"
    ;;
  *)
    echo "OCSERV_DATABASE_BACKEND must be postgres or mysql" >&2
    exit 2
    ;;
esac
exec docker compose -f "${compose_file}" "$@"
