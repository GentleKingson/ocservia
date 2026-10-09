# Try ocservia locally

Use the local stack to explore the Web console without connecting to a real VPN server. It starts the Controller, Web console, database, and simulated nodes on your machine.

This is a development stack, not a production path. It uses fixed development
credentials and a development-only authentication token, runs the bounded agent
simulator (`OCSERV_LOCAL_SIMULATOR=true`), and manages no real ocserv node. Do
not expose it beyond the loopback ports below or reuse it for a production
Controller, relay or managed-node installation; those use the
[managed-node](managed-node.md) and production paths.

## Requirements

- Git
- Docker Engine with the Compose v2 plugin, or Docker Desktop
- Free local ports `4173` and `8080` (the defaults; override with `OCSERV_WEB_PORT` and `OCSERV_HTTP_PORT`)

## Start the stack

```bash
git clone https://github.com/GentleKingson/ocservia.git
cd ocservia
deploy/compose/compose.sh up --build -d
```

PostgreSQL is the default. To exercise a pinned MySQL 8.4 LTS development
database, set `OCSERV_DATABASE_BACKEND=mysql` before invoking the
same launcher. The launcher selects the matching Compose descriptor explicitly
and rejects any other value. Keep the same `OCSERV_DATABASE_BACKEND` for every
later `compose.sh` command (including `down`) in that stack.

The first run builds the images locally, so it needs network access and time.
Open `http://127.0.0.1:4173` in a browser once the `web` service is up. The Controller exposes `/livez`, `/readyz`, and `/version` on `http://127.0.0.1:8080`.

## Stop the stack

For a disposable local stack, including the selected database volume (all simulator and database data in it is deleted and cannot be recovered):

```bash
deploy/compose/compose.sh down --volumes
```

For logs, simulator behavior, persistent data, and browser tests, see [Control-plane development](../development/control-plane.md).
