# Try ocservia locally

The local Compose stack runs the Controller, Web console, database, and simulated agent nodes on a single machine so you can explore the interface without connecting to a live VPN server.

Because the stack uses fixed development credentials, a development-only authentication token, and the bounded agent simulator (`OCSERV_LOCAL_SIMULATOR=true`) without managing a real ocserv daemon, it is only for local evaluation and testing. Keep it bound to the loopback ports below and do not reuse it for a production Controller, relay, or managed-node deployment; production hosts follow the [managed-node](managed-node.md) and production installation guides.

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

PostgreSQL is used by default. To run against the pinned MySQL 8.4 LTS development database instead, set `OCSERV_DATABASE_BACKEND=mysql` before calling the launcher; the script selects the matching Compose descriptor and rejects unknown backend values. Pass the same `OCSERV_DATABASE_BACKEND` value to every subsequent `compose.sh` invocation (including `down`) for that stack.

The initial run builds the container images locally and requires network access. Once the `web` service is healthy, open `http://127.0.0.1:4173` in a browser; the Controller serves `/livez`, `/readyz`, and `/version` on `http://127.0.0.1:8080`.

## Stop the stack

To tear down a disposable local stack and delete its database volume (permanently removing all simulator and database state):

```bash
deploy/compose/compose.sh down --volumes
```

Log inspection, simulator controls, persistent volume behavior, and browser test workflows are covered in [Control-plane development](../development/control-plane.md).
