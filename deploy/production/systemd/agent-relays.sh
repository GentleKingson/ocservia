#!/bin/sh
set -eu

: "${RELAY_URL_A:?set the dedicated HTTPS relay URL}"
set -- --controller "${CONTROLLER_ENDPOINT_ID:?}" --node-id "${NODE_ID:?}" \
  --controller-command-key-file "${CONTROLLER_COMMAND_VERIFICATION_KEY_FILE:?}" \
  --user-password-seal-key-id "${USER_PASSWORD_SEAL_KEY_ID:?}" \
  --user-password-seal-public-key-sha256 "${USER_PASSWORD_SEAL_PUBLIC_KEY_SHA256:?}" \
  --p12-password-seal-key-id "${P12_PASSWORD_SEAL_KEY_ID:?}" \
  --p12-password-seal-public-key-sha256 "${P12_PASSWORD_SEAL_PUBLIC_KEY_SHA256:?}" \
  --relay-mode custom --relay-url "$RELAY_URL_A"
if [ -n "${RELAY_URL_B:-}" ]; then
  set -- "$@" --relay-url "$RELAY_URL_B"
fi
exec /usr/libexec/ocservia/ocservia-agent "$@" \
  --relay-token-file /etc/ocservia-agent/relay-access-token
