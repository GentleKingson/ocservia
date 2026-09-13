# PR-07: Telemetry and Runtime Completion Review

Baseline: `8b42063` (merged PR-06). This is a partial implementation record,
not PR-07 acceptance or a production-support declaration. No historical
migration bytes are changed. The user subsequently authorized creating a
Draft PR with the remaining acceptance gaps disclosed; no merge or production
release is authorized.

## Storage Decision

The user confirmed keeping the existing monthly ordinary InnoDB tables for
MySQL/MariaDB. These are not native MySQL partitions: their node and ingest
batch foreign keys remain enforceable. Owner provisioning remains outside
ingestion transactions. PostgreSQL retains migration 000005's partitions.

## Reviewed and Changed

- Both history adapters previously recomputed only 48 hours although ingestion
  accepts samples up to 14 days old. They now recompute that full lateness
  window, starting at each resolution's complete UTC bucket. This also avoids
  overwriting the boundary bucket with a partial aggregate.
- PostgreSQL's rollup origin and retention interval arithmetic now explicitly
  use UTC rather than the session timezone. The history fixture uses
  `Asia/Kathmandu` to expose fractional-hour origin shifts.
- Normal role startup now checks transaction/row-lock support and, on the
  InnoDB backends, historical migration completion plus active, readable
  monthly tables covering the accepted 14-day age and five-minute clock skew.
  It does not provision tables, repair migrations or disable telemetry.
  Migration-only and bootstrap CLI paths keep their existing behavior.
- The telemetry service no longer exposes PostgreSQL pool/transaction bridge
  constructors or the unused `IngestWireTx` bridge. Its PostgreSQL test callers
  adapt their fixtures explicitly; ingestion still uses the same common Tx.
  Its removed driver allowances are deleted from the PR-01 baseline guard.
- PostgreSQL migration 35 and independently authored MySQL/MariaDB revision
  24 introduce restricted retention routines. Each invocation deletes at most
  1,000 expired rows per rollup table and retires/drops at most one expired
  month. MySQL physical collection remains owner-only, outside business Tx,
  and also stops after one month. These are row/month bounds, not latency SLOs.
- The oldest retirement candidate is aggregated before its raw data becomes
  unavailable, including after maintenance has been offline beyond 14 days.
  Retention clocks are checked against server time and cannot move cutoffs
  into the future. PostgreSQL pins UTC inside its definer functions.
- Existing direct rollup DELETE grants are revoked; PostgreSQL also revokes
  TRUNCATE and rejects inherited unrestricted cleanup privileges. MySQL and
  MariaDB reject DDL, global/schema DELETE authority and role inheritance at
  runtime startup. Required cleanup EXECUTE is checked without performing
  cleanup. MySQL catalog locking uses shared locks, not runtime catalog writes.
- Controller schema expectations advance to 35 without modifying historical
  migration or revision bytes. The historical schema-33 test fixture restores
  its old runtime contract only in a disposable copy; the current Controller
  has no missing-routine fallback or capability-check bypass.
- The real Controller E2E harness now supports both combined `all` and separate
  `api`, `worker`, `scheduler` processes, with committed scheduler maintenance
  evidence in addition to the existing transport/Agent/privd workflows.

## Remaining Acceptance Work

These are open requirements, not waivers or completed inventory entries:

| Requirement | Remaining work |
| --- | --- |
| Performance | Agree workload and thresholds before measuring ingestion, plans, lock waits, cleanup bounds and storage growth. Expanding the rollup scan window is a correctness change, not evidence of performance equivalence. |

The requested performance workload and thresholds have not yet been supplied:
node count, per-node telemetry frequency, ingestion/query P95 latency, lock
wait budget and per-cleanup time budget. Batch limits and maintenance cadence
must be justified against that workload rather than chosen to pass a benchmark.

The user subsequently requested a Draft PR before performance acceptance.
Performance remains an open acceptance gate, not a waived requirement.
Physical table reclamation tests do not establish
an overall storage-growth bound for retained batch receipts or shard metadata.

## PR-01 Inventory Closure

After explicit authorization, a Go type-aware rewrite removed the remaining
business pool/transaction compatibility entry points and adapted their test
callers to existing common Backend APIs. Argument order, constructor defaults
and transaction identity were retained. The manually reviewed exceptions are:

- Audit migration preflight now accepts `database.Tx`. PostgreSQL owns the
  historical table lock and legacy-row query through a narrow store capability;
  the audit module still verifies the same chain and checkpoint cryptography.
  The owner migration connection adapts the exact transaction, not a new one.
