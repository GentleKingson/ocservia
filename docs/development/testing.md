# Validate a change

Pick the smallest validation target that covers the code or documentation you
touched, and run compilation, linters, static checks, and tests inside an
authorized, task-isolated environment that has the required dependencies. If
that environment is unavailable, report which checks were blocked. GitHub
Actions remains authoritative for the pull request commit itself; local test
runs do not substitute for CI results.

## Choose the validation scope

When a change only touches documentation, run the focused checks:

```bash
make docs-check
make policy-check
git diff --check
```

Check any modified links and heading anchors manually as well, since
`docs-check` is not a general link checker. Newly created public files also
need manual inspection until they are added to the candidate index; keep
local-only notes out of that index and leave the user's git staging state
untouched.

`scripts/docs-check.sh` scans tracked files only: it rejects CRLF line endings
in `*.md`, `*.yaml`/`*.yml`, `*.proto`, `*.go`, `*.rs`, and `*.ts`, requires
every tracked Markdown file to be non-empty, restricts shared
`.claude/settings.json` to audited keys with an empty reviewed `allow` list,
and checks a small set of bootstrap text rules (the README reference to
`docs/getting-started/production.md` and the absence of `| bash -s` across
three entry pages). For agent instructions, the script checks that `AGENTS.md`
exists, that `CLAUDE.md` imports it, and that relative links and heading
anchors inside `AGENTS.md` resolve. Links or anchors in other files, along with
SQL syntax, authentication modes, database support claims, and other prose
facts, are not checked by `docs-check.sh`. `make policy-check` separately
rejects tracked local implementation-control paths and private disclosures. In
Basic CI, the `docs` job runs `docs-check.sh` on its own without `policy-check`
or `git diff --check`. If you edit executable command examples in the docs,
validate them in an authorized isolated environment.

