# Basic CI

> **CI reference.** Contributors should start with [Validate a change](testing.md).

The primary workflow, `.github/workflows/ci.yml`, runs Basic CI on
`pull_request`, pushes to `main`, and `workflow_dispatch`. It uses
GitHub-hosted `ubuntu-24.04` runners, `contents: read`, SHA-pinned checkout,
and no production secrets. A new commit cancels an older run of the same PR.
Main pushes and manual dispatches have run-specific concurrency groups and
do not cancel earlier runs.

## Basic checks

One small routing job selects up to five independent checks. There is no
runtime-artifact dependency, PostgreSQL matrix, or acceptance worker graph.

| Job | Command | Bootstrap profile | Coverage |
| --- | --- | --- | --- |
| `docs` | `scripts/docs-check.sh` | none | Line endings, nonempty Markdown, and bootstrap documentation |
| `go` | `scripts/go-check.sh standard` | `go-test` | gofmt, go vet, and ordinary Go tests |
| `rust` | `scripts/rust-check.sh` | `rust-basic` | Format, check, clippy, and workspace tests |
| `web` | `scripts/web-check.sh` | `web` | Format, lint, types, unit tests, builds, and generated-client authentication tests |
| `database-smoke` | `scripts/database-integration.sh` | `go-test` | PostgreSQL 17 migrations and database integration |

Go checks retain both existing Go modules, including unit tests for the G6
harness; they do not run G6 acceptance. Rust checks do not run cargo audit,
license policy, native integration, or separate boundary scripts. License
validation remains available through `scripts/license-check.sh`.

Bootstrap versions come from `toolchains.lock`; downloads are verified
against `scripts/checksums.txt`. The Go profile installs only Go and verifies
host jq. The Rust profile installs only Rust, rustfmt, and clippy. Web
bootstrap installs pinned Node/npm and dependencies, with
`npm_config_audit=false` and `npm_config_fund=false` for the entire job,
including npm installation. Ordinary CI does not run security audits.

The database job sets `PG_MAJOR=17` and lets the integration script build
`ocserv-control` itself. PostgreSQL 18 and its legacy upgrade fixture are
not run. It needs only the router, not a Rust build or a shared binary artifact.

## Path routing

`scripts/ci-relevance.sh` emits only five execution flags:
`run_docs`, `run_go`, `run_rust`, `run_web`, and `run_database`.
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

Mixed changes use the union of their checks. Documentation-only changes do
not activate language or database checks. Infrastructure changes, unknown
paths, and unclassifiable diffs conservatively run all five, never acceptance.
G6-specific and script-level manual acceptance paths are the exceptions: they
select only the basic docs check, not acceptance or additional contract checks.

PR routing uses `base...head`, excluding base-only changes after the branch
point. Main pushes use `before..head`. Deletions and both sides of renames
retain their path impact. Manual dispatch, empty diffs, invalid/all-zero or
unresolvable SHAs, and diff failures select all basic checks.

## Required check migration

`basic-ci-result` publishes the stable **Basic CI Result** check. It succeeds
only when routing succeeds, every selected job succeeds, and every unselected
job is skipped. Failed, cancelled, unexpectedly skipped, or missing results
fail this one summary. No legacy result aggregators remain.

Before merging this change, a repository administrator must replace the old
required contexts in the main ruleset with `Basic CI Result` after it has
succeeded on the candidate PR:

- `Backend Integration`
- `Web & Smoke`
- `Quality, Security & Native`
- `G6 Harness Smoke Core / G6 Harness Smoke Result`

Changing workflow YAML does not migrate GitHub rulesets. Leaving those old
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
cross-VM, browser E2E, security, or license acceptance. Those scripts and
manual entry points remain available; `make verify` is a broader local
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

Use the same commit and run these commands on `LocalServer`:

```bash
scripts/test-ci-relevance.sh
scripts/test-bootstrap-profiles.sh
scripts/docs-check.sh
scripts/bootstrap.sh go-test
scripts/go-check.sh standard
scripts/bootstrap.sh rust-basic
scripts/rust-check.sh
npm_config_audit=false npm_config_fund=false scripts/bootstrap.sh web
npm_config_audit=false npm_config_fund=false scripts/web-check.sh
PG_MAJOR=17 scripts/database-integration.sh
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
