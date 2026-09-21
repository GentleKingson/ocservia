# Basic CI

> **CI reference.** Contributors should start with [Validate a change](testing.md).

One `.github/workflows/ci.yml` runs on PRs, main pushes and manual dispatch.
Automatic runs use **Quick**. Manual dispatch accepts `quick|full`, defaulting
to **Full**. Full means **key compatibility checks on all supported units**,
not comprehensive acceptance. Product database support has not changed.

Warm-cache goals are 3-5 minutes for Quick and 8-10 minutes for Full, excluding
queue time. They are goals, not measured results or relaxed failure criteria.
Record queue, cold setup, execution wall time and summed job runner-minutes
separately. New Quick runs cancel obsolete Quick runs on the same PR/branch.
Full has a separate concurrency group and cannot cancel Quick.

## Basic checks

| Job | Retained checks |
| --- | --- |
| docs | Line endings, nonempty Markdown and documentation links/policy text |
| go | gofmt, vet and ordinary fast tests in both Go modules; no full-package race |
| rust | Format, clippy and workspace tests |
| web | Format, lint, types, unit tests, build and generated-client authentication; no browser installation/regression |
| database-smoke | Quick: PostgreSQL 17.10 (project default), MySQL 8.4.10. Full: also PostgreSQL 18.6 and MariaDB 12.3.2 |
| database-recovery-full | Existing short MySQL/MariaDB logical backup/restore loop, Full only |
| Basic CI Result | Always checks routing and selected job results; required missing/skipped/failed/cancelled jobs fail |

All database images retain their exact patch/digest pins. Each core job starts
one database service and initializes one schema for `TestDatabaseCoreSmoke`.
That flow migrates an empty database with the owner account, starts the real
Controller CLI with the runtime account, bootstraps and logs in a local
administrator, and reads the written workspace through the authenticated API.
Real transactions check commit, rollback and isolation from a second pooled
connection; a repeated migration must preserve the committed workspace.
Runtime DDL denial is the small retained failure path: it verifies the
Controller is not accidentally tested with owner credentials. MySQL/MariaDB
retain production configuration and verified TLS in this same flow.

Full adds **one direct upgrade per unit** in a separate schema on the same
service: PostgreSQL schema 35 -> current 36; MySQL/MariaDB immutable revision
25 -> current 26. These use the real migration runners and preserve seeded
business data. They are schema compatibility checks, not native release
upgrade certification. There is no separate history job or exhaustive revision
matrix, and no second Controller build. Core tests use `-count=1`, never
cached test results, and do not use `-race`.

`required-go-tests.sh --smoke <package> <test>` checks only the explicit
top-level test's run/final-pass events. A missing or skipped entry fails.
Basic CI has no per-subtest inventory, cumulative JSONL or matrix success
outputs. The old manifest/checker remains only for independent manual deep
acceptance callers; its redundant ordinary-unit inventory has been removed.

CI router/wrapper/bootstrap self-tests run only for their implementation or
shared CI/toolchain changes. Installer self-tests run only for installer
changes. Manual dispatch does not add these unrelated self-tests.
No role lifecycle matrix, restart, outbox/fencing/disconnect, exhaustive
authentication/API/policy suite, checksum/drift/invalid-configuration matrix,
or deep historical repair suite is moved from Quick into Full.

## Retained workflows

| File | Trigger / purpose |
| --- | --- |
| `ci.yml` | PR/main Quick; manual Quick/Full key checks |
| `security.yml` | Weekly/manual checks and reusable release prerequisite |
| `g6-readiness.yml`, `g6-harness-core.yml` | Independent manual formal readiness |
| `release.yml` | Tag/manual release packaging |
| `release-upgrade.yml` | Independent manual native release upgrades; optional published-node application matrix |

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

`scripts/ci-relevance.sh` uses directory rules, not function dependency analysis:

| Paths | Quick checks |
| --- | --- |
| Documentation / Markdown | docs only |
| Web (including its generated client) | web only |
| Rust | rust only |
| Controller commands, internal modules, storage, API, migrations, Go locks | go + core database smoke |
| Other Go sources / harness | go |
| Installer implementation/tests | rust + installer self-tests |
| Shared contracts, CI/toolchain infrastructure or unknown paths | all Quick checks |

