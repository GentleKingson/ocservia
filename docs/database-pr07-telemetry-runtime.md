# PR-07: Telemetry and Runtime Completion Review

> **Historical record, not a current operating guide.** Retained from the
> 2026-09-15 source snapshot `cc8399641dc32083466a9c77369fcb8debf1ee48`;
> this snapshot SHA is not a new acceptance baseline. Original execution dates,
> tested revisions, Draft status, PASS/FAIL and unverified scope below remain
> historical; an unrecorded test SHA is unknown, not the snapshot SHA.
> Use the [current guide](operations/production-deployment.md#database-support) and
> [maintained technical reference](development/telemetry.md) instead.

Baseline: `8b42063` (merged PR-06). This is a partial implementation record,
not PR-07 acceptance or a production-support declaration. No historical
migration bytes are changed. The user subsequently authorized creating a
Draft PR with the remaining acceptance gaps disclosed; no merge or production
release is authorized.

Current performance status (2026-09-13): the agreed four-workload, low-data
gate passes on all four engines; see [bounded-transaction measurements](#bounded-transaction-measurements-2026-09-13).
The sections below retain the chronological implementation and failed-attempt
record. Earlier statements that thresholds were unspecified or performance
was open describe their recorded candidate, not the latest measurements.

### Post-merge API fixture closure

The two pgx `42601` failures recorded below were present when PR #201 merged.
A fixture-only follow-up now runs their parameterized multi-statement setup SQL
with the simple protocol; production queries retain the pool's default extended
protocol. The same follow-up reports cleanup failures for the affected batch
authorization and bootstrap-token fixtures and adds both previously failing
tests to Basic CI's PostgreSQL-only `regression-auth-postgres` profile. On
2026-09-13, BuildServer passed
the two targeted tests and the expanded `internal/api` package on both
PostgreSQL 17 and 18, with no `42601` or cleanup error. The selected
`regression-auth-postgres` profile also passed both required tests with `-race`
on both versions. The historical PR #201 results below remain unchanged.

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

## Initial Acceptance Gaps

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

## PR #201 Review Corrections

The preceding validation record describes head `1954efe`, not acceptance of
the review corrections below. Performance acceptance remains open. The user
specified a single node and low data volume; explicit latency, lock-wait and
storage-growth thresholds have not been supplied.

- Append PostgreSQL migration 36 and MySQL/MariaDB revision 25 rather than
  changing any previously applied migration bytes. The current Controller
  compatibility contract advances to 36; immutable MySQL root metadata stays
  at 34.
- PostgreSQL ingestion no longer calls the partition-creation function.
  Owner migration preprovisions the previous/current/two future months on
  every `--migrate-only` run. Migration 36 revokes existing non-owner grants
  on the creation function, and runtime grant repair/startup also reject
  inherited EXECUTE capability. A DEFAULT-partition trigger rejects runtime
  insertion/update, while preserving owner access to historical default rows.
  Missing months therefore fail closed. Startup requires the accepted months
  to be attached, not merely present as standalone tables. Owners must rerun
  migration before the provisioning horizon expires; conflicting historical
  default rows cause provisioning to fail rather than being discarded.
- MySQL/MariaDB retirement performs final backfill inside its definer
  procedure before changing the catalog state. Direct EXECUTE cannot skip
  finalization. Both changes remain in the caller's fenced transaction, and
  owner collection still consumes committed retirement only. Backfill scans
  only complete retained buckets (90 days for 5m, 13 months for 1h), not an
  entire ancient month. Server-side aggregate merges replace the unbounded
  Go aggregate slice and per-aggregate network round trips. These changes do
  not establish a fixed server scan, execution-time or storage-growth bound.
- Revision 25 requeues preexisting retired shards whose physical tables
  survive, because old retirement receipts did not prove finalization. An
  interrupted DROP with an absent table remains eligible for receipt repair.
- MariaDB additionally receives SELECT on the two exact-rollup key side
  tables, required by its multi-table UPDATE privilege checks. They contain
  the same natural keys already visible in rollups; no new mutation or DDL
  privilege is granted. Runtime startup checks those reads too.

### Correction Validation

Executed on BuildServer using disposable databases and Go 1.26.6:

- PostgreSQL 17/18: upgrade from the actual schema-35 `1954efe` binary,
  verify migration 36 revokes legacy EXECUTE before grant repair, then run
  `TestTelemetryHistoryWorkflowIntegration` with `-race`. Direct creation
  calls are denied, startup rejects detached months, and missing-month
  ingestion leaves no DEFAULT rows. Existing history/retention controls pass.
- MySQL 8.4.10 / MariaDB 12.3.2: author revision 25 against both pinned
  historical roots, then run `TestRealPrivileges` and
  `TestRealTelemetryHistoryWorkflow` with `-race`. Direct retirement calls
  finalize retained buckets, rollback undoes both aggregates and retirement,
  ancient months produce no expired aggregates, and surviving old retirement
  receipts are requeued. Runtime cannot mutate the exact-key side tables.
- PostgreSQL 18 / MySQL / MariaDB: real Controller process startup tests
  pass. MySQL/MariaDB additionally exercise all/api/worker/scheduler refusal
  of missing telemetry months and focused backend HTTP read workflows.
- Controller compile-all, database boundary/access-inventory and migration
  metadata tests, Go formatting and changed shell-script syntax checks pass.

Earlier six-role TLS/Agent E2E results are not relabeled as results for this
patch; those complete workflows were not rerun. The two existing API fixture
`42601` failures remain separately disclosed and unchanged.

### Low-Data Measurements

These are database-store observations, not HTTP latency or performance
acceptance. Each engine ran sequentially on the shared BuildServer (2 ARM64
vCPUs, approximately 11.6 GiB RAM), without the race detector: one node, six
hourly metrics, 2,016 recent samples plus 4,464 samples across an outage month,
concurrency one, 100 six-sample ingestion transactions, 100 raw 24-hour history
queries and ten maintenance transactions. The first maintenance finalizes the
outage month. With only ten maintenance observations, nearest-rank P95 is the
maximum. No threshold assertion was applied.

| Engine | Ingestion P95 | Query P95 | First Maintenance / P95 |
| --- | --- | --- | --- |
| MySQL 8.4.10 | 14.58 ms | 3.55 ms | 32.83 s / 32.83 s |
| MariaDB 12.3.2 | 8.80 ms | 2.14 ms | 17.41 s / 17.41 s |

The seconds-scale maintenance results remain a material open performance
risk even at this low volume. Both runs reported zero additional global
`Innodb_row_lock_time` milliseconds, but concurrency one cannot demonstrate
contention behavior. Rollup row counts stayed at 12,972 over nine repeated
maintenance calls after collection. Estimated telemetry allocation from
`information_schema.tables` stayed at 8,781,824 bytes on MySQL and changed
from 6,291,456 to 9,027,584 bytes on MariaDB. These estimates and a short
no-new-data run do not establish long-term storage-growth acceptance.
The retained EXPLAIN JSON covers one physical raw input table, not the full
history UNION plan. PostgreSQL performance was not measured in this run.

Raw logs and the task-owned measurement/upgrade fixtures are retained locally
in `.cache/pr201-review-fixes/`, outside the commit. Diagnostic failures
(missing revision embed, MariaDB side-table SELECT, and an overbroad ancient
fixture assertion) were corrected before the final passing runs. Performance
thresholds, contention/storage-duration validation and formal PR-07 acceptance
remain open; the PR stays Draft and is not authorized for merge or release.

### Basic CI 907 Fixture Correction

Basic CI 907 on `ca58f49` exposed a separate PostgreSQL service fixture still
seeding infinite raw timestamps through runtime. Only that historical seed in
`TestTelemetryBackendWorkflowIntegration` now uses the explicit owner URL and
the PostgreSQL value adapter. The service, ordinary ingestion, duplicate and
rollback checks remain on the runtime account. Migration 36, its DEFAULT
trigger and runtime permissions are unchanged; missing owner credentials fail
the test rather than skipping it.

BuildServer passed the actual CI entry point
`PG_MAJOR=17 DATABASE_TEST_SCOPE=regression bash scripts/database-integration.sh`
and the corresponding PostgreSQL 18 command, including required-test inventory
checks and the formerly failing telemetry workflow. Go formatting also ran on
BuildServer. Raw results are retained in `.cache/pr201-fixture-fix/`. Initial
harness attempts lacked Ruby and the Docker CLI; those dependencies were
provided in a disposable runner container, not by relaxing repository checks.
The PR was restored from Ready to Draft. These functional results do not close
the performance Gate or replace fresh complete Basic CI results for this fix.

### Agreed Low-Data Performance Gate

The user subsequently supplied the acceptance limits below. They replace the
earlier "thresholds unspecified" status, not the earlier measured failures.
The workload remains one managed node, six metrics and the recorded low-data
dataset, with four simultaneous workloads: ingestion, history queries, an
ordinary Controller database operation and maintenance.

| Measurement | Limit |
| --- | --- |
| Ingestion P95 / P99 | 25 ms / 50 ms |
| History query P95 / P99 | 10 ms / 25 ms |
| Normal maintenance P95 / maximum | 1 s / 2 s |
| First outage/catch-up maintenance | 5 s target; over 10 s is a hard failure |
| Database lock wait P95 / maximum | 100 ms / 500 ms |
| Deadlocks, timeouts, business request failures | Zero |
| Fixed-dataset rollup counts after catch-up | Stable over ten maintenance calls |
| Actual allocated database space after catch-up | At most 5% growth over ten maintenance calls |

MySQL's 32.83 s and MariaDB's 17.41 s remain failures against these limits.
PostgreSQL measurements and four-workload contention/storage validation are
required before this Gate can close. Estimated table statistics alone are not
actual allocation evidence.

### Performance Candidate And Remaining Blockers

The local candidate adds MySQL/MariaDB revision 26, keeping Controller schema
36 and all published migration bytes unchanged. Two nonunique 255-byte
lookup indexes accelerate the rollup side-table triggers' full encoded-key
comparisons. They are not uniqueness constraints; equal prefixes with distinct
long suffixes remain valid. Recent rollup merges on both backends now avoid
rewriting unchanged aggregates.

The first four-workload MySQL run exposed a real deadlock: retirement scanned
the `start_at` index and attempted to lock a current-month catalog row while
holding node foreign-key locks. Ingestion held that catalog row and queued
behind a node update. Revision 26 adds and explicitly selects an expired-month
range index, retaining finalization-before-retirement inside the procedure.
The subsequent measured MySQL runs had no deadlock or business error, without
adding automatic retries, but still failed the latency/lock-wait Gate.

Final measurements used one node, 2,016 recent plus 4,464 outage-month samples,
and four simultaneous workloads: 200 six-sample insert transactions, 200 raw
history reads, 200 real updates to that same node's timestamp, and one catch-up
plus ten normal maintenance transactions. The first three workloads pause
10 ms between operations, maintenance 150 ms; this is a DB-store benchmark,
not HTTP/TLS/Agent E2E. After concurrent work finishes, one final catch-up
precedes the fixed-dataset ten-maintenance storage check. No race detector
or other task-owned verification ran alongside measurements.

| Engine | Ingest P95 / P99 (ms) | Query P95 / P99 (ms) | Catch-Up (s) | Normal P95 / Max (ms) | Lock Max Bound (ms) | Result |
| --- | --- | --- | --- | --- | --- | --- |
| PostgreSQL 17 | 5.14 / 7.43 | 1.76 / 3.03 | 0.227 | 32.08 / 32.08 | 16.82 | PASS |
| PostgreSQL 18 | 5.00 / 8.50 | 2.16 / 3.26 | 0.252 | 33.95 / 33.95 | 17.79 | PASS |
| MySQL 8.4.10 | 20.41 / 54.62 | 7.85 / 9.93 | 3.052 | 160.73 / 160.73 | 3013.90 | FAIL |
| MariaDB 12.3.2 | Not completed | Not completed | Not completed | Not completed | Not completed | FAIL: serialization failure |

The lock observer samples waiting transactions about every 5 ms and pads each
observed episode by the largest sampling gap. Unobserved shorter waits are
bounded by that gap; observer gaps over 100 ms fail verification rather than
counting as zero wait. These are conservative sampled bounds, not exact lock
event percentiles. Observed maximum gaps were 10.91 ms / 11.88 ms / 10.69 ms
for PostgreSQL 17 / PostgreSQL 18 / MySQL, respectively. MySQL's lock P95 bound
was 27.23 ms, but its maximum exceeded 500 ms; the Controller node update also
took 3.018 s. Its ingestion P99 exceeded 50 ms.

PostgreSQL 17 / 18 and MySQL retained 12,972 rollup rows and zero allocation
growth over ten fixed-dataset maintenance calls. Actual allocation was
16,283,315 / 16,684,735 / 66,109,440 bytes, respectively, measured using
`pg_database_size` or InnoDB tablespace `ALLOCATED_SIZE`, not estimated table
statistics. MariaDB's final run returned a serialization failure from recent
rollup maintenance and did not reach storage acceptance. An earlier MariaDB
diagnostic with no-op node updates passed, but is not acceptance evidence for
real concurrent Controller writes.

BuildServer separately passed Controller compile-all, database boundary and
migration metadata tests, and the four-engine targeted history/retention
regressions with `-race`. MySQL/MariaDB privilege and long-key write tests also
passed, including distinct equal-prefix long keys. Revision 26 fingerprints
were authored on both pinned roots of both engines. Raw logs and disposable
harnesses are retained in `.cache/pr201-performance/`; the opt-in reproducible
test is `TestTelemetryLowDataPerformance` (`PR07_PERFORMANCE=1`, isolated
owner/runtime databases required).

The performance Gate remains failed for the candidate measured above. The user
subsequently authorized durable, fenced small transactions instead of one
whole-maintenance transaction. No failed operation is automatically retried and
no acceptance threshold is relaxed.

### Bounded Transaction Candidate

The unpublished revision 26 now also introduces definer-only durable progress
and an 80-row staging table. Each capability call merges at most 80 aggregate
rows and advances its keyset cursor in the same caller-owned transaction.
The scheduler commits each page only after checking its leadership fence.
An interrupted run resumes committed progress; a rejected page and its cursor
roll back together. Offline-node changes remain atomic within the first fenced
batch. PostgreSQL retains its existing single maintenance transaction.

Aggregation is staged before the rollup write, separating its source snapshot
from foreign-key checking. MariaDB locks only the staged page's parent nodes
before starting the merge statement, preventing its concurrent parent-update
serialization failure. MySQL relies on its normal FK checks without this extra
prelock, which increased ingestion latency there. InnoDB enforces all node/batch foreign keys;
foreign-key checks are never disabled. The database selects at most one
expired month per job and, after both retained windows have been paged, checks
their complete aggregates again under the candidate catalog lock before
retirement. A changed historical bucket restarts backfill, not retirement.
Runtime receives no write privileges on progress or staging tables. Calls do
not create tables, commit, or bypass the caller's fence. The capability rejects
autocommit calls before reading or mutating tables: its private savepoint probe
only succeeds in a transaction. The savepoint name
`ocservia_telemetry_batch_tx` is reserved for this routine.

This is a bounded write/transaction design, not a claim that source aggregation
has constant read cost. Source queries and final verification still read the
retained dataset. Fresh functional and four-workload performance results are
required; the earlier table is not evidence for this changed candidate.

Short rollup metrics (at most 255 bytes) use a generated binary metric and a
native unique index over the complete `(node_id,bucket_at,short_metric)` tuple.
Longer metrics map to NULL in that index and retain the original guarded,
full-byte side-table constraint. No unique prefix or hash replaces equality.
Short metrics can therefore use native upsert without per-row side-table SQL;
the long-metric path keeps the existing full-key merge. Unchanged natural keys
do not rebuild their side-table receipt. These changes are telemetry-only;
node/workspace/identity uniqueness triggers are unchanged.

Raw insertion resolves each month once per call and batches at most 128 samples
per statement. Only duplicate-key failures fall back to the existing ordered,
first-sample-wins behavior; FK/CHECK failures, deadlocks and serialization
failures are not suppressed. The MySQL/MariaDB pool retains four idle
connections for the four-workload model; its maximum remains twenty.

### Bounded Transaction Measurements (2026-09-13)

The following replaces the failed whole-transaction measurements above for the
single-node, six-metric acceptance workload. Tests ran sequentially on BuildServer
against the actual revision manifests, with no diagnostic SQL replacement,
retries, race instrumentation, or concurrent task-owned builds. MySQL and
MariaDB revision 26 was authored against both supported historical roots.

| Metric | MySQL 8.4.10 | MariaDB 12.3.2 | PostgreSQL 17 | PostgreSQL 18 |
| --- | ---: | ---: | ---: | ---: |
| Ingestion P95 / P99 (ms) | 22.21 / 30.02 | 13.07 / 16.67 | 5.36 / 9.21 | 4.69 / 7.78 |
| Raw query P95 / P99 (ms) | 7.83 / 12.50 | 7.02 / 8.04 | 2.83 / 5.06 | 2.21 / 3.56 |
| First catch-up (s) | 4.351 | 2.387 | 0.250 | 0.219 |
| Normal maintenance P95 / max (ms) | 243.13 / 243.13 | 139.65 / 139.65 | 37.62 / 37.62 | 32.54 / 32.54 |
| Sampled lock P95 / max upper bound (ms) | 23.56 / 60.00 | 9.11 / 9.11 | 9.11 / 9.11 | 10.27 / 10.27 |
| Maximum observer gap (ms) | 9.96 | 9.11 | 9.11 | 10.27 |
| Allocated bytes before / after 10 fixed runs | 43,433,984 / 43,433,984 | 39,038,976 / 39,038,976 | 16,283,315 / 16,283,315 | 16,774,847 / 16,774,847 |
| Rollup rows before / after 10 fixed runs | 12,972 / 12,972 | 12,972 / 12,972 | 12,972 / 12,972 | 12,972 / 12,972 |

All four runs passed the unchanged thresholds, including zero deadlocks,
timeouts or business failures and zero measured allocation growth. Each run
used 2,016 recent samples and 4,464 outage samples, followed by 200 six-sample
ingestion transactions, 200 raw queries, 200 real node updates and 11 concurrent
maintenance calls. Lock figures are conservative sampled bounds, not exact
server event percentiles; no observed episode is reported as zero latency.
This measures the database-store paths, not HTTP/TLS end-to-end latency or an
arbitrary higher-volume workload. The MySQL ingestion/query/catch-up margins
are modest; these results must not be extrapolated to larger deployments.

Failed intermediate candidates remain evidence, not discarded retries:
MariaDB without page-parent prelocking returned a serialization failure;
adding that prelock to MySQL raised ingestion P95/P99 to 26.71/51.11 ms and
failed. The final engine-specific manifests address the different locking
behavior. Logs are retained in `.cache/pr201-performance/`:
`mysql-performance-engine-final.log`, `mariadb-performance-parent-lock.log`,
`pg17-performance-final.log`, and `pg18-performance-final.log`.

BuildServer also passed the four-engine history and runtime service regressions
with `-race`, MySQL/MariaDB privilege, long-key, bulk constraint, legacy migration
and interrupted-retirement recovery tests, database boundary/migration metadata
tests, and Controller compile-all. The batch unit tests verify fencing on every
commit and failure without automatic retry.

All six complete TLS/Agent E2E runs passed on this candidate, including committed
scheduler maintenance: MySQL all/split (203.59/174.75 s), MariaDB all/split
(159.29/161.09 s), and PostgreSQL 18 all/split (126.87/128.61 s). These use the
real CLI, independent identities, TLS relays, root attestation, certificate
lifecycle and user/group convergence, not mocked transports. Their logs are in
the corresponding `e2e-<engine>-<mode>/` evidence directories.

Latest-head Basic CI still must be confirmed after pushing this correction.
These results are not merge approval; PR #201 remains Draft. The two unrelated
API fixture `42601` failures disclosed above remain unchanged and are not
represented as a passing full API integration package.
