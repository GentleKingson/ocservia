#!/bin/sh
set -eu

# Only the production Compose entrypoint uses this wrapper. Direct binary
# invocation and the image's development/diagnostic entrypoint are unchanged.
: "${OCSERV_RELAY_URL_A:?set the dedicated HTTPS relay URL}"
if [ -e /run/secrets/relay_ca ] || [ -L /run/secrets/relay_ca ]; then
  set -- --relay-ca-file /run/secrets/relay_ca "$@"
fi
exec /usr/local/bin/ocservia-transportd --relay-mode custom \
  --relay-url "$OCSERV_RELAY_URL_A" "$@"
