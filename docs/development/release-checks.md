# Validation and release

Pick the smallest validation target that covers the code or documentation you
touched, and run compilation, linters, static checks, and tests inside a
task-isolated environment that has the required dependencies. If that
environment is unavailable, report which checks were blocked.

Validation runs in separate layers. Each layer covers a different scope, and
a pass in one layer does not substitute for another:

- Local checks run what you choose on your own checkout. GitHub Actions remains
  authoritative for the pull request commit itself.
- `make verify` is a broad local baseline. It starts no database and is not
  Basic CI.
- Basic CI (required check `Basic CI Result`) checks the changed units of one
  pull request or `main` commit.
- Release Check qualifies one merged `main` commit and intended version. It
  uses same-commit Quick CI and Security results, runs any missing release
  contract and database checks, and builds and qualifies the exact release
  artifacts. Integrated Business Smoke and Pebble ACME Quick Install run on
  `amd64` as soon as their inputs are ready.
- Tag pushes trigger Release to verify the matching qualification and publish
  its artifacts.

## Entry-point compatibility

The Controller accepts legacy `--role=all` and `OCSERV_DATABASE_BACKEND=postgres`
only at input boundaries; new deployments omit both. The local sealing export
CLI also accepts `--backend=postgres`. Unsupported values fail closed, and
split `api`, `worker`, or `scheduler` processes are unsupported even in tests.
The manual `database-controller-e2e.sh` harness still accepts its old
`postgres all` positional arguments at entry; new invocations omit them.
Keep acceptance/rejection tests at these entrypoints. Remove the compatibility
arguments only in an explicitly announced CLI/configuration contract change,
after supported installer and operator automation no longer supplies them.

The `/version` response retains `role: "all"` as a stable wire field. Database
engine identities in schema receipts, existing SQL artifacts and checksums,
persisted deployment-profile keys, historical audit values, journals, and signed
protocol bytes retain their original identifiers; they are not runtime configuration selectors.

## Choose the validation scope

Validation converges onto four explicit tiers:

| Tier | Purpose | Primary entrypoints |
| --- | --- | --- |
| Unit | Fast in-memory domain, protocol, UI, and CI-guard contract checks | `scripts/go-check.sh standard`, `make rust-check`, `scripts/web-check.sh basic`, `scripts/ci-tools-check.sh` |
| Key Integration | Live PostgreSQL 18 schema and API checks, native ocserv adapter integration, and Controller/Agent lifecycle suites | `scripts/database-postgres-smoke.sh`, `DATABASE_TEST_SCOPE=full PG_MAJOR=18 scripts/database-integration.sh`, `scripts/native-ocserv-integration.sh` |
| Security Regressions | Single-admin workspace boundary, authentication/session, fencing/outbox regressions, secret scanning, and dependency/image vulnerability gates | `regression-*` groups in `scripts/required-go-tests.txt`, `scripts/security-check.sh`, `security.yml`, `release-controller.yml` |
| Release E2E Acceptance | Playwright browser suite, Integrated Business Smoke with single-instance & sole-Relay offline-queue recovery, and Pebble ACME Quick Install | `scripts/web-check.sh full` / `make e2e`, `release-check.yml` |

When a change only touches documentation, run the focused checks:

```bash
make docs-check
make policy-check
git diff --check
```

Check external URLs, HTML `<a id>` anchors, and untracked public files manually
as well: `scripts/docs-check.sh` scans tracked files only. Keep local-only
notes out of the candidate index and leave the user's git staging state
untouched.

`scripts/docs-check.sh` rejects CRLF line endings, requires every tracked
Markdown file to be non-empty, restricts shared `.claude/settings.json` to
audited keys, checks bootstrap text rules (including the absence of
pipe-to-shell install examples in the entry pages), and verifies that
`AGENTS.md` exists, `CLAUDE.md` imports it, and relative Markdown links and
ASCII GitHub heading anchors resolve across `README.md`, `AGENTS.md`,
`CLAUDE.md`, `SECURITY.md`, and `docs/*.md`. It does not check external URLs,
duplicate-heading suffixes, SQL syntax, authentication modes, database support
claims, or other prose facts. `make policy-check` rejects tracked
local implementation-control paths (including `AGENTS.override.md`) and private
disclosures. In Basic CI, the `docs` job runs `docs-check.sh` and
`check-public-repository.sh`. Validate executable command examples in the docs in
an isolated environment.