- `coordination.Fence` requires the common transaction assertion directly.
  Unused production raw-SQL execution/commit helpers were removed; their stale
  leader test now uses `database.Within` and the production fence assertion.
- The auth service's fixture-only pool field was removed. Its local-limit
  integration fixture keeps its own explicit pool without changing runtime
  service ownership or cloning the shared service accidentally.

`database-access-disposition.tsv` accounts for all 252 original candidate
paths, including non-database keyword matches. The boundary test checks exact
inventory coverage, rejects business driver imports and raw SQL calls, and no
longer reads a temporary driver allowance file. Cross-package attestation and
semantic test fixtures remain test-only and cannot be imported by business
code. Driver-owned adapters, owner migrations and explicit PostgreSQL
deployment/backup/restore tooling are intentional boundaries, not unresolved
business leaks. Their retention does not claim MySQL production deployment
support, which remains outside this phase.

The read/query entries resolve to the existing telemetry read/history,
local-slice, operation, audit and RBAC stores. Verification uses actual prepared
writes and reads: missing snapshots and nullable audit/ban fields, exact JSON
numbers, UUID/session/event cursors, IPv4/IPv6 canonicalization and ordering,
and UTC history buckets. Transport cursor, quarantine and rollback tests stay
at the same common transaction boundary. Static inventory closure is not
performance acceptance or a declaration that every historical operations
script was re-executed.

## Earlier Validation

Execution is restricted to BuildServer, using the isolated directory
`/root/ocservia-pr07.Ycjb8t` and disposable database containers. An existing
module cache was incomplete (the UUID module directory lacked its source), so
validation uses a task-owned fresh module cache instead of deleting or repairing
shared caches.

| Executed check | Result |
| --- | --- |
| Controller `go test -run '^$' ./...` | Passed (compile-only) |
| PostgreSQL 17 and 18 history workflow, with `-race` | Passed, including both late/boundary subtests in a non-UTC session |
| MySQL 8.4.10 and MariaDB 12.3.2 history workflow, with `-race` | Passed, including active-month refusal and both late/boundary subtests |
| PostgreSQL 18 actual Controller process startup, with `-race` | Passed |
| MySQL/MariaDB actual Controller process startup, with `-race` | Passed, including missing-month refusal for all four roles |
| Focused telemetry validation/version, API history-error and bootstrap-password tests | Passed |
| Database boundary and retry tests | Passed |

Raw logs and the disposable validation harness are retained locally under
`.cache/pr07-validation/`. Task-owned remote containers, checkout and caches
were removed after retaining these results. The successful process startup test exercises the
existing combined-role CLI/login/readiness/maintenance path; it is not full
four-role successful E2E acceptance. Performance measurements and the remaining
requirements above are not passed or completed by these results.

## Continuation Validation

All execution took place on BuildServer in the isolated directory
`/root/ocservia-pr07-continue.tkMR0r`, with disposable databases. Evidence is
retained under `.cache/pr07-validation/continuation/` (including failed attempts
and their successful targeted reruns).

| Executed check | Result |
| --- | --- |
| MySQL/MariaDB revision 24 authoring from both pinned historical roots | Passed on actual MySQL 8.4.10 and MariaDB 12.3.2; generated manifests retained in source |
| PostgreSQL 17/18 history and privilege workflow, `-race` | Passed: late/full buckets, UTC, 1,000-row pruning, rollback, one-month physical drop and outage rollups; the final grant-receipt change was rechecked on PostgreSQL 18 |
| MySQL/MariaDB history and privilege workflow, `-race` | Passed: existing-account DELETE revocation, missing EXECUTE refusal, schema-wide DELETE refusal, late buckets, bounded pruning/retirement and physical collection |
| MySQL/MariaDB interrupted collection recovery, `-race` | Passed: resumed the retired receipt after physical DROP but before catalog update; both physical month tables were absent afterwards |
| Actual Controller startup and read HTTP workflow on all four database versions, `-race` | Passed, including all-role missing-month refusal on both InnoDB engines |
| MySQL/MariaDB initialization/history, operation reads and transport results, `-race` | Passed individually, including quarantine replay and rollback without cursor advancement |
| PostgreSQL 18, MySQL and MariaDB real E2E, each in combined and split role modes | All six passed: dedicated TLS relays, real transportd/Agent/privd/Ocserv, enrollment, root attestation, fenced certificate/one-use artifact and user/group workflows |
| Controller compile-all; database boundary, eventstream and transportclient unit tests | Passed; protected-gap buffering/retry and fetch acceptance included |
| Historical schema-33 fixture patch and build; edited shell syntax | Passed; the complete historical downgrade CI suite was not rerun |

