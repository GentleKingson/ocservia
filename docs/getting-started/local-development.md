# Try ocservia locally

Use the local stack to explore the Web console without connecting to a real VPN server. It starts the Controller, Web console, database, and simulated nodes on your machine.

## Requirements

- Git
- Docker Engine with the Compose v2 plugin, or Docker Desktop
- Free local ports `4173` and `8080`

## Start the stack

```bash
git clone https://github.com/GentleKingson/ocservia.git
cd ocservia
deploy/compose/compose.sh up --build -d
```

PostgreSQL is the default. To exercise a pinned MySQL-compatible development
database, set `OCSERV_DATABASE_BACKEND=mysql` or `mariadb` before invoking the
same launcher. The launcher selects the matching Compose descriptor explicitly.

Open `http://127.0.0.1:4173` in a browser. The Controller exposes `/livez`, `/readyz`, and `/version` on `http://127.0.0.1:8080`.

## Stop the stack

For a disposable local stack, including the selected database volume:

```bash
deploy/compose/compose.sh down --volumes
```

For logs, simulator behavior, persistent data, and browser tests, see [Control-plane development](../development/control-plane.md).
