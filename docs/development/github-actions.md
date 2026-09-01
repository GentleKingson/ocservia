# GitHub Actions validation

GitHub Actions is the authoritative merge-time validation environment. The
primary workflow runs on pull requests, pushes to `main`, and manual dispatch.
It uses GitHub-hosted `ubuntu-24.04` runners, read-only repository permissions,
and no production secrets. Local commands reproduce behavior but never replace
the required checks for the exact pull-request commit.

## Workflow inventory

The retained workflows are:

- **Change-Aware CI**: change-routed pull-request and `main` validation.
- **G6 PR Readiness Smoke**: required pull-request G6 smoke validation.
- **G6 Formal Readiness**: manually dispatched formal readiness acceptance.
- **G6 Readiness Core (Reusable)**: shared formal and smoke execution graph.
- **Manual Runtime & Security Acceptance**: high-cost runtime and security phases.
- **Cross-VM Iroh Enrollment E2E**: real two-runner enrollment acceptance.
- **Agent Release Packages**: multi-architecture build, validation, signing, and publishing.
- **G6 Rust Build Cache Provisioning**: trusted default-branch BuildKit cache producer.

The legacy `g6-ha-pitr.yml` entry point was removed after parity review. Formal
G6 covers its two failure domains, streaming standby, base backup and PITR,
primary isolation, promotion and post-promotion probes, role recovery,
former-primary rejoin, merged evidence, secret scanning, and independent
verification. The historical scripts and fixtures remain for reference.

## Execution graph

The primary workflow has 15 worker job definitions plus a `Change Impact Router`
classifier job. The PostgreSQL matrix expands one definition into separate
PostgreSQL 17 and 18 executions, so a full run has 16 worker executions.
Three lightweight result aggregators preserve the stable required-check
names. Conditional workers start after the relevance classifier except that
the two PostgreSQL workers and Control Plane ↔ Transport Local Integration
additionally wait for the commit-bound runtime artifact. Repository Secret Scan starts independently and is always
required by the quality aggregate.

| Worker execution | Coverage | Bootstrap profile | Timeout | Required-check aggregator |
| --- | --- | --- | --- | --- |
| Build Shared Runtime Binaries | Builds `ocserv-control` and `ocservia-transportd-stub` once | `go-rust-integration` | 15 minutes | Backend Integration |
| Go Quality & Unit Tests | Format, vet, staticcheck, unit tests, and govulncheck | `go-quality` | 20 minutes | Backend Integration |
| Go Race Tests | Full Go race suite | `go-test` | 20 minutes | Backend Integration |
| PostgreSQL 17 Migration & Integration | PostgreSQL 17 migrations, rollback, runtime, and failure behavior | `go-test` | 25 minutes | Backend Integration |
| PostgreSQL 18 Migration & Integration | PostgreSQL 18 coverage plus the legacy full upgrade fixture | `go-test` | 25 minutes | Backend Integration |
| Production Topology & Relay Contracts | Production topology, controlled relay failover, backup restore, Agent package lifecycle, and Docker-backed Controller lifecycle semantics | `go-rust-integration` | 25 minutes | Backend Integration |
| PostgreSQL Credential Rotation Integration | Application and backup credential rotation | none | 15 minutes | Backend Integration |
| Control Plane ↔ Transport Local Integration | Go, PostgreSQL, UDS, and Rust-stub integration | none | 15 minutes | Backend Integration |
| Web Quality, Unit & Build | Web format, type, unit, build, and audit checks | `web` | 20 minutes | Web & Smoke |
| Web Browser E2E | Isolated Compose Playwright desktop and mobile E2E | none | 25 minutes | Web & Smoke |
| Runtime Resilience Smoke (24 Agents) | Hosted-runner runtime resilience smoke | none | 20 minutes | Backend Integration |
| Repository Contracts & Policy | Repository, docs, workflow, generated-output, staged-feature, runtime-harness, and Cross-VM contracts | `contracts` | 20 minutes | Quality, Security & Native |
| Rust Quality, Tests & Boundaries | Rust format, clippy, workspace tests, audit, and agent/transport boundaries | `rust-validation` | 25 minutes | Backend Integration and Quality, Security & Native |
| Repository Secret Scan | Full-history repository gitleaks scan | `g6-secret-scan` | 20 minutes | Quality, Security & Native |
| Dependency License Policy | Go, Rust when not covered by Rust validation, and Web dependency license policy | `security` | 20 minutes | Quality, Security & Native |
| Native Ocserv / Agent Integration | Ephemeral native package, `ocpasswd`, OpenSSL, and loopback login fixture | `native` | 20 minutes | Quality, Security & Native |

