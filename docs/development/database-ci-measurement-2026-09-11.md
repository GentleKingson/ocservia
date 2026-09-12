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

## Follow-up: routing and phase diagnosis

The main-push routing regression is corrected: a push upgrades database scope
only when path classification already selected database checks. Documentation,
Web and Rust-only pushes keep their database jobs skipped; a mixed Web/API
push still selects full database acceptance. The existing classifier suite,
including these push cases, and shell syntax checks passed on BuildServer.
Business-test/testdata changes still force full scope; no skip list was widened.

A focused, paired comparison used the same baseline and optimized source
commits above on BuildServer (Linux aarch64), not GitHub's amd64 runners.
Each of the following existing tests ran baseline then optimized, sequentially,
in a fresh MySQL container. MySQL used the same pinned digest as CI; Go used
1.26.6-bookworm, digest
`sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36`.
Both race-instrumented test binaries were compiled before timing with shared
module/build caches. Each invocation used count 1, verbose output and a
10-minute focused-test timeout. All six invocations and both sets of four
outbox subtests passed.

Identical temporary timing logs were added to `fixture` and `migrateFixture`
in both exported source copies, not production or repository code. Create
includes connections and grants; body runs from completed migration to the
fixture's cleanup callback (including later test cleanup callbacks). The
initialization test body includes its intentional migration revalidation.
Drop measures DROP DATABASE; total also includes connection closes and logging.
Outbox values are sums of its four independently initialized subtests.

| Test | Source | Create (s) | Migrate (s) | Body (s) | Drop (s) | Total (s) |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| Initialization | baseline | 0.026 | 47.907 | 2.077 | 1.403 | 51.414 |
| Initialization | optimized | 0.024 | 47.819 | 2.055 | 1.361 | 51.260 |
| Approval | baseline | 0.024 | 47.574 | 0.631 | 1.408 | 49.638 |
| Approval | optimized | 0.023 | 48.171 | 0.630 | 1.395 | 50.222 |
| Outbox (four scenarios) | baseline | 0.125 | 183.540 | 3.605 | 5.145 | 192.419 |
| Outbox (four scenarios) | optimized | 0.130 | 184.701 | 3.648 | 5.074 | 193.559 |

Migration accounts for about 96% of approval time and 95% of outbox time.
The four outbox migrations alone total 183.54s / 184.70s, while the test bodies
sum to 3.61s / 3.65s. The paired totals differ by less than 1.2%; the multiple-fold
CI slowdown was not reproduced in these samples. This local result neither
identifies the CI resource bottleneck nor proves that a full CI run will pass.
Performance Schema top-20 digest samples were also collected; expensive entries
included ALTER TABLE statements. These truncated samples are not a complete
accounting of SQL time and do not establish a CPU/storage/lock-wait cause.

No shared-database rewrite was made: outbox dispatch scans eligible work across
nodes, so unique workspace/node IDs alone would not isolate its four scenarios.
Any follow-up reuse needs explicit isolation of outstanding dispatch work,
without weakening the existing commit-loss assertions. Full MySQL acceptance
remains outstanding. No new Basic CI run, timeout change or merge was performed.
Temporary source copies, binaries, caches, certificates and test containers were
removed from BuildServer after the results were captured.

## Follow-up: explicit automatic critical gate

This follow-up supersedes the routing policy above, not its measurements or
failure facts. PR and main now use path-selected `regression`; manual dispatch
and unset script scope use `full`. This is a reduction in automatic coverage,
not a same-coverage performance optimization. It does not resolve the earlier
full MySQL timeouts and does not establish release readiness.

Source: branch `codex/database-ci-scope`, HEAD
`6cab377cac2b3f4b5275203a084090872a578606`, plus the uncommitted critical-gate
changes. At entry, `origin/main` was
`58a0734773159b78edf6034a2d89f935792fa084`; the branch was two commits ahead,
with 12 changed files in the three-dot diff. Four preexisting dirty files
(the two CI documents, classifier and classifier test) were preserved.
No production Go code, migration SQL, historical checksum, dependency pins,
security/G6/release workflow or repository settings were changed by this task.
The selected manifest SHA-256 is
`efc4ea511d725b9da849341564f5c3568d844fcd7941db0b6c8f7940a42dd8e0`.

