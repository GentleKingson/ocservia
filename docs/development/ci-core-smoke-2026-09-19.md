# CI core-check implementation and measurements

## Source and scope

Implementation branch: `codex/ci-core-smoke`, based on fetched main
`bdebaabb7e087fc19dbeea48a07c3bb22df69bb0`. The candidate is the commit adding
this record; its resolved SHA is recorded in the task delivery. Nothing was
pushed, merged, released, or changed in branch protection. GitHub Quick/Full
were not dispatched: remote execution of this unpushed candidate is unverified.

Reference main `db5e19487183f2869c3b72b4b6af0fa78a04b46c` and Full sample
HEAD `38ff4ca38b1f60d641c213e895969bc758a28a7e` both have tree
`35098f8d1a7cdcd5595d1b3f6109fd762b18ebfa`. Latest main adds release-package
naming changes, not changes to these CI/database paths.

## GitHub baseline

These are historical successful runs, not candidate results. Wall time is
workflow created-to-updated time. Initial queue is creation to first job start,
not a claim to measure every inter-job scheduling gap. Runner-minutes sum
non-skipped job started-to-completed durations, without billing rounding.

| Run | HEAD | Wall | Initial queue | Runner-minutes |
| --- | --- | --- | --- | --- |
| [Quick 35378189054](https://github.com/GentleKingson/ocservia/actions/runs/35378189054) | `b4f0f78c0cf13d87c4e0f3b4e046e725892bca5e` | 10m46s | 5s | 31.92 |
| [Full 35419297195](https://github.com/GentleKingson/ocservia/actions/runs/35419297195) | `38ff4ca38b1f60d641c213e895969bc758a28a7e` | 29m09s | 40s | 89.47 |
| [Latest inspected Quick 35424125349](https://github.com/GentleKingson/ocservia/actions/runs/35424125349) | `3ab172ea7ee22a7cc8404e6ca57617f2568cdfaf` | 11m04s | 4s | 29.75 |

All three restored module and build caches for Go/current database jobs.
The old Full history jobs had no Go build-cache restore. No successful cold
GitHub baseline was measured here.

Reference Quick MySQL: job 606s, database script 588s. Reference Full MySQL
current: job 1443s/script 1426s; MariaDB current: 1687s/1670s; MySQL history:
679s/673s; MariaDB history: 536s/531s. Full recovery was 40s MySQL and 30s
MariaDB, supporting the decision to retain it.

## Candidate BuildServer results

All commands ran through `ssh BuildServer` on Linux ARM64, Go 1.26.6.
Database services used the workflow's pinned versions/digests. Each measurement
includes service initialization, real tests and cleanup. Build/module caches
and database images were locally warm; they are not GitHub Actions cache hits.
Some commands overlapped other checks on this host, so these are local observed
durations, not a controlled speedup ratio or a simulated parallel CI wall time.

| Profile / entry | Wall seconds | Result |
| --- | --- | --- |
| Quick PostgreSQL 17 | 5.59 | Pass |
| Quick MySQL | 53.94 | Pass |
| Full PostgreSQL 17, including 35 -> 36 | 6.47 | Pass |
| Full PostgreSQL 18, including 35 -> 36 | 6.22 | Pass |
| Full MySQL, including revision 25 -> 26 | 89.49 | Pass |
| Full MariaDB, including revision 25 -> 26 | 82.93 | Pass |
| Full MySQL backup/restore | 34.13 | Pass |
| Full MariaDB backup/restore | 33.34 | Pass |
| Go standard: format, vet and ordinary tests, both modules | 37.33 | Pass |
| Web basic, after dependency setup | 33.30 | Pass |

Local database-entry time totals: Quick 0.99 machine-minutes; Full including
recovery 4.21 machine-minutes. These are **not** total CI runner-minutes.
Candidate GitHub wall time, queue, runner-minutes and successful cold-cache
timings are unavailable. No 3-5 / 8-10 minute remote target is claimed met.
The existing cache identity includes the changed test scripts, so the first
candidate GitHub run will need a new cache identity; compatible warm hits must
be measured separately after the existing main-only writer populates it.
The longest measured retained local entry is MySQL compatibility (89.49s),
primarily two real migration fixtures plus repeat migration validation, not a
fixed sleep or role lifecycle matrix. Rust checks were unchanged and were not
rerun locally; the latest baseline Rust job was 247s and may become the remote
critical path. Only candidate GitHub runs can establish that.

Initial local setup attempts failed on an incomplete reused module cache and
copied macOS UID ancestry. A private module/build cache and task-owned source
directory fixed the environment without weakening production checks. One new
PostgreSQL upgrade fixture omitted required timestamps; it was corrected and
both PostgreSQL units rerun successfully. A cold compilation attempt ended in
the ancestry failure (69.49s), so it is not a successful cold-cache result.

## Failure and routing evidence

- Actual Git diff routing fixtures: docs/Web/Rust do not select databases;
  Controller/API/database/migration changes select Quick; Full emits exactly
  PostgreSQL 17/18, MySQL and MariaDB; unknown diffs fall back to Quick.
- The real wrapper rejects a failing, skipped or nonexistent test. A real
  core invocation with database connection variables removed fails rather
  than accepting its skip. A file counter proves two invocations both run,
  instead of reusing a Go test PASS.
- The workflow's actual result command rejects required jobs that fail,
  cancel, disappear or unexpectedly skip. Workflow assertions disallow
  `continue-on-error`; actionlint passes. GitHub job execution itself was not
  exercised locally.
- Existing manual wrapper selection and signal/timeout cleanup tests pass.
  The fixture compiler now consumes stdin, avoiding a pipefail/SIGPIPE race
  from the old `true` stub. Focused shellcheck passes with sourced files.
- Go standard and Web basic pass. No full race, historical sweep, browser
  suite, G6 or disaster-recovery acceptance was run or claimed.

Raw local command logs are retained in the task workspace's ignored
`.cache/ci-core-smoke-20260919/`; temporary BuildServer fixtures are removed.

## Coverage tradeoff and changed files

- `ci.yml` and `ci-relevance.sh`: two-unit Quick / four-unit compatibility Full,
  no history job, retained short recovery, simpler result gate, path-selected
  tooling checks and distinct Quick/Full concurrency.
- Database entry scripts, `startup_backend_integration_test.go`,
  `database_smoke_test.go` and two `upgrade_smoke_test.go` files: one shared
  current-schema flow, real runtime CLI/API, transactions and one direct
  upgrade per implementation. No production SQL, authorization or migration
  correctness rules changed. Recovery cleanup removes its anonymous volumes.
- `required-go-tests.sh` / `.txt`, `go-check.sh`, `docs-check.sh`,
  `web-check.sh` and focused script self-tests: explicit core entry guard,
  removed 126 redundant unit inventory entries, no CI JSONL totals or nested
  required names, no repeated race compiler probe in the wrapper, and browser
  checks remain opt-in. The jq/deep inventory is retained for its existing
  manual callers, not reused to enlarge Full.
- Usage documentation now distinguishes Full key compatibility from manual
  deep acceptance. Removed default coverage includes role/error matrices,
  broad API/policy/auth regressions, outbox/fencing/disconnect cases, restarts,
  exhaustive history/checksum/drift branches and per-database race. The tests
  remain accessible through existing manual entrypoints; they were not all
  reassigned to Full.

See [Basic CI usage](github-actions.md) for commands and profile details.
