# Full database CI sharding: 2026-09-12

## Baseline and scope

Fetched `origin/main` before starting: `76131a187ea542df0960380f319267494b0edb24`.
The working tree was clean. Task branch: `codex/full-database-ci-sharding`.
Implementation and all database measurements use candidate
`7235982b4f3bb060aa2fd95c16e5249f14775d6e`, tree
`760d99eb37e3b027f8bccc92f8d770e9af9b1820`. The BuildServer source snapshot
has exactly this Git tree. The subsequent measurement-document commit changes
no tested implementation.

Previously MySQL/MariaDB full ran one whole-package Go invocation (60 minutes),
then coordination (10 minutes), authentication (10 minutes), telemetry
(5 minutes), and final configuration checks. Actions allowed 75 minutes per
database job. Those timeout values, race detection, count 1, database image
digests, TLS, privileges and production/migration code are unchanged.

The full required manifest is unchanged: current 54, history 23, audit 6.
One current entry is MariaDB-specific, giving MySQL 82 and MariaDB 83 total
database requirements. Current-full validates current plus audit (59/60);
history validates 23. Default all must pass both guards before business checks.
Replaying the actual default-all JSON through the unchanged `backend-mysql-full`
guard also passed all 82 MySQL requirements.

The history expression comes from the existing manifest selector. Current runs
the whole package with that exact top-level expression as `-skip`; history
uses `-run`. Therefore their intersection is empty and their union is the
original package test set. Current is not a positive required-test selection:
new unregistered ordinary tests still run. Coordination, authentication,
telemetry and configuration checks run once, only in current/all.
Actual GitHub logs confirm 85 current and 23 history top-level tests per
engine: union 108, intersection zero, exactly matching the names returned by
BuildServer's `go test -race -list .` for the package. MySQL current passes 62 and intentionally skips 23; MariaDB
passes 63 and intentionally skips 22. Both history shards pass all 23.

PR/main routing, four automatic regression runners and critical inventories
are unchanged. PostgreSQL 17/18 full, pre-34, rollback/forward-only, permissions,
checksums and the additional PostgreSQL 18 upgrade leg are unchanged. Only
dispatch adds the two history runners. The required context remains
`Basic CI Result`, requiring both database matrices on dispatch and a skipped
history matrix on automatic events.

## BuildServer

Runs are sequential on Linux aarch64, with fresh task-private database
containers, Docker-assigned loopback ports and certificates. The disposable
tool container uses Go 1.26.6-bookworm at digest
`sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36`,
CGO/race enabled, jq and Ruby. Only compilation/module caches are shared.
Times include setup, compilation as encountered, business checks and cleanup.
All rows use the candidate SHA above; timestamps below are UTC.

| Engine/part | Start | End | Wall seconds | Required DB / business | Actual DB top / child passes | Result |
| --- | --- | --- | ---: | --- | --- | --- |
| MySQL/current, initial | 01:33:08 | 02:23:09 | 3001 | 59 / 23 | 62 / 91 | FAIL: final configuration fixture ancestry |
| MySQL/history | 02:23:09 | 02:43:19 | 1210 | 23 / 0 | 23 / 4 | PASS |
| MariaDB/current | 02:43:19 | 03:18:20 | 2101 | 60 / 23 | 63 / 91 | PASS |
| MariaDB/history | 03:18:20 | 03:32:57 | 877 | 23 / 0 | 23 / 4 | PASS |
| MySQL/default all | 03:32:57 | 04:40:52 | 4075 | 82 / 23 | 85 / 95 | PASS |
| MySQL/current, corrected environment | 04:40:57 | 05:29:24 | 2907 | 59 / 23 | 62 / 91 | PASS |

Business required counts comprise coordination 18 and authentication 5;
their actual JSON passes are 6 top-level and 17 child tests per current/all.
Telemetry is separately verbose and passed its one top-level workflow test.
Final configuration checks also passed in successful current/all rows.
Required skips/failures are zero. Whole-package current retains intentional
non-required authoring/crash-helper skips (22); MySQL also has its expected
MariaDB-only skip. History has no skips. No skipped helper is counted as a pass.

The initial current database binary passed in 2348.218 seconds, not 60 minutes;
coordination, authentication and telemetry also passed. Its final configuration
tests correctly rejected group-writable source-directory ancestors created by
archive extraction. Only directories in the disposable source copy were made
non-group/world-writable, and the existing configuration tests then passed
independently. No safety assertion was changed. This failed row is retained,
not relabeled as a successful shard. The standalone current rerun passed in
48m27s, using the same candidate SHA and a fresh container. An earlier harness setup attempt lacked
`/usr/bin/time` and exited 127 before starting any database tests; it is not
acceptance evidence. A long-lived SSH connection reset did not stop the
container's measurement process; it was inspected and awaited, not restarted.

