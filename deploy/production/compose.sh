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

for argument in "$@"; do
  case "${argument}" in
    up|create|run)
      backup_dir="${OCSERV_BACKUP_DIR:-}"
      if [[ -z "${backup_dir}" || ! -d "${backup_dir}" || -L "${backup_dir}" ]]; then
        echo "OCSERV_BACKUP_DIR must be an existing real directory" >&2
        exit 2
      fi
      if [[ "$(stat -c '%u:%g:%a' "${backup_dir}")" != "999:999:700" ]]; then
        echo "OCSERV_BACKUP_DIR must be owned by uid:gid 999:999 with mode 0700" >&2
        exit 2
      fi
      break
      ;;
  esac
done

exec docker compose -f "${ROOT}/deploy/production/compose.yaml" "$@"
