# Database CI measurement: 2026-09-11

## Method

- Baseline source: `58a0734773159b78edf6034a2d89f935792fa084`.
- Optimized source: `8bc0a4fce87ba50a2ed11a24d50872460cee9fe8`.
- The isolated `codex/database-ci-measurement` branch runs the same Basic CI
  jobs against an explicit source commit. Its manual scope override is not
  part of the implementation branch, `codex/database-ci-scope`.
  Harness commit: `d26c1d36bc9efd031eb12ed885c39909b3376bce`.
- Each run includes docs, Go, Rust, Web, PostgreSQL 17/18, MySQL and MariaDB.
  Matrix entries, race detection, TLS and permission assertions are unchanged.
- Runs were dispatched sequentially. Total time is workflow creation through
  completion of Basic CI Result, including routing, queueing and setup. Job
  time is the job's startedAt through completedAt.
- These are individual samples, not averages or a stable performance promise.
  Regression scope deliberately omits historical acceptance, so it is not an
  equal-coverage comparison with full scope.

## Results

| Measurement | Baseline full | Optimized full, attempt 1 | Optimized regression |
| --- | --- | --- | --- |
| Result | Pass | Fail: MySQL package timeout | Pass |
| Complete Basic CI | 26m 56s | 61m 36s | 18m 56s |
| MySQL job | 23m 46s | 61m 15s (failed) | 16m 07s |
| MariaDB job | 26m 29s | 50m 16s | 18m 31s |
| PostgreSQL 17 job | 5m 21s | 5m 13s | 5m 26s |
| PostgreSQL 18 job | 5m 38s | 5m 39s | 4m 59s |

Sources: [baseline full](https://github.com/GentleKingson/ocservia/actions/runs/34496829629),
[optimized full, attempt 1](https://github.com/GentleKingson/ocservia/actions/runs/34499783273/attempts/1),
[optimized regression](https://github.com/GentleKingson/ocservia/actions/runs/34506292701).

The successful regression run finished 8m 00s earlier than the successful
baseline full run: `(1616 - 1136) / 1616 = 29.7%`. MySQL job time fell by
7m 39s (32.2%), and MariaDB by 7m 58s (30.1%). Both successful runs had
MariaDB on the critical path. This observed reduction applies to regression
scope, not main/manual/database-infrastructure full acceptance.

## Coverage evidence

The database-package logs show 78/79 passing top-level `TestReal*` tests on
baseline MySQL/MariaDB and 55/56 on regression MySQL/MariaDB. The difference
is exactly the 23 tests in `backend-mysql-history`, with no other removed
top-level real-database tests. MariaDB has one additional engine-specific
test; its expected MySQL skip is not counted as a pass.

The regression required-test gates passed 59 MySQL and 60 MariaDB entries,
including four outbox subtests, with zero skipped required entries. Runtime
coordination, authentication and telemetry checks also passed. Both lighter
fixture tests passed: timestamp backfill took 0.01s and logical values 0.04s
on each backend in the regression run.

## Full-scope failure

The initial optimized full run is retained as a failed sample, not a speedup.
MySQL reached the unchanged 60-minute package timeout while
`TestRealSchemaTextRegex` was still initializing its database through
`migrateFixture` and `applyRevision`. That test had run for about 50 seconds;
it did not itself consume the full hour. The timeout occurred before either
of the two changed lightweight fixtures executed.

Many unchanged tests were slower throughout the run. For example,
`TestRealApprovalControllerWorkflow` took 15.09s in the baseline and 42.04s
in this attempt; `TestRealOutboxCommitDisconnect` took 57.90s and 260.97s.
Both used the same Ubuntu image version, `20260907.300.1`. The available
logs do not identify the CPU, storage or other cause of the slowdown.
MariaDB completed full scope, including all 83 required entries. No timeout,
production migration, checksum, repair rule or safety check was weakened.

The [MySQL retry](https://github.com/GentleKingson/ocservia/actions/runs/34499783273/attempts/2)
also failed at the unchanged 60-minute package timeout. The job took 61m 30s,
including setup and cleanup. It had reached `TestRealTelemetryHistoryWorkflow`,
which had run for about 50 seconds. This was a retry of the failed job at the
same source commit, not a fresh measurement of the entire workflow. No timing
from that retry replaces the failed first attempt in the table.

Full MySQL acceptance remains unverified. The daily-scope improvement is
measured, but the optimization is not fully accepted and should not be merged
on the strength of the regression run alone. These two failures are not enough
to attribute the slowdown to a particular runner resource or code change.