Rust Quality, Tests & Boundaries executes once and feeds two aggregators. Each aggregator has a
five-minute timeout, uses `always()`, and accepts a dependency only when it
succeeded or when the relevance classifier explicitly marked it not
applicable and it was skipped; every other result, including an unexpected
skip, fails the aggregator. The workflow uses no workflow-level path filters.

### Change relevance

The repository-owned `scripts/ci-relevance.sh` publishes one authorization
flag for every conditional worker. It ORs the impact domains of all recognized
paths, so a Web plus Rust change runs the Web, Browser, and Rust workers rather
than full CI. Ordinary documentation runs Repository Contracts & Policy plus the
structurally always-on Repository Secret Scan. Web unit/config-only inputs run Web Quality, Unit & Build
and Unit; Web runtime, build, dependency, and Playwright inputs also run
Web Browser E2E. Runtime Resilience Smoke is a backend/runtime responsibility and is not activated
by Web changes.

Database packages and migrations alone activate PostgreSQL 17/18, and
PostgreSQL or local-integration relevance explicitly implies Build Shared Runtime
Artifacts. Native, production relay/Controller lifecycle, credential rotation, stage-contract,
license, and G6 smoke flags each follow the inputs consumed by their own
harness. Machine-readable G6 acceptance contracts activate Contracts and
Policy plus G6 Smoke, while ordinary release-readiness Markdown does not.
Repository Secret Scan remains always-on and keeps the full-history `gitleaks git`
semantics; Dependency License Policy runs only for dependency manifests, lockfiles, or
license-policy inputs.

Pull requests are classified with `base...head` three-dot semantics, so base
branch changes after the PR branch point are excluded. Pushes to `main` use
`github.event.before..github.sha` two-dot semantics and are incrementally
routed. Manual dispatch remains full validation. Invalid or all-zero SHAs,
unresolvable merge bases, failed or empty diffs, global toolchain changes, the
primary CI routing authority, and unknown paths fail closed to full CI.
Known mixed changes never become full merely because they are mixed.

Staged-feature `--contract-only` modes omit only repeated Go or Rust language
suite invocations. Their static source, API, manifest, recovery, and policy
assertions now run conditionally inside Repository Contracts & Policy.

The 500-Agent Resilience & Capacity Acceptance remains a separate manual
`p1-capacity.yml` phase. It
runs the default 500-Agent single-VM profile and all fault phases with a
45-minute timeout. Capacity evidence is not part of ordinary pull-request
feedback, and the primary workflow keeps the smoke parameters unchanged.

`rust-cache-provision.yml` is a performance-input producer, not a validation:
it runs on trusted `main` pushes that touch the Rust workspace (plus a weekly
refresh and manual dispatch, which fail closed on any non-`main` ref before
checkout), rebuilds the shared `g6-rust-builder` stage with the strict cache
exporter, and publishes the `g6-rust-runtime` cache on the default branch. Pull-request workflows can restore base-branch caches, while
caches they write themselves are bound to their own merge ref, so this
producer is what lets a brand-new PR start from a warm dependency cache. A
red provision run never affects correctness on `main`; it only means that run
did not finish publishing the cache.

