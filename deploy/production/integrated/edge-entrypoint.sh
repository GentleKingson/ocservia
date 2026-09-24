#!/bin/sh
set -eu
for host in "${OCSERV_PUBLIC_HOST:?}" "${OCSERV_RELAY_PUBLIC_HOST:?}"; do
    if [ "${#host}" -gt 253 ] || ! printf '%s\n' "$host" | grep -Eq '^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'; then
        echo 'Edge requires two distinct lowercase DNS hostnames' >&2
        exit 2
    fi
done
[ "$OCSERV_PUBLIC_HOST" != "$OCSERV_RELAY_PUBLIC_HOST" ] || exit 2
# Only substitute these names; NGINX runtime variables must remain literal.
# shellcheck disable=SC2016
envsubst '${OCSERV_PUBLIC_HOST} ${OCSERV_RELAY_PUBLIC_HOST}' \
    < /etc/nginx/ocservia.conf.template > /tmp/nginx.conf
nginx -t -c /tmp/nginx.conf
exec nginx -c /tmp/nginx.conf -g 'daemon off;'
