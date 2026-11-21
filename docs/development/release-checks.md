# Validation and release

Pick the smallest validation target that covers the code or documentation you
touched, and run compilation, linters, static checks, and tests inside an
authorized, task-isolated environment that has the required dependencies. If
that environment is unavailable, report which checks were blocked.

Validation runs in separate layers. Each layer proves something different, and
a result from one layer never stands in for another:

- Local checks run what you choose on your own checkout. GitHub Actions remains
  authoritative for the pull request commit itself.
- `make verify` is a broad local baseline. It starts no database and is not
  Basic CI.
- Basic CI (required check `Basic CI Result`) checks the changed units of one
  pull request or `main` commit.
- Release Check qualifies one exact merged `main` commit for release, with
  native builds, image security scans, package smoke, Business Smoke and Quick
  Install on both architectures.
- Real three-VM acceptance and the manual Business integration profile exercise
  native services, a real browser and a hosted multi-VM topology that none of
  the layers above start.
- The Release workflow rebuilds from the tag and publishes only after its gates
  pass.

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
| Key Integration | Live PostgreSQL 18 schema/snapshot/API checks, native ocserv adapter integration, and Controller/Agent lifecycle suites | `scripts/database-postgres-snapshot.sh check`, `scripts/database-postgres-smoke.sh`, `DATABASE_TEST_SCOPE=full PG_MAJOR=18 scripts/database-integration.sh`, `scripts/native-ocserv-integration.sh` |
| Security Regressions | Single-admin workspace boundary, authentication/session, fencing/outbox regressions, secret scanning, and dependency/image vulnerability gates | `regression-*` groups in `scripts/required-go-tests.txt`, `scripts/security-check.sh`, `security.yml`, `release-products.yml` |
| Release E2E Acceptance | Playwright browser suite, Integrated Business Smoke with single-instance recovery, Pebble ACME Quick Install, and cross-VM private business | `scripts/web-check.sh full` / `make e2e`, `release-check.yml`, `release-business-diagnostic.yml`, `real-e2e.yml` |

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
claims, or other prose facts. `make policy-check` separately rejects tracked
local implementation-control paths (including `AGENTS.override.md`) and private
disclosures. In Basic CI, the `docs` job runs `docs-check.sh`
alone. Validate executable command examples in the docs in an authorized
isolated environment.