The G6 harness has two thin callers over `.github/workflows/g6-harness-core.yml`.
`g6-readiness.yml` is the queued manual formal caller and preserves the full
two-failure-domain production-readiness contract. `g6-harness-smoke.yml` is a
latest-wins pull-request check fixed to engineering authority. Its two hosted
jobs load the same frozen product release, bring up two Agents per domain,
exercise one authenticated cross-domain mutation and one standby promotion,
then freeze evidence after a bounded observation. Separate Builder, gitleaks,
and independent Verifier jobs feed `ocservia.g6-harness-smoke-result.v1`; the
smoke never enters a formal Environment,
produces a production-readiness verdict, or substitutes for the three required
CI aggregators or a formal G6 run. The secret-scan jobs on both profiles scan
every published evidence layer with the same repository gitleaks configuration
and bootstrap only the dedicated minimal `g6-secret-scan` profile; their
bootstrap, evidence download, scan, and result-publication spans are recorded
as non-authoritative timing diagnostics that never influence a verdict: every
timing call is fail-open guarded at the call site, so a telemetry failure can
neither skip nor fail the authoritative scan work. The minimal profile is
small enough that it bootstraps cold every run with no tooling cache.

The smoke caller always creates the same aggregate result check. Executable,
workflow, deployment, and acceptance-contract changes run the complete hosted
profile. Documentation-only pull requests publish a structured
`not_applicable` smoke result and succeed without reserving the two runtime
runners. Empty or unclassifiable diffs fail closed by running the full profile.
Workflow policy is checked by separate authority, release-identity, evidence,
and reusable-workflow contract tests; runtime adapter fixtures remain isolated
from those YAML-level contracts.

## Bootstrap profiles

`toolchains.lock` is the only version source, and `scripts/checksums.txt`
authenticates downloaded binaries. Every worker that bootstraps tools invokes
exactly one explicit profile:

| Profile | Installed or verified tools | Workers |
| --- | --- | --- |
| `go-test` | Go and host `jq` | Go Race Tests; PostgreSQL 17/18 Migration & Integration |
| `go-quality` | Go, staticcheck, govulncheck, and host `jq` | Go Quality & Unit Tests |
| `go-rust-integration` | Go, Rust, staticcheck, govulncheck, sccache, and host `jq` | Build Shared Runtime Binaries; Production Topology & Relay Contracts |
| `web` | Node, pinned npm, and `npm ci` dependencies | Web Quality, Unit & Build |
| `contracts` | Node/npm, Buf, OpenAPI Generator, oasdiff, host Java, and Web dependencies | Repository Contracts & Policy |
| `rust-validation` | Rust, rustfmt, clippy, cargo-audit, cargo-deny, and sccache | Rust Quality, Tests & Boundaries |
| `security` | Go, Node/npm, Rust, cargo-deny, sccache, and Web dependencies | Dependency License Policy |
| `native` | Rust and sccache | Native Ocserv / Agent Integration |
| `g6-secret-scan` | gitleaks and host `jq`/`openssl` | Repository Secret Scan; G6 Formal Evidence Secret Scan; G6 Smoke Evidence Secret Scan |
| `native-packages` | Rust and nfpm (Linux `aarch64` mirrors only this profile) | Agent Release Packages build matrix |

Staged-feature contracts, credential rotation, local integration, Web Browser E2E, and runtime resilience smoke
use runner-provided tools or artifacts and do not call bootstrap. Outside
GitHub Actions, `make bootstrap` explicitly selects the complete `all` profile.

## Caches

Tool caches contain only `.cache/downloads` and `.tools`. Their exact keys
include the profile, runner OS and architecture, `toolchains.lock`, checksums,
bootstrap code, and environment setup. They have no broad restore prefix. A
cache miss is supported because bootstrap revalidates versions and checksums.

| Tool-cache profile | Main-branch writer | Restore consumers |
| --- | --- | --- |
| `go-rust-integration` | Build Shared Runtime Binaries | Build Shared Runtime Binaries; Production Topology & Relay Contracts |
| `go-quality` | Go Quality & Unit Tests | Go Quality & Unit Tests |
| `go-test` | Go Race Tests | Go Race Tests; PostgreSQL 17/18 Migration & Integration |
| `web` | Web Quality, Unit & Build | Web Quality, Unit & Build |
| `contracts` | Repository Contracts & Policy | Repository Contracts & Policy |
| `rust-validation` | Rust Quality, Tests & Boundaries | Rust Quality, Tests & Boundaries |
| `security` | Dependency License Policy | Dependency License Policy |
| `native` | Native Ocserv / Agent Integration | Native Ocserv / Agent Integration |