Validation corrections were kept separate from product claims: a first grant
parser incorrectly matched an UPDATE column name containing `event`; a MySQL
locking read required catalog write authority; owner-visible grant metadata
did not reliably describe runtime grants; and schema grants could be cached
on an existing connection. The final checks cover the corrected paths.

The first transportclient unit run was interrupted after copied directory
ownership violated its trusted Unix-socket ancestry requirement. Only the
task-owned BuildServer fixture directory ownership was corrected, then all
transportclient tests passed with a 60-second timeout. No product trust check
was weakened. Two acceptance wrapper logs also record a trailing host-shell
`go: command not found` after every in-container test had passed, caused by
updating the temporary wrapper while it was running; those wrappers are not
reported as successful invocations. Their individual test results and the
separate successful collection/E2E runs are retained without deleting failures.

After retaining the logs and checking the six E2E workflow-log hashes against
their local copies, the task-owned remote checkout, fresh caches and temporary
scripts were removed. No test containers or task E2E networks remained. Shared
Docker build caches were not deleted.

## Inventory Continuation Validation

The authorized rewrite and follow-up checks ran only on BuildServer, in
`/root/ocservia-pr07-inventory.faW0NV`. Logs, rewrite input and disposable
validation harnesses are retained under `.cache/pr07-validation/inventory/`.

| Executed check | Result |
| --- | --- |
| Controller compile-all and zero-business-driver/raw-SQL boundary | Passed |
| Exact disposition coverage of all 252 PR-01 paths | Passed |
| Historical migration checksums and edited shell syntax | Passed |
| PostgreSQL 18 audit, coordination, connection-owner, owner-session and auth packages, `-race` | Passed, including legacy checkpoint preflight and shared transaction fencing; retained-epoch rollback fixture was not supplied and that dedicated test skipped |
| PostgreSQL 18 telemetry and PostgreSQL adapter packages, each on a fresh database, `-race` | Passed |
| PostgreSQL 17, MySQL and MariaDB telemetry ingestion/read workflow and Controller read HTTP, `-race` | Passed, including mixed IPv4/IPv6 ordering, optional ban duration, session cursor, exact JSON and missing snapshots |
| MySQL/MariaDB operation reads, history/retention and transport results, `-race` | Passed, including quarantine/replay and commit rollback |
| MySQL/MariaDB runtime diagnostics and privilege-upgrade workflow, `-race` | Passed, including the immutable root receipt and latest revision contract |
| PostgreSQL 18, MySQL and MariaDB real E2E after removing compatibility entry points | All six passed: combined `all` plus separate `api/worker/scheduler`, real TLS relays/transportd/Agent/privd/Ocserv and committed maintenance |

The expanded API package run is **not** reported as wholly passing:
`TestSyntheticCommandAuditUsesAuthenticatedOperator` and
`TestBatchRouteAllowsNodeScopedPerItemAuthorization` fail in their unchanged
fixture setup because parameterized multi-statement SQL is rejected by pgx
(`42601`). Both failures precede the changed constructor calls. Those unrelated
fixture SQL statements were not rewritten. The focused backend HTTP workflows
and real process E2E are separate evidence, not substitutes for those tests.

Failed attempts remain in the evidence directory. The first owner-session run
encountered the same copied-directory ownership constraint as the earlier
transport tests; only the disposable tree's ownership was corrected, and later
transfers preserved it. The broad PostgreSQL run also mixed audit fixtures with
different authentication keys; the PostgreSQL adapter suite passed on its own
fresh database. New IP assertions initially used a scalar where the fixture
requires an optional duration; that compile error was fixed before the passing
four-engine runs. A diagnostics-test attempt incorrectly changed the immutable
root compatibility receipt from 34 to 35; its original 35/34 mutation/restore
was restored, documented, and both engines passed. The latest revision's
Controller contract is independently 35. None of these corrections relaxed
runtime permissions, trust checks or schema validation.

All six final E2E workflow-log SHA-256 hashes matched the retained local
copies. No test containers or task networks remained. The task-owned remote
checkout, fresh caches and local temporary harness files were removed; shared
Docker images/caches were retained. Two SSH clients remained open after their
remote harnesses had exited and were closed only after checking process,
container and log completion. No performance run was performed. Commit, push
and Draft PR creation were authorized separately after this validation.
