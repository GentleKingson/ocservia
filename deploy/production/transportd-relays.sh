#!/bin/sh
set -eu

# Only the production Compose entrypoint uses this wrapper. Direct binary
# invocation and the image's development/diagnostic entrypoint are unchanged.
: "${OCSERV_RELAY_URL_A:?set the dedicated HTTPS relay URL}"
if [ -n "${OCSERV_RELAY_URL_B:-}" ]; then
  set -- --relay-url "$OCSERV_RELAY_URL_B" "$@"
fi
exec /usr/local/bin/ocservia-transportd --relay-mode custom \
  --relay-url "$OCSERV_RELAY_URL_A" "$@"
