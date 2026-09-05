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
  six packages, the versioned `controller-bootstrap.sh` and
  `managed-node-bootstrap.sh` copied byte-for-byte from the release checkout,
  plus the three Controller manifests
  (`controller-release.json`, `controller-release-amd64.json`,
  `controller-release-arm64.json`) on formal Controller releases.
- The Controller image build runs for tag-push release runs and manual
  `workflow_dispatch` dry runs as a
  two-leg matrix on native runners (`ubuntu-24.04` for `amd64`,
  `ubuntu-24.04-arm` for `arm64`, no emulation) with the pinned BuildKit
  builder. Each native leg exports each of its four first-party images as a
  single-platform Docker image archive. The smoke script loads that exact
  archive into the runner's Docker daemon before it asserts the image really
  targets the leg's architecture, boots the gateway image to serve a request
  as a non-root process, and drives the control, transport, and backup images
  to their startup boundaries on the native runner. The same archive is
  uploaded as the cross-job artifact and loaded again by the single
  `release-publishing`-gated publish job, which pushes the per-platform images under
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
  without the production pin. The build legs retain `contents: read` only,
  and manual dispatch remains a dry run with no production write; the
  protected publish job remains the sole registry-write boundary.
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
  release is never modified: GitHub's release attestation covers the bootstrap
  assets along with the rest of the exact asset set, while their entries in the
  Ed25519-signed `SHA256SUMS` preserve the independently pinned project trust
  chain. A rerun after a successful publication skips
  every production-write step and instead re-verifies the published
  release in place (immutability, attestation, tag binding, and the
  complete asset-name set), so a transient attestation delay cannot leave
  a correct release with a permanently red workflow; a leftover draft
  from an earlier failed attempt is the only thing it discards and
  recreates. Because immutable releases lock assets and the tag at
  publication, release notes are attached by a maintainer afterwards —
  title and notes remain editable on an immutable release.

## Deferred native validation

Native systemd/privd/PKI and live relay scenarios remain outside Basic CI.
Pure Rust adapter tests still run in the ordinary Rust workspace suite.
Use the script-level manual acceptance commands for environment-dependent checks.
