#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
backend="${OCSERV_DATABASE_BACKEND:-postgres}"
case "${backend}" in
  postgres)
    compose_file="${ROOT}/deploy/compose/compose.yaml"
    ;;
  *)
    echo "OCSERV_DATABASE_BACKEND must be postgres; MySQL and cross-engine migration are unsupported" >&2
    exit 2
    ;;
esac
exec docker compose -f "${compose_file}" "$@"
