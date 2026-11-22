#!/bin/sh
set -eu
for host in "${OCSERV_PUBLIC_HOST:?}" "${OCSERV_RELAY_PUBLIC_HOST:?}"; do
    if [ "${#host}" -gt 253 ] || ! printf '%s\n' "$host" | grep -Eq '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'; then
        echo 'Edge requires valid lowercase DNS hostnames' >&2
        exit 2
    fi
done
if [ "$OCSERV_PUBLIC_HOST" = "$OCSERV_RELAY_PUBLIC_HOST" ]; then
    OCSERV_RELAY_MAP_ENTRY=""
else
    OCSERV_RELAY_MAP_ENTRY="        ${OCSERV_RELAY_PUBLIC_HOST} 127.0.0.1:9443;"
fi
export OCSERV_RELAY_MAP_ENTRY
# Only substitute these names; NGINX runtime variables must remain literal.
# shellcheck disable=SC2016
envsubst '${OCSERV_PUBLIC_HOST} ${OCSERV_RELAY_MAP_ENTRY}' \
    < /etc/nginx/ocservia.conf.template > /tmp/nginx.conf
nginx -t -c /tmp/nginx.conf
exec nginx -c /tmp/nginx.conf -g 'daemon off;'