The release-packages workflow writes its own `native-packages` tool cache from
its per-architecture build matrix jobs.

The shared Go cache contains `.cache/go-build`, `.cache/go-mod`, and
`.cache/gopath`. Build Shared Runtime Binaries, Go Quality & Unit Tests, Go Race Tests,
PostgreSQL 17/18, Production Topology & Relay Contracts, and Dependency License Policy restore it.
Its primary key includes the exact commit and Go dependency inputs, with
dependency and platform prefix fallbacks. Go Quality & Unit Tests is its only
writer.

The shared npm cache contains `.cache/npm`. Web Quality, Unit & Build, Repository Contracts &
Policy, and Dependency License Policy restore it; Web Quality, Unit & Build is its only
writer. All explicit tool, Go, and npm cache writes require a successful push
to `main` and a primary-key miss. Pull-request workers are restore-only. CI
does not cache `node_modules`, credentials, environment files, logs, test
artifacts, or `rust/target`.

Rust compiler outputs use sccache instead of archiving `rust/target`. Build
Shared Runtime Binaries, Production Topology & Relay Contracts, Rust Quality,
Tests & Boundaries, Dependency License Policy, and Native Ocserv / Agent Integration:

- request repository-pinned sccache `0.17.0` through the SHA-pinned sccache
  Action;
- set `RUSTC_WRAPPER=sccache`, disable Cargo incremental compilation, and
  normalize the workspace base path;
- select the GitHub Actions backend only when its runtime credentials and
  cache endpoint are present;
- fall back to the local `.cache/sccache` directory outside that environment.

The downloaded sccache binary and platform checksums are pinned in the same
toolchain files as other bootstrap tools. Native Ocserv / Agent Integration preserves the required
sccache environment across its root fixture while keeping its Cargo target in
a unique directory below `RUNNER_TEMP`.

## Runtime artifact

Build Shared Runtime Binaries compiles `ocserv-control` and
`ocservia-transportd-stub`, then creates
`runtime-<run>/runtime-artifacts.tar.gz`. Its manifest records the
full candidate commit and a SHA-256 digest for each executable. Extraction
allows exactly the manifest and two expected binaries, rejects unsafe or
unexpected entries, validates both digests, and verifies that the manifest
commit equals `GITHUB_SHA`. The name omits the run attempt and the upload
overwrites, so "Re-run failed jobs" reuses the artifact the successful build
already produced while a full re-run replaces it; the manifest check still
binds the artifact to the exact commit.

PostgreSQL 17, PostgreSQL 18, and the local integration worker download and validate that
artifact instead of rebuilding the same binaries. The PostgreSQL matrix uses
`fail-fast: false`, so a failure in one major does not cancel evidence from the
other. The PostgreSQL 18-only legacy upgrade fixture runs for `PG_MAJOR=18` or
the local `PG_MAJOR=all` mode; it is not repeated in the PostgreSQL 17 worker.

## Diagnostics

Uploads use SHA-pinned Actions, one-day retention, and names bound to the run
(except the reusable runtime artifact) or to the run and attempt. Ordinary CI
diagnostics upload only after failure or cancellation; acceptance and release
evidence keeps its existing unconditional publication semantics.

| Worker | Artifact name |
| --- | --- |
| Build Shared Runtime Binaries | `runtime-<run>` |
| Go Quality & Unit Tests | `go-standard-<run>-<attempt>` |
| Go Race Tests | `go-race-<run>-<attempt>` |
| PostgreSQL 17/18 | `database-pg<major>-<run>-<attempt>` |
| Production Topology & Relay Contracts | `production-relays-<run>-<attempt>` |
| PostgreSQL Credential Rotation Integration | `credential-rotation-<run>-<attempt>` |
| Control Plane ↔ Transport Local Integration | `local-slice-<run>-<attempt>` |
| Web Quality, Unit & Build | `web-validation-<run>-<attempt>` |
| Web Browser E2E | `browser-e2e-<run>-<attempt>` |
| Runtime Resilience Smoke (24 Agents) | `p1-smoke-<run>-<attempt>` |
| Repository Contracts & Policy | `contracts-<run>-<attempt>` |
| Rust Quality, Tests & Boundaries | `rust-<run>-<attempt>` |
| Native Ocserv / Agent Integration | `native-ocserv-<run>-<attempt>` |
| 500-Agent Resilience & Capacity Acceptance | `p1-full-<run>-<attempt>` |

