# GitHub Actions validation

GitHub Actions is the authoritative merge-time validation environment. The
primary workflow runs three independent execution jobs in parallel on fresh
GitHub-hosted `ubuntu-24.04` VMs with read-only repository permissions. Pull
requests and pushes to `main` run the workflow, and maintainers can also start
it manually. Local commands reproduce behavior but never replace required
checks.

During the required-check migration, a temporary `CI Gate` compatibility job
depends on the three execution jobs. It is removed after the branch ruleset
requires the three execution checks directly.

## Coverage

| Job | Commands and level | Bootstrap profile | Trigger | Hard timeout |
| --- | --- | --- | --- | --- |
| Backend Integration | Go checks, PostgreSQL 17/18, I14-I19 boundaries, production topology and relay recovery, and the Go/PostgreSQL/UDS/Rust-stub local slice | `go-integration` | PR, `main`, manual | 40 minutes |
| Web & Smoke | Web static/unit/build/audit checks, isolated Compose Playwright desktop/mobile E2E, P1 harness tests, and the unchanged 24-Agent smoke profile | `web` | PR, `main`, manual | 35 minutes |
| Quality, Security & Native | repository policy, docs, workflow policy, contracts, Rust, privilege and transport boundaries, secret and license checks, then native Ocserv | `ci-quality` | PR, `main`, manual | 30 minutes |
| P1 Full Validation | default 500-Agent single-VM profile and all fault phases | none | manual only | 45 minutes |

The focused I10 journal tests are included in the Rust workspace run. The I11
Go/Rust tests are included in the language checks and its boundary assertions
run in the quality job. Separate focused jobs would repeat the same expensive
workspace tests, so their scripts remain local reproduction commands.

Backend Integration also validates the hardened production Compose topology, a
controlled two-relay failover, PostgreSQL base-backup restore, and the signed
Agent package install, upgrade, uninstall, and state-retention lifecycle.

Native Ocserv is deliberately last in Quality, Security & Native. Its ephemeral
package installation and root fixture cannot affect policy, contract, Rust,
security, or license validation earlier on the VM. The fixture still captures
diagnostics before performing scoped cleanup and fails if cleanup or artifact
secret scanning fails. Its root-side Cargo build uses a unique target below
`RUNNER_TEMP`, which is removed after the fixture, so it cannot change the
user-owned `rust/target` cache.

P1 Full Validation remains a separate `workflow_dispatch` workflow because the
500-Agent profile is capacity evidence, not ordinary pull-request feedback. The
primary workflow runs the unchanged smoke parameters only.

## Reproducibility and caches

`toolchains.lock` is the only version source and `scripts/checksums.txt`
authenticates downloaded binaries. Each execution job invokes exactly one
explicit bootstrap profile. The combined `ci-quality` profile installs Go,
Node/npm, Rust, Buf, OpenAPI Generator, oasdiff, gitleaks, rustfmt, clippy,
`cargo-audit`, and `cargo-deny`, then installs Web dependencies once. It relies
on the runner's Java runtime for OpenAPI Generator and does not install protoc
because contract generation uses pinned Buf remote plugins.

Cache ownership is intentionally narrow:

| Job | Cached paths |
| --- | --- |
| Backend Integration | `.cache/downloads`, `.tools`, `.cache/go-build`, `.cache/go-mod`, `.cache/gopath` |
| Web & Smoke | `.cache/downloads`, `.tools`, `.cache/npm` |
| Quality, Security & Native | `.cache/downloads`, `.tools`, `.cache/npm`, `.cache/go-mod`, `rust/target` |

Keys include the lockfiles and scripts that determine their contents. There are
no broad restore keys. A cache miss is fully supported, and bootstrap still
checks versions and checksums after restore. CI never caches `node_modules`,
credentials, environment files, logs, or test artifacts. Backend does not cache
`rust/target`; Quality owns that build cache. Locally, `make bootstrap`
explicitly selects the complete `all` profile.

All external Actions are pinned to full commits. Checkout does not persist its
credential. The workflows do not use secrets, `pull_request_target`, production
services, self-hosted runners, or application access to the Docker socket.

## Artifacts and debugging

Each execution job uploads one top-level artifact named with the workflow run
and attempt:

| Artifact | Suite directories and contents |
| --- | --- |
| `backend-<run>-<attempt>` | Go check, database integration, I14-I19, and local-slice logs and diagnostics |
| `web-smoke-<run>-<attempt>` | Web check log, Playwright report/results/traces/screenshots/videos, P1 metrics, fault outputs, and Compose diagnostics |
| `quality-<run>-<attempt>` | generated-output diff and status, Rust check log, and native Ocserv/OpenConnect diagnostics without keys or passwords |
| `p1-full-<run>-<attempt>` | manual full-profile parameters, metrics, resource samples, fault outputs, disk snapshots, Compose logs, and exit status |

E2E and P1 Smoke have different `RUN_ID`, `COMPOSE_PROJECT`, and artifact
directories. Their scripts capture diagnostics before cleanup, run Compose
`down --volumes --remove-orphans`, remove only their scoped resources, preserve
the original test result, and turn cleanup leftovers into job failures.

Open the failed workflow run, select the named step, then download its job
artifact and inspect the suite log or exit status. Reproduce from the same
commit with `make bootstrap` and the script named by the step. Use unique
`RUN_ID` and `COMPOSE_PROJECT` values for Docker tests. `make verify` covers the
local language, contract, security, generated, and policy baseline; `make e2e`
reproduces the browser stack.

## Required checks

The final branch ruleset requires all three execution check names with strict
up-to-date checking:

- `Backend Integration`
- `Web & Smoke`
- `Quality, Security & Native`

GitHub's required-check AND semantics replace the old single-runner gate. Never
remove the compatibility `CI Gate` from the workflow while a ruleset still
requires it. Confirm the three new checks succeed first, update only the
ruleset's required-status-check set, read it back, and then remove the
compatibility job.

## Deferred native validation

The `native_user_and_group_operations` ignored test and the real I13 loopback
login are automated in Quality, Security & Native. Pure adapter logic remains
in ordinary Rust tests. These tests are not reported as hosted validation:

- `native_controlled_operations` needs prepared live sessions, an IP ban, and a
  real `ocserv.service` lifecycle. A future isolated fixture must create those
  states without touching a runner service outside the test.
- `native_reload_failure_is_bounded` depends on a deliberately stopped native
  service. It should move with the same ephemeral systemd fixture.
- `relay_only_connection_and_disabled_relay_failure` and
  `relay_and_direct_paths_converge_to_direct` depend on a public Iroh relay and
  remain deferred. The required Backend Integration check instead uses two
  controlled local relays and deliberately removes the first relay before
  proving reconnection through the second.

A change is fully validated only when required checks for its exact commit pass,
diagnostic artifacts are available, cleanup succeeded, and any deferred native
or independent gate requirement is completed in its defined environment or
explicitly remains `IN_REVIEW`.