Mixed changes take the union. PRs use `base...head`; pushes use `before..head`.
Deletions/renames retain both affected paths. Invalid/empty/unresolvable diffs
fall back to Quick, never Full. Manual Full selects all basic domains and all
four database units. Manual Quick selects all domains but only its two units.

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
# On BuildServer; expensive opt-in checks:
DATABASE_TEST_SCOPE=full PG_MAJOR=all scripts/database-integration.sh
DATABASE_TEST_SCOPE=full ENGINE=mysql bash scripts/database-foundation-integration.sh
DATABASE_TEST_SCOPE=full ENGINE=mariadb bash scripts/database-foundation-integration.sh
scripts/go-check.sh race
scripts/web-check.sh full
```

The database scripts still default to legacy `full` when invoked without a
scope, preserving existing release/deep callers. `regression` and MySQL
`DATABASE_FULL_PART=all|current|history` also remain manual-only. Normal CI
explicitly passes `smoke` (Quick) or `compatibility` (Full).
Browser checks require Playwright Chromium installed separately. Disaster
recovery, complex races and fault injection remain in their existing manual
G6/deep scripts; no scheduled workflow was added.

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
  [Cross-VM real E2E validation](real-e2e.md) for manual execution.

`make real-e2e-check` only checks the three real-E2E scripts' Bash syntax. It
does not read workflow files or run live acceptance, and Basic CI does not call it.

## Reproduction

Run on `BuildServer` against the same candidate source:

```bash
scripts/bootstrap.sh go-test
scripts/go-check.sh standard
DATABASE_TEST_SCOPE=smoke PG_MAJOR=17 scripts/database-integration.sh
DATABASE_TEST_SCOPE=smoke ENGINE=mysql bash scripts/database-foundation-integration.sh
# Full: use compatibility for PG_MAJOR=17 and 18, ENGINE=mysql and mariadb.
DATABASE_TEST_SCOPE=compatibility PG_MAJOR=18 scripts/database-integration.sh
DATABASE_TEST_SCOPE=compatibility ENGINE=mariadb bash scripts/database-foundation-integration.sh
RUN_ID=local-mysql ARTIFACT_DIR="$PWD/.cache/recovery-mysql" ENGINE=mysql bash scripts/i18-mysql-backup-restore-smoke.sh
RUN_ID=local-mariadb ARTIFACT_DIR="$PWD/.cache/recovery-mariadb" ENGINE=mariadb bash scripts/i18-mysql-backup-restore-smoke.sh
```

With push/remote-run authorization, select the candidate branch in Actions or
run `gh workflow run ci.yml --ref <candidate-branch> -f profile=full`.
Use `profile=quick` for the other profile. Record the actual candidate SHA,
cache state and job/step timings. Local BuildServer results are not GitHub CI
acceptance.

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
  Agent, privd, and upgrader through the shared `build-release-agent.sh` and
  `build-agent-binaries.sh` entrypoints in a digest-pinned Rocky 9 container
  with glibc 2.34. Cargo objects are isolated from host-built objects; native
  host/daemon/container/ELF identity and all three binary versions are checked.
  It produces a signed tar archive plus deb/rpm,
  and runs `scripts/release-native-package-smoke.sh` for the candidate's
  deb install/upgrade/removal and rpm install/upgrade/erase scripts.
  It also runs the published v0.6.0 DEB/RPM baseline package smoke, executing
  all three installed candidate binaries on Ubuntu and systemd Rocky 9.
  The additional full upgrade gate below is independent of this release job.
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

## Manual native upgrade validation

`.github/workflows/release-upgrade.yml` is a separate, manual-only workflow.
By default it requires Agent and Controller upgrade results on both native
`ubuntu-24.04` and `ubuntu-24.04-arm` runners. `session_compatibility=true`
adds four published-node application cells (v0.6.0/v0.6.1 x amd64/arm64).
`session_only=true` runs those application cells even without the other flag,
and skips both native upgrade matrices and `Native Upgrade Result`. Both
flags default to `false`. Prepare still requires and freezes candidate
version, baseline tag and exact dispatch SHA in every mode; application jobs
always test both node baselines. The workflow neither publishes a release
nor becomes a Basic CI required check.

Application-only evidence does not satisfy native upgrade acceptance.
`Native Upgrade Result` covers only native units, not application outcomes.
The [finite compatibility contract](../reference/stable-contracts.md#finite-release-matrix)
defines required application workflows and the adopted exclusions; phase
failures remain failures even when they document an excluded promise.

See [Native upgrade validation](release-upgrade-validation.md) for dispatch,
trust anchors, evidence, reproduction, and the limits of this gate. Select the
pushed candidate branch, a numeric candidate version newer than the registered
baseline, and that branch's full SHA. The default registered baseline remains
`v0.6.0`; a new release does not automatically register itself. Unlike Quick/Full
CI this is cross-version native upgrade evidence, unlike Release it does not
publish, and unlike Formal G6 it does not certify full readiness, HA or PITR.

## Deferred native validation

Native systemd/privd/PKI and live relay scenarios remain outside Basic CI.
Pure Rust adapter tests still run in the ordinary Rust workspace suite.
Use the script-level manual acceptance commands for environment-dependent checks.
