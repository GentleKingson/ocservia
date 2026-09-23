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

## Release workflow

The [Release Check](release-checks.md) owns the complete release graph, including
the existing Full CI invocation, exact product builds, Package & Upgrade,
supported application compatibility, Business Smoke, selected Integration and
Resilience, and existing security checks. It is not added to PR required checks.
The initial migration selects every supported check; normal selection uses the
entire diff from the last published Release.

Dispatch `release.yml` with `version` and `arch=all` for a full dry-run.
It never publishes, writes to production registries or reads the production
signing key. `arch=amd64` or `arm64` is diagnostic only and skips Release Check.
Tag runs execute one integrated round before protected Publish; no extra full
dry-run is required. Only Publish obtains write permissions and release keys.

The [diagnostic workflow](release-upgrade-validation.md) has one purpose enum
instead of interacting booleans. Historical pre-1.0 application cells are not
part of the formal matrix. Existing registered v1.0.0 requirements remain.

Product consumers use actual producer artifact IDs plus explicit candidate
manifest and payload verification. Independent failed jobs may rerun without
requiring all architectures to share a run attempt. Shared cross-host faults
still require a coherent group rerun. See [Resilience](g6-readiness.md).

Source dependency/secret scans and Controller image scanning/SBOM policies are
unchanged. Image scans precede any production registry write, and Publish still
checks loaded image config digests against the scanned summary before pushing.
Final package re-signing may not change the tested payload archive; installer
signatures, embedded trust and payload equality are validated before release.
