#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

for variable in OCSERV_GATEWAY_IMAGE OCSERV_CONTROL_IMAGE OCSERV_TRANSPORT_IMAGE \
  OCSERV_BACKUP_IMAGE OCSERV_POSTGRES_IMAGE OCSERV_OTEL_IMAGE; do
  value="${!variable:-}"
  if [[ ! "${value}" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "${variable} must contain a full sha256 image digest" >&2
    exit 2
  fi
done

exec docker compose -f "${ROOT}/deploy/production/compose.yaml" "$@"
