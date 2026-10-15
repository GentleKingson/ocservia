# Basic CI

Start with [Validate a change](testing.md).

A single workflow file, `.github/workflows/ci.yml`, handles pull requests,
pushes to main, and manual dispatches. Manual Release Check also invokes this
workflow as `full-ci` with `profile: full`. Automatic triggers run the Quick
profile, while manual dispatch accepts `quick|full` and defaults to Full. Here,
Full runs current-candidate checks across all supported units without serving
as comprehensive release acceptance, and it leaves the product's database
support scope unchanged.

Target warm-cache wall times (excluding runner queue time) are 3 to 5 minutes
for Quick and 8 to 10 minutes for Full. Those numbers are operational targets,
not measured results or grounds to relax failure criteria; when recording
timings, track queue wait, cold setup, execution wall time, and total job
runner-minutes separately. On a given PR or branch, a new Quick run cancels
older Quick runs, whereas Full uses its own concurrency group and never cancels
Quick.

## Basic checks

| Job | Retained checks |
| --- | --- |
| docs | `scripts/docs-check.sh` only: CRLF, non-empty Markdown, audited Claude settings keys, a few bootstrap/`AGENTS.md` text and link rules (details in [testing](testing.md)); no general link, anchor, SQL or support-fact check, and no `policy-check` |
| go | `go-check.sh standard`: gofmt, vet and `go test -count=1` without race in both `control-plane` and `signer`. The same job also runs the selected CI guard and release self-test suites when routing sets `run_ci_tools` |
| rust | `rust-check.sh`: format, Clippy and workspace tests, only when routing sets `run_rust`. Full additionally runs the seven focused vendored iroh patch tests against the archive lock (`rust/vendor/iroh/OCSERVIA-PROVENANCE.md`) and fails unless all seven ran. The same job also runs the installer and Relay-launcher self-tests when routing sets `run_installers`, in which case `rust-check.sh` does not run unless Rust is also selected |
| web | `web-check.sh basic`: format, lint, types, unit tests, build and generated-client authentication; no browser installation/regression |
| web-browser | Only when routing sets `run_web_browser`: installs Playwright engines and runs `web/test/run-auth-browser.mjs` with the authentication, rollout, form/dialog, password, shell/Overview layout, Nodes/detail and Operations specs against the stubbed production build. Quick runs Chromium `desktop`/`mobile` plus the `firefox` and `webkit` key-spec projects; Full also serves the build through the production Standalone gateway and then the Integrated Edge and gateway (`--gateway=standalone`, `--gateway=integrated`), where those specs fail on any CSP violation and an enforced positive control must be observed. A missing browser or non-declared skip fails the job |
| database-smoke | PostgreSQL 18.x and MySQL 8.4 LTS; Quick runs smoke, Full runs full database acceptance (MySQL as three parallel `DATABASE_SHARD` legs: `mysql-cutover`, `mysql-core`, `services`); both first enforce the SQL artifact window and both check current snapshot equivalence (MySQL Full: once, in `services`). The job keeps this name in Full |
| database-recovery-full | PostgreSQL physical and MySQL logical backup/restore, including snapshot provenance, Full only |
| Basic CI Result | Always checks routing and selected job results; required missing/skipped/failed/cancelled jobs fail |

Database service images stay pinned to exact patches and digests. Each core
database job boots one engine instance and initializes a single schema for
`TestDatabaseCoreSmoke`, which migrates an empty database under the owner
account, starts the real Controller CLI under the runtime account, bootstraps
and signs in a local administrator, and reads the resulting workspace over the
authenticated API. Using a second pooled connection, the test verifies real
transaction commit, rollback, and isolation, then confirms that re-running
migrations leaves the committed workspace intact. It also retains a runtime DDL
denial check so the Controller cannot pass while accidentally running with
owner credentials. The MySQL leg runs this flow with production configuration
and verified TLS.

Both Quick and Full validate current SQL initialization, idempotent retry,
receipt integrity, and forward-only continuation. Separate snapshot checks fetch
the release history, verify that the active tree carries only the two SQL
artifacts per engine, reject stale artifacts, and compare fresh SQL against an
upgrade from the immutable v1.2.0 checkpoint for schema, security, and seed
equivalence. On PostgreSQL, the job also checks the live volume layout and
confirms that legacy layouts are rejected. Selecting Full switches to the full
database suites (including migration failures and backup recovery) without
introducing another database backend or a Controller-version admission window.
Smoke runs use fresh `-count=1` execution, dedicated lifecycle tests keep race
detection enabled, and explicit test inventories fail if any required lifecycle
case is skipped.

