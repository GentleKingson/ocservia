#!/usr/bin/env bash
set -euo pipefail

token_file="${IROH_RELAY_ACCESS_TOKEN_FILE:-/run/secrets/relay_access_token}"
if [[ ! -f "${token_file}" || -L "${token_file}" ]]; then
  echo "relay access token must be a regular file" >&2
  exit 1
fi
IROH_RELAY_ACCESS_TOKEN="$(cat "${token_file}")"
if (( ${#IROH_RELAY_ACCESS_TOKEN} < 32 || ${#IROH_RELAY_ACCESS_TOKEN} > 512 )) || [[ "${IROH_RELAY_ACCESS_TOKEN}" =~ [[:space:]] ]]; then
  echo "relay access token must be 32..512 non-whitespace bytes" >&2
  exit 1
fi
export IROH_RELAY_ACCESS_TOKEN
exec /usr/local/bin/iroh-relay "$@"
