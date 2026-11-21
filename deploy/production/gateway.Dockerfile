FROM node:24.18.1-bookworm-slim@sha256:235600a8101ab264e117b1768e925532262668dc9b581ef1dd7d96ced463b8e7 AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
COPY web/src/api/generated ./src/api/generated
RUN npm install --global npm@11.19.0 && npm ci
COPY web/ ./
RUN npm run build

FROM caddy:2.11.4-alpine@sha256:de23def33b17fb5d1290b0f6c2add1d70780e52341896c00a4c8a2a2fe9d355e AS runtime
# The upstream image has no caddy account. Seed ACME volume ownership and
# remove the file capability so Caddy can start with all capabilities dropped.
RUN apk upgrade --no-cache brotli-libs curl libcurl zlib \
    && addgroup -S -g 65532 caddy && adduser -S -D -H -u 65532 -G caddy caddy \
    && install -d -o caddy -g caddy -m 0700 /var/lib/caddy-acme \
    && setcap -r /usr/bin/caddy
COPY deploy/production/Caddyfile /etc/caddy/Caddyfile
COPY --from=web-build /src/web/dist /srv
USER caddy:caddy
EXPOSE 8443
