# Control-plane development

The I02 control plane is a modular Go binary backed by PostgreSQL. It exposes
only foundational health and build endpoints; node transport and remote side
effects are intentionally absent.

Start the public development stack:

```bash
docker compose -f deploy/compose/compose.yaml up --build
```

The Web shell is available at `http://127.0.0.1:4173`. The control-plane live,
readiness, and version endpoints are `http://127.0.0.1:8080/livez`, `/readyz`,
and `/version`.

The binary accepts `--role=api`, `--role=worker`, `--role=scheduler`, or
`--role=all`. `OCSERV_DATABASE_URL` is required. Development authentication is
disabled by default and can only be enabled with `OCSERV_DEV_AUTH=true` when
the environment is `development` and the HTTP listener is loopback-only.

Database migrations run on startup under a PostgreSQL advisory lock. Applied
migrations are immutable. Roll back an unshipped development deployment by
stopping the stack and removing its named volume; shipped schema changes use a
forward fix or database restore.
