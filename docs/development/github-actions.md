# GitHub Actions validation

GitHub Actions is the authoritative merge-time validation environment. Every
formal job uses a fresh GitHub-hosted `ubuntu-24.04` VM with read-only repository
permissions. Pull requests and pushes to `main` run the primary workflow;
maintainers can also start it manually. The full P1 profile is a separate manual
workflow. Local commands reproduce behavior but never replace required checks.

## Coverage

| Job                      | Commands and level                                                                     | Dependencies                                    | Trigger            | Typical budget   |
| ------------------------ | -------------------------------------------------------------------------------------- | ----------------------------------------------- | ------------------ | ---------------- |
| Public Repository Policy | repository policy tests and `docs-check.sh`                                            | Git                                             | PR, `main`, manual | under 2 minutes  |
| Toolchain Bootstrap      | `bootstrap.sh` and checksum/version verification                                       | pinned downloads, Java, jq, ShellCheck          | PR, `main`, manual | under 5 minutes  |
| Contracts                | generation, Buf format/lint/breaking, OpenAPI lint/compatibility, generated-clean      | pinned toolchain, Git history                   | PR, `main`, manual | 2-5 minutes      |
| Go                       | format, vet, staticcheck, unit, race, govulncheck                                      | Go modules                                      | PR, `main`, manual | 3-8 minutes      |
| Database Integration     | PostgreSQL 17/18 fresh, up/down/forward, roles, audit, schema and Go integration tests | Docker, loopback dynamic ports                  | PR, `main`, manual | 3-10 minutes     |
| Local Slice Integration  | Go, PostgreSQL, Rust transport stub, UDS, simulator, SSE and restart behavior          | Docker, Go, Rust, `/proc`                       | PR, `main`, manual | 3-10 minutes     |
| Rust                     | fmt, check, clippy, workspace tests, docs, audit and deny                              | Cargo                                           | PR, `main`, manual | 5-15 minutes     |
| Native Ocserv            | native `ocpasswd`/OpenSSL adapter and real loopback Ocserv login                       | ephemeral Ubuntu packages and safe root fixture | PR, `main`, manual | 5-15 minutes     |
| Web                      | format, lint, typecheck, unit, generated-client checks, build and npm audit            | Node/npm                                        | PR, `main`, manual | 3-8 minutes      |
| Browser E2E              | isolated Compose stack and Playwright desktop/mobile tests                             | Docker Compose                                  | PR, `main`, manual | 5-15 minutes     |
| P1 Smoke                 | boundary tests plus all resilience phases at reduced load                              | Docker Compose, curl, jq                        | PR, `main`, manual | 8-20 minutes     |
| Security and Licenses    | privilege/transport boundaries, secret scan and licenses                               | Git history and pinned scanners                 | PR, `main`, manual | 2-5 minutes      |
| P1 Full Validation       | default 500-Agent single-VM profile and all fault phases                               | Docker Compose, standard runner resources       | manual only        | up to 45 minutes |

The focused I10 journal tests are included in the Rust workspace run. The I11
Go/Rust tests are included in the language jobs and its boundary assertions run
in the security job. Separate focused jobs would repeat the same expensive
workspace tests, so their scripts remain local reproduction commands.

## Reproducibility and caches

`toolchains.lock` is the version source and `scripts/checksums.txt` authenticates
downloaded binaries. CI does not add an independent setup action or toolchain
version source. Cache keys include runner OS and architecture plus the relevant
toolchain, checksum, Go, Cargo, or npm lockfiles. Tool downloads, Go build/module
caches, Rust targets, and npm's download cache may be restored. A cache miss is
fully supported, bootstrap still checks versions and checksums after restore,
and `node_modules`, credentials, environment files, logs, and test artifacts are
never cached.

All external Actions are pinned to full commits. Checkout does not persist its
credential. The workflows do not use secrets, `pull_request_target`, production
services, self-hosted runners, or application access to the Docker socket.

## Artifacts and debugging

Integration scripts write only to `RUNNER_TEMP`, capture diagnostics before
cleanup, remove their own uniquely named resources, and fail when scoped
resources remain. Artifacts are named with the workflow run and attempt:

| Suite                   | Artifact contents                                                                                                   |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------- |
| Contracts               | generated-output diff and Git status                                                                                |
| Go, Rust, Web           | complete check log                                                                                                  |
| Database Integration    | control-plane/migration logs, PostgreSQL 17/18 logs and container status                                            |
| Local Slice Integration | control-plane, transportd, PostgreSQL, SSE and container-status logs                                                |
| Native Ocserv           | Ocserv and OpenConnect logs without keys or passwords                                                               |
| Browser E2E             | HTML report, test results, failure traces/screenshots/videos, Compose logs and status                               |
| P1 Smoke/Full           | parameters, metrics, summary, resource samples, fault outputs, disk snapshots, Compose logs, status and exit status |

Open the failed workflow run, download the artifact for its job, and inspect the
exit-status or suite log first. Reproduce from the same commit with
`make bootstrap` and the script named by the job; set a unique `RUN_ID` or
`COMPOSE_PROJECT` for Docker tests. `make verify` covers the local language,
contract, security, generated, and policy baseline, while `make e2e` reproduces
the browser stack.

## Deferred native validation

The `native_user_and_group_operations` ignored test and the real I13 loopback
login are automated in `Native Ocserv`. Pure adapter logic remains in ordinary
Rust tests. These tests are not reported as hosted validation:

- `native_controlled_operations` needs prepared live sessions, an IP ban, and a
  real `ocserv.service` lifecycle. A future isolated fixture must create those
  states without touching a runner service outside the test.
- `native_reload_failure_is_bounded` depends on a deliberately stopped native
  service. It should move with the same ephemeral systemd fixture.
- `relay_only_connection_and_disabled_relay_failure` and
  `relay_and_direct_paths_converge_to_direct` depend on a public Iroh relay and
  are unsuitable as deterministic required checks. A controlled disposable
  relay fixture is needed.

A change is fully validated only when all required checks for its exact commit
pass, applicable diagnostic artifacts are available, cleanup succeeded, and any
deferred native or independent gate requirement is either completed in its
defined environment or explicitly remains `IN_REVIEW`.