Repository Secret Scan and Dependency License Policy have no separate diagnostic artifact; their
results remain in the job log. Web Browser E2E and Runtime Resilience Smoke use distinct
`RUN_ID`, Compose project, and artifact paths. Docker and native scripts capture
diagnostics before scoped cleanup, preserve the original test result, and turn
their own leftovers into failures.

Download one artifact with:

```bash
gh run download <run-id> --name <artifact-name>
```

Reproduce from the same commit with the worker's bootstrap profile and script.
`make verify` covers the local language, contract, security, generated-output,
and policy baseline; `make e2e` reproduces the browser stack. Docker tests must
use unique `RUN_ID` and `COMPOSE_PROJECT` values.

## Required checks

Branch protection requires these stable result contexts:

- `Backend Integration`
- `Web & Smoke`
- `Quality, Security & Native`
- `G6 Harness Smoke Core / G6 Harness Smoke Result`

The worker names are visible checks but are not configured individually as
required checks. Backend Integration waits for its ten worker job definitions,
including both PostgreSQL matrix executions, Runtime Resilience Smoke, and the
contract worker that conditionally owns staged-feature assertions. Web & Smoke
waits for its two Web workers. Quality, Security & Native waits for Repository
Contracts & Policy, the shared Rust validation execution, Repository Secret
Scan, Dependency License Policy, and Native Ocserv / Agent Integration.

Do not rename an aggregator until the branch ruleset has first been migrated to
a successful check with the replacement name. A change is fully validated only
when all three aggregators pass for its exact commit, diagnostic uploads and
scoped cleanup complete, and any independent gate remains satisfied or is
explicitly recorded as pending.

## Release packages workflow

`.github/workflows/release.yml` builds the Agent distribution outside the
primary CI graph. It triggers when a lightweight `vX.Y.Z` tag (matching
`^v[0-9]+\.[0-9]+\.[0-9]+$`) is pushed, and on manual `workflow_dispatch`,
which always stays a dry run and never uploads release assets or writes to
GHCR: it exercises the Agent package build plus the Controller multi-arch
image build and its native image smoke, and every publishing step stays
skipped. Publishing
requires repository release immutability to be enabled first (Settings →
Releases → Enable release immutability, or `gh api -X PUT
/repos/{owner}/{repo}/immutable-releases`); the setting only applies to
releases published after it is enabled, so the mutable v0.1.x baseline used
by the upgrade smoke is unaffected. The publish job verifies this
prerequisite itself before any production write through the
`REPO_ADMIN_READ_TOKEN` secret of the `release-publishing` environment
(a fine-grained PAT with Administration: read), because `GITHUB_TOKEN`
cannot read the repository administration API.

- The build job runs as a two-leg matrix on native runners
  (`ubuntu-24.04` for `amd64`, `ubuntu-24.04-arm` for `arm64`, no emulation).
  Each leg bootstraps the `native-packages` profile, builds the release
  binaries natively, produces the signed archive triple plus `.deb` and
  `.rpm`, and runs `scripts/release-native-package-smoke.sh`: a full
  deb install/upgrade/removal lifecycle on the runner plus an rpm
  install/upgrade/erase lifecycle inside a systemd-enabled Rocky Linux 9
  container built for the run. A failure on either architecture fails the
  workflow before any assets can be published.
- Each build leg also runs
  `scripts/release-baseline-upgrade-smoke.sh`: it installs the published
  v0.1.1 `.deb` after verifying it against the release's signed
  `SHA256SUMS`, whose exact bytes the smoke pins in-repo (the v0.1.1
  release is not immutable), then upgrades it in place to the candidate
  while asserting the package state and the rollback snapshot. The
  published-baseline leg is deb-only; rpm cross-version coverage remains
  the fabricated-version lifecycle smoke inside the Rocky Linux 9
  container.
