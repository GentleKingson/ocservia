# Control-plane development

The control plane is a modular Go binary backed by PostgreSQL. The development
stack includes a Rust gRPC stub over a `0660` Unix domain socket and a bounded,
multi-node agent simulator. It does not use Iroh, Ocserv, TCP transport
fallbacks, privileged execution, or any remote side effect.

Start the public development stack:

```bash
docker compose -f deploy/compose/compose.yaml up --build
```

The Web shell is available at `http://127.0.0.1:4173`. The control-plane live,
readiness, and version endpoints are `http://127.0.0.1:8080/livez`, `/readyz`,
and `/version`.

The Overview page can run normal, duplicate-event, error, and disconnect probe
scenarios. Each probe is durably queued before the worker sends it over the
UDS. Events are idempotently stored, exposed by `GET /api/v1/events`, and
streamed by `GET /api/v1/events/stream`. SSE resumes with `Last-Event-ID`; the
REST collection remains the complete rebuild source.

The simulator is disabled unless `OCSERV_LOCAL_SIMULATOR=true`, is rejected in
production, and only accepts the typed `SimulationProbe` payload. Configure its
absolute socket path with `OCSERV_TRANSPORT_SOCKET`, the RPC deadline with
`OCSERV_TRANSPORT_TIMEOUT`, and the bounded consumer queue with
`OCSERV_TRANSPORT_QUEUE_CAPACITY` (1 through 4096).

The binary accepts `--role=api`, `--role=worker`, `--role=scheduler`, or
`--role=all`. `OCSERV_DATABASE_URL` is required. Development authentication is
disabled by default and can only be enabled with `OCSERV_DEV_AUTH=true` when
the environment is `development` and the HTTP listener is loopback-only.

Database migrations run as a separate one-shot process using `--migrate-only`,
an owner connection in `OCSERV_DATABASE_URL`, and the unprivileged role named
by `OCSERV_RUNTIME_DATABASE_ROLE`. The long-running control plane receives
only the runtime role credentials. Migration execution uses a PostgreSQL
advisory lock, validates the complete applied history, and grants the runtime
role ordinary data access while limiting `audit_events` to `SELECT` and
`INSERT`. Readiness requires the database schema to match the binary exactly.

Run the browser-to-simulator E2E with `make e2e`. The script scopes every
container, network, and volume to `COMPOSE_PROJECT` and removes them on success,
failure, or interruption.

Roll back an unshipped development deployment by stopping the stack, removing
its named volume, and reverting migration `000002_local_slice` before
`000001_foundation`. Shipped schema changes use a forward fix or database
restore. Reverting I03 removes only the development stub, simulator queue,
transport events, and their Web surface.
