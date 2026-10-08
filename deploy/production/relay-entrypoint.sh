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
# ACME mode: iroh-relay reads its hostname and contact only from the config file.
if [[ -n "${OCSERV_RELAY_CONFIG_TEMPLATE:-}" ]]; then
  host="${OCSERV_RELAY_PUBLIC_HOST:-}" email="${OCSERV_ACME_EMAIL:-}"
  if (( ${#host} > 253 )) || [[ ! "${host}" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ \
    || ! "${email}" =~ ^[A-Za-z0-9._%+-]+@[a-z0-9.-]+$ ]]; then
    echo "relay ACME config requires a lowercase DNS hostname and a plain email address" >&2
    exit 1
  fi
  config="$(<"${OCSERV_RELAY_CONFIG_TEMPLATE}")"
  config="${config//@OCSERV_RELAY_PUBLIC_HOST@/${host}}"
  config="${config//@OCSERV_ACME_EMAIL@/${email}}"
  (umask 077 && printf '%s\n' "${config}" >/tmp/relay.toml)
fi
exec /usr/local/bin/iroh-relay "$@"