When invoked as `required-go-tests.sh --smoke <package> <test>`, the runner
checks only that the named top-level test ran and passed; a missing or skipped
test fails the step. Basic CI does not emit per-subtest inventories, cumulative
JSONL logs, or matrix success summaries. The older manifest checker stays in
the repository only for standalone manual deep-acceptance scripts, and its
redundant unit-test inventory has been removed.

In Quick mode, the MySQL leg runs `TestDatabaseCoreSmoke`,
`TestDatabaseInitializationSmoke`, the enrollment restart check, and the
standalone `database-mysql-snapshot.sh check` equivalence step. The 14
`mysql-snapshot` crash and rejection cases run only under Full
(`backend-mysql-full` in the `mysql-cutover` shard), so pull requests that
touch MySQL `schema.sql`, `upgrade.sql`, or the migration runner should run a
candidate Full workflow before merging. Quick PostgreSQL continues to run the
full `postgres-snapshot` inventory.

Path routing also governs the repository's self-test suites. The CI guard tests
(`guards`: `test-ci-relevance.sh`, `test-required-go-tests.sh`, and
`test-bootstrap-profiles.sh`) run only when their own scripts or shared CI and
toolchain files change. The `release` suite (selected by `ci-tools-check.sh` to
cover release workflows, Controller manifest/smoke/image-security, the Stage-0
installer, build-cache contracts, and secret-scan configuration) triggers only
on release or cache-contract paths, and installer self-tests trigger only on
installer changes. Unknown paths and shared-contract changes enable all of
these suites, whereas manual workflow dispatches omit them. Moving from Quick
to Full does not pull in retired role-lifecycle matrices, restart suites,
outbox/fencing/disconnect harnesses, exhaustive auth/API/policy matrices,
checksum/drift/invalid-config matrices, or historical repair suites.

## Retained workflows

| File | Trigger / purpose |
| --- | --- |
| `ci.yml` | PR/main Quick; manual Quick/Full key checks; called by Release Check as Full |
| `security.yml` | Weekly/manual scans; reusable, and called by Release Check (not by `release.yml`) |
| `release-check.yml` | Manual main Full CI, Security, Integrated Business Smoke + single-instance recovery, Quick Install (Pebble ACME, amd64) |
| `release.yml` | Tag build/image-security/smoke/publish; manual build-only dry-run |
| `real-e2e.yml` | Manual two-hosted-VM enrollment with encrypted token exchange; outside Basic CI and Release Check |
| `release-upgrade.yml` | Manual local Business smoke or Integration diagnostics (workflow name Release Diagnostics) |
| `release-products.yml`, `release-business-diagnostic.yml` | Reusable build/smoke (called by `release.yml`) and native Business probe (called by Release Check and `release-upgrade.yml`); not triggered directly |
| `integrated-network.yml` | Manual or candidate-branch Integrated Network prototype (isolated network or public-fixture probe); not part of Basic CI or Release Check |
| `strip-claude-footer.yml` | PR-body housekeeping only; no validation |

## Independent security checks

`security.yml` runs on Mondays at 03:23 UTC, on manual dispatch, and as part of
manual Release Check on main. Using pinned bootstrap profiles, its four
read-only scan jobs run without production secrets:

- full-history `scripts/security-check.sh`, including its Gitleaks rule
  regression check
- `govulncheck ./...` inside both Go modules (`control-plane` and `signer`)
- `cargo audit` against `rust/Cargo.lock` and
  `deploy/production/relay.Cargo.lock`, paired with
  `cargo deny check advisories` across the Rust workspace
- `npm audit --package-lock-only` against `web/package-lock.json`, including
  development dependencies

Any scan failure fails the Security workflow and, when called from Release
Check, blocks the Release Check result. Because `release.yml` neither invokes
nor queries `security.yml`, tag publication relies on the operator verifying a
passing Release Check beforehand. This workflow does not widen Basic CI or run
live security acceptance, and a passing run reflects only what those scanners
and their current advisory databases can detect in the repository tree, not the
security posture of a deployed environment.

## Path routing

`scripts/ci-relevance.sh` evaluates changed paths using ordered first-match
patterns instead of package or call-graph analysis; see the script itself and
`scripts/test-ci-relevance.sh` for the exact rule order summarized below. Rules
for executable code and deployment configuration sit ahead of the generic
Markdown fallback. As a result, Markdown files under `web/`, `rust/`, `proto/`,
`openapi/`, or the `deploy/` subdirectories (compose, database-e2e,
test-fixtures, production, managed-node, package, systemd, and bootstrap) match
their respective subsystem rules and can trigger web, Rust, database,
installer, or full Quick suites. Markdown elsewhere in the repository,
including `control-plane/` and `docs/`, selects `docs` alone.