Default all's two database binaries took 2348.752 and 1173.983 seconds. Thus
the full command legitimately exceeded one hour without accumulating all
database tests in one 60-minute binary. The initial 50-minute command includes
compilation, business tests and failure diagnostics; its database shard was
39m08s. There is no measured need for a second-level split.

Peak process-tree RSS from GNU time, not database-container peak memory:
MySQL/history 359940 KiB, MariaDB/current 382632 KiB, MariaDB/history 358768 KiB,
MySQL/all 379604 KiB, corrected MySQL/current 380096 KiB. No claim about the CI CPU/storage/lock bottleneck follows
from these figures.

## GitHub full acceptance

One dispatch, no retries: [run 34673865598](https://github.com/GentleKingson/ocservia/actions/runs/34673865598),
candidate SHA above. Created 04:46:02 UTC; Basic CI Result completed 05:01:40.

| Job | Duration | Result |
| --- | --- | --- |
| PostgreSQL 17 full | 4m40s | PASS |
| PostgreSQL 18 full | 4m26s | PASS |
| MySQL current full | 14m51s | PASS |
| MySQL history full | 7m00s | PASS |
| MariaDB current full | 15m15s | PASS |
| MariaDB history full | 8m20s | PASS |
| Basic CI Result | 4s | PASS |

Docs, Go, Rust and Web also succeeded. **Full Database Acceptance PASS** applies
to this candidate and this completed run, not merely to regression or a subset.

## Performance

Historical samples remain in the [previous measurement record](database-ci-measurement-2026-09-11.md).
In particular: old MySQL full timed out twice at 60 minutes; its cause was
unconfirmed; paired BuildServer samples did not reproduce the multiple-fold
GitHub slowdown; migration dominated the measured ordinary-test fixtures.
None of those failed results or conclusions is overwritten here.

| Measurement | Historical successful full | Historical failed full, attempt 1 | New full |
| --- | --- | --- | --- |
| Workflow wall | 26m56s | 61m36s, FAIL | 15m38s |
| MySQL whole/current | 23m46s | 61m15s, timeout | 14m51s current |
| MySQL history | Included above | Included above | 7m00s |
| MariaDB whole/current | 26m29s | 50m16s | 15m15s current |
| MariaDB history | Included above | Included above | 8m20s |
| Sum of all job durations | 66.95 runner minutes | 128.42 runner minutes | 60.63 runner minutes |

Historical sources: [successful full](https://github.com/GentleKingson/ocservia/actions/runs/34496829629)
and [failed full attempt 1](https://github.com/GentleKingson/ocservia/actions/runs/34499783273/attempts/1).
Workflow wall includes routing/queue/setup through Basic CI Result. Runner
minutes sum every job's start-to-completion duration, including routing and
summary, without billing rounding; they are not workflow wall time.

Against the successful full sample, observed wall time fell 11m18s (42.0%)
and summed runner time fell 6m19s (9.4%). This is an equal-full-scope historical
comparison, not a regression-scope substitute or a controlled paired runner
benchmark. The design reduces critical-path serialization and removes the
single-binary cumulative timeout; it does not remove acceptance work. The
observed runner-minute decrease is not guaranteed by sharding or evidence of
reduced test coverage. Streaming and diagnostics are not credited as speedups.
These are single samples, not stable performance guarantees.

Failure diagnostics retain bounded process-list state, InnoDB status, statement
digest timing, pending metadata locks and file I/O summaries. Process-list SQL
text and digest text are deliberately omitted to avoid printing credentials;
credential-bearing InnoDB lines are filtered. Each query is time-bounded and
diagnostic failure cannot replace the original test exit status. This is not a
new monitoring system or a database-settings change.

## Contract validation

On BuildServer: `test-ci-relevance.sh`, `test-required-go-tests.sh`,
`test-bootstrap-profiles.sh`, `docs-check.sh`, Bash syntax checks for the three
database/guard scripts, and the actual implementation diff's `git diff --check`
passed. An isolated alternate index checked the diff without changing source
files used by database measurements.

The guard fixtures cover inventory union/separation, missing/skipped/failed
requirements, invalid scope/part, unset all and exact business routing,
real slash-separated Go selection, live JSON before exit, original failure
exit code, INT/TERM group cleanup, and a real Go timeout with child cleanup.
The actual workflow predicate is exercised for PR, push and dispatch,
including failed/cancelled/missing/unexpectedly skipped history results.
No production Go, migrations, checksums, database persistence settings,
security/release workflow, rulesets, secrets or environments were changed.
