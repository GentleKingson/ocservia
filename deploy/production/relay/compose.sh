#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"

if [[ ! "${OCSERV_RELAY_IMAGE:-}" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]]; then
  echo "OCSERV_RELAY_IMAGE must contain a full sha256 image digest" >&2
  exit 2
fi

secret_dir="${OCSERV_RELAY_SECRET_DIR:-}"
if [[ -z "${secret_dir}" || ! -d "${secret_dir}" || -L "${secret_dir}" \
  || "$(stat -c '%u:%a' "${secret_dir}")" != "$(id -u):700" ]]; then
  echo "OCSERV_RELAY_SECRET_DIR must be an existing mode-0700 directory owned by the launcher user" >&2
  exit 2
fi
for secret in tls.crt tls.key; do
  path="${secret_dir}/${secret}"
  if [[ ! -f "${path}" || -L "${path}" || "$(stat -c '%u:%a' "${path}")" != "$(id -u):444" ]]; then
    echo "${path} must be a launcher-owned regular file with mode 0444 inside the private secret directory" >&2
    exit 2
  fi
done
token="${secret_dir}/relay-access-token"
if [[ ! -f "${token}" || -L "${token}" || "$(stat -c '%u:%g:%a' "${token}")" != "65532:65532:400" ]]; then
  echo "${token} must be owned by uid:gid 65532:65532 with mode 0400" >&2
  exit 2
fi

exec docker compose -f "${ROOT}/deploy/production/relay/compose.yaml" "$@"
