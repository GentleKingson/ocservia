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
# Shared-domain ACME mode: wait for Gateway's ACME certificate and reload it
# for QUIC Address Discovery on UDP 7842 while HTTP traffic is proxied by Gateway.
if [[ -n "${OCSERV_RELAY_SHARED_ACME_DIR:-}" ]]; then
  host="${OCSERV_RELAY_PUBLIC_HOST:-}"
  acme_dir="${OCSERV_RELAY_SHARED_ACME_DIR}"
  if (( ${#host} > 253 )) || [[ ! "${host}" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
    echo "relay shared ACME config requires a lowercase DNS hostname" >&2
    exit 1
  fi
  if [[ ! "${acme_dir}" =~ ^/[A-Za-z0-9._/-]+$ ]]; then
    echo "relay shared ACME directory must be an absolute safe path" >&2
    exit 1
  fi
  if [[ -z "${OCSERV_RELAY_CONFIG_TEMPLATE:-}" || ! -f "${OCSERV_RELAY_CONFIG_TEMPLATE}" || -L "${OCSERV_RELAY_CONFIG_TEMPLATE}" ]]; then
    echo "relay shared ACME config requires a regular config template" >&2
    exit 1
  fi
  trap 'exit 0' TERM INT
  cert_path="" key_path="" newest_mtime=-1
  while [[ -z "${cert_path}" ]]; do
    for issuer_dir in "${acme_dir}"/certificates/*; do
      [[ -d "${issuer_dir}" && ! -L "${issuer_dir}" ]] || continue
      issuer_name="${issuer_dir##*/}"
      [[ "${issuer_name}" =~ ^[A-Za-z0-9_@.-]+$ ]] || continue
      candidate="${issuer_dir}/${host}/${host}.crt"
      companion="${issuer_dir}/${host}/${host}.key"
      if [[ -f "${candidate}" && ! -L "${candidate}" && -s "${candidate}" \
        && -f "${companion}" && ! -L "${companion}" && -s "${companion}" ]] \
        && grep -q -- '-----END CERTIFICATE-----' "${candidate}" \
        && grep -q -- '-----END .*PRIVATE KEY-----' "${companion}"; then
        mtime="$(stat -c '%Y' "${candidate}" 2>/dev/null || echo 0)"
        if (( mtime > newest_mtime )); then
          newest_mtime="${mtime}"
          cert_path="${candidate}"
          key_path="${companion}"
        fi
      fi
    done
    [[ -n "${cert_path}" ]] || sleep 1
  done
  trap - TERM INT
  config="$(<"${OCSERV_RELAY_CONFIG_TEMPLATE}")"
  config="${config//@OCSERV_RELAY_CERT_PATH@/${cert_path}}"
  config="${config//@OCSERV_RELAY_KEY_PATH@/${key_path}}"
  (umask 077 && printf '%s\n' "${config}" >/tmp/relay.toml)
# Distinct-domain ACME mode: iroh-relay reads its hostname and contact only from the config file.
elif [[ -n "${OCSERV_RELAY_CONFIG_TEMPLATE:-}" ]]; then
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
