FROM node:24.18.1-bookworm-slim AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
COPY web/src/api/generated ./src/api/generated
RUN npm install --global npm@11.19.0 && npm ci
COPY web/ ./
RUN npm run build

FROM caddy:2.10.2-alpine
COPY deploy/production/Caddyfile /etc/caddy/Caddyfile
COPY --from=web-build /src/web/dist /srv
USER caddy:caddy
EXPOSE 8443
