# GitHub Actions validation

GitHub Actions is the authoritative merge-time validation environment. The
primary workflow runs on pull requests, pushes to `main`, and manual dispatch.
It uses GitHub-hosted `ubuntu-24.04` runners, read-only repository permissions,
and no production secrets. Local commands reproduce behavior but never replace
the required checks for the exact pull-request commit.

## Execution graph

The primary workflow has 15 worker job definitions plus a `CI Relevance`
classifier job. The PostgreSQL matrix expands one definition into separate
PostgreSQL 17 and 18 executions, so a full run has 16 worker executions.
Three lightweight result aggregators preserve the stable required-check
names. Workers start independently after the relevance classifier except
that the two PostgreSQL workers and Local Slice additionally wait for the
commit-bound runtime artifact.

| Worker execution | Coverage | Bootstrap profile | Timeout | Required-check aggregator |
| --- | --- | --- | --- | --- |
| Build Runtime Artifacts | Builds `ocserv-control` and `ocservia-transportd-stub` once | `go-rust-integration` | 15 minutes | Backend Integration |
| Go Static and Unit | Format, vet, staticcheck, unit tests, and govulncheck | `go-quality` | 20 minutes | Backend Integration |
| Go Race | Full Go race suite | `go-test` | 20 minutes | Backend Integration |
| PostgreSQL 17 Integration | PostgreSQL 17 migrations, rollback, runtime, and failure behavior | `go-test` | 25 minutes | Backend Integration |
| PostgreSQL 18 Integration | PostgreSQL 18 coverage plus the legacy full upgrade fixture | `go-test` | 25 minutes | Backend Integration |
| I14-I19 Contracts | Focused I14-I17 and I19 contract assertions | none | 10 minutes | Backend Integration |
| I18 Production Relays | Production topology, controlled relay failover, backup restore, and Agent package lifecycle | `go-rust-integration` | 25 minutes | Backend Integration |
| PostgreSQL Credential Rotation | Application and backup credential rotation | none | 15 minutes | Backend Integration |
| Local Slice | Go, PostgreSQL, UDS, and Rust-stub integration | none | 15 minutes | Backend Integration |
| Web Static and Unit | Web format, type, unit, build, and audit checks | `web` | 20 minutes | Web & Smoke |
| Browser E2E | Isolated Compose Playwright desktop and mobile E2E | none | 25 minutes | Web & Smoke |
| P1 Smoke | P1 harness tests and the unchanged 24-Agent smoke profile | none | 20 minutes | Web & Smoke |
| Contracts and Policy | Repository policy, docs, workflow policy, generation, lint, and breaking-change checks | `contracts` | 20 minutes | Quality, Security & Native |
| Rust Validation | Rust format, clippy, workspace tests, audit, and agent/transport boundaries | `rust-validation` | 25 minutes | Backend Integration and Quality, Security & Native |
| Security and Licenses | Secret and license policy checks | `security` | 20 minutes | Quality, Security & Native |
| Native Ocserv | Ephemeral native package, `ocpasswd`, OpenSSL, and loopback login fixture | `native` | 20 minutes | Quality, Security & Native |

Rust Validation executes once and feeds two aggregators. Each aggregator has a
five-minute timeout, uses `always()`, and accepts a dependency only when it
succeeded or when the relevance classifier explicitly marked it not
applicable and it was skipped; every other result, including an unexpected
skip, fails the aggregator. The workflow uses no workflow-level path filters.

### Change relevance

The `CI Relevance` job classifies each pull-request change set with the
repository-owned `scripts/ci-relevance.sh` and publishes one authorization
flag per worker family (`run_backend`, `run_database`, `run_rust`,
`run_native`, `run_web`, `run_browser`, `run_p1_smoke`). A worker runs only
when its flag is true. Contracts and Policy and Security and Licenses are
structurally outside the classifier's authority: they carry no relevance
condition, start without waiting for the classifier, and the quality
aggregate accepts only their success — never a classifier-authorized skip —
because one of them runs the classifier's own tests. The classifier
authorizes reduced validation for exactly two high-confidence categories:

- Documentation-only changes (ordinary documentation paths, mirroring the G6
  smoke relevance classifier, with `docs/acceptance/**` excluded) keep
  Contracts and Policy plus Security and Licenses and skip the remaining
  fourteen worker executions. Documentation policy checks and the repository
  secret and license scan stay authoritative for documentation edits.
- Web-only changes (`web/**`) keep Web Static and Unit, Browser E2E, P1
  Smoke, Contracts and Policy, and Security and Licenses, and skip the
  backend, database, Rust, and native workers.

Everything else runs full validation: Go, Rust, migrations, workflow and
deployment changes, bootstrap and toolchain pins, acceptance contracts, and
any path the classifier does not recognize. Mixed categories, deleted and
renamed files that touch reduced and full categories at once, and empty or
unresolvable diffs also fail closed to full validation. Push and
manual-dispatch runs always request full validation, and the classifier
invariantly keeps the database flag a subset of the backend flag because the
PostgreSQL workers consume the commit-bound runtime artifact.