- The validate job downloads both legs' artifacts and runs
  `scripts/validate-release-packages.sh`: presence and naming of the six
  packages, both signed triples verified against the pinned fingerprint,
  `MANIFEST` and ELF architecture agreement, deb/rpm architecture metadata,
  identical embedded payloads, and a canonical `SHA256SUMS` covering those
  six packages plus the three Controller manifests
  (`controller-release.json`, `controller-release-amd64.json`,
  `controller-release-arm64.json`) on formal Controller releases.
- The Controller image build runs for tag-push release runs and manual
  `workflow_dispatch` dry runs as a
  two-leg matrix on native runners (`ubuntu-24.04` for `amd64`,
  `ubuntu-24.04-arm` for `arm64`, no emulation) with the pinned BuildKit
  builder. Each leg exports its four first-party images as OCI archives
  and loads the Docker representation from the same BuildKit solve into the
  runner's Docker daemon, whose classic image store cannot load OCI layouts
  back. The smoke script compares the archive config digest with the loaded
  image ID before it asserts each
  image really targets the leg's architecture, boots the gateway image to
  serve a request as a non-root process, and drives the control,
  transport, and backup images to their startup boundaries on the native
  runner. The legs hold only source-read
  permissions: no registry write of any kind happens before approval. The single `release-publishing`-gated publish
  job then loads both legs' archives, pushes the per-platform images under
  `<version>-linux-<arch>` companion tags, merges them with
  `docker buildx imagetools create` into one tagged multi-platform index
  per image, fails closed when an index lacks either architecture,
  generates the platform manifests, re-checks anonymous GHCR reads of the
  image indexes, pushes the four `actions/attest` provenance attestations
  against the final index digests, and finally signs and uploads the whole
  release asset set. Every production registry write therefore happens
  behind exactly one reviewer checkpoint.
- Every build job signs with an ephemeral key generated on the runner, so a
  `workflow_dispatch` run never touches the production signing credential,
  and the validate job checks the internal consistency of the signed set
  without the production pin.
- The publish job runs only for tag-push release runs behind the protected
  `release-publishing` environment with `contents: write`: it re-signs both
  archive checksum triples with the release key, rebuilds the native
  packages with the release trust anchor, validates the whole set against
  the `AGENT_TRUSTED_KEY_SHA256` pin, signs the unified `SHA256SUMS`, and
  only then publishes through the immutable-release sequence. Before any
  production write it binds the tag to the run's source commit (remote
  `refs/tags/vX.Y.Z` must resolve to exactly that commit as a lightweight
  tag; the check is repeated immediately before publishing, since a tag
  can still be moved until the release is published), and it preflights
  the repository immutability setting through `REPO_ADMIN_READ_TOKEN`.
  The sequence itself is: create a draft release for the tag, upload the
  complete asset set without `--clobber` (name collisions fail instead of
  overwriting), publish the draft, and verify the release attestation
  GitHub generated at publication with `gh release verify` plus an
  `immutable: true` assertion on the published release. A published
  release is never modified: a rerun after a successful publication skips
  every production-write step and instead re-verifies the published
  release in place (immutability, attestation, tag binding, and the
  complete asset-name set), so a transient attestation delay cannot leave
  a correct release with a permanently red workflow; a leftover draft
  from an earlier failed attempt is the only thing it discards and
  recreates. Because immutable releases lock assets and the tag at
  publication, release notes are attached by a maintainer afterwards —
  title and notes remain editable on an immutable release.

## Deferred native validation

The `native_user_and_group_operations` ignored test and the real I13 loopback
login run in Native Ocserv / Agent Integration. Pure adapter logic remains in ordinary Rust tests.
These tests are not reported as hosted validation:

- `native_controlled_operations` needs prepared live sessions, an IP ban, and a
  real `ocserv.service` lifecycle.
- `native_reload_failure_is_bounded` depends on a deliberately stopped native
  service and belongs with the same isolated systemd fixture.
- `relay_only_connection_and_disabled_relay_failure` and
  `relay_and_direct_paths_converge_to_direct` depend on a public Iroh relay.

Backend Integration instead uses two controlled local relays, removes the first
relay, and proves reconnection through the second. A hosted single-VM workflow
does not claim multi-host, multi-region, or production failure-domain evidence.