| Paths (first match wins) | Quick checks |
| --- | --- |
| Other Markdown, `LICENSE*`, `.claude/settings.json` | docs |
| `web/*`, `scripts/web-check.sh` | web; also web-browser for `web/src/{api,shared,components/ui,components/layout,features/operations}/*`, `App.vue`, `main.*`, login/operations/rollout/nodes/node-detail views, `components/{common,nodes}`, configuration/certificate features, `node-workflow.ts`, `user-password.ts`, the user-policy adapter and `upstream/UserPolicyFields.vue`, `web/e2e/*`, Playwright/Vite config, `index.html`, the browser runner and npm manifests |
| `rust/*` (Dockerfiles aside), `scripts/rust-check.sh` | rust |
| `control-plane/{cmd,migrations,internal}/*`, Go locks and `go.work*`, `deploy/compose`, `deploy/database-e2e`, database/MySQL/PostgreSQL scripts | go + database smoke on both engines |
| Other `control-plane/*`, `scripts/go-check.sh`, `signer/*.go`, `signer/go.mod`, `signer/go.sum` | go |
| Installer implementation/tests (list in the script) | installer self-tests in the `rust` job; `rust-check.sh` runs only if Rust is also selected |
| Release, package, Controller and Agent-upgrade scripts, release workflows, `deploy/{package,systemd,bootstrap,managed-node,production}` | release contract suite + installer self-tests |
| Manual-acceptance harness files (`deploy/real-e2e`, `scripts/real-e2e-*`, `scripts/p1-*`, `scripts/security-acceptance-*`) | release contract suite (names matching earlier rules, such as `*controller*`, also select installers); the harnesses themselves are not run |
| `ci.yml`, `security.yml`, `scripts/ci-*`, `test-ci-*`, required-test selectors, bootstrap tests | CI guard suite |
| `proto/*`, `openapi/*`, `control-plane/gen/*`, `scripts/bootstrap.sh`, `env.sh`, `checksums.txt`, `toolchains.lock` | all Basic flags and both CI suites at Quick scope |
| Any other path (for example non-Go `signer/` files, `.dockerignore`) | all Basic flags and both CI suites at Quick scope |

When a change touches multiple areas, the router takes the union of their
flags. Pull requests diff `base...head`, push events diff `before..head`, and
file renames or deletions evaluate both old and new paths. If a diff is empty,
invalid, unresolvable, from an unsupported event, or contains an unrecognized
path, the router fails closed by enabling every Basic flag and both CI tool
suites while keeping the Quick profile (smoke database scope); path routing
never escalates a Quick run to Full.

Manual workflow dispatches bypass path routing altogether: docs, go, rust, web,
web-browser, and database jobs always run, while the CI-tool suites, installer
self-tests, and Controller cache refresh stay off. A manual Full run expands
the database matrix to four legs (PostgreSQL plus the three MySQL shards) and
adds `database-recovery-full`, whereas a manual Quick run uses two database
legs (PostgreSQL and a single MySQL smoke leg). Controller cache refresh runs
only on pushes to main when routing sets `run_controller_cache`.

## Go caches

Go jobs retain the existing module and build caches alongside locked bootstrap
downloads. The database smoke cache key targets ordinary (non-race) build
objects and falls back to the unit-test build cache when needed; no separate
history job or history cache exists. Only passing pushes on main write new
cache entries (Go jobs save module and unit build caches, and one MySQL leg
saves smoke build objects), while pull requests and manual runs only restore
them. Restoring a build cache never skips executing the real database tests.

## Manual deep checks

You can still run the standalone deep-validation scripts directly in an
authorized, isolated environment that has their required dependencies, though
these entrypoints are distinct from Full CI:

```bash
# In an authorized isolated environment with the required dependencies; expensive opt-in checks:
DATABASE_TEST_SCOPE=full PG_MAJOR=all scripts/database-integration.sh
DATABASE_TEST_SCOPE=full ENGINE=mysql bash scripts/database-foundation-integration.sh
scripts/go-check.sh race
scripts/web-check.sh full
```

Invoking the database scripts without a scope defaults to current-candidate
`full` (whereas CI passes `smoke` for Quick and `full` for Full). Historical
upgrade matrices and the old MySQL history shard have been removed, leaving
current schema initialization, artifact integrity, and interrupted-SQL recovery
in the full suite; `regression` remains manual. Running browser checks locally
requires a separate installation of Playwright Chromium, plus the
`firefox`/`webkit` engines when exercising those projects. Disaster recovery,
complex race conditions, and fault-injection suites remain manual script
entrypoints with no scheduled Actions workflow.

## Required check

`basic-ci-result` publishes the `Basic CI Result` status context. It
passes only when path routing succeeds, every selected job passes, and every
unselected job is skipped; any missing, failed, cancelled, or unexpectedly
skipped job fails the check. Repository administrators must configure GitHub
rulesets to require this context and remove retired status contexts, since
editing workflow YAML does not update branch protection settings.

