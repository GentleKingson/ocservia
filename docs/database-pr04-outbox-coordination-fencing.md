# PR-04 Draft: Outbox Coordination and Fencing

Base: PR-01 through PR-03, merged through `1407b8d` (#194).
MySQL/MariaDB remain test/development-only. No Agent protocol, signature format,
schema migration, production admission, deployment or merge changes are included.

## Implementation

PR-02 already supplied backend-owned operation, outbox, command-limit, recovery,
result-ingress and ownership stores. PR-04 reuses those adapters and the existing
Controller services instead of introducing a second dispatch implementation.
Operation intent, command, outbox, operation event and audit intent still commit
in one transaction. Claim selection, eligibility, capacity, node lease, outbox
lock and attempt allocation remain in the same admission-locked transaction.

Lease authority now uses a fresh database wall clock after acquiring the relevant
locks. Scheduler and connection-owner renewal cannot revive a term that expired
while waiting; acquisition grants a deadline measured after that wait. PostgreSQL
assertions also recheck the clock after their row-share lock. Transaction-stable
time remains available for existing logical timestamps and is not globally changed.
Per-node first acquisition is serialized even before the fencing row exists.
Takeovers increment the retained epoch, reject overflow, and do not delete or
recreate authority records. Old terms still fail their exact owner/incarnation,
connection and epoch predicates. MySQL no-op matched scheduler/renewal writes
are distinguished from missing rows rather than assuming PostgreSQL RowsAffected
semantics. Expected node-lease conflicts do not suppress unrelated unique errors.

`database.WithinRetry` is opt-in for database-only claim, completion, reaping and
lease transactions. It makes at most three attempts with bounded delay and
cancellation-aware cleanup. Deadlock victims are retryable; other serialization
errors require an acknowledged rollback. Connection loss, lock timeout, ordinary
constraint errors and unknown commits are not blindly retried. The existing
`database.Within` callback contract remains non-retrying. The distinction follows
the [InnoDB transaction rollback rules](https://dev.mysql.com/doc/refman/8.4/en/innodb-error-handling.html)
and [PostgreSQL transaction retry guidance](https://www.postgresql.org/docs/18/mvcc-serialization-failure-handling.html).

When a commit acknowledgement is uncertain, the service performs bounded readback
of the original immutable intent, exact attempt/lease, completed attempt with its
sent envelope, or exact owner term and deadline. Missing, expired or superseded
evidence fails closed. It does not allocate a replacement intent or invoke
transport to resolve a database error. Repeated successful bookkeeping is
idempotent without requiring an Agent result to have arrived already.

Transport remains after claim commit and outside retryable database callbacks.
An observer's existing non-retrying ownership guard can still span a bounded
external mutation; this is not a retryable transaction. An abandoned sending
attempt, whether before send or after transport acceptance, follows the existing
Unknown/reconcile-only path. Global active capacity, workspace/node backlog,
per-node leases and reconciliation attempt ceilings are retained. No network
exactly-once delivery claim is made.

## Regression Coverage

The new shared real-database workflows use disposable databases and restricted
runtime accounts on PostgreSQL, MySQL and MariaDB:

- Atomic intent rollback, lost commit acknowledgement, same-key replay and
  conflicting intent rejection.
- Two workers contending for one active slot; transport observes a committed
  attempt, and an uncommitted claim cannot escape.
- Both abandoned-claim windows, expired claim rejection, reconcile-only identity
  preservation, retained Unknown outcome and exhausted attempt limits.
- Actual LocalSlice ingestion before bookkeeping, duplicate event delivery and
  late/repeated bookkeeping without duplicate terminal projection.
- A real two-transaction database deadlock, bounded retry and no partial writes.
- Two Controller ownership contenders, scheduler contenders, retained monotonically
  increasing epochs, denied runtime deletion and old-term renewal/release rejection.
- Lock waits spanning lease expiry, post-wait acquisition deadlines, matched no-op
  writes, owner/scheduler commit readback and delayed stale-owner bookkeeping.

`TestRealOutboxCommitDisconnect` additionally uses the existing wire-level proxy
on MySQL/MariaDB. It blackholes COMMIT requests or replies during claim and sent
bookkeeping, verifies the physical connection is discarded, and checks durable
state without replaying the transaction or external send.

The cross-backend crash windows abandon Controller memory at the transaction
boundary with a recording transport stub; they are not a full multi-process
transportd/Agent kill-and-restart test. Commit acknowledgement injection in the
shared suite wraps finalization of real transactions, while the MySQL/MariaDB
disconnect suite interrupts real protocol packets.

Existing PostgreSQL failure tests and historical migration/rollback harnesses
are unchanged. Existing MySQL/MariaDB command-limit, dispatch, recovery, ingress,
owner-session and scheduler suites are retained. The required-test manifest and
both existing database CI entry points require the new shared workflows, failing
on missing or skipped required entries. The native MySQL/MariaDB gate also
requires all four real outbox disconnect cases.

## Validation

Validation is executed only through `ssh BuildServer`, in the isolated checkout
`/root/ocservia-pr04.WXBOOp`. The scoped runner and raw logs are retained there.
Go checks use the `golang:1.26.6-bookworm` toolchain container.

| Check | Result |
| --- | --- |
| Controller `go test -run '^$' ./...`, targeted retry/worker units, scoped `go vet`, Controller build | Passed |
| PostgreSQL 17 shared coordination workflows with `-race` | 18/18 required entries passed, none skipped |
| PostgreSQL 18 shared coordination workflows with `-race` | 18/18 required entries passed, none skipped |
| MySQL 8.4.10 shared coordination workflows with `-race` | 18/18 required entries passed, none skipped |
| MariaDB 12.3.2 shared coordination workflows with `-race` | 18/18 required entries passed, none skipped |
| PostgreSQL 17/18 existing coordination, connectionowner, ownersession, operations, localslice integration suites and command limits, with `-race` | Passed, except the historical-fixture-only skip described below |
| MySQL/MariaDB native command limits, dispatch, reaping, results, owner/scheduler, real COMMIT disconnect and transaction cleanup, with `-race` | All 10 selected top-level tests passed on each engine, including all four outbox disconnect cases |
| Both database entry points `bash -n`, required-test guard positive/10 negative fixtures, 68 PostgreSQL migration checksums | Passed |

Successful run logs are `compile-final.log`, `pg17-final.log`, `pg18-2.log`,
`mysql-2.log` and `mariadb.log` under the BuildServer checkout. The required-test
gate inputs are retained as `artifacts/{pg17,pg18,mysql,mariadb}-coordination.json`.

The existing `TestConnectionOwnerTakeoverContinuesPastRetainedEpochIntegration`
requires `OCSERV_TEST_RETAINED_NODE_HEX` from the historical rollback harness and
was skipped in these current-schema fixtures. Its test and harness are unchanged;
the ordinary epoch-never-reused test and new repeated-takeover cases passed.
The full historical migration rollback cycle, full database CI scripts and
multi-process transportd/Agent fault harness were not rerun for this scoped change.
