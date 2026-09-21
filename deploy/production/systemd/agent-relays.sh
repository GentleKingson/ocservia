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
ca=/etc/ocservia-agent/relay-ca.pem
if [ -e "$ca" ] || [ -L "$ca" ]; then
  if [ ! -f "$ca" ] || [ -L "$ca" ] || [ ! -s "$ca" ] \
    || [ "$(stat -c '%u:%g:%a:%h' "$ca")" != 0:0:444:1 ]; then
    echo 'relay CA must be a nonempty one-link root:root regular file with mode 0444' >&2
    exit 2
  fi
  for ancestor in / /etc /etc/ocservia-agent; do
    mode=$(stat -c '%a' "$ancestor")
    if [ ! -d "$ancestor" ] || [ -L "$ancestor" ] \
      || [ "$(stat -c '%u' "$ancestor")" != 0 ] || [ "$((0$mode & 022))" -ne 0 ]; then
      echo 'relay CA ancestry must be root-owned real directories without group/world write' >&2
      exit 2
    fi
  done
  set -- "$@" --relay-ca-file "$ca"
fi
exec /usr/libexec/ocservia/ocservia-agent "$@" \
  --relay-token-file /etc/ocservia-agent/relay-access-token