## Separate acceptance

Single-node recovery runs inside Business Smoke or Integration when invoked
with `run-resilience=true`; Release Check runs this Smoke recovery suite
against one Controller, one database, one dedicated Relay, and the production
Signer. The older G6 workflows and evidence framework have been retired, while
runtime, security, and capacity acceptance scripts and cross-VM enrollment keep
their manual entrypoints.

Passing Basic CI does not certify production readiness, capacity, native
packaging, cross-VM enrollment, full browser E2E, security acceptance, or
license compliance. Those checks have separate workflows or manual scripts, and
`make verify` is a broader local verification target that does not mirror Basic
CI.

### Script-level manual acceptance

Several environment-heavy checks stay outside Basic CI because capacity runs
demand dedicated host resources, native security acceptance requires
systemd/privd/PKI fixtures, and cross-VM enrollment requires two separate Linux
VMs with outbound Internet relay access. Run them manually on appropriate local
or dedicated Linux hosts:

- `make p1-smoke` and `make p1-full` invoke `scripts/p1-resilience-capacity.sh`.
- `scripts/security-acceptance-f1.sh`, `scripts/security-acceptance-f2.sh`, and
  `scripts/security-acceptance-f3.sh` run the live security acceptance phases.
- `scripts/real-e2e-controller.sh`, `scripts/real-e2e-node.sh`,
  `scripts/real-e2e-artifact.sh`, and `deploy/real-e2e` support manual
  two-machine enrollment, including the opt-in hosted `real-e2e.yml`; see
  [Cross-VM enrollment validation](cross-vm-enrollment-validation.md).

Running `make real-e2e-check` only checks Bash syntax across the three real-E2E
scripts; Basic CI does not call it, and it does not inspect workflow files or
execute live enrollment.

## Reproduction

To reproduce the Go unit checks, Quick database smoke, and the Full-only MySQL
backup/restore smoke locally, run the following commands against the candidate
commit inside an authorized, isolated environment prepared according to
[testing](testing.md). Note that this sequence omits docs, Rust, Web, the SQL
artifact-window and snapshot-equivalence steps, the PostgreSQL volume-layout
check, and the full database suites:

```bash
scripts/bootstrap.sh go-test
scripts/go-check.sh standard
DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 scripts/database-integration.sh
DATABASE_TEST_SCOPE=smoke ENGINE=mysql bash scripts/database-foundation-integration.sh
RUN_ID=local-mysql ARTIFACT_DIR="$PWD/.cache/recovery-mysql" ENGINE=mysql bash scripts/i18-mysql-backup-restore-smoke.sh
```

When you have authorization to push or trigger remote workflows, run the
candidate branch on GitHub Actions from the UI or with
`gh workflow run ci.yml --ref <candidate-branch> -f profile=full` (or
`profile=quick`), and record the candidate SHA, cache state, and per-job and
per-step timings. Local runs do not substitute for GitHub CI results on the PR
commit.

## Release workflow

Qualification rules, coverage ownership, and manual diagnostic commands live in
[Release policy](release-checks.md). Within the release workflow, only the
tag-triggered `publish` job receives `contents: write`, `packages: write`, and
access to the `release-publishing` environment, which acts as an approval and
protection boundary without creating GitHub Deployment records. Rerunning a tag
release overwrites assets using `gh release upload --clobber`.

Release Controller jobs cache their image-security scanner binaries by OS,
architecture, and tool/bootstrap identity, but they never cache the
vulnerability database itself: each run fetches fresh advisory data and fails
if the feed is unavailable or invalid. Image scans inspect the built image
archives before smoke tests and registry uploads, whereas Business diagnostic
runs do not perform container image vulnerability scans.

## Optional build caches

Basic CI and Security cache pinned bootstrap downloads and `.tools` directories
keyed by profile, OS, architecture, and tool/bootstrap identity, so ordinary
source edits reuse cached tools until a dependency lockfile or bootstrap script
changes. Web CI and the npm Security scan cache `.cache/npm` (configured by
`scripts/env.sh`) instead of `node_modules`, and Web jobs still run `npm ci` on
every execution.

Rust Clippy and test jobs share `rust/target/debug` under a cache key built
from the Rust toolchain, Cargo manifests and lockfile, build flags, patched
dependencies, and the check script, appending the source revision so new runs
can restore compiled dependencies via prefix match and save updated build
artifacts.

Business and Release workflows keep their tool caches separate from the Agent
compilation cache, while sharing the `rust/target/agent-<arch>-<builder-hash>`
directory and cache identity to rebuild and verify the requested Agent version
on each run. None of the workflows cache runtime state or credentials, and the
per-image, per-architecture BuildKit GitHub Actions caches (`mode=max` with
bounded cold fallback) remain unchanged.
