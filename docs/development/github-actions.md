# Basic CI

Start with [Validate a change](testing.md).

One `.github/workflows/ci.yml` runs on PRs, main pushes and manual dispatch;
manual Release Check also calls it as `full-ci` with `profile: full`.
Automatic runs use **Quick**. Manual dispatch accepts `quick|full`, defaulting
to **Full**. Full means **current-candidate checks on all supported units**,
not comprehensive acceptance. Product database support has not changed.

Warm-cache goals are 3-5 minutes for Quick and 8-10 minutes for Full, excluding
queue time. They are goals, not measured results or relaxed failure criteria.
Record queue, cold setup, execution wall time and summed job runner-minutes
separately. New Quick runs cancel obsolete Quick runs on the same PR/branch.
Full has a separate concurrency group and cannot cancel Quick.

## Basic checks

| Job | Retained checks |
| --- | --- |
| docs | `scripts/docs-check.sh` only: CRLF, non-empty Markdown, audited Claude settings keys, a few bootstrap/`AGENTS.md` text and link rules (details in [testing](testing.md)); no general link, anchor, SQL or support-fact check, and no `policy-check` |
| go | `go-check.sh standard`: gofmt, vet and `go test -count=1` without race in both `control-plane` and `signer`. The same job also runs the selected CI guard and release self-test suites when routing sets `run_ci_tools` |
| rust | `rust-check.sh`: format, Clippy and workspace tests, only when routing sets `run_rust`. The same job also runs the installer and Relay-launcher self-tests when routing sets `run_installers`, in which case `rust-check.sh` does not run unless Rust is also selected |
| web | `web-check.sh basic`: format, lint, types, unit tests, build and generated-client authentication; no browser installation/regression |
| database-smoke | PostgreSQL 18.x and MySQL 8.4 LTS; Quick runs smoke, Full runs full database acceptance (MySQL as three parallel `DATABASE_SHARD` legs: `mysql-cutover`, `mysql-core`, `services`); both first enforce the SQL artifact window and both check current snapshot equivalence (MySQL Full: once, in `services`). The job keeps this name in Full |
| database-recovery-full | PostgreSQL physical and MySQL logical backup/restore, including snapshot provenance, Full only |
| Basic CI Result | Always checks routing and selected job results; required missing/skipped/failed/cancelled jobs fail |

All database images retain their exact patch/digest pins. Each core job starts
one database service and initializes one schema for `TestDatabaseCoreSmoke`.
That flow migrates an empty database with the owner account, starts the real
Controller CLI with the runtime account, bootstraps and logs in a local
administrator, and reads the written workspace through the authenticated API.
Real transactions check commit, rollback and isolation from a second pooled
connection; a repeated migration must preserve the committed workspace.
Runtime DDL denial is the small retained failure path: it verifies the
Controller is not accidentally tested with owner credentials. MySQL
retains production configuration and verified TLS in this same flow.

Both profiles verify current SQL initialization, identical retry, receipt
integrity and forward-only continuation. Independent checks compare fresh SQL
with the immutable v1.2.0 checkpoint upgrade for schema/security/seed equivalence
and refuse stale artifacts. Database checkout fetches that release history; the
active tree contains only the two SQL artifacts per engine. PostgreSQL also
checks its actual volume layout and legacy-layout rejection. Full selects the
existing full database suites, including migration failures and recovery; it
does not add another backend or a Controller-version admission window. Explicit
required test inventories reject skipped lifecycle cases. Smoke uses fresh
`-count=1` results; dedicated lifecycle checks retain race detection.

`required-go-tests.sh --smoke <package> <test>` checks only the explicit
top-level test's run/final-pass events. A missing or skipped entry fails.
Basic CI has no per-subtest inventory, cumulative JSONL or matrix success
outputs. The old manifest/checker remains only for independent manual deep
acceptance callers; its redundant ordinary-unit inventory has been removed.

Quick MySQL runs only `TestDatabaseCoreSmoke`, `TestDatabaseInitializationSmoke`,
the enrollment restart check and the separate `database-mysql-snapshot.sh check`
checkpoint equivalence step. The 14 `mysql-snapshot` crash/rejection cases run
only in Full (`backend-mysql-full`, `mysql-cutover` shard); PRs changing MySQL
`schema.sql`, `upgrade.sql` or the migration runner should run a candidate Full
before merge. Quick PostgreSQL still runs `postgres-snapshot`.

CI router/wrapper/bootstrap self-tests (`guards`: `test-ci-relevance.sh`,
`test-required-go-tests.sh`, `test-bootstrap-profiles.sh`) run only for their
implementation or shared CI/toolchain changes. The `release` suite (release,
Controller manifest/smoke/image-security, Stage-0 installer, build-cache and
secret-scan-config self-tests, selected by `ci-tools-check.sh`) runs only for
release or cache-contract paths. Installer self-tests run only for installer
changes. Unknown and shared-contract paths enable all of them. Manual dispatch
does not add these unrelated self-tests.
No role lifecycle matrix, restart, outbox/fencing/disconnect, exhaustive
authentication/API/policy suite, checksum/drift/invalid-configuration matrix,
or deep historical repair suite is moved from Quick into Full.

