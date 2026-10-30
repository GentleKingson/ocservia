# Try ocservia locally

The local Compose stack runs the Controller, the Web console, a database, and simulated agent nodes on one machine. Use it to explore the interface without a live VPN server.

The stack is for local evaluation and testing only. It uses fixed development credentials, a development-only authentication token, and the bounded agent simulator (`OCSERV_LOCAL_SIMULATOR=true`), and it does not manage a real ocserv daemon. Keep it bound to the loopback ports listed below, and do not reuse it for a production Controller, relay, or managed node. Production hosts follow the [managed-node](managed-node.md) and production installation guides.

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

The first run builds the container images locally and needs network access. When the `web` service is healthy, open `http://127.0.0.1:4173` in a browser. The Controller serves `/livez`, `/readyz`, and `/version` on `http://127.0.0.1:8080`.

The stack uses PostgreSQL by default. To use the pinned MySQL 8.4 LTS development database instead, set `OCSERV_DATABASE_BACKEND=mysql` before you run the launcher. The script picks the matching Compose descriptor and rejects unknown backend values. Pass the same `OCSERV_DATABASE_BACKEND` value to every later `compose.sh` command for that stack, including `down`.

## Stop the stack

To tear down the stack and delete its database volume, run the command below. This permanently removes all simulator and database state.

```bash
deploy/compose/compose.sh down --volumes
```

[Control-plane development](../development/control-plane.md) covers log inspection, simulator controls, persistent volume behavior, and browser test workflows.
