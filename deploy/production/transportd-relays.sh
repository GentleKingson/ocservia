#!/bin/sh
set -eu

# Only the production Compose entrypoint uses this wrapper. Direct binary
# invocation and the image's development/diagnostic entrypoint are unchanged.
: "${OCSERV_RELAY_URL_A:?set the dedicated HTTPS relay URL}"
if [ -n "${OCSERV_RELAY_URL_B:-}" ]; then
  echo 'only one dedicated Relay is supported; leave OCSERV_RELAY_URL_B empty' >&2
  exit 2
fi
if [ -e /run/secrets/relay_ca ] || [ -L /run/secrets/relay_ca ]; then
  set -- --relay-ca-file /run/secrets/relay_ca "$@"
fi
exec /usr/local/bin/ocservia-transportd --relay-mode custom \
  --relay-url "$OCSERV_RELAY_URL_A" "$@"