The focused I10 journal tests remain in the Rust workspace run. The I11
Go/Rust tests remain in the dedicated language workers, while its boundary
assertions run with the applicable contract and Rust workers. The I14-I19
`--contract-only` modes omit only repeated Go or Rust language-suite
invocations; they retain the stage-specific contract, topology, relay,
recovery, backup, packaging, and policy assertions owned by those scripts.

P1 Full Validation remains a separate manual `p1-capacity.yml` workflow. It
runs the default 500-Agent single-VM profile and all fault phases with a
45-minute timeout. Capacity evidence is not part of ordinary pull-request
feedback, and the primary workflow keeps the smoke parameters unchanged.

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
| `go-test` | Go and host `jq` | Go Race; PostgreSQL 17/18 Integration |
| `go-quality` | Go, staticcheck, govulncheck, and host `jq` | Go Static and Unit |
| `go-rust-integration` | Go, Rust, staticcheck, govulncheck, sccache, and host `jq` | Build Runtime Artifacts; I18 Production Relays |
| `web` | Node, pinned npm, and `npm ci` dependencies | Web Static and Unit |
| `contracts` | Node/npm, Buf, OpenAPI Generator, oasdiff, host Java, and Web dependencies | Contracts and Policy |
| `rust-validation` | Rust, rustfmt, clippy, cargo-audit, cargo-deny, and sccache | Rust Validation |
| `security` | Go, Node/npm, Rust, gitleaks, cargo-deny, sccache, and Web dependencies | Security and Licenses |
| `native` | Rust and sccache | Native Ocserv |
| `g6-secret-scan` | gitleaks and host `jq`/`openssl` | G6 Readiness Secret Scan; G6 Harness Smoke Secret Scan |
| `native-packages` | Rust and nfpm (Linux `aarch64` mirrors only this profile) | Release Packages build matrix |

Stage contracts, credential rotation, Local Slice, Browser E2E, and P1 Smoke
use runner-provided tools or artifacts and do not call bootstrap. Outside
GitHub Actions, `make bootstrap` explicitly selects the complete `all` profile.

## Caches

Tool caches contain only `.cache/downloads` and `.tools`. Their exact keys
include the profile, runner OS and architecture, `toolchains.lock`, checksums,
bootstrap code, and environment setup. They have no broad restore prefix. A
cache miss is supported because bootstrap revalidates versions and checksums.

| Tool-cache profile | Main-branch writer | Restore consumers |
| --- | --- | --- |
| `go-rust-integration` | Build Runtime Artifacts | Build Runtime Artifacts; I18 Production Relays |
| `go-quality` | Go Static and Unit | Go Static and Unit |
| `go-test` | Go Race | Go Race; PostgreSQL 17/18 Integration |
| `web` | Web Static and Unit | Web Static and Unit |
| `contracts` | Contracts and Policy | Contracts and Policy |
| `rust-validation` | Rust Validation | Rust Validation |
| `security` | Security and Licenses | Security and Licenses |
| `native` | Native Ocserv | Native Ocserv |

The release-packages workflow writes its own `native-packages` tool cache from
its per-architecture build matrix jobs.

The shared Go cache contains `.cache/go-build`, `.cache/go-mod`, and
`.cache/gopath`. Build Runtime Artifacts, Go Static and Unit, Go Race,
PostgreSQL 17/18, I18 Production Relays, and Security and Licenses restore it.
Its primary key includes the exact commit and Go dependency inputs, with
dependency and platform prefix fallbacks. Go Static and Unit is its only
writer.

The shared npm cache contains `.cache/npm`. Web Static and Unit, Contracts and
Policy, and Security and Licenses restore it; Web Static and Unit is its only
writer. All explicit tool, Go, and npm cache writes require a successful push
to `main` and a primary-key miss. Pull-request workers are restore-only. CI
does not cache `node_modules`, credentials, environment files, logs, test
artifacts, or `rust/target`.

Rust compiler outputs use sccache instead of archiving `rust/target`. Build
Runtime Artifacts, I18 Production Relays, Rust Validation, Security and
Licenses, and Native Ocserv:

- request repository-pinned sccache `0.17.0` through the SHA-pinned sccache
  Action;
- set `RUSTC_WRAPPER=sccache`, disable Cargo incremental compilation, and
  normalize the workspace base path;
- select the GitHub Actions backend only when its runtime credentials and
  cache endpoint are present;
- fall back to the local `.cache/sccache` directory outside that environment.

The downloaded sccache binary and platform checksums are pinned in the same
toolchain files as other bootstrap tools. Native Ocserv preserves the required
sccache environment across its root fixture while keeping its Cargo target in
a unique directory below `RUNNER_TEMP`.

## Runtime artifact