## Retained workflows

| File | Trigger / purpose |
| --- | --- |
| `ci.yml` | PR/main Quick; manual Quick/Full key checks; called by Release Check as Full |
| `security.yml` | Weekly/manual scans; reusable, and called by Release Check (not by `release.yml`) |
| `release-check.yml` | Manual main Full CI, Security, Integrated Business Smoke + single-instance recovery, Quick Install (Pebble ACME, amd64) |
| `release.yml` | Tag build/image-security/smoke/publish; manual build-only dry-run |
| `release-upgrade.yml` | Manual local Business smoke or Integration diagnostics (workflow name Release Diagnostics) |
| `release-products.yml`, `release-business-diagnostic.yml` | Reusable build/smoke (called by `release.yml`) and native Business probe (called by Release Check and `release-upgrade.yml`); not triggered directly |
| `integrated-network.yml` | Manual or candidate-branch Integrated Network prototype (isolated network or public-fixture probe); not part of Basic CI or Release Check |
| `strip-claude-footer.yml` | PR-body housekeeping only; no validation |

## Independent security checks

`security.yml` runs weekly on Monday at 03:23 UTC, on manual dispatch, and
from manual Release Check on main. It reuses pinned
bootstrap profiles to run the full-history `scripts/security-check.sh`
(including its Gitleaks rule regression check), `govulncheck ./...` run from
`control-plane` (the separate `signer` Go module is not scanned by this
workflow), `cargo audit` for `rust/Cargo.lock` and
`deploy/production/relay.Cargo.lock` plus `cargo deny check advisories` for the
Rust workspace, and `npm audit --package-lock-only` for the single
`web/package-lock.json`, including development dependencies.
The four scan jobs are read-only and need no production secrets. A failure
fails Security and so the Release Check result; `release.yml` neither calls nor
checks Security, so tag publication is gated only by the operator confirming a
passing Release Check. Security does not expand Basic CI or enable live security
acceptance. A successful scan covers these tools and their current databases,
not every possible vulnerability or the state of a deployed service.

## Path routing

`scripts/ci-relevance.sh` uses first-match path rules, not function dependency
analysis; the script and `scripts/test-ci-relevance.sh` are authoritative and
this table is a summary. Specific executable and configuration rules precede
the generic Markdown rule, so Markdown under `web/`, `rust/` and the `deploy/`
trees (compose, database-e2e, test-fixtures, production, managed-node, package,
systemd, bootstrap), or under `proto/` and `openapi/`, is claimed by those
rules and can run web, Rust, database, installer or all suites instead of
`docs`. Markdown elsewhere (including under `control-plane/` and `docs/`)
selects `docs` only.

| Paths (first match wins) | Quick checks |
| --- | --- |
| Other Markdown, `LICENSE*`, `.claude/settings.json` | docs |
| `web/*`, `scripts/web-check.sh` | web |
| `rust/*` (Dockerfiles aside), `scripts/rust-check.sh` | rust |
| `control-plane/{cmd,migrations,internal}/*`, Go locks and `go.work*`, `deploy/compose`, `deploy/database-e2e`, database/MySQL/PostgreSQL scripts | go + database smoke on both engines |
| Other `control-plane/*`, `scripts/go-check.sh`, `signer/*.go`, `signer/go.mod`, `signer/go.sum` | go |
| Installer implementation/tests (list in the script) | installer self-tests in the `rust` job; `rust-check.sh` runs only if Rust is also selected |
| Release, package, Controller and Agent-upgrade scripts, release workflows, `deploy/{package,systemd,bootstrap,managed-node,production}` | release contract suite + installer self-tests |
| Manual-acceptance harness files (`deploy/real-e2e`, `scripts/real-e2e-*`, `scripts/p1-*`, `scripts/security-acceptance-*`) | release contract suite (names matching earlier rules, such as `*controller*`, also select installers); the harnesses themselves are not run |
| `ci.yml`, `security.yml`, `scripts/ci-*`, `test-ci-*`, required-test selectors, bootstrap tests | CI guard suite |
| `proto/*`, `openapi/*`, `control-plane/gen/*`, `scripts/bootstrap.sh`, `env.sh`, `checksums.txt`, `toolchains.lock` | all Basic flags and both CI suites at Quick scope |
| Any other path (for example non-Go `signer/` files, `.dockerignore`) | all Basic flags and both CI suites at Quick scope |

Mixed changes take the union. PRs use `base...head`; pushes use `before..head`.
Deletions/renames retain both affected paths. Invalid, empty, unresolvable or
unsupported-event diffs and unknown paths fail closed by enabling every Basic
flag, but the profile stays Quick (smoke database scope); routing never
upgrades Quick to Full. Manual dispatch ignores paths: docs, go, rust, web and
database always run, with no CI-suite, installer or Controller-cache flags.
Manual Full selects four database matrix legs (PostgreSQL plus three MySQL
shards) and the Full-only `database-recovery-full` job; manual Quick selects
only two (PostgreSQL and one MySQL leg). Controller cache refresh runs on main
pushes only, when `run_controller_cache` is set.

