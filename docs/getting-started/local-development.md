# Try ocservia locally

Use the development stack to explore the Web console without a real Iroh
transport, a real ocserv server, or privileged node operations. It uses a
bounded Agent simulator and a Rust transport stub.

## Requirements

- Git
- Docker Engine with the Compose v2 plugin, or Docker Desktop
- Free local ports `4173` and `8080`

## Start the stack

```bash
git clone https://github.com/GentleKingson/ocservia.git
cd ocservia
docker compose -f deploy/compose/compose.yaml up --build -d
```

Open `http://127.0.0.1:4173` in a browser. The control plane exposes
`/livez`, `/readyz`, and `/version` on `http://127.0.0.1:8080`.

## Stop the stack

For a disposable local stack, including its database volume:

```bash
docker compose -f deploy/compose/compose.yaml down --volumes
```

For simulator behavior, logs, persistent data, and browser E2E, see
[Control-plane development](../development/control-plane.md).
