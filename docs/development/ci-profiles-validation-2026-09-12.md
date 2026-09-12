# Quick / Full CI profiles: 2026-09-12

## Baseline and candidate

The working tree was clean on `main`. After fetching origin, HEAD and
`origin/main` were `26ddf164fd5dddffc10410c6ed8b72bc0ae66926`.
[PR #197](https://github.com/GentleKingson/ocservia/pull/197) was merged at
2026-09-12 10:27:18 UTC; its final head was
`d0c976640fcf55e5b0c5b68dac4609547e8eb1c1`. The remote
`codex/full-database-ci-sharding` branch no longer exists. The earlier measured
candidate `7235982b4f3bb060aa2fd95c16e5249f14775d6e` remains available as a commit.
Its workflow, scripts, control-plane, Rust, Web and toolchain files are identical
to this baseline; the later changes were documentation only.

This candidate is **uncommitted changes on the baseline above**, not a new
commit SHA. No commit, push, PR, dispatch, settings change or release was made.
Consequently there is no remote candidate on which to run the new profiles.

Read-only GitHub rules inspection found ruleset `20192148` requiring
`Basic CI Result` with strict status checks. The legacy branch-protection
endpoint returned 404; this does not mean the ruleset is absent.

## Minimal implementation

- One `ci.yml`, unchanged path routing and stable Basic CI Result.
- Manual `profile=quick|full`, default full. PR/main always quick; both manual
  modes select every basic component. Routing resolves profile/scope once.
- Quick maps to regression and leaves `DATABASE_FULL_PART` unset. Full retains
  PG17, PG18, MySQL current/history and MariaDB current/history. Database jobs
  depend only on routing, not language or image builds.
- Each matrix unit emits a uniquely named completion output only after success.
  The always-running summary checks all selected jobs and all expected units,
  rejects malformed mode/scope/flags, and requires skipped history in Quick.
  This uses GitHub's documented [matrix output combination](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#jobsjob_idoutputs),
  without artifacts or an API permission increase.
- Unset database scope still defaults to full, but an explicitly empty scope
  now fails like other invalid values. Unset full part still defaults to all.
- No test inventory, Go assertion, dependency pin, database setting, timeout,
  diagnostic query, language command or cancellation policy was weakened.

The reference [authentik setup action](https://github.com/goauthentik/authentik/blob/1183742b1e65ea9d6977e50286236bb83f2491c4/.github/actions/setup/action.yml)
and [CI workflow](https://github.com/goauthentik/authentik/blob/1183742b1e65ea9d6977e50286236bb83f2491c4/.github/workflows/ci-main.yml)
were reread at the supplied commit. Task-specific dependencies, bounded task
matrices and always-running strict aggregation fit this repository. Its large
matrix, image dependency graph, publication and host cleanup were not copied.

## Coverage evidence

Go standard, Rust fmt/check/clippy/workspace tests and all Web checks remain
unchanged, including generated-auth and the required Chromium auth subset.
Quick keeps all four engines, required critical top-level/child checks, race,
count=1, TLS, privileges, authentication, rollback, Outbox, fencing, COMMIT-loss
and telemetry. Full adds existing history and complete business combinations;
Security, Formal G6, full browser E2E, native packages and Release remain
outside Basic CI exactly as before. See the [coverage map](github-actions.md).

On BuildServer, `go test -race -list . ./internal/database/mysql` enumerated
108 current package tests without running database cases. Freshly downloaded
real Go JSON from full run 34673865598 was compared to that inventory and
replayed through the unchanged required-test jq guard:

| Engine | Current / history top-level run sets | Union / intersection | Required current / history / full | Required coordination / auth |
| --- | --- | --- | --- | --- |
| MySQL | 85 / 23 | 108 / 0 | 59 / 23 / 82 PASS | 18 / 5 PASS |
| MariaDB | 85 / 23 | 108 / 0 | 60 / 23 / 83 PASS | 18 / 5 PASS |

Current remains the entire package minus the manifest's history top-level
expression; it is not a positive whitelist. Future ordinary tests therefore
remain in Full. Each history top-level case passed. Current had 62 passes and
23 intentional skips on MySQL, versus 63 passes and 22 intentional skips on
MariaDB. The difference is `TestRealQueryRowPoisonsBeforeScan`, MariaDB-only;
the shared skips are existing authoring/crash helpers, not removed assertions.
Required skips/failures were zero. Full business and final configuration checks
remain current/all only. PostgreSQL 18 retains the extra legacy upgrade leg;
PG17 retains its existing separate coverage boundary.

Quick run 34688487163's real JSON was also replayed through every selected
regression group for each of its four engines. All required run/final-pass
and child guards passed. These replays verify reusable historical evidence,
not execution of the modified candidate.

## Rechecked Actions measurements

Wall time is workflow `created_at` to **Basic CI Result `completed_at`**.
Runner-minutes sum non-skipped jobs' start-to-completion seconds / 60, without
billing rounding. Workflow `updated_at` is sometimes one second later and is
not used. All four samples below succeeded; none ran the modified candidate.

| Run | SHA prefix / scope | Wall | Critical path | Runner-minutes |
| --- | --- | --- | --- | ---: |
| [34663677355](https://github.com/GentleKingson/ocservia/actions/runs/34663677355) | 76131a1 / automatic regression | 6m52s | MariaDB 6m34s | 23.03 |
| [34673865598](https://github.com/GentleKingson/ocservia/actions/runs/34673865598) | 7235982 / manual full | 15m38s | MariaDB current 15m15s | 60.63 |
| [34687742744](https://github.com/GentleKingson/ocservia/actions/runs/34687742744) | d0c9766 / automatic regression | 11m42s | PG17 starts 8m38s after routing completes; executes 2m47s | 22.17 |
| [34688487163](https://github.com/GentleKingson/ocservia/actions/runs/34688487163) | 26ddf16 / automatic regression | 5m32s | MySQL 5m13s | 21.37 |

Across the three heterogeneous automatic samples, median=6m52s and max=11m42s.
For the one sharded Full sample, median=max=15m38s (n=1). These are descriptive
historical figures, not a same-SHA cold/warm cohort, long-term P95 or a promise.
The late PG17 start is visible in job timestamps, but its underlying cause is
unknown; it is not evidence that test compilation or downloads caused the delay.

Step measurements across these runs: Go bootstrap 2-14s, Rust bootstrap 9-10s,
Web bootstrap 12-14s, Chromium installation 20-26s. Full database jobs took
PG17 4m40s, PG18 4m26s, MySQL current/history 14m51s/7m00s and MariaDB
current/history 15m15s/8m20s. Existing detailed migration/body measurements and
both historical MySQL timeouts remain in the [earlier record](database-ci-measurement-2026-09-11.md)
and [sharding record](database-ci-sharding-measurement-2026-09-12.md).
The last test at timeout is not established as its root cause; a later pass
does not prove that the historical slowdown has been fixed.

## Cache decision

No cache layer was added. `env.sh` uses `.cache/go-mod`, `.cache/go-build`,
`.tools/rustup`, `.tools/cargo`, `.cache/npm`, `.cache/xdg` and
`.cache/downloads`, not default HOME locations. These Basic CI runs have no
Actions cache restore/save; logs show module downloads. Repository build/module
caches start cold on the fresh jobs, but runner-image and upstream cache state
are not controlled. No sample is labeled a verified final-candidate cold/warm run.

Go module/build reuse is the first candidate for a future measured comparison,
but restore/save cost and net savings have not been measured. Successful cold
repository-cache samples are already within budget; the observed late start
does not justify speculative caching. Rust/npm/browser caches likewise lack
net-benefit evidence. No database state, keys, certificates, session data or
test success is cached, and no new PR-to-main/release cache trust path exists.
Checksum verification, pinned installs and count=1 remain intact.

## Validation and limits

Validation used `ssh BuildServer` with a task-private source/index/cache at
`/var/tmp/ocservia-ci-profile-stMzWU` and disposable tooling container. The
aarch64 host used the existing Go 1.26.6-bookworm image pinned to
`sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36`,
with GCC/race enabled and container-local jq/Ruby/ShellCheck. No host permissions,
production resources or global Docker cleanup were changed.

Focused commands: `test-ci-relevance.sh`, `test-bootstrap-profiles.sh`,
`test-required-go-tests.sh`, `docs-check.sh`, Bash syntax checks and
`git diff --check`. The profile tests cover defaults, both manual modes,
automatic quick, invalid/empty inputs and scope. Summary tests cover missing
or bad individual completion outputs, failed/cancelled/skipped/missing jobs,
missing routing flags and inconsistent mode/scope. Existing guard tests cover
required-child loss/skip/rename, complement routing, timeout and exit-code cleanup.

The classifier passes ShellCheck. The existing classifier test has an unrelated
SC2155 warning at line 71 (declaration plus command substitution); it was not
silenced or refactored. Tool setup initially hit the local gh log-cache sandbox
restriction, then downloaded logs using a task-private XDG cache. A first
evidence parser run used US-ASCII and failed on log text; UTF-8 replay passed.
Neither setup failure is a database acceptance result.

**Correctness:** focused contract validation and historical coverage replay
pass. Actual matrix-output combination and the new dispatch profiles still
need Actions execution at the final committed SHA.

**Performance:** Quick is not stably within 10 minutes in the observed samples;
Full has one successful sub-30-minute historical sample, not stable proof.
Each profile still needs one explicitly cold and two warm successes on the
same final SHA/configuration. No remote candidate or push authorization exists,
so this evidence cannot be supplied in this task. No full database rerun or
language acceptance rerun was substituted for that missing Actions evidence.