Validation runs on BuildServer (Linux aarch64, Docker 29.8.0) in a task-private
`/var/tmp` directory and disposable database containers. The tooling container
uses Go 1.26.6-bookworm at the same digest recorded above, GCC 12.2.0,
`CGO_ENABLED=1`, jq 1.6 and Ruby 3.1.2. Repository Go environment/cache paths
are retained inside the isolated copy. Database images and script timeouts
are unchanged. Database runs are sequential, sharing only compilation/module
caches; every script provisions fresh database containers.

Environment/setup failures are not database passes:

- Native `bootstrap.sh go-test` initially failed because the existing Linux
  aarch64 branch does not set `go_platform`. Supplying `linux-arm64` explicitly
  validated the pinned toolchain without changing bootstrap code.
- The real, database-free subtest fixture caught a double-escaped regex caused
  by TSV encoding. It was fixed before database tests ran; a PostgreSQL entry
  attempt stopped at this self-check (2.29s, exit 5).
- A native PostgreSQL 17 attempt prepared current structure but stopped before
  its Go tests because no host C compiler was available (74.37s, exit 2).
  Subsequent runs use the disposable tool container, not disabled race checks.
- The first MySQL script run passed all 19 required database entries but failed
  its final configuration package tests (690.87s, exit 1). Their existing
  secure-key fixtures reject a world-writable source-directory ancestor.
  Only the disposable tool container's `/var/tmp` was tightened to mode 0755;
  the host's `/var/tmp` remained 1777 and production validation was unchanged.
  The configuration package then passed independently, before restarting the
  complete MySQL critical script. The failed script run is not a PASS sample.

The actual critical inventory and fixture dependencies are listed in
[Basic CI](github-actions.md). MySQL/MariaDB no longer execute the whole
database package minus history: selected top-level tests run as one package
batch, and selected disconnect/Outbox/fencing children have exact own-result
requirements. PostgreSQL uses current structure without building pre-34 or
executing its broad database package tree. The original full required entries
are retained exactly, apart from renaming their former `backend-mysql-regression`
inventory to `backend-mysql-current` to avoid confusing it with the new gate.

Successful critical runs (wall time includes setup/build/pulls as encountered,
not only test bodies):

| Backend | Scope | Wall seconds | Required entries | Actual top-level / child passes | Result |
| --- | --- | ---: | ---: | --- | --- |
| PostgreSQL 17 | regression | 205.76 | 19 | 11 / 10 | PASS |
| PostgreSQL 18 | regression | 76.65 | 19 | 11 / 10 | PASS |
| MySQL | regression | 673.71 | 19 | 13 / 9 | PASS |
| MariaDB | regression | 505.98 | 20 | 14 / 9 | PASS |

These are single BuildServer samples with different cache/pull costs, not
GitHub Actions measurements or a stable speedup ratio. Required-entry totals
include explicit child requirements; actual-pass totals also include their
parent events. The guard's synthetic self-test summaries are not counted as
database evidence. PostgreSQL additionally passed shell assertions for current
and repeat migration, permissions, compatibility and checksum rejection.
All four successful invocations exited 0, with no failed or skipped database
test events. MySQL/MariaDB also passed the unchanged final configuration
package checks (not included in the database event counts above). MariaDB's
additional required case is `TestRealQueryRowPoisonsBeforeScan`; MySQL does not
select it. Successful runs took place on 2026-09-11 UTC, spanning midnight into
2026-09-12 in Asia/Shanghai.

Full entry/coverage verification is static and contractual only: the manifest
comparison retained every prior required entry; the script contract checks
retain unfiltered MySQL/MariaDB full-package calls, full business groups,
PostgreSQL pre-34 setup and the PostgreSQL 17/18 upgrade-leg difference.
No four-backend full run or new full measurement was started. Full database
acceptance remains unverified; earlier MySQL full timeouts remain unresolved.

Final validation: `test-ci-relevance.sh`, `test-required-go-tests.sh`,
`test-bootstrap-profiles.sh`, `docs-check.sh`, Bash syntax checks and
`git diff --check` passed on BuildServer. The original full inventory comparison
also passed. The Basic CI Result predicate is unchanged, including rejection
of missing, cancelled, failed or unexpectedly skipped jobs. No new GitHub
Actions run was triggered, and these local results are not Actions results.

Database containers and their volumes, fixture processes, certificates and
temporary directories were cleaned. Logs were copied back as local evidence;
the task tooling container and isolated BuildServer source/cache directory
were then removed without global Docker cleanup. At validation completion, the
change was uncommitted and unpushed on the same branch. The critical gate is independently validated;
full database acceptance is explicitly not claimed.
