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

The production Overview page displays real operational state from the current
readiness, fleet, operations, and events APIs: fleet health and connectivity,
Agent version classification, recent operations, recent events, and notable
offline or stale nodes. The simulator is not part of the production Overview.
It is exposed only through the development-only `/dev` page, which can run
normal, duplicate-event, error, and disconnect probe scenarios. The production
runtime does not register the `/dev` route. Each development probe is durably
queued before the worker sends it over the UDS. Events are idempotently stored,
exposed by `GET /api/v1/events`, and streamed by `GET /api/v1/events/stream`.
SSE resumes with `Last-Event-ID`; the REST collection remains the complete
rebuild source.

Both platform and operation streams use one shared admission manager. The safe
production defaults are 128 global, 8 per identity, 4 per session, 32 per
workspace, and 16 per resource, with at most 64 active database watchers and a
128-event subscriber queue. Admission happens before SSE headers; scoped
excess returns 429, global/watcher overload returns 503, and both include
`Retry-After`. Empty scope counters are deleted, so the admission maps stay
bounded by the global stream count.

Subscribers do not own PostgreSQL tickers. One ref-counted watcher per active
workspace or operation polls the durable event tables and fans out through
bounded queues. A 1, 10, or 100 subscriber burst therefore produces one
steady-state query stream for that scope. Database errors use jittered
exponential backoff up to 10 seconds; a slow queue is disconnected and resumes
with its cursor rather than blocking peers. Watchers stop after the final
subscriber. Streams reauthenticate and reauthorize every 30 seconds, close no
later than session expiry or the 30-minute maximum, and operation streams drain
the terminal event before closing. Shutdown cancels every watcher. All limits
and intervals use validated `OCSERV_SSE_*` settings; invalid values fail startup.
Runtime metrics expose active and unhealthy watcher counts, query volume, and
backoff. Readiness fails while any active watcher is recovering from a durable
event-table query failure, then returns healthy after cursor catch-up succeeds.
The bounded series are `sse_active_streams`,
`sse_admission_rejections_total`, `sse_watchers`,
`sse_slow_consumer_disconnects_total`, and
`sse_database_backoff_seconds`; identity, session, workspace, and operation
values never become metric labels.

The simulator is disabled unless `OCSERV_LOCAL_SIMULATOR=true`, is rejected in
production, and only accepts the typed `SimulationProbe` payload. Configure its
absolute socket path with `OCSERV_TRANSPORT_SOCKET`, the RPC deadline with
`OCSERV_TRANSPORT_TIMEOUT`, and the bounded consumer queue with
`OCSERV_TRANSPORT_QUEUE_CAPACITY` (1 through 4096).
The stub retains accepted idempotency keys for its process lifetime. Once that
bounded set is full, it rejects new keys rather than evicting keys that a
durable dispatch retry may still need; restart the development stack to clear
simulator-only state.

The binary accepts `--role=api`, `--role=worker`, `--role=scheduler`, or
`--role=all`. `OCSERV_DATABASE_URL` is required; production may instead use
`OCSERV_DATABASE_URL_FILE`. The OIDC client secret, certificate-signer token,
session key, audit checkpoint key, and break-glass hash support the same
exclusive `_FILE` form for mounted secrets. Audit event authentication uses
`OCSERV_AUDIT_EVENT_KEY_ID` plus the strict `OCSERV_AUDIT_EVENT_KEY_FILE`; raw
event key environment values are not accepted outside the repository's
development and test fixtures and are rejected in production.
Development authentication is
disabled by default and can only be enabled with `OCSERV_DEV_AUTH=true` when
the environment is `development` and the HTTP listener is loopback-only. A
non-loopback development stack must instead set an explicit bearer credential
of at least 32 characters in `OCSERV_DEV_AUTH_TOKEN`; the browser development
server receives the same value as `VITE_DEV_AUTH_TOKEN`. Both modes are
forbidden in production.

Development bearer authentication has no persisted user identity or session.
It can exercise ordinary development reads and simulations, but it cannot
create or approve independent approval requests or perform approval-backed node
approval, revocation, or service reload operations. Use the test OIDC provider
when developing those identity-bound high-risk workflows.

Database migrations run as a separate one-shot process using `--migrate-only`,
an owner connection in `OCSERV_DATABASE_URL`, and the unprivileged role named
by `OCSERV_RUNTIME_DATABASE_ROLE`. The long-running control plane receives
only the runtime role credentials. Migration execution uses a PostgreSQL
advisory lock, validates the complete applied history, and grants the runtime
role ordinary data access while limiting `audit_events` to `SELECT` and
`INSERT`. Migration `000029` creates the authoritative singleton
`controller_schema_compatibility` row with an exact range. Readiness accepts a
Controller expected schema only when
`minimum_compatible_controller_schema <= expected <= current_schema` and the
applied migration history agrees with `current_schema`; missing, malformed, or
unaccounted-for future metadata fails closed. Every later migration starts with
an exact range and must explicitly declare a lower minimum in its own
transaction after compatibility review.

An additive migration is not automatically backward-compatible: verify all old
Controller queries and writes before lowering the minimum. Destructive cleanup
must follow an expand, deploy/migrate, and contract sequence, such as a
post-deployment migration after all consumers stop depending on the old shape.
The compatibility row is not a backup. PostgreSQL backups and PITR remain the
disaster-recovery mechanism.

Run the browser-to-simulator E2E with `make e2e`. The script scopes every
container, network, and volume to `COMPOSE_PROJECT` and removes them on success,
failure, or interruption.

The same Compose stack is used by the `Browser E2E` GitHub Actions job. The E2E
harness collects the Playwright HTML report, traces, screenshots, videos, test
results, Compose logs, and container status as diagnostics. GitHub Actions
uploads the failure diagnostics artifact only when the job fails or is
cancelled (`if: ${{ failure() || cancelled() }}`); a successful run does not
upload this diagnostics artifact. Scoped cleanup still runs, and `make e2e` is
the local reproduction command rather than a separate acceptance environment.

For a disposable development stack with no data to preserve, recreate the
database from the current schema with:

```bash
docker compose -f deploy/compose/compose.yaml down --volumes
```

For persisted or shipped schema changes, do not rely on historical manual
down-chains. Use a forward fix or a controlled database restore. Migration
down/up behavior is verified by the current database integration harness; see
[PostgreSQL backup](../operations/postgres-backup.md) for the restore workflow.
