#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

if [[ ! "${OCSERV_RELAY_IMAGE:-}" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]]; then
  echo "OCSERV_RELAY_IMAGE must contain a full sha256 image digest" >&2
  exit 2
fi

exec docker compose -f "${ROOT}/deploy/production/relay/compose.yaml" "$@"
