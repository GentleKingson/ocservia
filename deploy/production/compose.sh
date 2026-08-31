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

secret_dir="${OCSERV_SECRET_DIR:-}"
if [[ -z "${secret_dir}" || ! -d "${secret_dir}" || -L "${secret_dir}" \
  || "$(stat -c '%u:%a' "${secret_dir}")" != "$(id -u):700" ]]; then
  echo "OCSERV_SECRET_DIR must be an existing mode-0700 directory owned by the launcher user" >&2
  exit 2
fi
if [[ "${secret_dir}" != /* || "$(realpath -e -- "${secret_dir}")" != "${secret_dir}" ]]; then
  echo "OCSERV_SECRET_DIR must be an absolute canonical path without symlink ancestry" >&2
  exit 2
fi
ancestor="${secret_dir}"
while true; do
  IFS=: read -r ancestor_uid ancestor_mode < <(stat -c '%u:%a' "${ancestor}")
  if [[ "${ancestor_uid}" != "0" && "${ancestor_uid}" != "$(id -u)" ]] \
    || (( (8#${ancestor_mode} & 8#022) != 0 )); then
    echo "OCSERV_SECRET_DIR ancestry must be root- or launcher-owned and not group/world writable" >&2
    exit 2
  fi
  [[ "${ancestor}" == "/" ]] && break
  ancestor="$(dirname -- "${ancestor}")"
done
general_secrets=(tls.crt tls.key postgres-owner-password postgres-app-password postgres-backup-password \
  postgres.pgpass database-owner-url database-app-url oidc-client-secret session-key \
  audit-checkpoint-key certificate-signer-token otel-client.crt otel-client.key otel-ca.crt)
for secret in "${general_secrets[@]}"; do
  path="${secret_dir}/${secret}"
  if [[ ! -f "${path}" || -L "${path}" || "$(stat -c '%u:%a' "${path}")" != "$(id -u):444" ]]; then
    echo "${path} must be a launcher-owned regular file with mode 0444 inside the private secret directory" >&2
    exit 2
  fi
done
for secret in audit-event-key controller-command-signing-key.pem; do
  path="${secret_dir}/${secret}"
  if [[ ! -f "${path}" || -L "${path}" \
    || "$(stat -c '%u:%g:%a' "${path}")" != "65534:65532:400" ]]; then
    echo "${path} must be owned by uid:gid 65534:65532 with mode 0400" >&2
    exit 2
  fi
done
for secret in relay-access-token controller-iroh.key; do
  path="${secret_dir}/${secret}"
  if [[ ! -f "${path}" || -L "${path}" || "$(stat -c '%u:%g:%a' "${path}")" != "65532:65532:400" ]]; then
    echo "${path} must be owned by uid:gid 65532:65532 with mode 0400" >&2
    exit 2
  fi
done

prepare_transport_runtime=false
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
      if [[ "${argument}" == "up" ]]; then
        prepare_transport_runtime=true
      fi
      break
      ;;
  esac
done

compose=(docker compose -p ocservia-production -f "${ROOT}/deploy/production/compose.yaml")
if [[ "${prepare_transport_runtime}" == true ]]; then
  "${compose[@]}" stop control-plane transportd
  "${compose[@]}" run --rm --no-deps transport-runtime-init
fi

exec "${compose[@]}" "$@"