For a new workspace, run the supported bootstrap profile that matches your
target check and host platform; tool versions come from
[`toolchains.lock`](../../toolchains.lock) and downloads are verified against
[`scripts/checksums.txt`](../../scripts/checksums.txt). `make bootstrap`
installs the `all` profile, which most single-module edits do not need (see
[Linux ARM64 Go validation](#linux-arm64-go-validation) for platform details). Reuse a prepared
environment across changes. Direct `go`, `cargo` or `npm` runs need
`source scripts/env.sh` first; the system versions are not the locked toolchain.
The local development stack is started as described in the
[README](../../README.md).

For day-to-day work inside a single module, use `make test-go`,
`make test-rust`, or `make test-web`. `make test-go` runs `go test ./...` in
`control-plane` and `signer` without `gofmt`, `go vet`, `-count=1` or race
runs. `make go-check` (`scripts/go-check.sh`, default mode `full`) covers both
`control-plane` and `signer` with `gofmt`, `go vet`, and `go test -race -count=1`
(or non-race `go test -count=1` in `standard` mode), using a private `0700` `TMPDIR`
outside the repository checkout (such as `$XDG_RUNTIME_DIR` or `$HOME/.cache`)
because the signer store and UDS checks refuse state directories beneath a
group- or world-writable parent such as `/tmp` or a `0775` checkout. Basic CI invokes
`go-check.sh standard` in Quick runs and `go-check.sh full` in Full runs. The top-level `make test` (`scripts/test.sh`) runs
`control-plane` and `signer` Go tests, Rust workspace tests, and Web `npm test`
without linters or race runs. For Rust, `make rust-check` runs
`cargo fmt --check`, Clippy with `-D warnings`, and workspace tests. Web checks
are in [Web validation](#web-validation).

## Choose a broader check when needed

Use `make verify` when a change spans multiple modules and needs a full local
baseline, not for small single-module edits. `make verify` (`scripts/verify.sh`)
runs `scripts/lint.sh common` (ShellCheck, Ruff, Buf, OpenAPI lint,
`check-public-repository.sh`, `docs-check.sh`), the Buf breaking-change check,
`go-check.sh` in full mode, `rust-check.sh`, the transport and Agent boundary
checks, `web-check.sh` in full mode, the toolchain consistency self-test,
`security-check.sh` (Gitleaks), `license-check.sh`, and the generated-code checks.
It does not start database containers or
run `make integration`, `make e2e`, the CI router/selector/bootstrap self-tests,
the installer or release self-tests, or manual acceptance harnesses.

Pick the entry point that matches the behavior under test:

- Quick database feedback:
  `DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 scripts/database-integration.sh`.
  PostgreSQL smoke runs `TestControllerLockSessionLoss`,
  `TestDatabaseCoreSmoke`, `TestDatabaseInitializationSmoke`, and the enrollment
  restart check.
- Full database migrations and failure scenarios: `make database-integration`
  (PostgreSQL, where `PG_MAJOR=all` and `18` both target PostgreSQL 18).
- Local Go and transport integration: `make integration` runs
  `scripts/development-simulation-integration.sh`, pairing the `control-plane` binary with
  a PostgreSQL container and the Rust `ocservia-transportd-stub` over local
  Unix sockets, without real `transportd`, an Agent, a Relay, or a node.
- Browser and runtime stack: `make e2e` runs `scripts/e2e.sh` (PostgreSQL,
  `transportd-stub`, Controller and Web under Docker Compose, then the
  Playwright `e2e` service); it does not exercise a native node.
- Real processes without a native host:
  `scripts/database-controller-e2e.sh` runs
  `TestControllerTransportBackendE2E` with the real Controller, `transportd`,
  Agent, `privd`, Relay, and ocserv in one container (no systemd PID 1, no
  managed-node installer). It is manual and outside CI; native systemd behavior
  belongs to [Business validation](#business-validation).
- Release qualification: [Business validation](#business-validation) and
  [Release policy](#release-policy) (module tests and browser fixtures do not
  replace them).

The database-backed Go groups over the Controller services and HTTP layer are
`backend-policy-config`, `backend-policy-useroperations`, `backend-policy-api`,
`regression-auth` and the startup group. They run over restricted runtime
database connections and cover signed command intent, transactional session and
workspace-admin checks and rollback, policy cleanup errors, leadership fencing,
HTTP status ordering, secret access denials, and first-request server assembly.
Owner credentials are for schema setup and fault injection only. Every required
case must execute: skipped database tests do not satisfy acceptance, and HTTP
intent tests do not verify Agent execution, package installation, or release
readiness.

Native ocserv adapter integration runs via `scripts/native-ocserv-integration.sh`
as root in a disposable Linux environment with TUN and `NET_ADMIN`.

### Command wire contracts

Go's `commandauth.TestStrictWireCommandFixtures` and Rust's
`contracts::strict_wire` suite share `testdata/command-strict-wire.json`: the
historical vectors plus every payload variant populated with non-default field
values, and a full echo envelope for authorization, owner fences, timestamps and
envelope metadata. The Go test uses reflection to verify that at least one
fixture populates every field reachable from `CommandEnvelope`, and the Rust
suite asserts the decoded values on its own.

The Rust tests compare the hand-maintained wire policy against the generated
`FILE_DESCRIPTOR_SET` rooted at `CommandEnvelope`, and Go writes the imported
`Timestamp` descriptor into the fixture file and checks it on each run. The
suites test message boundaries, scalar wire types and reserved tag holes across
tags 1 through 128, and reject unknown tags, all five mismatched wire types, and
truncated nested messages. Descriptor-mutation tests confirm that adding or
removing fields, altering wire or nested message types, or adding payload
variants fails the check. Production allowlists are never auto-generated from
descriptors; adding a protocol field requires an explicit review of the
protocol version, capability negotiation, and strict wire policy. The vectors
test raw wire decoding, not authenticated execution: they populate deprecated
fields, carry dummy signature bytes, and do not relax protocol 1.1's runtime
rejection of legacy password fields. Canonical signing inputs, owner fences, and
privd receipts have their own golden suites.

Run the focused contract suites from an isolated checkout:

```bash
(cd control-plane && go test -race -count=1 -skip Integration ./internal/commandauth ./internal/semanticpayload ./internal/privdattestation)
(cd rust && cargo test --locked -p ocservia-contracts -p ocservia-command-authorization -p ocservia-privd-attestation)
```

When intentionally updating the wire fixtures, run the Go test with
`UPDATE_STRICT_WIRE_FIXTURES=1` (an explicit fixture generator, not an automatic
golden-file updater), then rerun both suites without that variable and inspect
the diff. After regenerating protobuf code, run the Buf generation and
breaking-change checks and `scripts/generated-clean.sh --skip-generate`; if the
OpenAPI schemas did not change, skip Web client generation. The focused Go
command skips database integration tests. Separately,
`scripts/test-secret-scan-config-runtime.sh` verifies that the fixed public
fixture values pass the exact-value Gitleaks allowlist while modified secrets
are still caught.

### SQL artifact acceptance

`scripts/database-artifact-policy.sh postgres` checks the embedded PostgreSQL 18
migration artifacts. Run `PG_MAJOR=18 scripts/database-integration.sh` against
live PostgreSQL for schema initialization, runtime permissions, transaction
checks and migration replay. The external PostgreSQL backup and restore smoke
check verifies that the Goose version history survives a restore and that the
restored runtime role can read but cannot perform owner-only DDL.

Before cutting over a major database change, qualify the intended version and
candidate commit on `main` through Release Check.

### Bundled PostgreSQL initialization

Validate bundled PostgreSQL initialization in an isolated Linux Docker environment using the digest-pinned PostgreSQL 18 image and `scripts/database-integration.sh` / `scripts/test-postgres-volume-layout.sh`; never test with real credentials or against production volumes.

### Signer and certificate verification

Run the suite with the pinned Go and Rust toolchains, a checkout under trusted
directory ancestry, and a private `TMPDIR`. Tests generate disposable fixture
CAs and node keys only.

```sh
cd signer
GOWORK=off go test -race -count=1 ./...
GOWORK=off go test -c -o /absolute/task/signer-interop.test
cd ../control-plane
GOWORK=off go test -race -count=1 -skip Integration ./internal/certificates ./internal/platform/config ./internal/platform/app ./internal/enrollment ./cmd/ocserv-sealing-export
# With an isolated, migrated PostgreSQL database and restricted runtime account:
GOWORK=off go test -race -count=1 ./internal/enrollment -run '^TestEnrollmentBackendIntegration$' -v
cd ../rust
OCSERV_SIGNER_INTEROP_HELPER=/absolute/task/signer-interop.test \
SIGNER_NODE_EXPORT_SCRIPT=/absolute/repo/scripts/export-node-sealing-keys.py \
cargo test --locked -p ocservia-ocserv-adapter certificate_p12_is_encrypted_bounded_and_replayable -- --nocapture
```

The Rust interop test exercises the privd adapter and OpenSSL decryption, P12
bundle creation, wrong-purpose rejection and idempotent replay against
ciphertext produced by the Go HTTP handler from node-script key exports. It
covers adapter interoperability only; full privd RPC and Controller end-to-end
flows, registry image checks and live ocserv CRL enforcement belong to
Business Smoke (see [Business validation](#business-validation)). Unit and subprocess
fault-injection tests cover crash boundaries during preparation, signing,
pre-commit and post-commit lost replies, concurrent duplicates, ledger
corruption, recovery floors, TLS trust and SAN isolation, authentication,
request size limits and disabled bindings.

### Linux ARM64 Go validation

`scripts/bootstrap.sh` installs pinned repository tools without managing host
OS packages. Linux `aarch64`, Linux AMD64, and Darwin ARM64 all provide full
artifact mappings across every bootstrap profile (including `all`, `web`,
`source-style`, `contracts`, and security profiles). The ARM64 Go procedure
below covers only Linux ARM64 Go, not other platforms or Rust acceptance.

On Linux ARM64, `scripts/bootstrap.sh go-test` fetches
`go<VERSION>.linux-arm64.tar.gz` for the version locked in `toolchains.lock`
from `https://go.dev/dl/`. The SHA-256 digests in `scripts/checksums.txt` come
from the [official download manifest](https://go.dev/dl/?mode=json&include=all),
never from hashing an unverified download, and a missing checksum or a mismatch
aborts before extraction. The archive is unpacked into a temporary
`.tools/.go-install-*` directory, and the new binary must report the locked
`GOVERSION` and native `GOHOSTOS`/`GOHOSTARCH` before it replaces `.tools/go`.
Sourcing `scripts/env.sh` sets `GOTOOLCHAIN=local`, puts repository tools first
in `PATH`, and directs Go caches to `.cache/go-build`, `.cache/gopath` and
`.cache/go-mod`. It never falls back to a system Go, downloads toolchains on the
fly, touches `/usr/local/go`, or edits your shell profile. Bootstrap checks for
`jq` only, so a passing bootstrap does not mean the host is ready for every
suite.

#### Dependencies by entrypoint

Every script expects Bash and standard Unix file and process utilities. Beyond
that, prepare only what the entrypoint you run requires:

| Entrypoint | Additional host dependencies |
| --- | --- |
| `bootstrap.sh go-test` | curl with trusted CA certificates, tar, gzip, sed, awk, `sha256sum` or `shasum`, jq |
| `go-check.sh standard` | Installed Go/gofmt |
| `docs-check.sh` | Git and `jq`; no toolchain or platform self-tests |
| `go-check.sh race` (also the race part of `full`) | `CGO_ENABLED=1`, a C compiler selected by `go env CC`, linker and C development headers; no Docker requirement |
| `database-integration.sh` smoke (delegates to `database-postgres-smoke.sh`) | Go, jq, setsid, Python 3, Docker CLI and daemon, and the race prerequisites (cgo, C compiler) |
| `database-integration.sh` manual regression/full | The smoke set plus curl, sha256sum |

If a required command is missing, the Docker daemon is unreachable, cgo is
disabled, or the C toolchain cannot link a test binary, the scripts exit nonzero
before launching heavy suites or database containers. Preflight errors are hard
failures, never SKIP, and the race preflight never disables `-race` or overrides
`CGO_ENABLED` (see the
[Go race detector requirements](https://go.dev/doc/articles/race_detector#Requirements)).
The required-test wrapper preserves child exit codes, runs tests in their own
process groups, forwards SIGINT and SIGTERM, and cleans up stray subprocesses.

Database runs start disposable loopback-bound containers with generated
test-only owner and runtime credentials; never pass production credentials or
print DSNs to logs. If you invoke the `database-*` required-test groups
directly, set both `OCSERV_TEST_DATABASE_URL` and
`OCSERV_TEST_OWNER_DATABASE_URL`. Only the required-case summaries count as
acceptance evidence (synthetic selector JSON does not), and passing the
PostgreSQL 18 smoke suite does not certify the full database scope or release
readiness.

#### Native preparation and execution

Verify the host dependencies before starting, and never run sudo to install
global system packages as part of a validation task. Create the working
directory under a short, private path inside a trusted home directory. Avoid
`/tmp` and symlinks: the transport socket tests check directory ownership up the
ancestor chain and reject group- or world-writable directories (including sticky
`/tmp`), and deep paths can exceed the Unix socket length limit. Keep each task's
checkout, temporary directory, and evidence directory isolated:

```bash
task="$(mktemp -d "$HOME/oa-XXXXXX")"
git clone https://github.com/GentleKingson/ocservia.git "$task/repo"
cd "$task/repo"
# Check out the exact candidate commit before validation.
mkdir -p "$task/tmp" "$task/evidence"
test ! -e .tools/go
./scripts/bootstrap.sh go-test
source ./scripts/env.sh
command -v go
go version
go env GOVERSION GOHOSTOS GOHOSTARCH GOOS GOARCH CGO_ENABLED CC
./scripts/go-check.sh standard
./scripts/bootstrap.sh go-test
```

If the host already has a working C toolchain for race checks and Docker,
run the selector and bootstrap profile self-tests, race checks and database smoke
scripts directly on the host. Otherwise use the isolated container below.
Passing native bootstrap and ordinary Go checks does not show that race or
database prerequisites are present.

#### Isolated ARM64 execution environment

When the host lacks the extra userland tools, build a task-scoped ARM64 helper
container and mount the workspace into it. Rerun it for each candidate and keep
that candidate's evidence. The image omits Go so every command in the container
runs `.tools/go/bin/go` from the host and keeps system packages inside the
disposable image. The Debian base image is pinned by digest, while APT resolves
Bookworm packages at build time, so save the build log, image ID, and installed
package versions with the evidence instead of assuming `toolchains.lock` freezes
them.

```bash
image="ocservia-go-validation:$(basename "$task")"
docker build --platform linux/arm64 -t "$image" -f - "$task" <<'DOCKERFILE'
FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl tar gzip coreutils jq util-linux gcc libc6-dev \
    docker.io openssl patch python3 git shellcheck \
    && rm -rf /var/lib/apt/lists/*
ENV GOTOOLCHAIN=local
DOCKERFILE
docker image inspect --format '{{.Architecture}} {{.Id}}' "$image"
docker run --rm --init --name "$(basename "$task")-validation" \
  --network host --user "$(id -u):$(id -g)" \
  -v "$task:$task" -v /var/run/docker.sock:/var/run/docker.sock \
  -w "$task/repo" -e TMPDIR="$task/tmp" "$image" bash -euo pipefail -c '
    source scripts/env.sh
    go version
    go env GOVERSION GOHOSTOS GOHOSTARCH GOOS GOARCH CGO_ENABLED CC
    gcc --version
    scripts/go-check.sh standard
    DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 scripts/database-integration.sh
    scripts/docs-check.sh
    git diff --check
  '
```

Run the whole sequence inside a single `--init` container so the init process
reaps children and shell exports (including the `GOWORK=off` fixtures) stay
intact; pass outer variables with explicit Docker `--env` flags. Sharing the
host network lets the container reach the loopback database ports the scripts
start. Mounting the task directory at the identical absolute path on host and
container makes temporary files and TLS fixtures visible to the host Docker
daemon, and a remote Docker context cannot work for that reason. Docker socket
access grants elevated host privileges and fits only a trusted, nonproduction
validation environment. Matching UID:GID keeps file ownership consistent without
altering permission checks, SQL assertions, timeouts or race flags.

Record the exit codes and logs alongside the candidate SHA. Each script tears
down its own fixtures and database containers, and `--rm` removes the runner
container. After saving the evidence, delete only this task's helper image and
private directory. Leave shared Docker base images, caches, and other tasks'
`.tools` directories alone. In your report, distinguish initial failures,
skipped non-required integration tests, simulated platform checks, native host
Go runs, and containerized runs.

## CI layers

### Basic CI

[`ci.yml`](../../.github/workflows/ci.yml) handles pull requests, pushes to
`main` and manual dispatches. Automatic triggers run the Quick profile. Manual
dispatch accepts `quick|full` and defaults to Full. Release Check uses Quick as
its CI gate and reuses successful protocol and database checks from Full when
available for the same commit. A new Quick run cancels older Quick runs on the
same pull request or manual-dispatch branch. Full has its own concurrency group.

The workflow file defines the matrices. The jobs and what they own:

| Job | Responsibility |
| --- | --- |
| `docs` | `scripts/docs-check.sh` and `scripts/check-public-repository.sh` |
| `ci-tools` | Selected CI guard (including workflow contract checks), release, source style and contract suites via `scripts/ci-tools-check.sh` |
| `go` | `go-check.sh standard` in Quick runs and `go-check.sh full` (including `-race`) in Full runs for `control-plane` and `signer` |
| `rust` | `rust-check.sh` across the Rust workspace against the locked crates.io `iroh` dependency in [`rust/Cargo.lock`](../../rust/Cargo.lock) |
| `installers` | Installer (including host bootstrap) and Relay-launcher contract self-tests |
| `web` | `web-check.sh basic`; no browser installation |
| `web-browser` | Stubbed production-build browser regressions through `web/test/run-auth-browser.mjs` inside the version-matched Playwright container image (`PLAYWRIGHT_WORKERS=4`, with discarded trace/video recording disabled); Full serves the full regression spec set through the Standalone gateway and the core auth/install/CSP suite through the Integrated gateway with CSP enforcement checks. A missing browser or non-declared skip fails the job |
| `database-smoke` | PostgreSQL 18.x; Quick runs smoke, Full runs full database acceptance; both enforce Goose migration artifact policy via `scripts/database-artifact-policy.sh` (see [SQL artifact acceptance](#sql-artifact-acceptance)) |
| `database-recovery-full` | Bundled and external PostgreSQL physical backup/restore with Goose version-history verification, Full only |
| `Basic CI Result` | Always checks routing and the selected job results |

Database service images stay pinned to exact patches and digests.
`TestDatabaseCoreSmoke` migrates an empty database under the owner account,
starts the real Controller CLI under the runtime account, signs in a local
administrator, checks transaction commit, rollback and isolation, and keeps a
runtime DDL denial check so the Controller cannot pass with owner credentials.
Smoke runs use `-count=1`, and explicit inventories fail if a required case is
skipped (`required-go-tests.sh --smoke <package> <test>` checks only that the
named top-level test ran and passed). Pull requests that touch PostgreSQL
migration SQL or the migration runner should also run a candidate
Full workflow before merging so `database-recovery-full` and full database
acceptance execute.

The required check is `Basic CI Result` (job `basic-ci-result`). It passes only
when path routing succeeds, every selected job passes, and every unselected job
is skipped; a missing, failed, cancelled or unexpectedly skipped job fails it.
Repository administrators must configure GitHub rulesets to require this context
and remove retired status contexts, since editing workflow YAML does not update
branch protection. Passing Basic CI does not certify production readiness,
capacity, native packaging, cross-VM enrollment, full browser E2E, security
acceptance or license compliance.

### Path routing

From a clean candidate commit, run `scripts/ci-preflight.sh origin/main` to
print the PR routing decisions and run the selected CI tool suites locally.
Use the same base commit as the PR and install its selected tool profiles
with `scripts/bootstrap.sh`. GitHub runs CodeQL and the other selected jobs.
Register release tests in `scripts/ci-release-tests.txt`, one interpreter,
script path and optional whitespace-separated arguments per line. The suite
executes each entry directly and the router uses its path to select release checks.

[`scripts/ci-relevance.sh`](../../scripts/ci-relevance.sh) evaluates changed
paths with ordered first-match patterns instead of package or call-graph
analysis and is the source of the exact rule order:

- Rules for executable code and deployment configuration sit ahead of the
  generic Markdown fallback. Markdown under `web/`, `rust/`, `proto/`,
  `openapi/` or the `deploy/` subdirectories matches its subsystem rule and can
  trigger web, Rust, database, installer or full Quick suites. Other Markdown,
  `docs/upstream/*`, repository metadata and `scripts/docs-check.sh` select
  `docs` alone, apart from exceptions named in the script.
- `web/*` selects `web` (`web/src/*`, `web/index.html`, `web/e2e/*`, Playwright
  harness and web build/lock files also select `web-browser`), `rust/*`
  selects `rust`, and `control-plane/{cmd,migrations,internal}/*`, Go locks and
  the database scripts select `go` plus PostgreSQL database smoke.
- Installer, release, package, Controller and Agent-upgrade scripts, and
  `deploy/{package,systemd,bootstrap,managed-node,production}` select the
  release contract suite and/or installer self-tests; `release*.yml` selects
  `guards` and `release` without unrelated installer self-tests. Manual-acceptance
  harnesses (`scripts/resilience-*`, `scripts/security-acceptance-*`) are
  checked for syntax/contracts but not executed in Basic CI.
- `ci.yml`, `security.yml`, `release*.yml`, `scripts/ci-*`, the required-test
  selectors, `test-ci-relevance.sh`, `test-toolchain-consistency.sh`,
  `test-workflow-contracts.py` and public-repository guards select the CI guard
  suite.
- `proto/*`, `openapi/*`, `control-plane/gen/*`, `scripts/bootstrap.sh`,
  `env.sh`, `checksums.txt`, `toolchains.lock`, `.github/actions/setup-tools/*`
  and any unknown path enable all Basic flags and every CI suite at Quick scope.

The `source-style` suite checks tracked Shell sources using their shebangs
(including extensionless launchers) and Python with the pinned Ruff
`E4,E7,E9,F` rules, plus the explicit spacing and blank-line rules
`E225,E226,E231,E302,E303,E305,E306` (which require preview mode in the pinned
version). The Web job also checks the maintained root MJS files and
build-cache Action with the same ESLint and Prettier configuration. The
`contracts` suite runs Buf/Redocly lint, breaking checks and a clean regeneration
in that order, when schemas, templates, generated outputs or generation tools
change. These suites run in the dedicated `ci-tools` job so they run in
parallel with `go` and only bootstrap `source-style`, `contracts` or `node`
tools when selected; ordinary documentation does not install their toolchains.

Contract comparison uses the PR merge-base or the push event before SHA. Manual
Basic CI requires an explicit `contract_base_ref` naming an existing commit
different from the candidate. Release Check accepts an optional
`contract_base_ref` and defaults to the latest `v*.*.*` tag reachable before
`HEAD` (falling back to `HEAD~1`). A successful CI contract check records its
baseline commit in the job steps. Release Check uses that result when the
baseline SHA matches, and otherwise compares against the release baseline.
An unresolvable or self baseline fails. `generated-clean.sh`
requires a clean committed candidate before regenerating and rejects changed or
untracked outputs. `--skip-generate` only compares with HEAD (including staged
changes); it does not prove reproducible generation.

When a change touches several areas, the router takes the union of their flags.
Pull requests diff `base...head`, push events diff `before..head`, and renames
or deletions evaluate both old and new paths. If a diff is empty, invalid,
unresolvable, from an unsupported event, or contains an unrecognized path, the
router fails closed by enabling every Basic flag and CI tool suite while keeping
the Quick profile. Path routing never escalates Quick to Full.

Manual dispatches bypass path routing: `docs`, `ci-tools` (`guards`, `release`,
`source-style` and `contracts`), `go`, `rust`, `web`, `web-browser`,
`installers` and database jobs always run. Controller cache refresh stays off.
A manual Full run executes the full PostgreSQL database suite and
`database-recovery-full`; a manual Quick run executes the PostgreSQL smoke leg.
Controller cache refresh runs only on pushes to main when routing sets
`run_controller_cache`. It refreshes the union of the changed images' Dockerfile
inputs on both native architectures, selects all images for shared build
machinery and unknown paths, falls back to all images on an empty selection, and
rejects invalid image names before Docker runs. Product builds always build all
images.

### Workflows

| File | Trigger / purpose |
| --- | --- |
| `ci.yml` | PR/main Quick and manual Quick/Full. Provides CI results for Release Check. |
| `security.yml` | Weekly and manual scans. Provides security results for Release Check. |
| `release-check.yml` | Manual `main` qualification for a commit and version. Builds both architectures, reuses upstream checks, and records the artifacts after all acceptance jobs pass. |
| `release-agent.yml` | Native Agent package builder called by Release Check. |
| `release-controller.yml` | Native Controller image build, exact archive scan and image smoke called by Release Check. |
| `release.yml` | Publishes the qualified artifacts on a matching version tag. |

### Security checks

`security.yml` runs on Mondays and on manual dispatch. Release Check uses the
latest same-commit `main` result and dispatches a scan if there is no matching
run. Its read-only scan jobs use pinned bootstrap profiles and no
production secrets: full-history `scripts/security-check.sh` (including its
Gitleaks rule regression check); `govulncheck ./...` in both Go modules;
`cargo audit --file deploy/production/relay.Cargo.lock` with
`cargo deny --locked check advisories bans licenses sources` across the
Rust workspace; `npm audit --package-lock-only --ignore-scripts` against `web/package-lock.json`,
including development dependencies; and `scripts/license-check.sh --skip-rust`
using `go-security` (pinned `go-licenses`) for Go (`control-plane` and `signer`)
and lockfile-direct inspection for Web dependency licenses without running `npm ci`.
Any scan failure blocks qualification. The result reflects the advisory databases
used at scan time.

### Caches

Go jobs keep module caches keyed by module lockfiles (`basic-go-mod-v2`) and
compiler build caches (`basic-go-build-v1`). Only passing pushes on main write
cache entries; pull requests, Security scans and manual runs restore only, and a
restored build cache never skips real database tests (Release database acceptance
restores the Go caches read-only when running on `release-check.yml`).
`.tools` is managed through
[`.github/actions/setup-tools`](../../.github/actions/setup-tools/action.yml)
and keyed by profile, OS, architecture and tool/bootstrap identity (with an
optional `Cargo.lock` suffix and base toolchain fallback when `.tools/cargo`
caches crates), without caching `.cache/downloads`. Web CI and `contracts`
checks cache `.cache/npm` instead of `node_modules`, and Web jobs still run
`npm ci`; lockfile-only `npm audit` and `Go & Web licenses` jobs skip
`.cache/npm`. Rust jobs restore `rust/target/debug` built with
`CARGO_INCREMENTAL=0` and `CARGO_PROFILE_DEV_DEBUG=0`, prune incremental/stale
workspace test binaries before saving only on passing pushes to main, and fall
back to the matching Rust toolchain prefix when manifests or lockfiles change.
Rust bootstrap resolves the components declared in `rust/rust-toolchain.toml`
before parallel security checks start, including after a tool cache restore.
Native Agent release builds key their isolated Rocky 9 compilation cache
(`release-agent-target-v3`) deterministically by Rocky glibc/gcc ABI, save only
on `main` (pruning older ABI directories), and `smoke-rc-lifecycle` restores it
read-only when running `RC_LIFECYCLE=true`. Image builds use per-image,
per-architecture BuildKit GitHub Actions caches. No workflow caches runtime
state or credentials, and build caches are never accepted release products or
qualification evidence.
Gateway and Edge release builds refresh all installed Alpine packages from the
pinned base image’s configured repositories. BuildKit bypasses the package
upgrade stage cache while retaining compiler and dependency caches.
Release Controller jobs cache image-security scanner binaries and overlap a
fresh uncached Grype vulnerability database download in `$RUNNER_TEMP` with
parallel image builds: each run fetches fresh advisory data and fails if the
feed is unavailable or invalid. Image scans inspect the built image archives
before smoke tests and registry uploads; Business diagnostic runs do not scan
images.

### Manual deep checks and reproduction

Run the standalone deep-validation scripts directly; they are separate from Full CI:

```bash
# In an authorized isolated environment with the required dependencies; expensive opt-in checks:
DATABASE_TEST_SCOPE=full PG_MAJOR=all scripts/database-integration.sh
scripts/go-check.sh race
scripts/web-check.sh full
```

Invoking the database scripts without a scope defaults to `full`; CI passes
`smoke` for Quick and `full` for Full, and the `regression` scope is manual.
Local browser checks need a separate Playwright Chromium install, plus
`firefox`/`webkit` for those projects. Disaster recovery, race and
fault-injection suites stay manual with no scheduled workflow. Capacity runs and
native security acceptance also stay outside Basic CI,
because they need dedicated resources or systemd/privd/PKI fixtures:

- `make resilience-smoke` and `make resilience-full` invoke `scripts/resilience-capacity.sh`
  (see [Resilience and capacity](#resilience-and-capacity)).
- Transport connection-budget and admission checks run via `make rust-check`.
- `scripts/security-acceptance-sse-capacity.sh` checks SSE delivery, reconnect,
  stream cleanup and resource bounds at 1, 10 and 100 viewers.
- `scripts/security-acceptance-privd.sh` checks the root boundary, systemd
  sandbox, attestation keys, certificate lifecycle and package upgrade/rollback.
  Results use `SSE_CAPACITY_ACCEPTANCE` and `PRIVD_ACCEPTANCE`, with
  `sse-capacity-summary.json` and `privd-summary.txt` respectively.

To reproduce the Go unit checks, Quick database smoke, and the Full-only
PostgreSQL backup/restore smoke scripts, run these against the candidate commit.
They omit docs, Rust, Web, the PostgreSQL volume-layout check, and the full
database suite (Quick smoke still runs `database-artifact-policy.sh`):

```bash
scripts/bootstrap.sh go-test
scripts/go-check.sh standard
DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 scripts/database-integration.sh
RUN_ID=local-pg ARTIFACT_DIR="$PWD/.cache/recovery-pg" PG_MAJOR=18 bash scripts/postgres-backup-restore-smoke.sh
RUN_ID=local-ext-pg ARTIFACT_DIR="$PWD/.cache/recovery-ext-pg" PG_MAJOR=18 bash scripts/external-postgres-backup-restore-smoke.sh
```

With authorization to trigger remote workflows, run the candidate branch with
`gh workflow run ci.yml --ref <candidate-branch> -f profile=full -f contract_base_ref=<base-commit>`
(or `profile=quick`; the baseline must differ from the candidate, see
[Path routing](#path-routing)) and record the candidate SHA, cache state and
per-job timings.

## Business validation

Integrated Business Smoke runs directly inside [`release-check.yml`](../../.github/workflows/release-check.yml)
on the native `amd64` runner once Security passes and the dual-architecture
build is verified. It exercises the real Controller, production Signer
sealing and internal TLS, dedicated Relay, native systemd
Agent/`privd`/`ocserv` services, one local administrator browser sign-in, workspace
isolation, VPN user creation, direct reload, ConfigPlan apply, an OpenConnect
VPN session with ICMP, four single-instance recovery scenarios, and terminal
real-node revocation. Every production binary, service, Relay, and VPN path runs
without mocks.

The driver ([`scripts/release-business-probe.sh`](../../scripts/release-business-probe.sh),
paired with [`scripts/release_business_scenarios.py`](../../scripts/release_business_scenarios.py),
[`scripts/release-business-browser.mjs`](../../scripts/release-business-browser.mjs),
and [`scripts/release-common.sh`](../../scripts/release-common.sh)) consumes
the verified `amd64` Agent package and Controller image artifacts from the
same Release Check run, loads and pushes the candidate images to a loopback
registry, and installs through `controller.sh install --release-file` with
standard v2 JSON configuration. It never publishes releases, pushes to GHCR, or
reads production secrets.

### Evidence and limits

`real_browser_subset` runs Playwright Chromium against the built gateway and
Controller API without route mocks, transport simulators, or TLS bypasses, and
captures Content Security Policy violations into `browser-csp.json` (failing on
any unexpected violation or missed positive control). Both `smoke_user` and
browser flows exercise the single local administrator identity, exact request replay,
changed-intent idempotency conflicts, production Signer password sealing, and
durable cross-checks across the API, database, Agent SQLite journal, and `privd`
receipts.

GitHub Actions job status is the pass/fail authority. `result.json` records
`scope="business-smoke"`, `profile="smoke"`, `terminal="revocation"`,
`operator_mode="single_admin"`, stage timings, passed checkpoints, and the four
required recovery checkpoints (`resilience_controller`, `resilience_agent`,
`resilience_database`, `resilience_relay`). Terminal revocation verifies
rejected anonymous/foreign-Origin requests, synchronous HTTP 200 trust
convergence and connection close, owner lease invalidation, offline status while
the same Agent PID stays up, and rejected post-revocation reloads with zero new
`privd` side effects.

The job summary reports `scenario_timings` in milliseconds. It lists each of
the four recovery scenarios and separates the revocation request, ownership
invalidation and node offline waits. A timed phase that fails keeps its elapsed
time and `FAIL` status in the diagnostic artifact.

Sanitized diagnostic artifacts keep the environment inventory, stage timings and
exit status for seven days on passing and failing runs. Logs stay private until
exact-value redaction completes, and bootstrap tokens (`oit1_`/`obt2_`), session
cookies, raw database dumps and password files are rejected from diagnostics.
Cleanup tears down containers, the `business-test-client` network namespace,
temporary iptables rules and the private working directory. Native package
installation mutates the disposable hosted runner itself, so never run the probe
on a shared machine.

A passing positive ConfigPlan apply requires native execution of the
[complete configuration contract](../reference/stable-contracts.md#configuration-and-node-tls),
and recovery validation must respect the
[matched-release recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary).
Passing Business Smoke on `amd64` combined with native `amd64`/`arm64` package
and image smoke does not certify cross-host high availability, historical
cross-release upgrades, or third-party CA/HSM integrations.

## Single Relay validation

Production installers and launch entry points admit one dedicated Relay (see
[Dedicated Relay](../operations/production-deployment.md#dedicated-relay)). Run
these checks inside a task-owned checkout on a disposable Linux environment. Any
step that installs native packages, modifies systemd units or injects network
faults belongs on the disposable Business runner or in a dedicated test
container.

### Lightweight checks

Run the Relay filter tests with the repository's Rust toolchain:

```sh
cd rust
cargo test --locked -p ocservia-agent -p ocservia-transportd relay -- --nocapture
cargo fmt --all -- --check
```

The Relay test filter exercises URL parsing, normalized duplicate detection,
forbidden URL rejection, token-file loading, and the default, disabled, and
single-relay recovery behavior of the transport and agent crates. Production
installers and launch entry points admit only one Relay, so these unit tests
do not certify multi-Relay deployments, failover, or an authenticated dedicated
Relay deployment.

To check the installer contracts, use a disposable Linux container with Bash,
Node, sudo, jq and Python, and run the scripts as an unprivileged user with
passwordless sudo so the run exercises controlled privilege elevation alongside
the root paths:

```sh
python3 scripts/test-node-bootstrap.py
bash scripts/test-controller-install.sh
bash scripts/test-controller-bootstrap.sh
bash scripts/test-controller-host-bootstrap.sh
docker run --rm --network none -v "$PWD:/source:ro" \
  python:3.12-slim-bookworm@sha256:935c3c5b12357a7e10caa9c1e6ca5692344d97cf229832ef584be2cec9fc03df \
  python3 /source/scripts/test-relay-launchers.py
```

The node installer has one token pipeline for the hidden prompt, protected
file and retained bootstrap state. The configuration questionnaire, install.env
allowlist and manual installer branch are retired. Business Smoke exercises the
production token installer directly. Manual uncertain-response recovery remains
in the Agent CLI procedures; none of these checks resets identities or keys.


`test-relay-launchers.py` swaps the fixed binary paths for argv recorders. Never run it on an installed host. In Basic CI, the installer and launcher self-tests run only when [path routing](#path-routing) sets the installer flag.

To validate the Compose configuration and authentication/database contracts, run:

```sh
bash scripts/test-production-auth-config.sh
bash scripts/docs-check.sh
```

### Network probe

After building the current transportd runtime image (with its Relay launcher) and
the authenticated iroh-relay image from `deploy/production/relay.Dockerfile`,
render the Standalone production Compose JSON with
`deploy/production/compose.sh config --format json > "$COMPOSE_JSON"` and run
the probe on the disposable test host:

```sh
python3 scripts/single-relay-network-smoke.py \
  --compose-json "$COMPOSE_JSON" \
  --transport-image "$TRANSPORT_IMAGE" --relay-image "$RELAY_IMAGE" \
  --artifacts "$NETWORK_EVIDENCE" --cold-start
```

The probe reads the merged production Compose JSON. It configures transportd with
the production network memberships, UID and container hardening, and places the
Relay on a separate bridge network reached over a host-bridge TCP port, never on
the internal application network. It starts no Controller backend, authorizes no
Agent and runs no command workflows. It verifies external DNS resolution, strict
CA-validated TLS, authenticated iroh connection events and Pong frames, cold-start
connection establishment, and recovery from a Relay outage without restarting
transportd. The 90-second recovery window is fixed before fault injection to
match iroh's 10-second connection timeout and retry backoff.

To reproduce the earlier isolated-network failure, strip only `relay-egress` from
transportd's network memberships and top-level networks in the rendered JSON,
invoke the probe with `--expect-isolated` in place of `--cold-start`, and keep the
modified JSON with the artifacts. Never alter live production networks to
reproduce it.

### Real Agent chain

End-to-end single-node recovery runs in `Business Smoke (amd64)` inside
`release-check.yml` (see [Resilience and capacity](#resilience-and-capacity) and
[Business validation](#business-validation)). With non-DNS outbound UDP blocked
for `ocserv-agent` (IPv4 and IPv6), the sole-Relay recovery scenario stops the
sole Relay container, waits for owner lease invalidation and offline telemetry,
submits an authorized reload while offline, and verifies that the command stays
`queued` with an unpublished outbox record, zero sent attempts, zero new Agent
journal entries, zero new `privd` effects, zero new native reloads, unchanged
service PIDs/identity and an uninterrupted VPN tunnel. Once the same Relay
restarts, it requires a new connection ID and higher owner epoch within 90
seconds, verifies the queued operation finishes with one Agent journal entry, a
verified root receipt, one published outbox event, an audit record and exactly
one additional native reload, and confirms that replaying the API idempotency
key returns the existing operation and command without another reload.

### Package lifecycle and cleanup

For current DEB and RPM packages, run `scripts/release-native-package-smoke.sh` inside disposable systemd and packaging
containers (see
[Release candidates and package validation](#release-candidates-and-package-validation)).
When a native test container starts sibling Docker containers through the host
socket, mount `RUNNER_TEMP` at the same absolute path on the host and inside the
parent container. Run native DEB and RPM checks on matching host and binary
architectures, and never report an unexecuted architecture as passing.

Preserve test logs, the rendered Compose topology, stage timings, checksums and
non-secret JSON summaries as evidence. Keep enrollment tokens, session cookies,
credentials, private CA or signing keys and fixture secret directories out of
published artifacts. Each integration script cleans up its own containers,
volumes and networks through exit traps. After exporting the evidence, remove the
task-owned systemd container, temporary image tags, built test packages and
private working directories without a global Docker prune.

The endpoint workspace uses upstream crates.io `iroh` directly (pinned in
[`rust/Cargo.lock`](../../rust/Cargo.lock)).
Keeping persistent connections across multiple Relays would still need at least
two distinct Relays in configuration. Single-Relay reconnection reuses the
existing Endpoint and supervisor loop without a separate connection manager.

## Resilience and capacity

Supported deployments run one Controller, one database instance and one dedicated
Relay (see [Support scope](../operations/production-deployment.md#support-scope)).
Controller high availability, multi-Relay failover, database replication
clusters, PostgreSQL automatic failover and PITR readiness are out of scope.
Recovery procedures and boundaries are in
[Incident recovery](../operations/incident-recovery.md).

### Recovery coverage and ownership

Single-instance recovery checks run under the Business and database owners.
Manual Release Check invokes Integrated Business Smoke, which always runs all
four resilience scenarios (`R1`-`R4`).

| Check | Owner and entrypoint | Required observation |
| --- | --- | --- |
| R1 Controller stack | Integrated Business Smoke | Restart Controller and transport together; both ready, fresh Agent session and new authorized business succeeds. |
| R2 Agent stack | Same smoke environment | One systemd restart transaction for privd and its dependent Agent; both active, fresh session and unchanged identity. |
| R3 Database interruption | Same smoke environment | Stop the bundled PostgreSQL container, require database-unavailable readiness, start it, recover readiness and Agent connectivity, then complete normal business. No outage mutation. External PostgreSQL databases have no Business-level recovery check. |
| R4 Sole Relay interruption | Same smoke environment | Stop the only Relay, observe Agent offline with direct UDP blocked, restart Relay, acquire a fresh session and recover authorized business and VPN traffic. |

Release Check also requires the full database suite and the bundled and
external PostgreSQL backup/restore smoke scripts. A successful same-commit
Full CI can provide those database results.

Acceptance relies on the Business job status and the scenario checkpoints in
`result.json`; sanitized failure diagnostics are kept as seven-day Actions
artifacts. Only `controller`, `agent`, `database`, and `relay` are required
checkpoints, with no synthetic overall checkpoint or standalone transport
scenario, and each must finish and pass. During Business runs the harness loads
the prebuilt candidate Controller images (including `relay`) from `IMAGES_DIR`;
the standalone [network probe](#network-probe) checks transport reachability
in isolation and does not exercise the business recovery chain. In every
scenario the command pipeline enforces owner/epoch/fence/lease validation, the
transactional outbox, session and workspace-admin authorization, signatures,
idempotent command IDs, the Agent SQLite journal, and privd receipts. A queued
offline command proven never sent must never be treated as expected unknown if
it fails. Passing these checks does not imply exactly-once execution or
production RTO/RPO guarantees.

### Simulated load and capacity

The simulated-Agent harness ([`scripts/resilience-capacity.sh`](../../scripts/resilience-capacity.sh))
drives the read-only control path with up to 500 side-effect-free simulated
Agents that send heartbeats (every 30 seconds by default), emit bounded
telemetry and carry deterministic Direct and Relay path metadata. During the run
it injects slow SSE consumers, restarts the Controller, sends SIGKILL to the
`transportd-stub` container and restarts it, pauses PostgreSQL
(`compose pause postgres`), and tracks an in-flight operation across the outage
to confirm its final state stays explicit. It runs no real transportd, Agent,
Relay or native node. Concurrent Viewer SSE streams (the smoke and full profiles
use different counts) share a single workspace watcher; the run fails if watcher
instances or steady-state SQL query counts scale with the number of subscribers.
The collector records stream, watcher and query counters, slow-consumer
disconnects, file descriptors, goroutines, RSS and the completion of an
unrelated probe.

Both profiles are manual and outside Basic CI, with no Actions workflow.
Use a Linux host with Docker, Compose, `curl` and `jq`:

```bash
make resilience-smoke
make resilience-full
```

The next release tag containing these names establishes the automation
compatibility baseline. Earlier tags and their `p1-*`, F1/F2/F3 and Stage-0
entrypoints are not compatibility targets; use the canonical entrypoints above.
`OCSERVIA_RESILIENCE_PROFILE` selects `smoke` or `full`, and evidence filenames
use the `resilience-` prefix. Historical artifacts and published document
anchors retain their original bytes.

The same release changes the instrumentation scope `ocservia.localslice` to
`ocservia.transportevents`, and span names `local_slice.event.ingest`,
`local_slice.queue` and `local_slice.dispatch` to `transport.event.ingest`,
`development.simulation.queue` and `development.simulation.dispatch`. Update
collector filters, queries and alerts at deployment; spans are emitted once
and historical traces are not rewritten.

`make resilience-smoke` keeps all fault phases and resource assertions with a smaller
workload; `make resilience-full` uses the full defaults. The parameters and their capacity
upper bounds are defined in the script and the [`Makefile`](../../Makefile)
targets. `REQUEST_CONCURRENCY` must be an integer in `1..32`, and any parameter
outside the capacity envelope is rejected before the script touches Docker or creates
temporary directories; environment overrides may adjust values within the
envelope but not exceed it.

The transport stats writer renames each JSON snapshot atomically to avoid
partial reads. A premature sampler exit, malformed snapshot or missing phase
observation fails the run, and the valid samples must span the load, slow SSE,
Controller restart, transport interruption and recovery, and PostgreSQL pause and
recovery phases. An operation running when transport stopped must converge to
`unknown` within the bounded wait; `queued`, `dispatched`, `accepted` and
`running` are rejected as terminal states. The report captures latency
percentiles, telemetry volume, path distribution, goroutines, simulator tasks,
RSS, file descriptors, database pool activity, SSE and watcher counters,
per-phase sample counts and sampler exit status.

The topology runs simulated components on one host, so passing it does not
establish production capacity or multi-host Relay behavior. Every Docker resource
carries the Compose project label, and the cleanup trap deletes only that
project's containers, networks, volumes, and locally built images on success,
failure, or signal. The run directory keeps the parameters, metrics, JSON summary,
samples, slow-SSE output, interrupted-operation state, disk snapshots, Compose
logs, container statuses, and exit code; no workflow uploads them, so point
`ARTIFACT_DIR` outside the temporary directory to keep them. The script does not
preflight host disk, CPU, or memory, so an undersized machine shows up as a
timeout or failed assertion, and the harness never lowers the configured load.

## Web validation

Select suites according to [Choose the validation scope](#choose-the-validation-scope). Unless a repository-root script is shown, file paths in the table below are relative to `web/`.

| Change | Relevant evidence |
| --- | --- |
| API adapters, session or workspace | `test/api-client.test.ts`, request serialization checks; `node test/run-auth-browser.mjs` against the production build for login/workspace/SSE races |
| Configuration/certificate workflows | `test/configuration-feature.test.ts`, `test/certificates-feature.test.ts`, `test/node-workflows.test.ts`; browser spec `e2e/node-forms.spec.ts` on desktop/mobile |
| Operations/rollouts | `test/operation-state-tone.test.ts`, `test/rollout-views.test.ts`, `test/nodes-rollout-submit.test.ts`; browser spec `e2e/agent-rollout.spec.ts` for unknown states, partial failure, polling disposal and single resume; a real one-node rollout runs in [Business validation](#business-validation) |
| Audit, overview | Corresponding view/unit tests (`test/audit-view.test.ts`, `test/overview.test.ts`) and browser spec `e2e/apple-layout.spec.ts`; verify authorization, stale/empty states and workspace switching |
| Shared UI, styles or shell | `bash scripts/web-check.sh basic` from repository root; full `npm run test:e2e`, desktop/mobile layout and keyboard checks below |
| ESLint configuration | `npm run lint`, `npx vitest run test/eslint-vue.test.ts` (Vue rules reach SFCs) and `npx --no-install prettier --check eslint.config.ts` |

`scripts/web-check.sh basic` runs formatting checks, ESLint (over `web/` and the root JS scripts), `vue-tsc --noEmit`, Vitest unit tests, and the production Vite build. Running `scripts/web-check.sh full` adds the focused authentication browser runner (`test/run-auth-browser.mjs`), while the full Playwright E2E suite requires `npm run test:e2e` or `make e2e`. This runner and stubbed Playwright specs do not validate a live Controller, database recovery, or host installation. To run focused Playwright specs against an already-serving production bundle, export `PLAYWRIGHT_BASE_URL` beforehand.

Playwright projects are named after their browser target: `desktop` runs Chromium, `mobile` emulates an iPhone 13 viewport inside Chromium (not Safari engine coverage), and `firefox` and `webkit` run the cross-engine subset covering login/workspace selection, rollouts, node forms, password dialogs, shell/Overview layout, Nodes/detail, Operations, and audit. `npm run test:e2e` runs all four projects unless filtered with `--project`. `node test/run-auth-browser.mjs` accepts the same `--project` flags (for example `node test/run-auth-browser.mjs agent-rollout.spec.ts --project=desktop --project=webkit`) and exits with an error if a requested browser binary is missing; report any project whose browser engine is not installed as not run.

Passing `--gateway` (which requires Docker) serves `dist/` behind the pinned production Caddy image and `deploy/production/Caddyfile` over HTTPS in place of Vite preview. Passing `--gateway=integrated` places the Integrated Edge container (`edge.Dockerfile` base image with the NGINX template and entrypoint from `deploy/production/integrated`) in front of the Integrated Caddyfile at `https://controller.localhost` on `127.0.0.1:443` so Caddy receives traffic only through Edge's SNI router and PROXY protocol. The cross-engine specs import `e2e/csp.ts`, which fails the test on any `securitypolicyviolation` event. `csp-control.spec.ts` also triggers a deliberate inline violation and asserts that the browser blocks it with `disposition=enforce` across every selected project, because the Content Security Policy is enforced without a report endpoint. Playwright's WebKit and Firefox builds do not substitute for testing on minimum supported Safari 16.4 or Firefox 128 releases.

When a pull request modifies shared UI primitives, layout, or styles, include the following evidence in the PR description:

- Before-and-after screenshots captured against identical fixtures at both 1440 px and 390 px widths for overview, nodes, node detail (including missing-node state), operations, rollout, audit, settings, and login that confirm the document never scrolls horizontally.
- Verification of the keyboard skip link, desktop sidebar and mobile Sheet navigation, Nodes filter controls and detail links, and dialog open, cancel, and confirm flows returning focus to the triggering element.
- CSS and JS production bundle size comparisons against the base branch, along with the rationale for any newly added package dependencies.
- Manual Safari and Firefox test results whenever the change touches browser-sensitive CSS or DOM APIs, since Chromium mobile emulation does not cover those engines.

## Release policy

Every release qualifies through a passing CI run along this pipeline:

```text
PR -> Basic CI -> merge main
   -> manual Release Check (version required, contract_base_ref optional)
      - reuse or dispatch same-commit Quick CI and Security
      - reuse or run release contract and PostgreSQL acceptance and recovery
      - build the release artifacts once for both architectures
      - start amd64 Business Smoke and Quick Install when their artifacts and Security are ready
      - run package smoke, synthetic RC lifecycle and asset assembly on parallel branches
   -> PASS -> operator pushes the matching vX.Y.Z-rc.N or vX.Y.Z tag
   -> Release verifies qualification and publishes the accepted artifacts
```

On pull requests, `Basic CI Result` remains the required check. On `main`, run
`gh workflow run release-check.yml --ref main -f version=1.4.0` with the version
you intend to tag. Use `contract_base_ref` to override the latest `v*.*.*` tag
reachable from `HEAD~1`, or `HEAD~1` itself when there is no such tag. Release
Check waits on matching in-progress upstream runs and dispatches missing Quick
CI or Security runs. A failed, cancelled or timed-out required run
blocks qualification. If `main` advances before a missing run can start, start
a new Release Check on the new candidate.

Release Check builds and scans Controller images on both native architectures.
After both Agent package uploads, candidate package smoke and asset assembly
start in parallel. For RC versions, the synthetic upgrade lifecycle also
starts and restores the compilation cache from the package builds. Once
Security passes and the verified `amd64` inputs are available, Business Smoke
and Quick Install start alongside the other jobs. Consumers verify the exact
artifact ID, source, version, architecture, inventory and file digests. The
final gate waits for these jobs and records the artifacts and upstream results
in a single qualification record.

Business Smoke reads the Signer client certificate lifetime from the rendered candidate
Compose configuration. It generates a test issuer and root with enough remaining
validity for issuance and checks both certificates before installing the Controller.

Quick Install runs two isolated Pebble instances on separate runners. The lifecycle
job uses a day-long certificate and checks the ACME volume, account and leaf across
repeat install, upgrade and rollback. The renewal job uses 15-minute certificates
and captures the served leaf after the Agent reload, immediately before waiting
for rotation. Both jobs consume the same candidate artifacts. The renewal job checks
the default shared hostname, verifies the node's execution receipts after a reload
over Relay, and waits for Gateway to obtain a new certificate from Pebble. After a
controlled Relay restart, the probe verifies the CA and hostname on UDP 7842 and
compares the presented leaf with Gateway's renewed leaf. Dual-domain operation
continues to be covered by Business Smoke.

Image scans apply approved entries in
[`deploy/production/image-scan-exemptions.json`](../../deploy/production/image-scan-exemptions.json),
each with a review deadline; an entry past its deadline fails the gate again.

After Release Check passes, push the matching version tag. Release verifies
the latest qualification for that exact SHA and version, downloads the images
and assets from that run, and verifies the same qualification again
before writing to GitHub or GHCR. A newer failed or unfinished qualification
blocks publication. Only the tag-triggered `publish` job receives
`contents: write`, `packages: write` and access to the approval-protected
`release-publishing` environment.

Published release artifacts are native Agent DEB and RPM packages, Controller container images, minimal Controller runtime bundles
with published checksums, and the version-stamped bootstrap files consumed by
the installers. Plain checksums guard against download corruption; they are not
cryptographic signatures or a trust root. `controller-release.json` is the
standard deployment configuration for Integrated lifecycle consumers.

The manifest generator records the highest numbered Goose migration in `control-plane/migrations/sql` as the revision in `database_migration`, with epoch 1. The source commit binds the embedded SQL.

AgentUpgrade keeps the Controller-authorized `target_version`, `package_sha256`
and `architecture` fields. Before protected staging, architecture verification,
safe extraction and service lifecycle transitions, the upgrade path checks the
downloaded archive's SHA-256 against the command and aborts on a mismatch. All
standard command guarantees stay in force: runtime command authorization,
transactional session and workspace-admin checks, owner/epoch/fence/lease checks,
idempotency, the Agent SQLite journal, privd receipts, unknown outcome
reconciliation, semantic payload hashes, command/fence/receipt signatures and
durable immutable operation intent. The operator-provisioned release catalog
supplies the authorized upgrade package digests. When changing upgrade or
bootstrap paths, test authorized AgentUpgrade digest acceptance and rejection and
Bootstrap launcher download and checksum failures.

### Release candidates and package validation

Release candidates must use `X.Y.Z-rc.N` where N is a positive integer without
leading zeros; any other prerelease tag or build metadata is rejected.
Publishing an RC passes `--prerelease --latest=false` and pushes only the exact
RC image tags, while manual dispatches never publish assets or images. Archive
filenames and compiled binaries report `X.Y.Z-rc.N`, but nFPM maps native package
versions to `X.Y.Z~rc.N-1` for DEB and Version `X.Y.Z~rc.N`, Release `1` for
RPM. Package validation has three separate scopes:

1. Same-source relabeled native package lifecycle: without `PRODUCTS_DIR`,
   [`scripts/release-native-package-smoke.sh`](../../scripts/release-native-package-smoke.sh)
   rebuilds the current checkout under synthetic version labels (`2.0.0 -> 2.0.1`
   by default, or `2.0.0-rc.1 -> 2.0.0-rc.2 -> 2.0.0` with `RC_LIFECYCLE=true`)
   on a disposable native systemd runner:

   ```bash
   RC_LIFECYCLE=true RUN_ID=<unique-id> ARTIFACT_DIR=<path> \
     scripts/release-native-package-smoke.sh
   ```

   It always builds and exercises real compiled binaries. The multi-version sequence exercises installation, upgrade, downgrade, file ownership and package scriptlets. Relabeling one source
   tree does not prove cross-release binary upgrade compatibility.
2. Current candidate product build and install smoke: in
   [`release-check.yml`](../../.github/workflows/release-check.yml),
   `PRODUCTS_DIR` and `VERSION` feed the candidate packages built for `VERSION`
   into `scripts/release-native-package-smoke.sh` on native `amd64` and `arm64`
   runners, checking package ownership, binary versions, removal and state preservation. The build job uploads the package
   artifact before a separate smoke job downloads that exact immutable artifact
   ID. The final qualification waits for the smoke job. Each consumer downloads
   its own copy of the original artifact, so the privileged RPM systemd helper
   can affect only its disposable copy.
3. Cross-historical-release compatibility is outside the promises of the
   [version policy](../operations/production-deployment.md#version-policy) and
   not certified by either smoke check.

### Immutable publication and recovery

Before the first publication with this workflow, a repository administrator must
select Settings → General → Releases → Enable release immutability (see
[GitHub's configuration guide](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/establish-provenance-and-integrity/prevent-release-changes)).
This protects future releases only and does not retrofit historical ones. The
setting API requires Administration permission, which the publishing
`GITHUB_TOKEN` does not have, so keep the existing job permissions and do not add
an admin token. Check the setting before approving the `release-publishing`
environment. The workflow verifies `immutable: true` after publication and fails
if GitHub does not enforce it. A failure at that final check can leave a public,
mutable Release; it is not a successful publication and must be investigated.

Publication is one-shot: an existing Release (including a draft) or any
exact-version GHCR tag blocks it. No asset upload uses `--clobber`, and a rerun
never replaces an existing version. With a matching Release Check and its
qualified native build, publication:

1. Requires all expected pre-publication assets and image archives. Lists all GitHub Releases,
   including drafts, and probes every GHCR version tag (each image role has
   `vVERSION`, `vVERSION-amd64` and `vVERSION-arm64`). Only an explicit registry
   `404 MANIFEST_UNKNOWN` means an absent tag; authentication, rate-limit,
   transport, malformed-response and server errors fail closed.
2. Creates a new Draft with generated notes and the exact build SHA. Note cleanup
   happens while the Release is still a draft.
3. Rechecks each image tag immediately before writing it. Verifies each pushed
   image's configuration digest against the scanned archive's loaded image ID,
   then builds the version index from those registry digests and requires both
   `linux/amd64` and `linux/arm64` entries. A Docker push reporting `unknown blob`
   gets at most two retries, after 5 and 15 seconds, against the same loaded
   image; each retry first requires the tag to remain absent, and a written tag
   or registry lookup error stops publication. Other push errors are not
   retried. Failed commands retain both output streams, and the job records the
   Docker, storage-driver and Buildx versions.
4. Writes the verified multi-architecture index digests into both platform
   manifests, rejects every tag-only image reference, then uploads all assets
   without replacement and verifies their inventory, size, state and
   GitHub-provided SHA-256 digest.
5. Rechecks assets, all image tag digests, draft identity and the Git tag SHA,
   then publishes the Draft. Success requires a published immutable Release,
   complete verified assets, unchanged image digests and RC exclusion from
   Latest.

The readable GHCR version tags remain aliases for operators, but immutable
release assets select images only through the recorded OCI index digests. The
per-ref Actions concurrency group serializes same-tag runs without
cancelling an active publication. GHCR has no atomic create-only tag operation in
this flow: the checks protect this workflow's writes but cannot stop an
independent writer racing the check or changing tags later, so restrict package
write access to this publication path. GitHub Release immutability does not
protect GHCR tags. There is no cross-service transaction: a failure can leave a
Draft and some GHCR tags, and the workflow does not delete, overwrite or resume
them. Preserve the failed run evidence, inspect the partial state, and use a new
version with its own qualification. Do not delete and recreate a released version
to retry. The next RC compares against the highest-numbered earlier published RC
of the same series, skipping failed drafts and unused numbers: if RC.6 failed
after creating a draft, RC.7 uses published RC.5 as its notes base and RC.6
remains untouched.

If no Release or version image tags were created, rerunning failed jobs is safe
while the required artifacts remain available. Consumers download the immutable
artifact IDs returned by the successful upstream jobs, including jobs retained
from an earlier attempt of the same run, and do not rebuild names from the
consumer's incremented attempt number. Build and qualification artifacts are
retained for seven days. If the artifacts or qualification record have expired,
run Release Check for the intended version on `main` again.
Once a draft or version image tag exists, the one-shot preflight requires a new
version. GitHub
creates its platform release attestation for an immutable Release; this flow adds
no separate signing service, key management or custom attestation, and plain
archive checksums keep their role.

## Database checkpoint release boundary

The v2.0.0 baseline initializes fresh PostgreSQL 18 databases with Goose.
Schema changes must pass the [SQL artifact acceptance checks](#sql-artifact-acceptance)
and the bundled and external PostgreSQL backup and restore checks. The exact main
commit must pass Release Check before tagging.

## Release notes and changelog

GitHub Releases are the primary user-facing record for each version, combining
change summaries, merged pull requests, downloadable assets, upgrade notes and
known issues. The Release workflow generates notes through
`gh release create --generate-notes` using the category rules in
[`.github/release.yml`](../../.github/release.yml), which is the source of the
category-to-label mapping. Create the labels it names before using them; area
labels are optional. Reserve `release/internal` for changes with no user-visible
effect (such as test refactoring or CI maintenance), since it omits the pull
request from generated notes even when other labels are present, and never attach
it to breaking changes or user-facing security fixes.

After GitHub generates the initial notes body,
[`scripts/strip-release-owner-attribution.py`](../../scripts/strip-release-owner-attribution.py)
removes the repeated ` by @GentleKingson` suffix from PR list items. External
contributor attributions, PR links, category headings, manual notes, and the
`Full Changelog` link stay untouched. Cleanup is idempotent within draft
preparation, because the workflow compares the stripped text with the current
release body before calling `gh release edit --notes-file`, and existing releases
are rejected before note preparation on reruns. Do not add `GentleKingson` to
`changelog.exclude.authors` in `.github/release.yml`, as GitHub would drop those
pull requests from the notes altogether. Published historical releases are left
as they are.

Pull requests include an optional one-sentence release note field in the
[PR template](../../.github/PULL_REQUEST_TEMPLATE.md). Write clear user-facing PR
titles and use the field for manual highlights or upgrade guidance, because
GitHub's generator reads PR titles and labels, not the PR body. Leaving it blank
is fine, and no CI job enforces it.

[`CHANGELOG.md`](../../CHANGELOG.md) tracks concise, durable summaries of stable
releases and keeps older entries intact. For new releases, omit empty headings,
link to the GitHub Release instead of duplicating its PR list or workflow logs,
and include Highlights, Upgrade notes, Known issues, or a brief Validation note
only when backed by actual results. Detailed run evidence stays in GitHub
Actions. None of these note rules alters Release Check qualification or the
native build, image-security, smoke, and publication gates.

For release notes, `rc.1` and final stable releases diff against the most
recently published non-draft stable Release (ignoring the target release itself
on reruns). For `rc.N` with N > 1, the highest-numbered lower RC in the same
`X.Y.Z` series must exist as a published prerelease. Drafts, the current or later
RC and other series are excluded, with all release pages searched; failed or
unused numbers may be skipped, and no published earlier RC or an API error fails
the workflow before image publication. This follows a single linear release train
instead of sorting semantic versions across maintenance branches. Keep published
RC releases in history, create a dedicated commit and tag for the final release
so installers see one release identity at HEAD, and run a fresh Release Check on
the exact commit for every RC and final tag.

## Coverage ownership

### Published installation assets

After an RC's Release workflow has uploaded its assets, run
[`release-asset-smoke.sh`](../../scripts/release-asset-smoke.sh) against the
published tag (`VERSION=<X.Y.Z-rc.N>`) on a disposable native runner to download the
exact release's DEB, RPM, and managed-node bootstrap over HTTPS. The smoke check validates the embedded release tag and installs the downloaded native packages. The summary records the requested version, architecture, and asset URLs.

For published Agent enrollment, use a separate Ubuntu 26.04 ARM64 systemd
runner with ocserv configured and no Agent package installed. Set
`RELEASE_JOIN_TOKEN_FILE` to the protected installation token and
`RELEASE_DEB_SHA256` to the verified release digest, in addition to
`RUN_ID` and `ARTIFACT_DIR`. The same script then runs the stamped installer
through Controller-confirmed enrollment and leaves the test node enrolled.

### Existing checks

| Behavior | Owner |
| --- | --- |
| Native `oit1_` -> `obt2_` bootstrap installation and online node, browser administrator login/workspace/user creation/reload/ConfigPlan apply, production Signer sealing, real VPN and internal TLS | Integrated Business Smoke (`release-check.yml`, `amd64`) |
| Controller/transport, Agent/privd, bundled PostgreSQL and UDP-isolated sole-Relay offline-queue/exactly-once/idempotent-replay recovery (external PostgreSQL has no Business-level recovery) | Integrated Business Smoke (`release-check.yml`, `amd64`) |
| Terminal real-node revocation: rejected unauthorized requests, direct administrator revoke with synchronous trust update and close, disconnect and no new privd effect from old authority | Integrated Business Smoke (`release-check.yml`, `amd64`) |
| Default shared-domain Quick Install with Pebble, credential and root-CA custody, rerun, bootstrap resume, upgrade and rollback, real Agent reload over Relay, and renewed QUIC certificate after a controlled Relay restart | Quick Install (`release-check.yml`, `amd64`) |
| DEB/RPM file ownership, install/removal and state preservation on `amd64`/`arm64` | Native package smoke in Release Check |
| Native Controller execution and exact archive OS vulnerability scans before smoke/upload, both architectures | Controller build jobs in Release Check |
| Source/dependency vulnerabilities and repository secrets, within the [tool scope](#security-checks) | Security |
| Fail-fast qualification and publication of the recorded artifacts to GitHub/GHCR | Tag-triggered Release |

Business and Quick Install workflows first require the runner, kernel, dpkg and
Docker architectures to match and check every loaded Controller image's platform.
Native package checks require the host kernel, Docker daemon, container image and
executed ELF binaries to share one architecture, so cross-compilation alone never
counts as native acceptance. They enforce architecture and version matching,
root-owned staging permissions and SHA-256 verification. Never install test
packages on a shared machine or clear its binfmt handlers. Passing the current
suites does not certify upgrades from historical releases or mixed-version
clusters.

## Diagnostics and build rehearsal

The Edge and Gateway base images are digest-pinned in their Dockerfiles. The
Relay image is built by [`scripts/build-relay.sh`](../../scripts/build-relay.sh),
which reads the Relay source version from [`toolchains.lock`](../../toolchains.lock),
verifies the archive checksum from `scripts/checksums.txt`, and builds against
[`deploy/production/relay.Cargo.lock`](../../deploy/production/relay.Cargo.lock).
No upstream Relay changes are made.

The `relay-network-probe` target in `rust/test-runtime.Dockerfile` accepts
`URL CA_FILE TOKEN_FILE`. It uses the locked Relay client to check a valid TCP
upgrade and an explicit bad-token rejection, then runs QUIC address discovery
with HTTPS fallback probes disabled. Mount only its test CA and token, map the
test hostname to the authorized public IP, and run it from the same-host egress
network. A timeout is not an authentication rejection or a QUIC pass. The
diagnostic runs as UID/GID 65532, so use a task-owned token copy owned by
65532:65532 with mode 0400 and a public CA file readable by that UID. From the
repository root, set `RELAY_EGRESS_NETWORK` to the same-host egress network,
`RELAY_HOST` to the test certificate's hostname, `PUBLIC_IP` to the authorized
endpoint, `RELAY_URL` to its HTTPS URL, and the two file variables to absolute
paths of the public test CA and private test token:

```sh
docker build -f rust/test-runtime.Dockerfile --target relay-network-probe \
  -t ocservia-relay-network-probe:test .
docker run --rm --network "$RELAY_EGRESS_NETWORK" \
  --add-host "$RELAY_HOST:$PUBLIC_IP" \
  -v "$RELAY_CA_FILE:/test-ca.pem:ro" -v "$RELAY_TOKEN_FILE:/test-token:ro" \
  ocservia-relay-network-probe:test "$RELAY_URL" /test-ca.pem /test-token
```

This diagnostic is not single-node recovery acceptance and does not establish a
real Agent command result; that chain belongs to Integrated Business Smoke
(see [Resilience and capacity](#resilience-and-capacity)).

### Build rehearsal

To rehearse an RC build and qualification across `amd64` and `arm64`, run
Release Check on `main`:

```bash
gh workflow run release-check.yml --ref main -f version=<X.Y.Z>-rc.1
```

This includes candidate smoke, synthetic RC lifecycle, image scans, asset
preparation and business acceptance. Publication requires a separate tag push.
For local validation of release changes, run
`scripts/test-release-upgrade.mjs`, the package and installer test scripts,
ShellCheck (`scripts/lint.sh`), any local workflow linter you have installed, and
the documentation checks from
[Choose the validation scope](#choose-the-validation-scope). Never publish release
assets or touch production deployments during validation.

If a validation suite fails, rerun its owning workflow with the full set of
checks (for recovery failures, rerun the entire Business job). Failed, cancelled,
missing or unexpectedly skipped required checks block qualification. Save the run
evidence first, then clean up only the resources created for that task.
