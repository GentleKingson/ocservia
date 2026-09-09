# PR-01: Controller database boundary

## Baseline and scope

- Repository: GentleKingson/ocservia.
- Fetched `origin/main` on 2026-09-09. HEAD, origin/main and review baseline
  were all `a9ead272f5e5e88efce1919b11b160ee15fe3072`.
- Initial `git status --short --branch`: `## main...origin/main`, no changes.
- Branch: `pr-01-database-boundary`. No reset, merge or deployment.
- Only PostgreSQL is supported. This PR does not register MySQL or MariaDB,
  introduce an ORM, change pgx pool settings, or translate SQL strings.

## Contract

`internal/database` defines a small SQL Store, Tx, Backend, isolation levels,
capability checks and errors usable with `errors.Is`. SQL Store is an adapter
primitive, not a promise that SQL is portable. Business code uses domain Stores.
Unknown isolation levels/capabilities fail closed. Default isolation preserves
the connection's PostgreSQL default; explicit levels map to pgx options.
No automatic retry is performed, including serialization/deadlock errors.
Commit errors must not be assumed to mean that no commit occurred.

PostgreSQL wraps existing pgx pools/transactions without owning their lifecycle.
Cancellation/deadline errors remain inspectable. Known SQLSTATEs, no rows and
closed/aborted transactions acquire neutral error categories; unknown errors are
preserved without guessed retryability. The original error chain remains for
legacy callers, but new business code must not inspect pgconn or SQLSTATE.
`Within` owns commit and uses a separate bounded cleanup context on error,
cancellation or panic. Stores never open independent transactions.

The first migrated loop is `telemetry.Ingest` / `useroperations.RecordUsageTx`
to `userusage.RecordTx`: cursor locking, delta computation, replay handling and
monthly/lifetime aggregation. Business validation stays in userusage; its SQL
is moved unchanged to PostgreSQL UsageStore. Both existing callers wrap their
exact pgx Tx, preserving node locks, audit, fencing and surrounding writes.
The two explicit adapter construction sites are temporary bridges, not a new
pattern for business modules. They are removed in PR-03 below.

For a new business operation, bind every participating domain Store to the
same Tx inside `database.Within`. Never construct a store from a pool while
inside that callback. Do not replace a transaction with multiple callbacks.

## Backend and DSN configuration

- Unset `OCSERV_DATABASE_BACKEND` means the historical PostgreSQL mode, not
  inference from the DSN. Explicitly set values accept exactly `postgres`.
- Empty, unknown, `mysql`, `mariadb` and `auto` selectors are rejected, in all
  environments and CLI modes. There is no connection-failure fallback.
- `OCSERV_DATABASE_URL` is retained as the DSN key; it must still be a
  `postgres://` or `postgresql://` URL with a host. No new DSN alias is added.
- `OCSERV_DATABASE_URL_FILE` retains the existing file checks and newline
  handling. Presence of both keys is an error even when an inline value is
  empty. The selector is non-secret and has no `_FILE` variant.
- URL query parameters, TLS handling and pgx/libpq environment behavior are
  unchanged. DSN contents must not be copied to logs or PR evidence.

## Access inventory and removal ownership

`database-access-files.txt` is the broad candidate inventory, including SQL
and external operations, not just imports. `database-driver-baseline.txt` is
the narrower per-file driver symbol/import ceiling enforced by an AST test.
Test fixtures remain PostgreSQL-specific and are not business driver leakage.
The following PR identifiers are the proposed follow-up sequence for this
refactor, not claims that GitHub PRs with those numbers already exist.

| Removal PR | Modules / paths | Boundaries that must remain atomic |
| --- | --- | --- |
| PR-02 identity and authority | `internal/auth`, `rbac`, `approvals`, `audit`; corresponding API handlers | login/session/local credential/bootstrap mutations; elevated role binding + approval consumption + audit; audit chain workspace advisory lock and repeatable-read verification |
| PR-03 command and observation | `operations` (including worker, recovery, agentupgrade), `commandlimit`, `localslice`, `telemetry`, `userstate`, `useroperations`, `configplan` | command/outbox/attempt/event writes; backlog locks; node locks + cursor/usage writes; approval/audit calls use the same Tx; remove both PR-01 UsageStore bridges |
| PR-04 trust and fencing | `enrollment` (including convergence), `certificates`, `privdattestation`, `coordination`, `connectionowner`, `ownersession` | token consumption + trust state + audit; receipt/effect deduplication; scheduler/connection owner row locks through commit; dedicated session advisory locks retain connection ownership |
| PR-05 composition and PostgreSQL operations | `platform/app`, remaining `api` SQL and pgx error checks, `attestationtest`; `migrations`, CLI wiring, `scripts`, `deploy` | preserve startup schema gate, migration lock/preflight/grants, role separation and backup/restore behavior; PostgreSQL SQL/psql remains in explicitly PostgreSQL-owned adapters/tooling rather than being made generic |