For a new workspace, run the supported bootstrap profile that matches your
target check and host platform; tool versions come from
[`toolchains.lock`](../../toolchains.lock) and downloads are verified against
[`scripts/checksums.txt`](../../scripts/checksums.txt). `make bootstrap`
installs the `all` profile, which most single-module edits do not need and
which Linux ARM64 does not support (see
[Linux ARM64 Go validation](#linux-arm64-go-validation)). Reuse a prepared
environment across changes. Direct `go`, `cargo` or `npm` runs need
`source scripts/env.sh` first; the system versions are not the locked toolchain.
The local development stack is started as described in the
[README](../../README.md).

For day-to-day work inside a single module, use `make test-go`,
`make test-rust`, or `make test-web`. `make test-go` runs `go test ./...` in
`control-plane` and `signer` without `gofmt`, `go vet`, `-count=1` or race
runs. `make go-check` (`scripts/go-check.sh`, default mode `full`) covers both
`control-plane` and `signer` with `gofmt`, `go vet`, `go test -count=1`, and
(except in `standard` mode) `go test -race`, using a private `TMPDIR` under
`.cache/` because the signer store refuses state directories beneath a group-
or world-writable parent such as `/tmp`. Basic CI invokes
`go-check.sh standard`. The top-level `make test` (`scripts/test.sh`) runs
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
checks, `web-check.sh` in full mode, the policy and toolchain self-tests,
`security-check.sh` (Gitleaks), `license-check.sh`, and the generated-code checks.
It does not start database containers or
run `make integration`, `make e2e`, the CI router/selector/bootstrap self-tests,
the installer or release self-tests, or manual acceptance harnesses.

Pick the entry point that matches the behavior under test:

- Quick database feedback:
  `DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 scripts/database-integration.sh`.
  PostgreSQL smoke runs `TestDatabaseCoreSmoke`,
  `TestDatabaseInitializationSmoke`, the enrollment restart check, and the
  `postgres-snapshot` inventory. The local smoke command does not run the
  v1.2.0 checkpoint comparison that CI adds (see
  [SQL artifact acceptance](#sql-artifact-acceptance)).
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
- Multi-host and release qualification: [Real three-VM acceptance](#real-three-vm-acceptance)
  (module tests and browser fixtures do not replace it),
  [Business validation](#business-validation), and [Release policy](#release-policy).

The database-backed Go groups over the Controller services and HTTP layer are
`backend-policy-config`, `backend-policy-useroperations`, `backend-policy-api`,
`regression-auth` and the startup group. They run over restricted runtime
database connections and cover signed command intent, transactional session and
workspace-admin checks and rollback, policy cleanup errors, leadership fencing,
HTTP status ordering, Secret access denials and first-request server assembly.
Owner credentials are for schema setup and fault injection only. Every required
case must execute: skipped database tests do not satisfy acceptance, and HTTP
intent tests do not verify Agent execution, package installation or release readiness.

Native ocserv adapter integration runs via `scripts/native-ocserv-integration.sh`
as root in a disposable Linux environment with TUN and `NET_ADMIN`.

### Command wire contracts

Go's `commandauth.TestStrictWireCommandFixtures` and Rust's
`contracts::strict_wire` suite share `testdata/command-strict-wire.json`: the
historical vectors plus every payload variant populated with non-default field
values, and a full echo envelope for authorization, owner fences, timestamps and
envelope metadata. The Go test uses reflection to verify that at least one
fixture populates every field reachable from `CommandEnvelope`, and the Rust
suite independently asserts the decoded values.

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

Run the focused contract suites in an authorized, isolated checkout:

```bash
(cd control-plane && go test -race -count=1 -skip Integration ./internal/commandauth ./internal/contractpolicy ./internal/privdattestation)
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

`scripts/database-artifact-policy.sh all` verifies that `control-plane/migrations`
contains only `schema.sql` and `upgrade.sql`, that no legacy migration manifests
remain, and that the shared marker parser and PostgreSQL 18 metadata checks
pass. The prior release checkpoint stays pinned to v1.2.0 regardless of newer
commits on main.

In `scripts/required-go-tests.txt`, the `postgres-snapshot` inventory requires
live PostgreSQL 18 execution where every listed test runs and passes with zero
skips across both PostgreSQL smoke and Full CI. The snapshot suite verifies
schema, ACL and seed equivalence between a fresh install and an upgrade from
the v1.2.0 checkpoint, forward-only upgrades, zero-mutation rejection of unknown
or tampered databases, raw checksum tampering detection, and PostgreSQL
transactional rollback and locking. Never substitute a database-free run for it.

Every database CI job fetches the v1.2.0 release history and runs
`database-postgres-snapshot.sh check`. Full Basic CI also runs the physical
PostgreSQL (external topology) backup/restore smoke script
(`external-postgres-backup-restore-smoke.sh`), confirming that migration
receipts survive backup, restore and repeated migration, and that restored
PostgreSQL enforces runtime read access and owner-only DDL. Audit log
authenticity, checkpointed-tail transitions and compaction remain required
business checks.

Before cutting over a major database change, qualify the exact candidate commit
through Full Basic CI, Security, Business integration (with the production Signer
and resilience enabled), and a build-only Release rehearsal, recording workflow
URLs and commit SHAs. Branch diagnostic runs do not replace the main-branch
Release Check, and publishing still requires explicit operator confirmation.
Checkpoint and receipt identities are defined in the
[upgrade checkpoint](../reference/stable-contracts.md#upgrade-checkpoint).

### Bundled PostgreSQL initialization

Validate bundled PostgreSQL initialization in an isolated Linux Docker environment using the digest-pinned PostgreSQL 18 image and `scripts/database-integration.sh` / `scripts/test-postgres-volume-layout.sh`; never test with real credentials or against production volumes.

### Signer and certificate verification

Run the suite inside an authorized, isolated environment with the pinned Go and
Rust toolchains, a checkout under trusted directory ancestry, and a private
`TMPDIR`. Tests generate disposable fixture CAs and node keys only.

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
flows, registry image checks and live ocserv CRL enforcement belong to the
extended Business integration profile (see
[Business validation](#business-validation)). Unit and subprocess
fault-injection tests cover crash boundaries during preparation, signing,
pre-commit and post-commit lost replies, concurrent duplicates, ledger
corruption, recovery floors, TLS trust and SAN isolation, authentication,
request size limits and disabled bindings.

### Linux ARM64 Go validation

`scripts/bootstrap.sh` installs pinned repository tools without managing host
OS packages. On Linux `aarch64` it supports the profiles `go-test`,
`rust-basic`, `native-packages`, `package-tools`, `image-security` and
`npm-security`; any other profile (such as `all`, `native`, Web, quality or
combined security) fails up front because sccache and quality-tool binary
mappings are not provided for that architecture. Linux AMD64 and Darwin ARM64
keep their full mappings. The ARM64 Go procedure below validates neither those
platforms nor Rust acceptance.

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
directory under a short, private path inside a trusted home directory, avoiding
`/tmp` and symlinks: the transport socket tests check directory ownership up the
ancestor chain and reject group- or world-writable directories (including sticky
`/tmp`), and deep paths can exceed the Unix socket length limit. Keep each task's
checkout, temporary directory and evidence directory isolated:

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
that candidate's own evidence. The image deliberately omits Go so that every
command in the container runs the native `.tools/go/bin/go` installed on the
host, and system packages stay confined to this disposable image. The Debian
base image is pinned by digest, but APT resolves Bookworm packages at build
time, so save the build log, image ID and installed package versions with the
evidence instead of assuming `toolchains.lock` freezes them.

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
private directory, leaving shared Docker base images, caches and other tasks'
`.tools` directories alone. In your report, distinguish initial failures,
skipped non-required integration tests, simulated platform checks, native host
Go runs and containerized runs.

## CI layers

### Basic CI

[`ci.yml`](../../.github/workflows/ci.yml) handles pull requests, pushes to
main and manual dispatches. Manual Release Check also calls it as `full-ci` with
`profile: full`. Automatic triggers run the Quick profile; manual dispatch
accepts `quick|full` and defaults to Full. Full runs current-candidate checks
across all supported units. It is not comprehensive release acceptance and
leaves the product's database support scope unchanged. A new Quick run cancels
older Quick runs on the same PR or branch; Full has its own concurrency group
and never cancels Quick.

The workflow file defines the matrices. The jobs and what they own:

| Job | Responsibility |
| --- | --- |
| `docs` | `scripts/docs-check.sh` only |
| `ci-tools` | Selected CI guard, release and cross-VM self-tests, source style and contract suites via `scripts/ci-tools-check.sh` |
| `go` | `go-check.sh standard` for `control-plane` and `signer` |
| `rust` | `rust-check.sh` plus the vendored iroh security regressions; Full also runs the focused vendored iroh patch tests against the lock in [`rust/vendor/iroh/OCSERVIA-PROVENANCE.md`](../../rust/vendor/iroh/OCSERVIA-PROVENANCE.md) |
| `installers` | Installer and Relay-launcher contract self-tests |
| `web` | `web-check.sh basic`; no browser installation |
| `web-browser` | Stubbed production-build browser regressions through `web/test/run-auth-browser.mjs` inside the version-matched Playwright container image (`PLAYWRIGHT_WORKERS=4`); Full also serves the build through the Standalone and Integrated gateways with CSP enforcement checks. A missing browser or non-declared skip fails the job |
| `database-smoke` | PostgreSQL 18.x; Quick runs smoke, Full runs full database acceptance; both enforce the SQL artifact window and snapshot equivalence (see [SQL artifact acceptance](#sql-artifact-acceptance)) |
| `database-recovery-full` | Bundled and external PostgreSQL physical backup/restore with snapshot provenance, Full only |
| `Basic CI Result` | Always checks routing and the selected job results |

Database service images stay pinned to exact patches and digests.
`TestDatabaseCoreSmoke` migrates an empty database under the owner account,
starts the real Controller CLI under the runtime account, signs in a local
administrator, checks transaction commit, rollback and isolation, and keeps a
runtime DDL denial check so the Controller cannot pass with owner credentials.
Smoke runs use `-count=1`, and explicit inventories fail if a required case is
skipped (`required-go-tests.sh --smoke <package> <test>` checks only that the
named top-level test ran and passed). Pull requests that touch PostgreSQL
`schema.sql`, `upgrade.sql` or the migration runner should also run a candidate
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

[`scripts/ci-relevance.sh`](../../scripts/ci-relevance.sh) evaluates changed
paths with ordered first-match patterns instead of package or call-graph
analysis and is the source of the exact rule order. In outline:

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
- Installer, release, package, Controller and Agent-upgrade scripts, release
  workflows and `deploy/{package,systemd,bootstrap,managed-node,production}`
  select the release contract suite and/or installer self-tests.
- Cross-VM files (`real-e2e.yml`, `.github/actions/real-e2e-*/*`,
  `deploy/real-e2e/*`, `scripts/real-e2e-*`) select the `cross-vm` suite
  (`make real-e2e-check`). The credentialed three-VM run is never started, and
  the manual-acceptance harnesses (`scripts/resilience-*`,
  `scripts/security-acceptance-*`) are checked but not run.
- `ci.yml`, `security.yml`, `scripts/ci-*`, the required-test selectors,
  `test-ci-relevance.sh`, `test-toolchain-consistency.sh` and public-repository
  guards select the CI guard suite.
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
parallel with `go` and only bootstrap `source-style` or `contracts` tools when
selected; ordinary documentation does not install their toolchains.

Contract comparison uses the PR merge-base or the push event before SHA. Manual
Basic CI and Release Check require an explicit `contract_base_ref` naming an
existing commit different from the candidate; Release Check forwards it to
Full CI. A missing, unresolvable or self baseline fails. `generated-clean.sh`
requires a clean committed candidate before regenerating and rejects changed or
untracked outputs. `--skip-generate` only compares with HEAD (including staged
changes); it does not prove reproducible generation.

When a change touches several areas, the router takes the union of their flags.
Pull requests diff `base...head`, push events diff `before..head`, and renames
or deletions evaluate both old and new paths. If a diff is empty, invalid,
unresolvable, from an unsupported event, or contains an unrecognized path, the
router fails closed by enabling every Basic flag and CI tool suite while keeping
the Quick profile. Path routing never escalates Quick to Full.

Manual dispatches bypass path routing: `docs`, `ci-tools` (`source-style` and
`contracts`), `go`, `rust`, `web`, `web-browser` and database jobs always run.
Installer self-tests and Controller cache refresh stay off. A manual Full run
executes the full PostgreSQL database suite and `database-recovery-full`; a
manual Quick run executes the PostgreSQL smoke leg. Controller cache refresh
runs only on pushes to main when routing sets `run_controller_cache`. It
refreshes the union of the changed images' Dockerfile inputs on both native
architectures, selects all images for shared build machinery and unknown paths,
falls back to all images on an empty selection, and rejects invalid image names
before Docker runs. Product builds always build all images.

### Workflows

| File | Trigger / purpose |
| --- | --- |
| `ci.yml` | PR/main Quick; manual Quick/Full; called by Release Check as Full |
| `security.yml` | Weekly/manual scans; reusable, and called by Release Check (not by `release.yml`) |
| `release-check.yml` | Manual main Full CI, Security, Release products build/image-security/smoke, Integrated Business Smoke with single-instance recovery, and Quick Install (Pebble ACME), each on native amd64 and arm64 |
| `release.yml` | Tag build/image-security/smoke/publish; manual build-only dry-run |
| `release-products.yml`, `release-business-diagnostic.yml` | Reusable native product build/smoke and Integrated Business Smoke workflows called by `release-check.yml` and `release.yml` |
| `real-e2e.yml` | Manual three-hosted-VM enrollment and private business over an ephemeral Tailscale network and a temporary dedicated HTTPS Relay; the schedule is locked off, and unauthorized refs and missing infrastructure fail closed; outside Basic CI and Release Check |

### Security checks

`security.yml` runs on Mondays, on manual dispatch, and inside manual Release
Check on main. Its read-only scan jobs use pinned bootstrap profiles and no
production secrets: full-history `scripts/security-check.sh` (including its
Gitleaks rule regression check); `govulncheck ./...` in both Go modules;
`cargo audit` against `rust/Cargo.lock` and `deploy/production/relay.Cargo.lock`
with `cargo deny check advisories` across the Rust workspace; and
`npm audit --package-lock-only` against `web/package-lock.json`, including
development dependencies. Any scan failure fails the workflow and blocks Release
Check when called from it. `release.yml` neither invokes nor queries
`security.yml`, so tag publication relies on the operator verifying a passing
Release Check first. A pass reflects only what those scanners and their current
advisory databases detect in the repository tree, not the security of a deployed
environment.

### Caches

Go jobs keep module and build caches next to locked bootstrap downloads. Only
passing pushes on main write cache entries; pull requests and manual runs
restore only, and a restored build cache never skips real database tests.
Bootstrap downloads and `.tools` are managed through
[`.github/actions/setup-tools`](../../.github/actions/setup-tools/action.yml)
and keyed by profile, OS, architecture and tool/bootstrap identity (with an
optional `Cargo.lock` suffix and base toolchain fallback when `.tools/cargo`
caches crates). Web CI, `contracts` checks and the npm Security scan cache
`.cache/npm` instead of `node_modules`, and Web jobs still run `npm ci`. Rust
jobs restore `rust/target/debug` and `rust/vendor/iroh/target/debug` with
`CARGO_INCREMENTAL=0` and save them only on passing pushes to main, falling
back to the matching Rust toolchain prefix when manifests or lockfiles change.
Image builds use per-image, per-architecture BuildKit GitHub Actions caches. No
workflow caches runtime state or credentials, and build caches are never
accepted release products or qualification evidence. Release Controller jobs
cache image-security scanner binaries but never the vulnerability database:
each run fetches fresh advisory data and fails if the feed is unavailable or
invalid. Image scans inspect the built image archives before smoke tests and
registry uploads; Business diagnostic runs do not scan images.

### Manual deep checks and reproduction

Run the standalone deep-validation scripts directly in an authorized, isolated
environment that has their dependencies. They are distinct from Full CI:

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
fault-injection suites stay manual with no scheduled workflow. Capacity runs,
native security acceptance and cross-VM enrollment also stay outside Basic CI,
because they need dedicated resources, systemd/privd/PKI fixtures or separate
VMs:

- `make resilience-smoke` and `make resilience-full` invoke `scripts/resilience-capacity.sh`
  (see [Resilience and capacity](#resilience-and-capacity)).
- Transport connection-budget and admission checks run via `make rust-check`.
- `scripts/security-acceptance-sse-capacity.sh` checks SSE delivery, reconnect,
  stream cleanup and resource bounds at 1, 10 and 100 viewers.
- `scripts/security-acceptance-privd.sh` checks the root boundary, systemd
  sandbox, attestation keys, certificate lifecycle and package upgrade/rollback.
  Results use `SSE_CAPACITY_ACCEPTANCE` and `PRIVD_ACCEPTANCE`, with
  `sse-capacity-summary.json` and `privd-summary.txt` respectively.
- `scripts/real-e2e-controller.sh`, `scripts/real-e2e-node.sh`,
  `scripts/real-e2e-artifact.sh` and `deploy/real-e2e` support manual
  two-machine enrollment and the hosted `real-e2e.yml` (see
  [Real three-VM acceptance](#real-three-vm-acceptance)).

To reproduce the Go unit checks, Quick database smoke and the Full-only
PostgreSQL backup/restore smoke scripts, run these against the candidate commit
in an authorized, isolated environment. They omit docs, Rust, Web, the SQL
artifact-window and snapshot steps, the PostgreSQL volume-layout check and the
full database suite:

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

Business Smoke runs the real Controller, native systemd Agent/privd/ocserv
services, one Local Admin browser sign-in, Workspace selection, VPN
user creation, direct reload and ConfigPlan apply, and an OpenConnect
VPN session. It runs on one disposable native runner (`arch=amd64` or `arm64`;
Release Check runs both) with one Controller and one dedicated Relay, and every
production binary, service, Relay and VPN path runs without mocks.
`production_signer=true` (the default in `release-check.yml` / `release-business-diagnostic.yml`)
enables production Signer sealing and internal TLS; `false` selects the
standalone topology without them. Smoke also provisions a separate API-created
VPN user and, with `production_signer=true`, verifies that the production Signer
seals its password.

The `extended` business profile adds PKI/P12 issuance and revocation
(including browser flows), rollback and deeper fault recovery. Before the VPN and
recovery stages it applies the node configuration once through the browser and
checks the native file hash against the operation receipts. Both profiles require
`real_browser_subset`, and a missing browser checkpoint fails the run. The
browser runner captures Content Security Policy violations from the page (the
gateway enforces CSP without a report endpoint) into `browser-csp.json`.
Acceptance fails on any page violation or console CSP error outside the final
positive control, if the collector misses that control, or if the control is
reported without being enforced.

To run the business harness locally on a disposable Ubuntu 24.04 systemd host or VM:

```bash
VERSION=0.0.0 BUSINESS_PROFILE=extended PRODUCTION_SIGNER_ACCEPTANCE=true \
  BUSINESS_RUN_RESILIENCE=true bash scripts/release-business-probe.sh
```

In `release-check.yml`, `arch` (`amd64` and `arm64`) runs the native `smoke`
profile with `production_signer=true` and `run-resilience=true`.
`ocserv_package=distribution` keeps the runner's Ubuntu ocserv;
`ocserv_package=backport-1.5.0` installs the pinned, SHA256-checked
[ocserv-backport](https://github.com/GentleKingson/ocserv-backport/releases/tag/1.5.0-2)
1.5.0 package and also requires the session terminate checkpoints.
Diagnostic runs never publish releases, push to GHCR or read production Secrets,
and do not substitute for Release Check.

### Evidence and limits

The driver builds native packages and container images from the candidate tree,
pushes images to a loopback registry and runs
`controller.sh install --release-file` with standard JSON configuration. With the
production Signer, Integrated configuration upgrades and rollbacks test digest
and version-tag references against those local images, not compatibility across
historical releases. Package scriptlets validate the embedded archive checksum in
root-owned staging, and token installation through Controller-confirmed `ONLINE` runs
through the shipped installer.

After the direct Admin positive ConfigPlan apply, both profiles bring up an
OpenConnect tunnel and verify end-to-end ICMP. The extended profile then submits a
reviewed revision built to fail during ocserv reload, confirms the node restores
the previous files byte for byte, and establishes a fresh tunnel with working
ICMP. This tests automatic rollback of a failed apply, not an operator-initiated
rollback of an active revision. Its final stage builds the next patch version of
the Agent package, places it in the node's root-owned upgrade spool, recreates the
Controller with a diagnostics-only Compose override that mounts a release catalog
(absent from the shipped launcher), and drives a browser-initiated, direct Admin
one-node canary rollout. The rollout completes without any approval queue, leaves
the installed binaries and the live Agent reporting the target version, and
records zero unexpected CSP violations. Stage durations go to `timings.json`; no
duration target is a release gate.

GitHub Actions job status is the pass/fail authority. `result.json` records the
selected scope (`smoke` or `extended`, never full release
acceptance), the diagnostic schema version, overall outcome, stage timings and
completed checkpoints, with `operator_mode=single_admin`. The Admin performs
management operations directly; workspace-scoped denials are exercised by an
unauthorized non-admin identity. Exact request replay must recover the
same operation, changed intent with the same idempotency key must fail, and
native receipts and effects must agree with the recorded operation.
Checkpoints are written only after their assertions succeed, and unreached
phases stay NOT RUN. Across retries, keep failed runs, the original run and
attempt numbers, and the exact candidate SHA. The harness records these limits
and never scores them as PASS:

- Local artifacts leave the public GitHub Release download endpoint untested;
  Bootstrap launcher download/failure fixtures and Release build-only smoke cover it.
- The private Relay CA is provisioned through the documented protected public CA
  files, and the shipped Compose overlay and native launcher enable the existing
  strict-TLS flag unpatched. Both Smoke and Extended profiles enroll via
  `POST /api/v1/node-bootstrap-tokens` (`installation: true` `oit1_` -> `obt2_`),
  passing `--non-interactive --join-token-file` to the managed-node installer.
  The harness checks that the Controller persisted only the child token digest,
  bound it to the Agent's EndpointID, activated the node immediately, registered
  the root `privd` attestation key via `/api/v1/node-bootstrap/{attestation,register}`,
  and that subsequent installer reruns preserve the node ID and identity files.
- The initial Local bootstrap creates one single workspace Admin. The browser uses
  that identity without devAuth or injected sessions, and direct database writes are
  limited to initial workspace provisioning before Local bootstrap. The suite
  checks anonymous access, non-admin `403`/`404` denials, request binding and
  idempotency conflicts. No second login or approval queue authorizes management
  operations.
- Extended exercises real Signer OpenSSL CSR verification, signing, public-key
  sealing and revocation acknowledgements over authenticated TLS, plus
  certificate checks across node restarts and single-use P12 downloads.
  The exported P12 bundle authenticates against a second native `ocserv`; after
  the Controller revokes the certificate, the harness exports and verifies a
  fresh CRL, places it atomically and sends `SIGHUP`. Acceptance requires
  `ocserv` to reject the revoked certificate, report an incremented CRL number
  and still authenticate an unrevoked control certificate. This shows the
  explicit operator CRL refresh workflow, not scheduled distribution, OCSP or
  active session termination. `crl-acceptance.json` is kept with the API,
  root-receipt, browser and business logs, and a missing file or failed
  assertion blocks acceptance. The task-controlled Signer CA fixture does not
  certify third-party CA or HSM integrations.
- A real browser drives the built gateway and Controller API without route mocks,
  transport simulators or TLS bypasses. Unexecuted phases stay NOT RUN, and only
  recorded runtime checkpoints count as evidence for a candidate commit.
- Smoke recovery verifies readiness, fresh Controller and Agent sessions,
  preserved node identity and working operations after reconnection; manual
  integration adds outage queue semantics and durable receipt checks. They give
  no high availability, Relay redundancy or RTO/RPO guarantee. The OpenConnect
  client runs in an isolated network namespace on the same runner, with
  synthetic traffic on one machine.
- After Relay recovery, manual integration drives the Controller's session and
  IP-ban operations against real `occtl` state. A disconnect removes the live
  session; wrong passwords get the client banned under ocserv's default ban
  score, so even the valid password fails; the Controller removes the ban and the
  valid password authenticates again. The distribution ocserv predates
  `occtl terminate` (added in 1.4.2), so the default run does not exercise
  terminate. With the 1.5.0 backport, a cookie from `--authenticate` must still
  authenticate a replaying client after the first client is killed without
  logging off, the replaying client must not reconnect after a Controller
  terminate, and a fresh client replaying the cookie is rejected while a new
  password authentication establishes a session. Both checkpoints are required,
  and a terminate ending `unknown` fails the run.
- By default both profiles end with a terminal revocation of the real node (in
  Extended, after the rollout). Anonymous, role-less and foreign-Origin
  requests are rejected while the node stays active. Direct Admin
  revocation must return 200, meaning the request itself applied the revoked
  trust and the connection close. It passes only after all of the following
  hold:
  - the node and its endpoint key read revoked;
  - the request settled the revoked trust convergence task at that revision;
  - no owner lease remains;
  - the node reads offline while the same Agent process stays up and logs the
    ended Controller connection.

  A new reload request against the revoked node then creates no operation and leaves privd effects, the
  Agent journal, ocserv reloads and node files unchanged. A 503 (synchronization
  left to the background worker) or a timeout fails the run.

In the offline-Relay scenario, the harness first proves the queued command was
never transmitted while the Relay was down, then requires the original operation
to finish with exactly one additional native ocserv reload once the same Relay
returns. An EXPECTED-UNKNOWN classification cannot turn a failure here into a
pass. For other interrupted operations with uncertain effects, the system relies
on read-only reconciliation, then an explicit safe retry once absence of effect
is confirmed.

`result.json` also records `resilience_requested`, `resilience_result` and
per-scenario checkpoints. With recovery enabled, acceptance requires one PASS for
each of four scenarios: the Controller stack (which in Smoke includes
transport), the Agent/privd stack, bundled PostgreSQL and the sole Relay.
External PostgreSQL deployments have no Business-level recovery scenario.
Missing fields or checkpoints, cancellation, test failures and unexpected skips
block acceptance; runs without recovery record SKIPPED.

Sanitized diagnostic artifacts keep the environment inventory, stage timings and
exit status for seven days on passing and failing runs; manual integration adds
API, database, Agent journal and root receipt observations. Keep logs private
until redaction completes, and never upload the workspace secret directory,
session cookies, raw database dumps or password files. Cleanup removes only
task-created containers, the client network namespace, builder state and the
private working directory. Native package installation mutates the disposable
hosted runner itself, so never apply this cleanup to a shared machine.

A passing positive ConfigPlan apply requires native execution of the
[complete configuration contract](../reference/stable-contracts.md#configuration-and-node-tls),
and recovery validation must respect the
[matched-release recovery boundary](../reference/stable-contracts.md#matched-release-recovery-boundary).
When a check fails, rerun the owning suite with its complete check set and
shared fault timeline on the current candidate commit; earlier candidates cannot
fill in missing acceptance or establish cross-version safety. Passing Business
validation does not certify native package distribution or high availability.

### Manual package removal terminal

The native business harness accepts `BUSINESS_TERMINAL=uninstall` as an
opt-in alternative to the default `revocation` terminal. Under `smoke`,
it reuses bootstrap installation enrollment, Online convergence, browser
configuration apply and the real VPN before removing the package. It adds no
Basic CI or Release Check E2E requirement and changes no production lifecycle behavior.

```sh
VERSION=0.0.0 BUSINESS_PROFILE=smoke BUSINESS_TERMINAL=uninstall \
  PRODUCTION_SIGNER_ACCEPTANCE=true BUSINESS_RUN_RESILIENCE=false \
  bash scripts/release-business-probe.sh
```

The terminal needs a disposable Ubuntu 24.04 systemd runner of the selected
architecture. It records NodeID, EndpointID, protected identity/configuration
file digests, active native units and process IDs, ocserv PID/start time, and the
Controller trust revision and owner session before running
`apt-get remove -y ocservia-agent`. It never substitutes revocation or a
separate service stop for the DEB transaction. Evidence includes
`package-removal.log` and separate `package_removal`, `agent_offline`,
`identity_preserved` and `ocserv_unaffected` API checkpoints; the last also
requires a fresh authenticated OpenConnect tunnel with working ICMP.

Offline follows `telemetry.OfflineAfter` (90 seconds since the last heartbeat),
independently of the default 30-second owner lease. The test allows 180 seconds
for Offline, then 60 seconds for lease expiration, then observes 32 more seconds
with no new owner epoch, running Agent component, active unit or timer,
authorization revision or endpoint-key change, identity or configuration change,
privileged effect, or ocserv restart or reload. It also checks removed payloads
and `/proc` executables, including unlinked running binaries. An API error,
removal failure, stale online state, live owner, unexpected trust
revision/revocation or incomplete evidence fails the run; process exit alone
cannot pass it.

The lifecycle interpretation follows [Debian Policy section 6.5](https://www.debian.org/doc/debian-policy/ch-maintainerscripts.html#summary-of-ways-maintainer-scripts-are-called),
[dpkg remove/purge](https://manpages.debian.org/bookworm/dpkg/dpkg.1.en.html) and
[systemctl disable/stop](https://www.freedesktop.org/software/systemd/man/latest/systemctl.html).
DEB removal calls `prerm remove`; upgrades call `prerm upgrade` and do not enter
this path. `disable --now` also stops the units; disabling alone does not. The
repository's preremove delegates removal to the verified archive's uninstaller
without `--purge-state`, preserving identity, state and configuration. dpkg purge
removes conffiles and runs a package postrm if present; it does not imply the
repository's explicit `--purge-state`. The Controller also persists
`nodes.status=offline` on disconnect or telemetry maintenance and exposes it as
API `trust_status`; this transition is normal as long as the endpoint key stays
active and the authorization revision is unchanged.

## Real three-VM acceptance

The hosted fixture ([`real-e2e.yml`](../../.github/workflows/real-e2e.yml),
workflow name Cross-VM Private Business) runs three independent Ubuntu 24.04 VMs:
Controller with PostgreSQL and transportd, the real Agent, and one disposable
iroh HTTPS Relay. It reuses the locked Relay Dockerfile and enrollment protocol;
production Relay configuration and the default transportd image target are
unchanged.

Three-VM acceptance is proven only by an authorized run on the exact merged main
SHA that succeeds, after the administrator setup below. Local contracts, Basic CI
and CodeQL do not prove native three-VM business behavior, and there is no
Release Check gate for it.

### One-time administrator setup

1. Use a dedicated test Tailnet without Tailnet Lock (the pinned Action's WIF
   mode does not support Tailnet Lock). Create `tag:ocservia-ci-peer` and
   `tag:ocservia-ci-relay`, owned only by authorized administrators. Configure
   two separate OIDC federated identities, with writable `auth_keys` scope and
   only their respective tag. No persistent auth key or OAuth secret is needed.
2. For both identities, use issuer
   `https://token.actions.githubusercontent.com`, and the exact subject:
   `repo:GentleKingson@72605370/ocservia@1319465232:environment:cross-vm-test-relay`.
   The repository's OIDC customization API currently reports `use_default=true`,
   `use_immutable_subject=true` and the ID-bearing prefix above. Recheck this
   before installation. The Environment replaces the branch suffix in default
   `sub`; the subject alone does not constrain the branch.
   Add exact custom claim rules:

   | Claim | Required value |
   |---|---|
   | `repository` | `GentleKingson/ocservia` |
   | `repository_id` | `1319465232` |
   | `repository_owner_id` | `72605370` |
   | `ref` | `refs/heads/main` |
   | `workflow_ref` | `GentleKingson/ocservia/.github/workflows/real-e2e.yml@refs/heads/main` |
   | `environment` | `cross-vm-test-relay` |
   | `runner_environment` | `github-hosted` |

   Use each identity's generated audience exactly. Do not use a wildcard subject,
   repository, ref, workflow or audience. The local pre-join guard also compares
   workflow SHA to the checked-out SHA and rejects a different reusable callee.
   First qualification uses direct `workflow_dispatch`; callers with another
   `workflow_ref` are intentionally rejected by this identity policy.
3. Apply the following grant to the test Tailnet policy:

   ```json
   {
     "tagOwners": {
       "tag:ocservia-ci-peer": ["autogroup:admin"],
       "tag:ocservia-ci-relay": ["autogroup:admin"]
     },
     "grants": [{
       "src": ["tag:ocservia-ci-peer"],
       "dst": ["tag:ocservia-ci-relay"],
       "ip": ["tcp:3443"]
     }]
   }
   ```

   Review the entire effective policy, including existing ACLs and grants:
   rules are additive. Remove overlapping allow-all, wildcard, group, tag or
   address-range rules that allow peer-to-peer, relay-to-peer, other Relay ports
   or access to unrelated devices. Do not merely append this grant to the default
   allow-all policy. Use Tailscale policy tests to require peer → relay TCP 3443
   and reject peer → peer, relay → peer and peer → relay TCP 22/443/8080 and UDP
   7842. Do not grant routes, exit nodes, SSH or Funnel. Existing networks must
   not be silently broadened or disrupted; use the isolated test Tailnet.
4. Preserve GitHub Environment `cross-vm-test-relay` with only branch `main`
   allowed, no tag patterns. Do not relax its protection rules. Keep the existing
   `CROSS_VM_RELAY_TOKEN` secret only there (32 to 512 non-whitespace bytes); never
   put it in variables or artifacts. The old URL/CA secrets are no longer read
   by this workflow; removal can wait until no older/manual consumers need them.
5. Set these Environment variables, not secrets: `TS_PEER_CLIENT_ID`,
   `TS_PEER_AUDIENCE`, `TS_RELAY_CLIENT_ID`, `TS_RELAY_AUDIENCE`. Only after the
   above effective-policy/identity review, set `TS_POLICY_REVIEWED=true` there.
   This is an administrator attestation, not an independent API policy audit.
   Missing configuration fails preflight with `BLOCKED_INFRA` before any join.
6. Keep the schedule disabled. The workflow defines no schedule trigger and its
   admission job hardcodes the schedule input to `false`, so an inaccessible or
   stale variable such as `CROSS_VM_SCHEDULE_ENABLED` cannot enable an
   unqualified topology. Do not connect a schedule during initial acceptance;
   that needs a separate change after the first real success. Keep Release
   Check unchanged.

No administrator Tailnet API credential is passed to CI, so CI cannot
independently enumerate all policy rules or confirm server-side device deletion.
Do not label either as API-verified. If stronger deletion evidence is needed, an
administrator can inspect the three run-scoped hostnames in the device API before
and after the run without adding credentials to this workflow.

### Hosted execution and evidence

After review and explicit merge authorization, record the merged SHA and run:

```sh
gh workflow run real-e2e.yml --ref main
```

Check that the run's `head_sha` equals that recorded SHA. The graph is
`admission → preflight → relay/controller/node → verdict`. Admission is
uncredentialed and checks repository, main ref, defining workflow ref/SHA and
event before checkout. All three role jobs use the protected Environment and
job-local `id-token: write`. The Tailscale Action is pinned by SHA in the
workflow; it requests ephemeral, preapproved nodes with in-memory state, and no
long-lived Tailscale key is supplied.

Two kinds of connectivity are kept separate. The Tailnet is only the underlay
between the VMs. The application path is an iroh connection through a temporary
dedicated HTTPS Relay on its own VM, authenticated with `CROSS_VM_RELAY_TOKEN`.
That Relay generates a one-day CA and a DNS-SAN leaf certificate for
`relay.ocservia.test`, disables QUIC discovery in its disposable configuration,
and publishes only its Tailnet IPv4 TCP 3443. No public port, external VPS,
public DNS or public Relay is used, so the run does not exercise public Relay
discovery. Its readiness artifact holds only the URL, Tailnet IP, public CA and
fingerprint, source SHA, run ID, attempt and boot ID, and peers reject stale,
mixed-source, same-VM or malformed readiness. Peers map the fixed hostname
locally, and a test-only Compose overlay maps it inside transportd and endpoint
bootstrap; production Compose networks are unchanged.

The Relay job checks its container and verified HTTPS health until both full peer
jobs complete, outside the controlled recovery interval. Failed peers fail
qualification, and waits have deadlines. The Controller's transportd container
uses an opt-in image target containing the existing iroh probe; its real
authenticated connection and typed wrong-token denial must pass before
controller-ready is published. The Agent checks the correct CA and token,
requires curl error 60 for an unrelated CA (timeouts and DNS errors fail), and
requires typed `ServerDeniedAuth` for the wrong token before real enrollment.
TLS and hostname verification stay enabled everywhere.

The OAEP exchange, real Agent enrollment and Controller database assertions
remain: active UUIDv7 node, EndpointID, workspace/environment, and consumed
derived `obt2_` token -> node binding. Controller and Agent compare distinct boot IDs, source/run
identity, common Relay metadata and a third distinct Relay boot ID. Those boot
IDs and the authenticated Relay connection are the application Relay evidence.
A compact post-traffic Tailscale peer observation records `CurAddr`, `Relay`,
`PeerRelay` and `Active`; an absent path is unknown, not proof of direct traffic.
Tailscale transport (direct, DERP or peer relay) is separate from application
iroh Relay use, and `tailscale ping` is not enrollment evidence. The Controller
and node register the root attestation key during the installation handshake
before confirming native startup, because registration advances authorization
and the first Agent session must negotiate that revision before business commands are queued.

Cleanup steps run with `always()`: Controller services/volumes/networks, Agent
native units/package/process/build/private files, Relay container/image/TLS/token
files, local hosts mapping, Tailscale logout, daemon shutdown and in-memory
credentials. Explicit cleanup failures fail their job even though the upstream
Action's post hook only warns, and that hook may warn about an explicitly stopped
daemon; the checked cleanup step owns the result. Runner cancellation or loss
remains best effort. Tailscale expects ephemeral server removal, but the run
always reports `SERVER_DEVICE_DELETION=NOT_INDEPENDENTLY_VERIFIED`.

Verdict deletes all run-attempt rendezvous artifact kinds, re-lists them and
requires zero remaining. Allowlisted evidence artifacts remain for seven days.
Any failed, cancelled, unexpectedly skipped, timed-out or cleanup-failed
dependency prevents PASS, and a disabled schedule reports NOT_RUN, never PASS.
The final positive markers are valid only for the exact successful run:

```text
CROSS_VM_ENROLLMENT=PASS
CLEANUP=PASS
CROSS_VM_PRIVATE_BUSINESS=PASS
DEDICATED_RELAY_RECOVERY=PASS
CROSS_WAN_PUBLIC_INGRESS=NOT_TESTED
SERVER_DEVICE_DELETION=NOT_INDEPENDENTLY_VERIFIED
```

Record run ID and attempt, SHA, the three boot IDs, TLS/token positive and
negative results, DB binding, path observation, the effective-policy review
basis, and artifact versus local/server cleanup. Report wall time and summed job
seconds from the Jobs API, with runner minutes separate from billed minutes.

Local checks (no Tailnet credentials or actual joins):

```sh
make real-e2e-check
actionlint .github/workflows/real-e2e.yml
shellcheck -x scripts/real-e2e-{hosted,controller,node,artifact,relay-host}.sh
```

`make real-e2e-check` only runs `bash -n` on the real-E2E scripts; it executes no
test, starts no VM, container or Agent, and does not verify enrollment. The
repository does not bundle `actionlint`. Design references:
[Tailscale Action](https://github.com/tailscale/github-action),
[workload identity federation](https://tailscale.com/docs/features/workload-identity-federation),
[additive Grants/ACL behavior](https://tailscale.com/docs/reference/grants-vs-acls),
[GitHub OIDC claims and immutable subjects](https://docs.github.com/en/actions/reference/security/oidc).

### Manual execution

Before starting, export a unique `RUN_ID` (letters, digits, dots, underscores
and hyphens) and an absolute, private `RUNNER_TEMP` directory on each VM. On the
Controller VM, also export a unique, Docker-compatible `COMPOSE_PROJECT`. Reuse
the same values across every phase through cleanup, and set `ARTIFACT_DIR` to
write diagnostics outside the default location under `RUNNER_TEMP`.

On each VM, supply `REAL_E2E_RELAY_URL`, `REAL_E2E_RELAY_CA` and
`REAL_E2E_RELAY_TOKEN` from protected local configuration, then run
`python3 scripts/real-e2e-relay.py "$RUNNER_TEMP/ocservia-real-e2e-<role>-$RUN_ID/relay"`,
replacing `<role>` with `controller` or `node`. This creates a new private
directory and refuses to replace an existing one. Unset the input variables
afterward. Controller startup uses sudo only to assign its copied token to
container UID/GID 65532; the node's copy stays owned by its Agent process user.
Run these scripts only on explicitly authorized test hosts.

1. On the Controller, run `scripts/real-e2e-controller.sh start`. On the node,
   run `scripts/bootstrap.sh native-packages` (see
   [Linux ARM64 Go validation](#linux-arm64-go-validation)), source
   `scripts/env.sh`, and run `scripts/real-e2e-node.sh build`.
2. Copy
   `${RUNNER_TEMP}/ocservia-real-e2e-controller-${RUN_ID}/outbox/controller-ready`
   from the Controller to the node, then run
   `scripts/real-e2e-node.sh prepare CONTROLLER_DIR` against the received copy.
3. Copy
   `${RUNNER_TEMP}/ocservia-real-e2e-node-${RUN_ID}/outbox/agent-endpoint`
   from the node to the Controller and run
   `scripts/real-e2e-controller.sh issue-token AGENT_ENDPOINT_DIR`.
4. Copy the Controller's `outbox/enrollment-token` directory to the node and run
   `scripts/real-e2e-node.sh enroll TOKEN_DIR` before the ten-minute token TTL
   elapses.
5. Copy the node's `outbox/enrollment-result` directory back to the Controller,
   then run `scripts/real-e2e-controller.sh verify-enrollment RESULT_DIR` to
   confirm the active UUIDv7 node record and its EndpointID binding.
6. Run `scripts/real-e2e-controller.sh authority AGENT_ENDPOINT_DIR`; copy its
   `outbox/business-authority` to the node and run
   `scripts/real-e2e-node.sh activate AUTHORITY_DIR`.
7. Copy the node's `outbox/business-node-ready` to the Controller; run
   `scripts/real-e2e-controller.sh register-privd NODE_READY_DIR`. Copy its
   `outbox/business-authority-registered` to the node and run
   `scripts/real-e2e-node.sh start-native REGISTERED_DIR`. Then run
   `scripts/real-e2e-controller.sh business NODE_READY_DIR AGENT_ENDPOINT_DIR`.
   Copy its `outbox/business-controller-done` to the node and run
   `scripts/real-e2e-node.sh verify-business DONE_DIR`.
8. Copy the node's `outbox/business-node-result` to the Controller; run
   `scripts/real-e2e-controller.sh verify-business RESULT_DIR`. Only this
   reconciliation proves the native business checkpoint.
9. Run the `diagnostics` phase and then the `cleanup` phase of the local script
   on each VM, including after a failed run, and delete any directories copied
   between the machines. Keep the enrollment token, private keys and unredacted
   logs private.

Each `outbox` path is relative to that role's working directory under
`RUNNER_TEMP`; the directory arguments point to the copies on the receiving VM.
Transfer them over a secure channel such as SSH or SCP. The scripts do not
automate file exchange, artifact uploads, retention or workflow cleanup.
`scripts/real-e2e-artifact.sh` serves GitHub Actions artifact operations only;
its download mode needs a live run with `GITHUB_RUN_ID`, `GITHUB_RUN_ATTEMPT`,
`GITHUB_REPOSITORY`, `GITHUB_API_URL` and `GITHUB_TOKEN`, so the manual
procedure does not use it.

### Scope and recovery profile

This profile checks enrollment and private business across separate VMs with a
persistent Agent identity, native Agent/privd/ocserv units, one local
Admin identity, privileged config and reload operations, and
an OpenConnect client in a local network namespace. Only after B01-B05
reconciliation does the hosted workflow start R01-R05:

- Capture the actual Relay path, single custom Relay arguments, native process
  and identity baselines, and the existing VPN session. Agent-owned outbound UDP
  except DNS is rejected with IPv4/IPv6 rules; Tailscale's daemon is unaffected.
- Stop the sole Relay container. Require verified HTTPS unreachability, owner
  lease invalidation and offline telemetry before submitting one direct Admin reload.
- Keep the Relay stopped until the Controller proves an unpublished Outbox and no
  sent attempt, and the Node proves no Journal/root execution, no reload,
  unchanged identity/processes and a working original VPN session.
- Start the same container, require a new connection ID and higher owner epoch
  within a 90-second observation window, and complete the same operation.
- Reconcile Controller DB, Agent Journal, privd receipt and exactly one native
  reload, including an idempotent API replay, and preserve the original VPN
  session, identity and native process starts. A stopped Relay is restored before
  cleanup even on failure; task UDP rules and the VPN client are removed by Node
  cleanup.

The observation window is a test bound, not a production RTO. All B and R
evidence must come from one exact main SHA, run and attempt. The VPN session is
local to the Agent VM, so this profile does not prove WAN ingress, production
Signer acceptance or production failure domains.

### Nodes installation wizard acceptance

On reviewed, merged `main`, manually dispatch `real-e2e.yml` with the Boolean
`node-install` input set to `true` (default `false`; reusable invocations keep
their behavior and admission restrictions) and record the run's `head_sha`, since
main can advance between inspection and dispatch. GitHub requires a dispatched
workflow to exist on the default branch, and the
[`inputs` context preserves Boolean types](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#onworkflow_dispatchinputs).
The protected Environment, main-only workflow/OIDC checks and Tailnet Grants
still apply; do not run the candidate from a PR ref or bypass review. The
operator steps of the wizard are in
[Install a managed node](../operations/agent-lifecycle.md#install-a-managed-node).

The Controller builds the current Web and mounts its output read-only at `/srv`
in the existing loopback HTTPS gateway (production Caddy is unchanged) with a
disposable certificate that the browser accepts. Playwright uses a disposable
browser context to log in as the temporary Admin, verifies the authorized
Workspace (the fixture has one, shown as text; multiple-Workspace selection stays
covered by the Web regressions), fills the install dialog and reads its
secret-free `install.env`. The configuration travels in the controller-ready
artifact. The Agent validates its exact literal fields and paths, uses the packaged
Agent offline identity preparation and fixture-owned sealing-key provisioning,
and verifies persistent identity files and stopped Agent/privd before
publishing its endpoint. The Controller uses the same UI configuration to create
one installation token;
a durable attempt marker and no retry loop prevent replay after failed or unknown
issuance. The request must carry the matching Workspace header and body,
`installation=true`, TTL 900 and no `expected_endpoint_id`. The OAEP exchange
carries the `oit1_` token, and a mode-0600 source feeds the native installer through
`--join-token-file`. The installer securely obtains configuration, registers and
activates the node, registers its privd attestation key and starts the services
without a second identity or manual confirmation. It must return `ONLINE`, retain
the prepared identity files and EndpointID, and remove temporary token material.
Controller evidence checks the consumed, identity-bound enrollment derived from
the installation token, active node and registered privd key. This fixture uses
local same-SHA release inputs; it does not prove public release downloads.

Single Admin authorization, request binding, native startup,
business checks and Relay recovery remain mandatory, and this mode also records
active, online, fresh telemetry with a heartbeat no older than 120 seconds.
Browser execution uses the library API without screenshots, traces, videos,
storage-state files or a test reporter, and errors expose only a fixed message.
Tokens, cookies and keys stay out of the allowlisted evidence and rendezvous
artifacts (except the encrypted token). All VM, Tailnet and artifact cleanup must
succeed before the verdict emits `NODE_INSTALL_E2E=PASS`.

Release boundary: the same checked-out SHA supplies a locally built v0.0.0
native package, preinstalled before the production installer runs, and the UI
Release field and installer version are v0.0.0. This exercises real
configuration, installation convergence and registration, not published asset
retrieval: `RELEASE_ASSET_DOWNLOAD=NOT_TESTED`. Local contract tests and Basic CI
are not three-VM acceptance, and `SERVER_DEVICE_DELETION=NOT_INDEPENDENTLY_VERIFIED`
still applies.

## Single Relay validation

Production installers and launch entry points admit one dedicated Relay (see
[Dedicated Relay](../operations/production-deployment.md#dedicated-relay)). Run
these checks inside a task-owned checkout on a disposable Linux environment. Any
step that installs native packages, modifies systemd units or injects network
faults belongs on the disposable Business runner or in a dedicated test
container.

### Lightweight checks

Inside an isolated environment with the repository's Rust toolchain, run:

```sh
cd rust
cargo test --locked -p ocservia-agent -p ocservia-transportd relay -- --nocapture
cargo test --locked -p ocservia-agent fenced_agent_supervisor_redials_over_hot_standby_and_advances_epoch -- --nocapture
cargo fmt --all -- --check
```

The Relay test filter exercises URL parsing, normalized duplicate detection,
forbidden URL rejection, token-file loading, and the default and disabled
behavior of the transport library. The transport crate keeps generic
multi-member and fenced-supervisor tests, but production installers and launch
entry points admit only one Relay, so these tests certify neither multi-Relay
deployments nor failover, and they do not validate an authenticated dedicated
Relay deployment. The hosted [three-VM run](#hosted-execution-and-evidence) does
start a temporary token-authenticated dedicated HTTPS Relay, but that is a test
topology reached over a Tailnet, not an installed production Relay.

To check the installer contracts, use a disposable Linux container with Bash,
Node, sudo, jq and Python, and run the scripts as an unprivileged user with
passwordless sudo so the run exercises controlled privilege elevation alongside
the root paths:

```sh
bash scripts/test-managed-node-install.sh
bash scripts/test-controller-install.sh
bash scripts/test-controller-bootstrap.sh
bash scripts/test-agent-upgrade-retry.sh
docker run --rm -v "$PWD:/source:ro" node:24.18.1-bookworm \
  python3 /source/scripts/test-relay-launchers.py
```

The node installer has one token pipeline for the hidden prompt, protected
file and retained bootstrap state. The configuration questionnaire, install.env
allowlist and manual installer branch are retired. Business fixtures already
use installation tokens; the three-VM fixture prepares its existing identity
with the Agent offline CLI and provisions fixture sealing keys directly before
running the same token installer. Manual uncertain-response recovery remains
in the Agent CLI procedures; none of these checks resets identities or keys.


`test-relay-launchers.py` swaps the fixed binary paths for argv recorders. Never run it on an installed host. In Basic CI, the installer and launcher self-tests run only when [path routing](#path-routing) sets the installer flag.

To validate the Compose configuration and authentication/database contracts, export a unique `RUN_ID`, `ARTIFACT_DIR`
and a private, owner-controlled `RUNNER_TEMP`, then run:

```sh
bash scripts/test-production-auth-config.sh
bash scripts/docs-check.sh
```

### Network probe

After building the current transportd runtime image (with its Relay launcher) and
the authenticated iroh-relay image from `deploy/production/relay.Dockerfile`, run
the probe on the disposable test host:

```sh
python3 scripts/single-relay-network-smoke.py \
  --compose-json "$ARTIFACT_DIR/platform-compose-single-empty.json" \
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

End-to-end single-node recovery runs in the Business Smoke and Integration
environment through the Release Diagnostics workflow with `run-resilience=true`
(see [Resilience and capacity](#resilience-and-capacity) and
[Business validation](#business-validation)). In Smoke, the sole-Relay recovery
scenario (R4) stops and restarts the Relay and requires a fresh Agent session, an
authorized reload and VPN traffic.

The manual integration profile adds the offline-queue scenario. While the Relay is
stopped, it confirms the node depends on that Relay, waits for the prior owner
lease to expire, and verifies that an authorized offline command produces no
published outbox record, transmission attempt, Agent journal entry or root side
effect. Once the same Relay restarts, the queued operation must finish with one
Agent journal entry, a verified root receipt and one additional native reload.
Replaying the API idempotency key must return the existing operation and command
without another reload, and a queued command that fails here cannot be accepted
as expected Unknown.

### Package lifecycle and cleanup

For current DEB and RPM packages, run `agent-package-smoke.sh` and
`release-native-package-smoke.sh` inside disposable systemd and packaging
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

The vendored iroh tree retains the local connection-lifecycle patch (see
[`rust/vendor/iroh/OCSERVIA-PROVENANCE.md`](../../rust/vendor/iroh/OCSERVIA-PROVENANCE.md)).
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
Manual Release Check invokes Integrated Business Smoke with `run-resilience=true`.

| Check | Owner and entrypoint | Required observation |
| --- | --- | --- |
| R1 Controller stack | Business Smoke with `run-resilience` | Restart Controller and transport together; both ready, fresh Agent session and new authorized business succeeds. |
| R2 Agent stack | Same smoke environment | One systemd restart transaction for privd and its dependent Agent; both active, fresh session and unchanged identity. |
| R3 Database interruption | Same smoke environment | Stop the bundled PostgreSQL container, require database-unavailable readiness, start it, recover readiness and Agent connectivity, then complete normal business. No outage mutation. External PostgreSQL databases have no Business-level recovery check. |
| R4 Sole Relay interruption | Same smoke environment | Stop the only Relay, observe Agent offline with direct UDP blocked, restart Relay, acquire a fresh session and recover authorized business and VPN traffic. |

The extended manual integration profile replaces R1 through R4 with deeper
variants: separate Controller and transport restarts, mutations submitted during
the outage, unsent offline queueing, idempotent replay, exact native reload
counts, and cross-checks across the API, database, Agent journal and root
receipts. Release Smoke does not require them. Full CI separately runs the full
database suite and the bundled and external PostgreSQL backup/restore smoke
scripts, which validate database-level recovery but not Controller, Agent or
Relay restarts.

Acceptance relies on the Business job status and the scenario checkpoints in
`result.json`; sanitized failure diagnostics are kept as seven-day Actions
artifacts. Only `controller`, `agent`, `database` and `relay` are required
checkpoints, with no synthetic overall checkpoint or standalone transport
scenario, and each must finish and pass. Runs without resilience record
`SKIPPED`. During Business runs the harness builds the Relay image from the
candidate tree; the standalone [network probe](#network-probe) verifies transport
reachability in isolation and does not exercise the business recovery chain. In
every scenario the command pipeline still enforces owner/epoch/fence/lease
validation, the transactional outbox, session and workspace-admin authorization, signatures, idempotent command IDs, the Agent SQLite journal and privd
receipts. A queued offline command proven never sent must never be treated as
expected Unknown if it fails. Passing these checks does not imply exactly-once
execution or production RTO/RPO guarantees.

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

The topology runs simulated components on one host, so passing it establishes
neither production capacity nor multi-host Relay behavior. Every Docker resource
carries the Compose project label, and the cleanup trap deletes only that
project's containers, networks, volumes and locally built images on success,
failure or signal. The run directory keeps the parameters, metrics, JSON summary,
samples, slow-SSE output, interrupted-operation state, disk snapshots, Compose
logs, container statuses and exit code; no workflow uploads them, so point
`ARTIFACT_DIR` outside the temporary directory to keep them. The script does not
preflight host disk, CPU or memory, so an undersized machine shows up as a
timeout or failed assertion, and the harness never lowers the configured load.

## Web validation

Run verification checks inside an authorized, isolated environment with all dependencies installed, selecting suites according to [Choose the validation scope](#choose-the-validation-scope). Unless a repository-root script is shown, file paths in the table below are relative to `web/`.

| Change | Relevant evidence |
| --- | --- |
| API adapters, session or Workspace | `test/api-client.test.ts`, generated-auth serialization checks; `node test/run-auth-browser.mjs` against the production build for login/Workspace/SSE races |
| Configuration/certificate workflows | `test/configuration-feature.test.ts`, `test/certificates-feature.test.ts`, `test/node-workflows.test.ts`; browser spec `e2e/node-forms.spec.ts` on desktop/mobile |
| Operations/rollouts | `test/operation-state-tone.test.ts`, `test/rollout-views.test.ts`, `test/nodes-rollout-submit.test.ts`; browser spec `e2e/agent-rollout.spec.ts` for unknown states, partial failure, polling disposal and single resume; a real one-node rollout runs in [manual integration](#business-validation) |
| Audit, overview | Corresponding view/unit tests (`test/audit-view.test.ts`, `test/overview.test.ts`) and browser spec `e2e/apple-layout.spec.ts`; verify authorization, stale/empty states and Workspace switching |
| Shared UI, styles or shell | `bash scripts/web-check.sh basic` from repository root; full `npm run test:e2e`, desktop/mobile layout and keyboard checks below |
| ESLint configuration | `npm run lint`, `npx vitest run test/eslint-vue.test.ts` (Vue rules reach SFCs) and `npx --no-install prettier --check eslint.config.ts`; lint's prelint builds the generated client |

`scripts/web-check.sh basic` builds the generated OpenAPI client and runs formatting checks, ESLint, typecheck, Vitest unit tests, the production Vite build, and generated-auth verification. Running `scripts/web-check.sh full` adds the focused authentication browser runner (`test/run-auth-browser.mjs`), while the full Playwright E2E suite requires `npm run test:e2e` or `make e2e`. Neither this runner nor stubbed Playwright specs validate a live Controller, database recovery, or host installation. Specs that exercise the development simulator (`development-simulation` and the first two `overview` tests) require a running backend stack; if the backend is not running, report those specs as not run. To run focused Playwright specs against an already-serving production bundle, export `PLAYWRIGHT_BASE_URL` beforehand.

Playwright projects are named after their browser target: `desktop` runs Chromium, `mobile` emulates an iPhone 13 viewport inside Chromium (and therefore does not provide Safari engine coverage), and `firefox` and `webkit` run the cross-engine subset covering login/Workspace selection, rollouts, node forms, password dialogs, shell/Overview layout, Nodes/detail, Operations, and audit. `npm run test:e2e` executes all four projects unless filtered with `--project`. `node test/run-auth-browser.mjs` accepts the same `--project` flags (for example `node test/run-auth-browser.mjs agent-rollout.spec.ts --project=desktop --project=webkit`) and exits with an error if a requested browser binary is missing; always report any project whose browser engine is not installed as not run.

Passing `--gateway` (which requires Docker) serves `dist/` behind the pinned production Caddy image and `deploy/production/Caddyfile` over HTTPS in place of Vite preview. Passing `--gateway=integrated` places the Integrated Edge container (`edge.Dockerfile` base image with the NGINX template and entrypoint from `deploy/production/integrated`) in front of the Integrated Caddyfile at `https://controller.localhost` on `127.0.0.1:443` so Caddy receives traffic only through Edge's SNI router and PROXY protocol. The cross-engine specs import `e2e/csp.ts`, which fails the test on any `securitypolicyviolation` event. `csp-control.spec.ts` also triggers a deliberate inline violation and asserts that the browser blocks it with `disposition=enforce` across every selected project, because the Content Security Policy is enforced without a report endpoint. Playwright's WebKit and Firefox builds do not substitute for testing on minimum supported Safari 16.4 or Firefox 128 releases.

When a pull request modifies shared UI primitives, layout, or styles, include the following evidence in the PR description:

- Before-and-after screenshots captured against identical fixtures at both 1440 px and 390 px widths for overview, nodes, node detail (including missing-node state), operations, rollout, audit, settings, and login, confirming that the document itself never scrolls horizontally.
- Verification of the keyboard skip link, desktop sidebar and mobile Sheet navigation, Nodes filter controls and detail links, and dialog open, cancel, and confirm flows returning focus to the triggering element.
- CSS and JS production bundle size comparisons against the base branch, along with the rationale for any newly added package dependencies.
- Manual Safari and Firefox test results whenever the change touches browser-sensitive CSS or DOM APIs, since Chromium mobile emulation does not cover those engines.

## Release policy

Every release qualifies through a passing CI run along this pipeline:

```text
PR -> Basic CI -> merge main
   -> manual Release Check
      - Full CI
      - Security
      - amd64 and arm64 Release products: build + Controller image security
        + package/image smoke (same reusable workflow as Release)
      - amd64 and arm64 Business Smoke + four finite single-instance recoveries
      - amd64 and arm64 Quick Install against a Pebble ACME directory
   -> PASS -> operator version confirmation -> vX.Y.Z-rc.N or vX.Y.Z tag
   -> Release
      - amd64 build + Controller image security + install/image smoke
      - arm64 build + Controller image security + install/image smoke
      - require a successful Release Check for the exact tag/build SHA
      - publish Agent assets and Controller GHCR images only after these gates pass
```

On pull requests, `Basic CI Result` remains the required check. Once changes
merge to `main`, an operator triggers Release Check manually via
`workflow_dispatch` with the required `contract_base_ref`. Every required job must pass; a failure, cancellation or
unexpected skip blocks qualification. Each run executes from scratch, without
inheriting earlier results or nominating a persistent candidate. Full CI inside
Release Check is the Basic CI Full profile (see [Basic CI](#basic-ci)), not
comprehensive acceptance. In Release Check, each native architecture builds its
Agent packages and Controller images once in `release-products.yml`. Image
scanning, native image smoke and the package lifecycle gate finish before the
products are uploaded. Business consumes those Agent packages and images;
Quick Install consumes the same Controller image artifacts for Pebble ACME,
reruns, upgrades and rollbacks. Both select exact artifact IDs returned by the
producer, then verify the producer's identity-manifest SHA-256, candidate commit,
version, architecture, complete file inventory and every file's SHA-256 before
use. Missing or mismatched products fail without rebuilding. Explicit per-arch
producer jobs avoid ambiguous matrix outputs, and failed-job reruns keep the
successful producer's exact IDs rather than reconstructing artifact names from
the new run attempt. Manual Business diagnostics still build candidate
artifacts locally and install from a loopback registry on disposable native
runners of the selected architecture; they do not publish GHCR images.

Once Release Check succeeds on the target commit, the operator confirms the
version and pushes the version tag. The tag-triggered Release workflow builds
from the tag, scans the built Controller image archives on both native
architectures, and runs install and image smoke before publishing. Image scans
gate on findings except entries approved in
[`deploy/production/image-scan-exemptions.json`](../../deploy/production/image-scan-exemptions.json),
each with a review deadline; an entry past its deadline fails the gate again.

Inside the `release-publishing` environment, before any image or Release write,
`publish` verifies that the current tag resolves to the build SHA and queries
`release-check.yml` runs for that exact SHA on `main`, triggered by
`workflow_dispatch`. The latest run must be completed successfully. Missing,
failed, cancelled or unfinished qualification and API errors block publication,
including reruns, and an older PASS cannot override a newer unsuccessful run. The
accepted run URL and SHA are recorded in the job summary. Build-only dispatches
are exempt, and Release does not rerun Full CI, Security or Business acceptance.
Only the tag-triggered `publish` job receives `contents: write`,
`packages: write` and access to the `release-publishing` environment, which is an
approval and protection boundary without GitHub Deployment records.

Three-VM acceptance is independent of this automated gate. For the next RC, the
operator must record its run or evidence reference and verified source SHA
alongside Release Check and the tag commit, and confirm all three SHAs match
before approving publication. Whether to make it a hard gate waits until the real
acceptance runs are stable.

Published release artifacts are Agent archives, plain archive checksums, DEB and
RPM packages, Controller container images, minimal Controller runtime bundles
with published checksums, and the version-stamped bootstrap files consumed by
the installers. Plain checksums guard against download corruption; they are not
cryptographic signatures or a trust root. `controller-release.json` is the
standard deployment configuration for Integrated lifecycle consumers.

During SQL artifact builds, the tooling reads the current `schema.sql` headers to
record PostgreSQL `database_migration` as `{"epoch": N, "revision": R}`, and the
source commit binds the matching schema and upgrade files. Existing launchers
also accept the positive integer format of earlier manifests, so generating a new
manifest does not need historical migration files.

AgentUpgrade keeps the Controller-authorized `target_version`, `package_sha256`
and `architecture` fields. Before protected staging, architecture verification,
safe extraction and service lifecycle transitions, the upgrade path checks the
downloaded archive's SHA-256 against the command and aborts on a mismatch. All
standard command guarantees stay in force: runtime command authorization,
transactional session and workspace-admin checks, owner/epoch/fence/lease checks,
idempotency, the Agent SQLite journal, privd receipts, Unknown outcome
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
   rebuilds the current checkout under synthetic version labels (`1.0.0 -> 1.0.1`
   by default, or `1.2.0-rc.1 -> 1.2.0-rc.2 -> 1.2.0` with `RC_LIFECYCLE=true`)
   on a disposable native systemd runner:

   ```bash
   RC_LIFECYCLE=true RUN_ID=<unique-id> ARTIFACT_DIR=<path> \
     scripts/release-native-package-smoke.sh
   ```

   `STUB_BINARIES=false` exercises real compiled binaries; `STUB_BINARIES=true`
   is reserved for fast scriptlet checks in PR CI. The fixed `1.2.0` sequence
   verifies DEB and RPM `~rc.N` version sorting, upgrade and rollback state
   snapshots, and package scriptlet transitions. Relabeling one source tree does
   not prove cross-release binary upgrade compatibility.
2. Current candidate product build and install smoke: in
   [`release-products.yml`](../../.github/workflows/release-products.yml),
   `PRODUCTS_DIR` and `VERSION` feed the candidate packages built for `VERSION`
   into `scripts/release-native-package-smoke.sh` on native `amd64` and `arm64`
   runners, checking ELF architecture identity, fresh installation, same-version
   reinstall and upgrade, rollback, package removal, state preservation and
   rejection of corrupted archives.
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
never replaces an existing version. After the same-SHA Release Check gate and
both native product builds, publication:

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
consumer's incremented attempt number. Missing IDs fail closed, and expired
artifacts require a full rerun. Once a draft or version image tag exists, even a
full rerun is blocked by the one-shot preflight and needs a new version. GitHub
creates its platform release attestation for an immutable Release; this flow adds
no separate signing service, key management or custom attestation, and plain
archive checksums keep their role.

## Database checkpoint release boundary

Stable v1.2.0 is the single prior database checkpoint; its fixed source commit,
epoch and revision numbers and schema receipts are specified in the
[upgrade checkpoint](../reference/stable-contracts.md#upgrade-checkpoint).
Databases on older releases must upgrade to v1.2.0 before moving to epoch 2, and
commits landing on main do not shift that checkpoint.

Any major database change must pass the
[SQL artifact acceptance checks](#sql-artifact-acceptance), including
fresh-install versus checkpoint-upgrade equivalence and both bundled and
external PostgreSQL backup/restore smoke paths. A build-only Release run can
validate package construction ahead of time,
but a major release still needs a passing Release Check on the exact merged main
commit before the operator tags it. Branch diagnostic runs and earlier release
results do not satisfy that gate.

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
removes the repeated ` by @GentleKingson` suffix from PR list items, leaving
external contributor attributions, PR links, category headings, manual notes and
the `Full Changelog` link untouched. Cleanup is idempotent within draft
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
and include Highlights, Upgrade notes, Known issues or a brief Validation note
only when backed by actual results, leaving detailed run evidence in GitHub
Actions. None of these note rules alters Release Check qualification or the
native build, image-security, smoke and publication gates.

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
exact Release's Agent archive, SHA-256 sidecar, DEB, RPM and managed-node
bootstrap over HTTPS, verify the archive checksum, and check the bootstrap's
embedded Release version (`RELEASE_ASSET_DOWNLOAD=PASS`). It checks the
downloaded script against release download/checksum contracts and feeds the
downloaded packages to native installation Smoke, keeping the requested tag,
architecture, exact URLs, downloaded hashes and logs without building or
publishing assets (`latest`, branch names and `0.0.0` are rejected).

This verifies published downloads and native package installation. Installer
fault injection uses mocked host commands and is labeled separately; it does not
repeat browser enrollment, automatic Controller activation or live Relay connectivity. The
three-VM wizard run's `RELEASE_ASSET_DOWNLOAD=NOT_TESTED` stays accurate for its
local same-SHA `v0.0.0` inputs, and only a successful published-RC verification closes the
gap.

### Existing checks

| Behavior | Owner |
| --- | --- |
| Native `oit1_` -> `obt2_` bootstrap installation and online node, browser Admin login/Workspace/user creation/reload/ConfigPlan apply, production Signer sealing, real VPN and internal TLS | Integrated Business Smoke (`release-business-diagnostic.yml`), amd64 and arm64 with the same checkpoint gate |
| Controller/transport, Agent/privd, bundled PostgreSQL and sole Relay recovery (external PostgreSQL has no Business-level recovery) | Business Smoke with `run-resilience=true` |
| Terminal real-node revocation: rejected unauthorized requests, direct Admin revoke with synchronous trust update and close, disconnect and no new privd effect from old authority | Business Smoke and extended business harness |
| Native session disconnect and IP-ban removal; CSR, issue, one-use P12, revoke and persistence (including browser checks); offline queue and exact root effects; one-node browser canary rollout to a locally built next Agent version | Extended business harness (`BUSINESS_PROFILE=extended`) |
| DEB/RPM install, retry/removal, state preservation and unsafe-package rejection on amd64/arm64 | Native package build/install smoke (Release Check and Release) |
| Native Controller execution and exact archive OS vulnerability scans before smoke/upload, both architectures | Release Controller products (Release Check and Release) |
| Source/dependency vulnerabilities and repository secrets, within the [tool scope](#security-checks) | Security |
| Tagged-source build and GitHub/GHCR publication | Release |
| Three-VM enrollment, private business and dedicated Relay recovery on separate hosted VMs | `real-e2e.yml` (see [Real three-VM acceptance](#real-three-vm-acceptance)) |

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
network. A timeout is neither an authentication rejection nor a QUIC pass. The
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
real Agent command result; that chain belongs to the Business Smoke and
extended harness (see [Resilience and capacity](#resilience-and-capacity)).

### Build rehearsal

To rehearse an RC build across `amd64` and `arm64` (running both the candidate
product smoke for `version` and the `RC_LIFECYCLE=true` sequence while skipping
`publish`), trigger:

```bash
gh workflow run release.yml --ref <candidate-branch> -f version=<X.Y.Z>-rc.1
```

To rehearse a build-only release without publishing:

```bash
gh workflow run release.yml --ref main -f version=0.0.0
```

In a rehearsal run, the native Agent and Controller build, image-security and
smoke jobs must pass alongside asset preparation, and the Publish job must be
skipped. For local validation of release changes, run
`scripts/test-release-upgrade.mjs`, the package and installer test scripts,
ShellCheck (`scripts/lint.sh`), any local workflow linter you have installed, and
the documentation checks from
[Choose the validation scope](#choose-the-validation-scope). Never publish release
assets or touch production deployments during validation.

If a validation suite fails, rerun its owning workflow with the full set of
checks (for recovery failures, rerun the entire Business job). Failed, cancelled,
missing or unexpectedly skipped required checks block qualification. Save the run
evidence first, then clean up only the resources created for that task.