When setting up a workspace for the first time, run the supported bootstrap
profile that matches your target check and host platform; tool versions come
from `toolchains.lock`. Running `make bootstrap` installs the `all` profile,
which is unnecessary for most single-module edits and is not supported on every
architecture (consult
[Linux ARM64 dependencies and supported profiles](#linux-arm64-go-validation)
before bootstrapping a Linux ARM64 host). Once an environment is prepared,
reuse it across changes instead of reinstalling tools on each edit. On a
supported macOS arm64 or Linux x86-64 machine, `make bootstrap` requires
`curl`, `tar`, `unzip`, `xz`, Java 17, `jq`, and ShellCheck on the host; it
reads runtime and code-generator versions from
[`toolchains.lock`](../../toolchains.lock) and verifies every downloaded
archive against [`scripts/checksums.txt`](../../scripts/checksums.txt).

For day-to-day work inside a single module, use `make test-go`,
`make test-rust`, or `make test-web`. Note that `make test-go` runs
`go test ./...` inside `control-plane` alone, skipping the `signer` module as
well as `gofmt`, `go vet`, `-count=1`, and race runs. By contrast,
`make go-check` (`scripts/go-check.sh`, which defaults to mode `full`) covers
both `control-plane` and `signer` with `gofmt`, `go vet`, `go test -count=1`,
and (except in `standard` mode) `go test -race`, using a private `TMPDIR` under
`.cache/` because the signer store refuses state directories beneath a group-
or world-writable parent such as `/tmp`. Basic CI invokes
`go-check.sh standard` without the race detector. The top-level `make test`
target (`scripts/test.sh`) runs `control-plane` Go tests, Rust workspace tests,
and Web `npm test` without covering signer, linters, or race runs, so module
targets give faster feedback during local iteration.

## Choose a broader check when needed

Reach for `make verify` when a change spans multiple modules and needs a full
local baseline, not as the default check for small single-module edits.

Under the hood, `make verify` (`scripts/verify.sh`) runs `scripts/lint.sh common`
(ShellCheck, Buf, OpenAPI lint, `check-public-repository.sh`, and
`docs-check.sh`), the Buf breaking-change check, `go-check.sh` in full mode
across both Go modules, `rust-check.sh`, the transport and Agent boundary
checks, `web-check.sh` in full mode, the public-repository policy and toolchain
consistency self-tests, `security-check.sh` (Gitleaks), `license-check.sh`, the
generated-code checks, and the P1 harness bounds self-test, deduplicating the
vet, Clippy, and Web format/lint/typecheck passes so each runs once.
Running standalone `make lint` still executes all lint steps, though its
`go vet` pass covers only `control-plane`. Even so, `make verify` does not
start database containers or run `make integration`, `make e2e`, the CI
router/selector/bootstrap self-tests, the installer or release self-tests, or
manual acceptance harnesses, and it is distinct from Basic CI.

In `scripts/web-check.sh`, the script builds the generated API client once and
then runs `npm --ignore-scripts run lint` and `npx --no-install vue-tsc --noEmit`
from `web` (the hook override applies only to that lint invocation; running
`npm run lint` or `npm run typecheck` directly still builds the generated
client first). Locally, `make web-check` uses `full` mode, which adds the
Playwright Chromium browser authentication suite
(`web/test/run-auth-browser.mjs`), whereas Basic CI runs `basic` mode.

For subsystem-specific and integration checks, pick the entrypoint that matches
the behavior under test:

- For quick database feedback, run
  `DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 scripts/database-integration.sh` and
  `DATABASE_TEST_SCOPE=smoke ENGINE=mysql bash scripts/database-foundation-integration.sh`.
  PostgreSQL smoke executes `TestDatabaseCoreSmoke`,
  `TestDatabaseInitializationSmoke`, the enrollment restart check, and the
  `postgres-snapshot` inventory; MySQL smoke runs the same first three checks
  while omitting `mysql-snapshot`.
- Across the supported database engines (PostgreSQL 18.x and MySQL 8.4 LTS),
  Quick CI runs the smoke scope and Full CI runs `DATABASE_TEST_SCOPE=full`
  (splitting MySQL across three `DATABASE_SHARD` legs). Both CI profiles also
  run `database-artifact-policy.sh` and `database-<engine>-snapshot.sh check`
  to compare current SQL against the fixed v1.2.0 checkpoint, which the local
  smoke commands above do not invoke. Because the MySQL crash and rejection
  cases run only in Full, a passing Quick MySQL run does not cover them.
- For full database migrations and failure scenarios, `make database-integration`
  calls `scripts/database-integration.sh` against PostgreSQL (`PG_MAJOR=all`
  and `18` both target PostgreSQL 18), while MySQL uses
  `DATABASE_TEST_SCOPE=full ENGINE=mysql bash scripts/database-foundation-integration.sh`.
- For local Go and transport integration, `make integration` runs
  `scripts/local-slice-integration.sh`, pairing the `control-plane` binary with
  a PostgreSQL container and the Rust `ocservia-transportd-stub` over local
  Unix sockets (without MySQL, real `transportd`, an Agent, a Relay, or a
  native node).
- For browser and runtime stack behavior, `make e2e` runs `scripts/e2e.sh`,
  which starts PostgreSQL, `transportd-stub`, the Controller, and Web under
  Docker Compose before running the Playwright `e2e` service; it does not
  exercise MySQL or a native managed node.
- For real processes without a native host,
  `scripts/database-controller-e2e.sh <postgres|mysql> [all|split]` runs
  `TestControllerTransportBackendE2E` with the real Controller, `transportd`,
  Agent, `privd`, Relay, and ocserv inside one container (no systemd PID 1 and
  no managed-node installer). It is a manual check outside CI; native systemd
  behavior belongs to [Business Smoke and Integration](real-business-validation.md).
- For Rust workspace checks, run `make rust-check` (`cargo fmt --check`, Clippy
  with `-D warnings`, and workspace tests); for full Web verification including
  the browser auth run, use `make web-check`.
- For multi-host, native-runner, and release qualification checks, follow
  [cross-VM enrollment validation](cross-vm-enrollment-validation.md) (module
  tests and browser fixtures do not replace two-VM testing),
  [Business Smoke and Integration](real-business-validation.md) on authorized
  disposable native runners, and [Release Check](release-checks.md) (with
  single-node recovery covered in [Resilience](resilience.md)).

## Command wire contracts

Go's `commandauth.TestStrictWireCommandFixtures` and Rust's
`contracts::strict_wire` test suite share `testdata/command-strict-wire.json`.
The file keeps the six historical test vectors and adds all 18 payload variants
populated with non-default field values, while the full echo envelope exercises
authorization, owner fences, timestamps, and envelope metadata. Using
reflection, the Go test verifies that at least one fixture populates every
field reachable from `CommandEnvelope` (including every payload variant), and
the Rust suite independently asserts the decoded values.

On the Rust side, the tests compare the hand-maintained wire policy against the
generated `FILE_DESCRIPTOR_SET` rooted at `CommandEnvelope`. Go writes the
imported `Timestamp` descriptor into the shared fixture file and checks it
against Go's generated descriptor on each run. Together, the suites test
message boundaries, scalar wire types, and reserved tag holes across tags 1
through 128, while rejecting unknown tags, all five mismatched wire types, and
truncated nested messages along every reachable path. Descriptor-mutation tests
confirm that adding or removing fields, altering wire types or nested message
types, or introducing new payload variants fails the check instead of passing
silently. Production allowlists are never auto-generated from descriptors;
adding a protocol field requires an explicit review of the protocol version,
capability negotiation, and strict wire policy.

Run the focused contract suites in an authorized, isolated checkout:

```bash
(cd control-plane && go test -race -count=1 -skip Integration ./internal/commandauth ./internal/contractpolicy ./internal/privdattestation)
(cd rust && cargo test --locked -p ocservia-contracts -p ocservia-command-authorization -p ocservia-privd-attestation)
```

When intentionally updating the wire fixtures, run the Go test with
`UPDATE_STRICT_WIRE_FIXTURES=1`, then rerun both the Go and Rust suites without
that environment variable and inspect the resulting diff. That flag invokes an
explicit test-fixture generator, not an automatic golden-file updater. After
regenerating protobuf code, run the Buf generation and breaking-change checks
along with `scripts/generated-clean.sh --skip-generate`; if the HTTP OpenAPI
schemas did not change, you can skip Web client generation.

The focused Go command above skips database integration tests. Separately,
`scripts/test-secret-scan-config-runtime.sh` verifies that the fixed public
fixture values pass the exact-value Gitleaks allowlist while modified secrets
are still caught.

Keep in mind that the vectors in `testdata/command-strict-wire.json` test raw
wire decoding instead of authenticated command execution: they populate
deprecated fields to exercise accepted wire tags, carry dummy signature bytes,
and do not relax protocol 1.1's runtime rejection of legacy password fields.
Canonical signing inputs, owner fences, and privd receipts remain governed by
their own golden test suites.

## Linux ARM64 Go validation

`scripts/bootstrap.sh` installs pinned repository tools without managing host
OS packages. On Linux `aarch64`, bootstrap supports `go-test`, `rust-basic`,
`native-packages`, `package-tools`, `image-security`, and `npm-security` (the
last of which installs pinned Node and npm for native Business configuration
helpers). Requesting any other profile on Linux ARM64 (such as `all`, `native`,
Web, quality, or combined security) fails up front because sccache and
quality-tool binary mappings are not provided for that architecture. Linux
AMD64 and Darwin ARM64 retain their full profile mappings, though running the
ARM64 Go procedure below does not validate those platforms or execute Rust
acceptance.

When you run `scripts/bootstrap.sh go-test` on Linux ARM64, the script fetches
`go<VERSION>.linux-arm64.tar.gz` for the version locked in `toolchains.lock`
from `https://go.dev/dl/`. The expected SHA-256 digests in
`scripts/checksums.txt` are taken from the
[official download manifest](https://go.dev/dl/?mode=json&include=all), never
from hashing an unverified local download, and any missing checksum or digest
mismatch in a download or cache entry aborts before extraction. Once verified,
the archive is unpacked into a temporary `.tools/.go-install-*` directory, and
the new binary must report the locked `GOVERSION` and native
`GOHOSTOS`/`GOHOSTARCH` (ignoring any `GOOS`/`GOARCH` cross-compilation
variables) before it replaces `.tools/go`. Subsequent runs reuse a matching
`.tools/go` installation in place.

Sourcing `scripts/env.sh` sets `GOTOOLCHAIN=local`, puts repository tools
at the front of `PATH`, and directs Go caches to
`.cache/go-build`, `.cache/gopath`, and `.cache/go-mod`, with downloaded
archives stored in `.cache/downloads`. It never falls back to a system Go
installation, downloads Go toolchains on the fly, touches `/usr/local/go`, or
edits your shell profile. While bootstrap checks that host jq is
present, it does not check for Docker, Ruby, or a C compiler, so a
passing bootstrap alone does not mean the host is ready for every test suite.

### Dependencies by entrypoint

Every script expects Bash and standard Unix file and process utilities. Beyond
that, prepare only the host dependencies required by the entrypoint you plan to
run:

| Entrypoint | Additional host dependencies |
| --- | --- |
| `bootstrap.sh go-test` | curl with trusted CA certificates, tar, gzip, sed, awk, `sha256sum` or `shasum`, jq |
| `go-check.sh standard` | Installed Go/gofmt |
| `test-required-go-tests.sh` | Go, jq, setsid, Ruby, Python 3; includes real standalone `GOWORK=off` fixtures and signal/timeout tests |
| `test-bootstrap-profiles.sh` | Ruby, tar, gzip, a SHA-256 utility and jq; disposable platform/preflight fixtures run only for CI/tooling changes |
| `docs-check.sh` | Git and `jq`; no toolchain or platform self-tests |
| `go-check.sh race` (also the race part of `full`) | `CGO_ENABLED=1`, a C compiler selected by `go env CC`, linker and C development headers; no Docker requirement |
| `database-integration.sh` smoke (delegates to `database-postgres-smoke.sh`) | Go, jq, setsid, Python 3, Docker CLI and daemon, and the race prerequisites (cgo, C compiler) |
| `database-integration.sh` manual regression/full | The smoke set plus Ruby, curl, sha256sum |
| `database-foundation-integration.sh` (MySQL) | Go, jq, setsid, Python 3, OpenSSL with `req -addext`, Docker CLI and daemon and the race prerequisites in every scope; full scope also needs `timeout` |

If a required command is missing, the Docker daemon is unreachable, cgo is
disabled, or the C toolchain cannot compile and link a test binary, the scripts
exit with a nonzero status before launching heavy test suites or database
containers. Those preflight errors are treated as hard failures, never as
SKIP, and the race preflight never disables `-race` or overrides
`CGO_ENABLED` (see the
[Go race detector requirements](https://go.dev/doc/articles/race_detector#Requirements)).
The required-test wrapper preserves child exit codes, runs test processes in
their own process groups, forwards SIGINT and SIGTERM, and cleans up stray
compiler or test subprocesses.

For database runs, the scripts spin up disposable loopback-bound containers
with generated test-only owner and runtime credentials; never pass production
credentials or print DSNs to logs. If you invoke the `database-*` required-test
groups directly, you must still set both `OCSERV_TEST_DATABASE_URL` and
`OCSERV_TEST_OWNER_DATABASE_URL`, whereas the MySQL wrapper scripts populate
their `PR02_*` environment variables automatically. Only the required-case
summaries count as acceptance evidence (synthetic selector JSON does not), and
passing the PostgreSQL 18 and MySQL smoke suites below does not certify the
full database scope or overall release readiness.

### Native preparation and execution

Verify that the host has the necessary dependencies before starting, and never
run sudo to install global system packages as part of a validation task.

Create the working directory under a short, private path inside a trusted home
directory, avoiding `/tmp` and symlinks: the transport socket tests check
directory ownership all the way up the ancestor chain and reject group- or
world-writable directories (including sticky `/tmp`), while deep paths can
exceed the Unix domain socket length limit. Keep each task's checkout,
temporary directory, and evidence directory isolated:

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

If the host already has Ruby, a working C toolchain for race checks, and Docker,
you can run the selector and bootstrap profile self-tests, race checks, and
database smoke scripts directly on the host. When any of those host packages
are missing, use the isolated container environment below, since passing native
bootstrap and ordinary Go checks does not verify that Ruby,
race, or database prerequisites are present.

### Isolated ARM64 execution environment

When the host lacks the extra userland tools above, build a task-scoped
ARM64 helper container and mount the workspace into it. This environment was
validated against the native toolchain when written; rerun it for each
candidate and retain that candidate's own evidence. The container
image deliberately omits Go so that every command inside the container runs the
same native `.tools/go/bin/go` binary installed on the host. System
packages stay confined to this disposable image: the Debian base image is
pinned by SHA-256 digest while APT resolves maintained Bookworm packages at
build time, so save the build log, image ID, and installed package versions
with the candidate's validation evidence instead of assuming `toolchains.lock`
freezes system-package versions.

```bash
image="ocservia-go-validation:$(basename "$task")"
docker build --platform linux/arm64 -t "$image" -f - "$task" <<'DOCKERFILE'
FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl tar gzip coreutils jq util-linux ruby gcc libc6-dev \
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
    ruby --version
    gcc --version
    bash scripts/test-required-go-tests.sh
    bash scripts/test-bootstrap-profiles.sh
    scripts/go-check.sh standard
    DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 scripts/database-integration.sh
    ENGINE=mysql DATABASE_TEST_SCOPE=smoke bash scripts/database-foundation-integration.sh
    scripts/docs-check.sh
    git diff --check
  '
```

Run the entire validation sequence inside a single `--init` container so the
init process reaps child processes and shell exports (including the `GOWORK=off`
fixtures) stay intact, instead of wrapping `go` in a per-invocation container
script. If you need to pass outer environment variables into the run, pass them
with explicit Docker `--env` flags. Sharing the host network lets the container
reach the loopback database ports started by the scripts, and mounting the task
directory at the identical absolute path on both host and container ensures that
temporary files and TLS fixtures are visible to the local host Docker daemon (a
remote Docker context will not work because it cannot see host paths). Docker
socket access grants elevated host privileges and is appropriate only within a
trusted, nonproduction validation environment. Matching the host and container
UID:GID keeps file ownership consistent across the bind mount without altering
runtime permission checks, SQL assertions, timeouts, or race flags.

Record the command exit codes and logs alongside the candidate SHA. Each test
script tears down its own temporary fixtures and database containers, and
`--rm` removes the runner container on exit. Once you have saved the validation
evidence, delete only this task's helper image and private directory, leaving
shared Docker base images, build caches, and other tasks' `.tools` directories
alone. In your report, distinguish any initial failures, skipped non-required
integration tests, simulated platform checks, native host Go runs, and
containerized runs.

## SQL artifact acceptance

Running `scripts/database-artifact-policy.sh all` verifies that each database
engine directory contains only `schema.sql` and `upgrade.sql`, that no legacy
migration manifests remain, and that the shared marker parser and backend
metadata checks pass. The prior release checkpoint stays pinned to v1.2.0
regardless of newer commits on main.

In `scripts/required-go-tests.txt`, both the `postgres-snapshot` and
`mysql-snapshot` test inventories require live database execution where every
listed test runs and passes with zero skips. While `postgres-snapshot` runs in
both PostgreSQL smoke and Full CI, `mysql-snapshot` belongs to
`backend-mysql-full` (`mysql-cutover` shard) and runs only in Full CI. As a
result, any change to MySQL `schema.sql`, `upgrade.sql`, or the migration
runner requires a Full run before merge. Together, the snapshot suites verify
schema, ACL, and seed equivalence between a fresh install and an upgrade from
the v1.2.0 checkpoint, forward-only upgrades, zero-mutation rejection of
unknown or tampered databases, raw checksum tampering detection, PostgreSQL
transactional rollback and locking, and MySQL subprocess kill and recovery
across fresh-install, transition, and cleanup boundaries. In the full MySQL
suite, these artifact checks run alongside the existing authentication, audit,
scheduler, telemetry, transaction, and permission tests; never substitute a
database-free test run for them.

Every database CI job fetches the v1.2.0 release history and runs
`database-<engine>-snapshot.sh check` to verify fresh-versus-checkpoint
equivalence (in Full CI, MySQL runs this step once in the `services` shard).
Full Basic CI also executes the physical PostgreSQL (external topology) and
logical MySQL backup/restore smoke scripts
(`i18-external-postgres-backup-restore-smoke.sh` and
`i18-mysql-backup-restore-smoke.sh`), confirming that migration receipts
survive backup, restore, and repeated migration and that restored PostgreSQL
instances enforce runtime read access and owner-only DDL boundaries. Across
these suites, PostgreSQL covers cleanup rollback, concurrent migration locking,
and runtime privilege separation, while MySQL Full covers every cleanup DDL
interruption point, exact repair, refusal of foreign schema replacements, and
data rollback. Audit log authenticity, checkpointed-tail transitions, and
compaction remain required business checks following the removal of legacy
migrations.

Before cutting over a major database change, qualify the exact candidate commit
through Full Basic CI, Security, Business integration (with the production
Signer and resilience enabled), and a build-only Release rehearsal, recording
the workflow URLs and commit SHAs. Branch diagnostic runs do not replace
main-branch [Release Check](release-checks.md), and publishing still requires
explicit operator confirmation. Supported checkpoint and receipt identities are
defined in the
[database migration contract](control-plane.md#current-sql-artifacts-and-bounded-upgrades).

## Bundled PostgreSQL initialization

Run the bundled PostgreSQL initializer regression only inside an authorized,
isolated Linux environment with a private checkout directory, host root access,
Python 3, `setpriv`, and a local Docker daemon. Never run it on a production
machine, with real credentials, or against existing database volumes, and pass
the target release's digest-pinned PostgreSQL 18 image instead of `latest`:

```bash
python3 scripts/test-postgres-init-observation.py \
  --image "${OCSERV_POSTGRES_IMAGE:?set the target PostgreSQL 18 digest}" \
  --expect-argv no
```

Using generated task directories, synthetic credentials, an internal Docker
network, and the real container entrypoint, the script first verifies that it
can read process argv entries before asserting that plaintext and
base64-encoded passwords never appear in argv. It also tests special
characters in passwords, preservation of existing database roles, owner and
runtime logins, physical backup verification, invalid password rejection, error
propagation, and suppression of HBA appends when an SQL statement
fails. To widen the process-inspection window, the test holds a catalog lock
during initialization, so its wall-clock runtime is longer than a normal
container startup.

Log and temporary-file checks in this script are supplementary: the harness
does not assert that log collection succeeded, and pattern matching looks only
for complete raw or base64 password strings, not arbitrary SQL-escaped
variants. Consequently, a passing run does not prove that every possible output
channel is free of credentials; document this boundary alongside saved test
results. Record the candidate commit SHA, script digest, container image
digest, exit status, and JSON report for each execution without reusing logs
from an earlier commit. The script removes its task-owned container, network,
and temporary directory on exit, and because this privileged regression is
manual-only, a passing workspace CI run does not mean it was executed.

## Authoritative references

- The current targets are defined in [`Makefile`](../../Makefile).
- The pull-request job and relevance map are in [GitHub Actions validation](github-actions.md).
