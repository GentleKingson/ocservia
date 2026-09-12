# Basic CI

> **CI reference.** Contributors should start with [Validate a change](testing.md).

The primary workflow, `.github/workflows/ci.yml`, runs Basic CI on
`pull_request`, pushes to `main`, and `workflow_dispatch`. It uses
GitHub-hosted `ubuntu-24.04` runners, `contents: read`, SHA-pinned checkout,
and no production secrets. A new commit cancels an older run of the same PR.
Main pushes and manual dispatches have run-specific concurrency groups and
do not cancel earlier runs.

## Retained workflows

| File | Workflow | Trigger |
| --- | --- | --- |
| `ci.yml` | Basic CI | PRs, pushes to `main`, manual dispatch |
| `security.yml` | Security Checks | Weekly schedule, manual dispatch, reusable release prerequisite |
| `g6-readiness.yml` | G6 Formal Readiness | Manual dispatch only |
| `g6-harness-core.yml` | G6 Readiness Core (Reusable) | Reusable workflow called by formal G6 |
| `release.yml` | Agent Release Packages | Version tag pushes and manual dry runs |

## Basic checks

One small routing job selects up to five independent checks. Manual full also
starts the complementary database history matrix. There is no runtime-artifact
dependency between workers.

| Job | Command | Bootstrap profile | Coverage |
| --- | --- | --- | --- |
| `docs` | `scripts/docs-check.sh` | none | Line endings, nonempty Markdown, and bootstrap documentation |
| `go` | `scripts/go-check.sh standard` | `go-test` | gofmt, go vet, and ordinary Go tests |
| `rust` | `scripts/rust-check.sh` | `rust-basic` | Format, check, clippy, and workspace tests |
| `web` | `scripts/web-check.sh` | `web` | Format, lint, types, unit tests, builds, generated-client authentication tests, and 12 required authentication browser regressions on desktop Chromium |
| `database-smoke` | `scripts/database-integration.sh` / `scripts/database-foundation-integration.sh` | `go-test` | Four-backend critical regression automatically; PostgreSQL full and MySQL/MariaDB current on dispatch |
| `database-history-full` | `scripts/database-foundation-integration.sh` | `go-test` | MySQL/MariaDB complementary full history, dispatch only |

Go checks retain both existing Go modules, including unit tests for the G6
harness; they do not run G6 acceptance. Rust checks do not run cargo audit,
license policy, native integration, or separate boundary scripts. License
validation remains available through `scripts/license-check.sh`.

Bootstrap versions come from `toolchains.lock`; downloads are verified
against `scripts/checksums.txt`. The Go profile installs only Go and verifies
host jq. The Rust profile installs only Rust, rustfmt, and clippy. Web
bootstrap installs pinned Node/npm and dependencies, with
`npm_config_audit=false` and `npm_config_fund=false` for the entire job,
including npm installation. Ordinary CI does not run `go-race`, `npm audit`,
`cargo audit`, `cargo deny`, or `govulncheck`, nor repository secret scans,
license scans, native ocserv integration, P1 smoke, the full browser E2E matrix,
or G6 smoke. The Web job does run the required authentication browser subset:
12 desktop Chromium regressions from `web/e2e/login.spec.ts` and
`web/e2e/auth-workspace.spec.ts`. It installs Playwright Chromium and its system
dependencies after Web bootstrap and before `scripts/web-check.sh`.

The database matrix retains PostgreSQL 17/18, MySQL and MariaDB. The PostgreSQL
script builds `ocserv-control` itself; only full scope builds its historical
Controller, and only PostgreSQL 18/all runs the additional legacy upgrade leg.
The automatic matrix needs only the router; manual history starts independently.
Neither needs a Rust build or a shared binary artifact.

## Independent security checks

`security.yml` runs weekly on Monday at 03:23 UTC, on manual dispatch, and
from the release workflow against its candidate commit. It reuses pinned
bootstrap profiles to run the full-history `scripts/security-check.sh`
(including its Gitleaks rule regression check), `govulncheck` for both Go
modules, `cargo audit` and `cargo deny check advisories` for the Rust workspace,
and `npm audit` for both npm lockfiles, including development dependencies.
The four scan jobs are read-only and need no production secrets. A failure
blocks release publishing; it does not expand Basic CI or enable live security
acceptance. A successful scan covers these tools and their current databases,
not every possible vulnerability or the state of a deployed service.

## Path routing

`scripts/ci-relevance.sh` emits five execution flags:
`run_docs`, `run_go`, `run_rust`, `run_web`, and `run_database`.
The workflow event alone selects database scope, independently of these flags.
Reason and changed-file count are diagnostic metadata.