Build Runtime Artifacts compiles `ocserv-control` and
`ocservia-transportd-stub`, then creates
`runtime-<run>/runtime-artifacts.tar.gz`. Its manifest records the
full candidate commit and a SHA-256 digest for each executable. Extraction
allows exactly the manifest and two expected binaries, rejects unsafe or
unexpected entries, validates both digests, and verifies that the manifest
commit equals `GITHUB_SHA`. The name omits the run attempt and the upload
overwrites, so "Re-run failed jobs" reuses the artifact the successful build
already produced while a full re-run replaces it; the manifest check still
binds the artifact to the exact commit.

PostgreSQL 17, PostgreSQL 18, and Local Slice download and validate that
artifact instead of rebuilding the same binaries. The PostgreSQL matrix uses
`fail-fast: false`, so a failure in one major does not cancel evidence from the
other. The PostgreSQL 18-only legacy upgrade fixture runs for `PG_MAJOR=18` or
the local `PG_MAJOR=all` mode; it is not repeated in the PostgreSQL 17 worker.

## Diagnostics

Uploads use SHA-pinned Actions, one-day retention, and names bound to the run
(except the reusable runtime artifact) or to the run and attempt:

| Worker | Artifact name |
| --- | --- |
| Build Runtime Artifacts | `runtime-<run>` |
| Go Static and Unit | `go-standard-<run>-<attempt>` |
| Go Race | `go-race-<run>-<attempt>` |
| PostgreSQL 17/18 | `database-pg<major>-<run>-<attempt>` |
| I14-I19 Contracts | `stage-contracts-<run>-<attempt>` |
| I18 Production Relays | `production-relays-<run>-<attempt>` |
| PostgreSQL Credential Rotation | `credential-rotation-<run>-<attempt>` |
| Local Slice | `local-slice-<run>-<attempt>` |
| Web Static and Unit | `web-validation-<run>-<attempt>` |
| Browser E2E | `browser-e2e-<run>-<attempt>` |
| P1 Smoke | `p1-smoke-<run>-<attempt>` |
| Contracts and Policy | `contracts-<run>-<attempt>` |
| Rust Validation | `rust-<run>-<attempt>` |
| Native Ocserv | `native-ocserv-<run>-<attempt>` |
| P1 Full Validation | `p1-full-<run>-<attempt>` |

Security and Licenses has no separate diagnostic artifact; its secret and
license results remain in the job log. Browser E2E and P1 Smoke use distinct
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

Branch protection requires only the three stable result aggregators:

- `Backend Integration`
- `Web & Smoke`
- `Quality, Security & Native`

The worker names are visible checks but are not configured individually as
required checks. Backend Integration waits for its nine worker job definitions,
including both PostgreSQL matrix executions. Web & Smoke waits for its three
workers. Quality, Security & Native waits for Contracts and Policy, the shared
Rust Validation execution, Security and Licenses, and Native Ocserv.

Do not rename an aggregator until the branch ruleset has first been migrated to
a successful check with the replacement name. A change is fully validated only
when all three aggregators pass for its exact commit, diagnostic uploads and
scoped cleanup complete, and any independent gate remains satisfied or is
explicitly recorded as pending.

## Release packages workflow

`.github/workflows/release.yml` builds the Agent distribution outside the
primary CI graph. It triggers on a published release (tags matching
`^v[0-9]+\.[0-9]+\.[0-9]+$`) and on manual `workflow_dispatch`, which always
stays a dry run and never uploads release assets.

- The build job runs as a two-leg matrix on native runners
  (`ubuntu-24.04` for `amd64`, `ubuntu-24.04-arm` for `arm64`, no emulation).
  Each leg bootstraps the `native-packages` profile, builds the release
  binaries natively, produces the signed archive triple plus `.deb` and
  `.rpm`, and runs `scripts/release-native-package-smoke.sh`: a full
  deb install/upgrade/removal lifecycle on the runner plus an rpm
  install/upgrade/erase lifecycle inside a systemd-enabled Rocky Linux 9
  container built for the run. A failure on either architecture fails the
  workflow before any assets can be published.
- The validate job downloads both legs' artifacts and runs
  `scripts/validate-release-packages.sh`: presence and naming of the six
  packages, both signed triples verified against the pinned fingerprint,
  `MANIFEST` and ELF architecture agreement, deb/rpm architecture metadata,
  identical embedded payloads, and a canonical `SHA256SUMS`.
- The publish job runs only for release events with `contents: write`, signs
  `SHA256SUMS` with the `AGENT_SIGNING_KEY` secret, revalidates against the
  `AGENT_TRUSTED_KEY_SHA256` pin, and only then uploads the assets to the
  release. Without the signing secret the build jobs sign with an ephemeral
  key, which the publish job rejects.

## Deferred native validation

The `native_user_and_group_operations` ignored test and the real I13 loopback
login run in Native Ocserv. Pure adapter logic remains in ordinary Rust tests.
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
