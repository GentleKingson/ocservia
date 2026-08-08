FROM node:24.18.1-bookworm-slim@sha256:235600a8101ab264e117b1768e925532262668dc9b581ef1dd7d96ced463b8e7 AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
COPY web/src/api/generated ./src/api/generated
RUN npm install --global npm@11.19.0 && npm ci
COPY web/ ./
RUN npm run build

FROM caddy:2.10.2-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d
COPY deploy/production/Caddyfile /etc/caddy/Caddyfile
COPY --from=web-build /src/web/dist /srv
USER caddy:caddy
EXPOSE 8443