| Changed paths | Selected checks |
| --- | --- |
| Documentation, Markdown, license text | docs |
| G6 workflows, actions, scripts, harness, deployment fixtures, and dedicated Rust runtime files | docs |
| Manual P1/security acceptance scripts, real-E2E scripts and their checks, `deploy/real-e2e` | docs |
| Web | web + docs |
| Go sources, module/workspace files, control-plane code and migrations | go + database-smoke |
| Rust workspace | rust |
| Workflows, scripts, shared toolchain files, Makefile | All five basic checks |
| Unrecognized paths | All five basic checks |

Mixed changes use the union of their checks. Documentation-only PRs do
not activate language or database checks. Infrastructure changes, unknown
paths, and unclassifiable diffs conservatively run all five, never acceptance.
G6-specific and script-level manual acceptance paths are the exceptions: they
select only the basic docs check, not acceptance or additional contract checks.

PR routing uses `base...head`, excluding base-only changes after the branch
point. Main pushes use `before..head`. Deletions and both sides of renames
retain their path impact. Manual dispatch, empty diffs, invalid/all-zero or
unresolvable SHAs, and diff failures select all basic checks.

Database jobs retain PostgreSQL 17/18, MySQL, MariaDB, and their existing race,
TLS and permission checks. PR and main pushes use the same path routing and
always select `regression` when database checks are needed. Recognized pure
docs/Web/Rust changes do not start database jobs; mixed changes take the union.
Unknown automatic changes run all basic checks, still with database regression.
Database implementations, migrations, dependencies, tests and CI scripts do not
upgrade scope. Such changes also need focused validation of their direct impact.

`workflow_dispatch` selects `full`. Both database scripts default to `full`
when `DATABASE_TEST_SCOPE` is unset; invalid values fail. Regression is an
explicit selection from the `regression-*` groups in
`scripts/required-go-tests.txt`, not a whole package minus a history blacklist.

| Critical group | Coverage and initialization |
| --- | --- |
| `regression-mysql` | Empty/current migration, repeat migration and checksum refusal; trusted/untrusted TLS; runtime/maintenance permissions; transaction cancellation, rollback and panic cleanup; logical values, identity profiles and audit/RBAC. Each test keeps its own fixture; logical values/transaction probes need only small tables. The snapshot-abort test is MariaDB-only. |
| `regression-postgres` | Transaction cancellation and recovery, cross-store commit/rollback, logical values, identity profiles and audit/RBAC on current structure. The script separately checks current migration, idempotence, privileges, incompatible Controller rejection and checksum corruption. |
| `regression-auth` | Local HTTP login/logout, password changes, session revocation, denied management/self-approval, OIDC identity-store boundaries, and audit/business rollback. Both top-level tests run intact because Safety has assertions outside its children. |
| `regression-oidc` | PostgreSQL authorization-code/PKCE, issuer identity boundaries, invalid-token rejection and session behavior. MySQL/MariaDB retain their backend identity-store coverage, not this PostgreSQL-specific protocol fixture. |
| `regression-outbox` | Atomic intent and ambiguous commit, two workers with durable claim/readback, early and duplicate results. Each selected child owns a disposable current-schema database. |
| `regression-fencing` | Owner expiry while waiting to assert; competing controllers, retained epochs and rejected stale owners. Parent initialization is retained; these children create independent nodes. |
| `regression-disconnect` | MySQL/MariaDB real COMMIT request/response loss during claim, readback and connection discard. Both selected children initialize independently. |
| `regression-telemetry` | Current ingestion, duplicate rejection, rollback, queries, rollups and fenced maintenance; current telemetry tables/months are prepared by the existing fixture. |