## Go caches

Keep the existing module/build caches and locked bootstrap downloads.
The database smoke key uses ordinary (non-race) objects and can fall back to
the existing unit build cache. Only successful main pushes save: Go writes
module/unit caches and one MySQL unit writes smoke build objects. PR/manual
runs restore only. No history job remains to need its own cache.
Cache hits never bypass real database tests.

## Manual deep checks

The original independent entrypoints remain available, but are **not Full CI**:

```bash
# In an authorized isolated environment with the required dependencies; expensive opt-in checks:
DATABASE_TEST_SCOPE=full PG_MAJOR=all scripts/database-integration.sh
DATABASE_TEST_SCOPE=full ENGINE=mysql bash scripts/database-foundation-integration.sh
scripts/go-check.sh race
scripts/web-check.sh full
```

The database scripts default to current-candidate `full` when invoked without a
scope. Historical database upgrade matrices and the MySQL history shard have
been removed; current initialization, content integrity and interrupted-SQL
recovery remain in the full suite. `regression` remains manual-only. Normal CI
passes `smoke` for Quick and `full` for Full.
Browser checks require Playwright Chromium installed separately. Disaster
recovery, complex races and fault injection remain in their existing manual
deep scripts; no scheduled workflow was added.

## Required check

`basic-ci-result` publishes `Basic CI Result`: routing and every selected job
must succeed; every unselected job must be skipped. Missing, failed, cancelled
or unexpectedly skipped results fail. Administrators must configure GitHub
rulesets to require this context and remove retired contexts; workflow YAML
does not change branch protection.

## Separate acceptance

Selected single-node recovery runs in Business Smoke or Integration with
`run-resilience=true`; Release Check runs it in Smoke with the production
Signer, on one Controller, one database and one dedicated Relay. The old G6 workflows and evidence framework are retired.
Runtime/security/capacity acceptance and cross-VM enrollment retain their
existing manual entry points.

Basic CI does not claim production readiness, capacity, native package,
cross-VM, browser E2E, security, or license acceptance. Those scripts
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
  [Cross-VM enrollment validation](cross-vm-enrollment-validation.md) for manual execution.

`make real-e2e-check` only checks the three real-E2E scripts' Bash syntax. It
does not read workflow files or run live acceptance, and Basic CI does not call it.

## Reproduction

Run against the same candidate source in an authorized isolated environment
with the dependencies described in [testing](testing.md). These commands
reproduce only the Go, Quick database smoke and MySQL recovery steps (the last
is a Full-only job); they omit docs, Rust, Web, the SQL artifact-window and
snapshot-check steps, the PostgreSQL volume-layout check and the Full database
scope:

```bash
scripts/bootstrap.sh go-test
scripts/go-check.sh standard
DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 scripts/database-integration.sh
DATABASE_TEST_SCOPE=smoke ENGINE=mysql bash scripts/database-foundation-integration.sh
RUN_ID=local-mysql ARTIFACT_DIR="$PWD/.cache/recovery-mysql" ENGINE=mysql bash scripts/i18-mysql-backup-restore-smoke.sh
```

With push/remote-run authorization, select the candidate branch in Actions or
run `gh workflow run ci.yml --ref <candidate-branch> -f profile=full`.
Use `profile=quick` for the other profile. Record the actual candidate SHA,
cache state and job/step timings. Local validation results are not GitHub CI
acceptance.

## Release workflow

[Release policy](release-checks.md) owns qualification, coverage and manual
diagnostics. Only the tag-triggered `publish` job has `contents: write`,
`packages: write` and the `release-publishing` environment. Reruns use ordinary
`gh release upload --clobber`.

`release-publishing` is an approval/protection boundary; the publish job does not
create GitHub Deployment records.

Release Controller jobs cache image-security tools by OS, architecture and
tool/bootstrap identity, but never cache the vulnerability database. Each
invocation updates it and fails on unavailable/invalid data. Scans check the
exact archives after build and before smoke/upload; Business has no image scan.

## Optional build caches

Basic CI caches pinned bootstrap downloads and `.tools` per profile, OS,
architecture and tool/bootstrap identity. Dependency lockfiles invalidate the
relevant profiles; ordinary source revisions do not invalidate tool caches.
Security uses the same pattern per scan profile. Web and npm Security cache
`.cache/npm` (the directory selected by `scripts/env.sh`), never `node_modules`;
Web still runs `npm ci` on every invocation.

Rust clippy/test reuse `rust/target/debug`. Their cache identity includes the
Rust toolchain, Cargo lock/manifests, build flags, patched dependencies and check
script; a source-revision suffix saves new objects while a matching identity
prefix reuses dependency compilation across source revisions.

Business tools are separate from the Agent compilation cache. Business and
Release use the same `rust/target/agent-<arch>-<builder-hash>` directory and
cache identity, always rebuilding and validating the requested version.
No runtime state or credentials are cached. Existing per-image/per-architecture
BuildKit GHA caches, `mode=max` and bounded cold fallback remain unchanged.