Detailed non-import findings:

- API files such as `rbac.go`, `audit.go`, `auth_handlers.go` and
  `event_streams.go` execute SQL through Server.pool without importing pgx.
  `operations/worker.go` also executes transaction SQL without a pgx import.
- `auth/local_lifecycle.go` inspects `23505`; telemetry quarantine inspects
  class `22` and `54001`. Numerous modules use `pgx.ErrNoRows`; agentupgrade's
  local query interface exposes `pgconn.CommandTag`. These are not removed by
  renaming a pool type and stay on the corresponding removal PR's checklist.
- SQL relies on `$n`, casts (`uuid[]`, `bytea`, `jsonb`, `interval`, numeric),
  `ANY`, `RETURNING`, `ON CONFLICT`, CTE updates, partial indexes and PostgreSQL
  time functions. `now()` transaction time and `clock_timestamp()` real time
  are intentionally different in fencing checks.
- `postgresinput.ValidText` encodes PostgreSQL text limits (UTF-8, no NUL),
  even though it performs no I/O and imports no driver. PR-03 owns its caller
  contract; removing pgx imports must not remove input validation.
- Queue claimers use `FOR UPDATE SKIP LOCKED`; authority checks also use
  `FOR SHARE`. Command admission, audit and initialization use transaction
  advisory locks; migration and scheduler coordination include session locks.
  An adapter must not emulate these with independent transactions.
- Migration 000005 has range partitions/default partition, SECURITY DEFINER
  partition functions with fixed search_path and explicit EXECUTE grants.
  Maintenance invokes these functions. Migration runner grants table, column,
  sequence and function privileges separately to the runtime role.
- `cmd/ocserv-control/main.go` has no direct SQL: Config.Load and app.Run own
  migration-only, schema compatibility, bootstrap and completion execution.
- External DB executors include `scripts/database-integration.sh`,
  `postgres-backup.sh`, `i18-*`, `local-slice-integration.sh`, `real-e2e-controller.sh`,
  `g6-ha-pitr-*`, `g6-readiness-*`, `g6-relay-diagnostics.sh`,
  `p1-resilience-capacity*` and `relay-recovery-experiment.sh`.
  They invoke psql/pg_dump/pg_restore/replication tooling or helpers and inspect
  PostgreSQL catalogs, roles, locks, WAL, recovery and fencing state.
- Deployment compose files (compose, production, real-e2e, g6-readiness,
  g6-ha-pitr) configure PostgreSQL and role-specific URLs. Production
  `postgres-init/001-runtime-role.sh`, `rotate-postgres-credentials.sh`, backup
  entrypoint/image and controller installation scripts own external PostgreSQL
  operations. Tests/manifest builders in the candidate list assert these
  contracts rather than necessarily connecting themselves.

## Historical migrations

All 68 SQL files (000001 through 000034, up and down) retain filenames and
original bytes. `database-migrations.sha256` records SHA-256 for each, checked
before the existing integration matrix. Runner, embedded checksums, applied
metadata validation and schema compatibility behavior are unchanged.
No rollback boundary is bypassed: 000034 remains forward-only; audit and
session authority guards remain; 000024/000025 retain fencing epochs.
The existing pre-34 source fixture is still built by the integration script
without changing production migrations.

## Validation

All execution is on BuildServer via `ssh BuildServer`, in isolated checkout
`/root/ocservia-pr01.A8SNio`; no existing server checkout is overwritten.
The existing `scripts/database-integration.sh` remains the PostgreSQL 17/18
acceptance entrypoint, including schema compatibility, historical fixtures,
authentication required-test lists and rollback checks. It now also checks
historical byte hashes and executes the database boundary tests on each major.
The existing CI database job now runs as a 17/18 matrix with fail-fast disabled.

New tests cover error categories, fail-closed capabilities/isolation,
cancellation/deadline cleanup, rollback, commit after rollback, explicit
isolation levels and two usage stores sharing one transaction, including
replay/delta behavior and all-or-nothing persistence.

Execution results are recorded in the PR-01 validation report.