The existing required-test guard checks exact run/final-pass events, including
selected children; missing, renamed, failed or skipped required cases fail.
New critical regressions must be added to this manifest. Selection escapes
regex literals and supports either top-level sets or children of one parent,
avoiding cross-product matches between unrelated parents.
See [Go subtest matching](https://go.dev/blog/subtests) for the slash-separated
matching rules. Child-only groups use separate invocations; top-level sets are
batched by package.

Full retains whole-package MySQL/MariaDB tests, the original required inventory
(`backend-mysql-current`, `backend-mysql-history` and audit groups), complete
coordination/authentication combinations, historical upgrades/data conversions
and PostgreSQL pre-34 rollback/upgrade fixtures. PostgreSQL 17 still omits the
additional PostgreSQL 18 upgrade leg. Full coverage has not become an alias for
regression. Critical CI success is not full acceptance or release readiness.

Manual dispatch runs MySQL/MariaDB full as complementary `current` and `history`
jobs on independent runners. The existing four-backend `database-smoke` matrix
runs PostgreSQL 17/18 unchanged and MySQL/MariaDB current; the dispatch-only
`database-history-full` matrix runs MySQL/MariaDB history. Automatic PR/main
regression selection and runner counts are unchanged. `Basic CI Result` requires
both matrices to succeed on dispatch, and history to be skipped automatically.

`DATABASE_FULL_PART=all|current|history` is an internal full-only control for
`database-foundation-integration.sh`; setting it with regression is rejected.
Unset means `all`: current complement, history, coordination, authentication,
telemetry and final configuration checks, in that order. History runs only the
existing `backend-mysql-history` manifest selection. Current runs the whole
package with exactly those top-level tests excluded, not a positive inventory;
new unregistered tests therefore remain in full. The current guard combines
current and audit requirements; the unchanged full guard remains their union
with history. Business acceptance runs only in current/all, never history.
Each package shard retains race detection, count 1 and its 60-minute timeout;
each Actions database job retains 75 minutes. JSON streams live through the
required-test guard. Failed full shards print bounded database diagnostics.
Parallelization can reduce wall time without reducing total runner minutes.
See the [full sharding measurement](database-ci-sharding-measurement-2026-09-12.md)
for coverage evidence, retained failure records and measured runner time.

Before release, run both scripts with `DATABASE_TEST_SCOPE=full` for all four
backends on BuildServer, or manually dispatch the existing Basic CI workflow.
GitHub requires that workflow to exist on the default branch before dispatch;
then select the candidate branch using the UI or
`gh workflow run ci.yml --ref <candidate-branch>`. A branch-only workflow is not
a workaround for this prerequisite. See [GitHub manual workflow documentation](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow).

## Required check migration

`basic-ci-result` publishes the stable **Basic CI Result** check. It succeeds
only when routing succeeds, every selected job succeeds, and every unselected
job is skipped. Failed, cancelled, unexpectedly skipped, or missing results
fail this one summary. No legacy result aggregators remain.

The former required contexts below are historical names, not current jobs.
The replacement summary check is `Basic CI Result`:

- `Backend Integration`
- `Web & Smoke`
- `Quality, Security & Native`
- `G6 Harness Smoke Core / G6 Harness Smoke Result`

Changing workflow YAML does not migrate GitHub rulesets; administrators must
check the ruleset separately. Leaving those old
contexts required will block merging. The workflow does not bypass or modify
branch protection.

## Separate acceptance

G6 runs only through manual `workflow_dispatch` in `g6-readiness.yml`,
which calls `g6-harness-core.yml` with `profile=formal`, the selected
`authority`, and the exact `candidate_sha`. Run it before releases or major
architecture changes. The smoke caller and all seven smoke jobs are removed;
ordinary PRs do not run G6 smoke or formal G6. No replacement G6 check is added.
Formal G6 and release packaging remain separate workflows, not ordinary CI or
Basic CI prerequisites. Runtime/security/capacity acceptance and cross-VM
enrollment are script-level manual acceptance, not GitHub Actions workflows.
There is no separate G6 Rust cache warmup workflow. Formal G6 retains its
BuildKit cache support and can build cold when no cache is available; cache
warmup is not a prerequisite for Basic CI or formal G6. Without advance
warmup, a cold formal build may take longer, but its acceptance checks remain
unchanged.

Basic CI does not claim production readiness, capacity, native package,
cross-VM, full browser E2E matrix, security, or license acceptance. Its required
authentication browser subset is not full browser E2E acceptance. Those scripts
and manual entry points remain available; `make verify` is a broader local
command, not an alias for Basic CI.

### Script-level manual acceptance

These environment-dependent checks are not part of Basic CI: capacity runs
need substantial resources, native security checks need systemd/privd/PKI
fixtures, and cross-VM enrollment needs two distinct Linux VMs and Internet
relay connectivity. Run them manually on suitable local or dedicated servers:

- `make p1-smoke` and `make p1-full` call `scripts/p1-resilience-capacity.sh`.
- `scripts/security-acceptance-f1.sh`, `scripts/security-acceptance-f2.sh`, and
  `scripts/security-acceptance-f3.sh` retain the live security acceptance phases.
- `scripts/real-e2e-controller.sh`, `scripts/real-e2e-node.sh`,
  `scripts/real-e2e-artifact.sh`, and `deploy/real-e2e` remain available; see
  [Cross-VM real E2E validation](real-e2e.md) for manual execution.

`make real-e2e-check` only checks the three real-E2E scripts' Bash syntax. It
does not read workflow files or run live acceptance, and Basic CI does not call it.

## Reproduction

Use the same commit and run these commands on `BuildServer`:

```bash
scripts/test-ci-relevance.sh
scripts/test-bootstrap-profiles.sh
scripts/docs-check.sh
scripts/bootstrap.sh go-test
scripts/go-check.sh standard
scripts/bootstrap.sh rust-basic
scripts/rust-check.sh
npm_config_audit=false npm_config_fund=false scripts/bootstrap.sh web
(
  source scripts/env.sh
  cd web
  npx playwright install --with-deps chromium
)
npm_config_audit=false npm_config_fund=false scripts/web-check.sh
DATABASE_TEST_SCOPE=regression PG_MAJOR=all scripts/database-integration.sh
DATABASE_TEST_SCOPE=regression ENGINE=mysql bash scripts/database-foundation-integration.sh
DATABASE_TEST_SCOPE=regression ENGINE=mariadb bash scripts/database-foundation-integration.sh
```

Job logs contain diagnostics; Basic CI has no artifact upload/download graph.
GitHub checks for the exact candidate commit remain the merge-time authority.

## Release packages workflow

`.github/workflows/release.yml` builds the Agent distribution outside the
primary CI graph. It triggers on `v*.*.*` tag pushes, requiring a lightweight
`vX.Y.Z` tag with a plain SemVer version, and on manual `workflow_dispatch`.
Dispatch always stays a dry run: it never publishes a GitHub Release, writes
to GHCR, or loads the production signing key.

- Dispatch accepts `version` and `arch` (`amd64`, `arm64`, or `all`).
  The default `amd64` builds and smokes only one architecture, avoiding the
  arm64 Agent and Controller build legs for faster feedback. Tag pushes
  always build both amd64 and arm64, regardless of dispatch defaults.
- Agent packages build natively on `ubuntu-24.04` (amd64) and
  `ubuntu-24.04-arm` (arm64), without emulation. Each selected leg builds
  Agent, privd, and upgrader, produces a signed tar archive plus deb/rpm,
  and runs `scripts/release-native-package-smoke.sh` for the candidate's
  deb install/upgrade/removal and rpm install/upgrade/erase scripts.
  Neither published-baseline upgrade smoke runs in the release workflow.
- Tag pushes and `arch=all` dry runs download both package sets and run
  `scripts/validate-release-packages.sh`. This retains package presence,
  signatures, architecture metadata, embedded payload consistency, and
  canonical checksum coverage of packages and versioned bootstrap assets.
  Single-architecture dry runs skip this two-architecture aggregate job;
  their candidate lifecycle smoke still runs.
- Controller builds use the same selected architectures and pinned BuildKit.
  Each leg exports the four first-party images as Docker archives and runs
  `scripts/release-controller-image-smoke.sh` against those exact archives
  on its native runner. Package sets and Controller image archives remain
  uploaded as artifacts. Native smoke diagnostics upload only on failure
  or cancellation, without duplicate baseline diagnostics.
- Build jobs use ephemeral signing keys and source-read permissions only.
  The tag-push-only publish job remains behind the `release-publishing`
  environment with `contents: write` and `packages: write`. It checks
  SemVer and binds the remote tag to the source commit once before
  production writes. Runner environment recording runs only here and is
  non-blocking.
- The publish job loads the built Controller archives, pushes both platforms
  to GHCR, assembles multi-platform indexes, checks both architectures and
  anonymous reads, and generates the platform manifests plus the amd64
  compatibility alias. It re-signs Agent archives with the release key,
  rebuilds native installers with the release trust anchor, validates against
  `AGENT_TRUSTED_KEY_SHA256`, and signs and verifies `SHA256SUMS`.
  The complete package, Controller manifest, and bootstrap asset set is
  uploaded to a draft GitHub Release before publication, without
  `--clobber`. A leftover draft may be recreated; an already-published
  release is not modified, and reruns have no read-only recovery path.
- Release immutability prerequisites, `REPO_ADMIN_READ_TOKEN`, gh release
  verification support checks, image provenance attestations, post-publish
  attestation/immutability verification, and repeated tag binding are not
  required by this workflow. Existing package signatures and installer trust
  verification remain unchanged.

## Deferred native validation

Native systemd/privd/PKI and live relay scenarios remain outside Basic CI.
Pure Rust adapter tests still run in the ordinary Rust workspace suite.
Use the script-level manual acceptance commands for environment-dependent checks.
