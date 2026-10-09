# Validate a change

Use the smallest validation that covers the code or documentation you changed.
Run compilation, lint, static checks and tests in an authorized, task-isolated
environment with the required dependencies. Report blocked checks when that
environment is unavailable. GitHub Actions remains authoritative for the exact
pull request commit; local results are not CI results.

## Choose the validation scope

For documentation-only changes, the focused checks are:

```bash
make docs-check
make policy-check
git diff --check
```

Also inspect changed links and anchors: `docs-check` is not a link checker.
New public files need separate inspection until they are included in the
candidate index. Keep local-only material out of that index and preserve the
user's staging state. `scripts/docs-check.sh` inspects tracked files only. It
rejects CRLF in `*.md`, `*.yaml`/`*.yml`, `*.proto`, `*.go`, `*.rs` and `*.ts`;
requires every tracked Markdown file to be non-empty; limits shared
`.claude/settings.json` to audited keys and an empty reviewed `allow` list; and
checks a few bootstrap text rules (the README reference to
`docs/getting-started/production.md` and no `| bash -s` in three entry pages).
It also checks the agent entry: `AGENTS.md` exists, `CLAUDE.md` imports it, and
only `AGENTS.md`'s own relative links and heading anchors resolve. It does not
verify links or anchors in any other file, nor SQL, authentication modes,
database support or any other prose fact. `make policy-check` only rejects tracked local
implementation-control paths and disclosures. The CI `docs` job runs
`docs-check.sh` alone, not `policy-check` or `git diff --check`. Changed
executable examples need focused validation in an authorized isolated environment.

For first-time environment preparation, use the supported bootstrap profile
for the selected check and host, with versions from `toolchains.lock`.
`make bootstrap` selects `all`; it is not required for every edit and is not
supported on every architecture. See the [Linux ARM64 dependencies and supported profiles](#linux-arm64-go-validation)
before preparing a Linux ARM64 environment. Reuse a correctly prepared
environment instead of reinstalling it for each change.
A supported macOS arm64 or Linux x86-64 host can prepare the full profile with
`make bootstrap`; host prerequisites include `curl`, `tar`, `unzip`, `xz`,
Java 17, `jq` and ShellCheck. Runtime and generator versions come from
[`toolchains.lock`](../../toolchains.lock), and bootstrap verifies downloads
against [`scripts/checksums.txt`](../../scripts/checksums.txt).

For module-local iteration, keep using `make test-go`, `make test-rust` or
`make test-web`. `make test-go` runs `go test ./...` only in `control-plane`:
it does not enter the `signer` module and adds no `gofmt`, `go vet`, `-count=1`
or race run. `make go-check` (`scripts/go-check.sh`, default mode `full`)
covers both `control-plane` and `signer`: `gofmt`, `go vet`, `go test -count=1`
and, outside `standard`, `go test -race`, under a private `TMPDIR` in
`.cache/` because the signer store rejects state under a group/world-writable
ancestor such as `/tmp`. Basic CI runs `go-check.sh standard` (no race).
`make test` (`scripts/test.sh`) runs the `control-plane` Go tests, the Rust
workspace tests and Web `npm test`; it has no signer, lint or race coverage and
is not the shortest feedback loop for a one-module edit.

## Choose a broader check when needed

Use `make verify` for a complete baseline when the change needs cross-module
validation, not as the default first step for ordinary edits.

`make verify` (`scripts/verify.sh`) runs `scripts/lint.sh common` (ShellCheck,
Buf, OpenAPI lint, `check-public-repository.sh`, `docs-check.sh`), the Buf
breaking check, `go-check.sh` (full, both Go modules), `rust-check.sh`, the
transport and Agent boundary checks, `web-check.sh` (full), the public-repository
policy and toolchain-consistency self-tests, `security-check.sh` (Gitleaks),
`license-check.sh`, the generated-code checks and the P1 harness bounds
self-test. Equivalent vet, Clippy and Web format/lint/type checks run once.
Standalone `make lint` still performs all lint checks, but its `go vet` covers
`control-plane` only. `make verify` does not start databases, `make integration`,
`make e2e`, the CI router/selector/bootstrap self-tests, installer or release
self-tests, or any manual acceptance, and it is not Basic CI.

`scripts/web-check.sh` builds the generated client once, then uses
`npm --ignore-scripts run lint` and `npx --no-install vue-tsc --noEmit` from
`web`. The hook override applies only to that explicit lint command. Independent
`npm run lint` and `npm run typecheck` still prepare their generated client.
`make web-check` runs mode `full`, which adds the browser authentication run
(`web/test/run-auth-browser.mjs`, needs Playwright Chromium); CI runs `basic`.

- Quick database feedback: `DATABASE_TEST_SCOPE=smoke PG_MAJOR=18 scripts/database-integration.sh` and `DATABASE_TEST_SCOPE=smoke ENGINE=mysql bash scripts/database-foundation-integration.sh`. PostgreSQL smoke runs `TestDatabaseCoreSmoke`, `TestDatabaseInitializationSmoke`, the enrollment restart check and the `postgres-snapshot` inventory. MySQL smoke runs the same first three but not the `mysql-snapshot` inventory.
- Supported database units: PostgreSQL 18.x and MySQL 8.4 LTS. Quick CI selects smoke; Full CI selects `DATABASE_TEST_SCOPE=full` (MySQL as three `DATABASE_SHARD` legs). Both also run `database-artifact-policy.sh` and `database-<engine>-snapshot.sh check` (current SQL against the fixed v1.2.0 checkpoint). The local smoke commands above do not run those two steps. Quick MySQL is not evidence for the MySQL crash/rejection cases; those run only in Full.
- Full database migrations and failure scenarios: `make database-integration` runs `scripts/database-integration.sh`, which is PostgreSQL only (`PG_MAJOR=all` and `18` both select 18). MySQL needs its own backend script: `DATABASE_TEST_SCOPE=full ENGINE=mysql bash scripts/database-foundation-integration.sh`.
- Go and transport local integration: `make integration` runs `scripts/local-slice-integration.sh`: the `control-plane` binary against a PostgreSQL container and the Rust `ocservia-transportd-stub` over local sockets. No MySQL, real `transportd`, Agent, Relay or native node is involved.
- Browser or runtime behavior: `make e2e` runs `scripts/e2e.sh`: Docker Compose with PostgreSQL, `transportd-stub`, the Controller and Web, then the Playwright `e2e` service. It is not a native-node or MySQL check.
- Rust behavior or boundaries: `make rust-check` (`cargo fmt --check`, Clippy with `-D warnings`, workspace tests)
- Web behavior: `make web-check` (full mode, includes the browser run)
- Real cross-VM behavior: follow [cross-VM enrollment validation](cross-vm-enrollment-validation.md); module checks and browser fixtures are not substitutes
- Business checks on authorized disposable native runners: [Business Smoke and Integration](real-business-validation.md)
- Release acceptance: use [Release Check](release-checks.md); selected single-node recovery checks are described in [Resilience](resilience.md)

## Command wire contracts

`testdata/command-strict-wire.json` is shared by Go's
`commandauth.TestStrictWireCommandFixtures` and Rust's `contracts::strict_wire`
tests. It retains the six historical vectors and adds all 18 payload variants
with non-default fields. The full echo envelope covers authorization, owner
fences, timestamps and envelope metadata. Go reflection requires every field
reachable from `CommandEnvelope`, including every payload alternative, to be
populated by at least one fixture. Rust independently asserts decoded values.

The Rust tests compare the hand-maintained policy to the existing generated
`FILE_DESCRIPTOR_SET`, starting only at `CommandEnvelope`. Its omitted imported
`Timestamp` descriptor is supplied by Go in the same fixture file and checked
against Go's generated descriptor on every run. The tests cover message
edges, scalar wire types and holes within the reviewed tag range (1 through
128), and reject unknown tags, all five incorrect wire types and truncated
nested bodies along every reachable message path. Descriptor mutation tests
prove that added/removed fields, changed wire types, changed nested message
types and new payload alternatives do not silently pass. Descriptors never
generate the production allowlist. New fields require explicit protocol
version, capability and strict-policy compatibility review.

Run focused checks in an authorized, isolated checkout:

```bash
(cd control-plane && go test -race -count=1 -skip Integration ./internal/commandauth ./internal/contractpolicy ./internal/privdattestation)
(cd rust && cargo test --locked -p ocservia-contracts -p ocservia-command-authorization -p ocservia-privd-attestation)
```

For deliberate fixture updates, run the Go test with
`UPDATE_STRICT_WIRE_FIXTURES=1`, then rerun both languages without that variable
and inspect the diff. This is an explicit test-only generator, not an automatic
golden update. Run the existing Buf generation/breaking checks and
`scripts/generated-clean.sh --skip-generate` after protobuf generation. No Web
generation is needed when HTTP schemas are unchanged.

The focused Go command excludes database integration tests. The existing
`scripts/test-secret-scan-config-runtime.sh` checks that fixed public fixture
values pass the exact-value allowlist while other values remain detectable.

These are raw-wire vectors, not executable or authenticated requests: deprecated
fields remain populated to exercise accepted wire tags, signatures are dummy
bytes, and protocol 1.1 execution still rejects legacy password fields. The
independent canonical signing, fence and receipt goldens remain authoritative
for their own contracts.

## Linux ARM64 Go validation

Bootstrap installs repository tools, not system packages. Linux `aarch64`
supports `go-test`, `rust-basic`, `native-packages`, `package-tools`,
`image-security` and `npm-security`. The last installs pinned Node/npm for
native Business configuration helpers. Other ARM64 profiles (including
`all`, `native`, Web, quality and combined security) fail before installation:
their sccache or quality-tool artifact mappings are not supplied. Linux
AMD64 and Darwin ARM64 keep their existing mappings; this ARM64 procedure does
not certify every profile on those platforms or run Rust acceptance.

`scripts/bootstrap.sh go-test` downloads the version in `toolchains.lock` from
`https://go.dev/dl/`. The ARM64 artifact is `go<VERSION>.linux-arm64.tar.gz`.
Expected SHA-256 values in `scripts/checksums.txt` come from the independent
[official download manifest](https://go.dev/dl/?mode=json&include=all), not from
hashing an untrusted local download. Missing checksums and mismatched downloads
or cache entries fail without extraction or replacement. A verified archive is
unpacked in a temporary `.tools/.go-install-*` directory; its executable must
report the locked `GOVERSION` and native `GOHOSTOS`/`GOHOSTARCH` before replacing
`.tools/go`. `GOOS`/`GOARCH` cross-compilation targets do not determine host
identity. Repeating bootstrap reuses a matching executable without reinstalling.

`scripts/env.sh` keeps `GOTOOLCHAIN=local`, repository tools first in `PATH`,
and Go caches in `.cache/go-build`, `.cache/gopath` and `.cache/go-mod`.
Downloads live in `.cache/downloads`. No system Go fallback, automatic Go
version download, `/usr/local/go` replacement or shell-profile change is needed.
Bootstrap still checks host jq but does not require Docker, Ruby or a C compiler
to install Go. It is not a complete development-machine readiness check.

### Dependencies by entrypoint

All scripts require Bash and ordinary Unix file/process utilities. Prepare only
the additional dependencies for the entrypoint being used:

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

Missing commands, inaccessible Docker, disabled cgo and a compiler unable to
compile/link fail with a nonzero status before expensive tests or database
containers start. These failures are not SKIP. The race preflight does not
disable `-race` or change `CGO_ENABLED`; see the
[Go race detector requirements](https://go.dev/doc/articles/race_detector#Requirements).
The existing required-test wrapper retains exit codes, independent process
groups, SIGINT/SIGTERM forwarding and cleanup of compiler/test children.

Database scripts create disposable loopback-only fixtures and their test-only
owner/runtime credentials; do not supply production credentials. Calling the
`database-*` required-test groups directly still requires both
`OCSERV_TEST_DATABASE_URL` and `OCSERV_TEST_OWNER_DATABASE_URL`. MySQL-compatible
fixtures prepare their existing `PR02_*` variables themselves. Do not print
DSNs. Required-case summaries are acceptance evidence; synthetic selector JSON
is not. PostgreSQL 18 and MySQL smoke below do not certify full scope or release readiness.

### Native preparation and execution

Check host dependencies before running. Do not install global packages with
sudo as part of a validation task.

Use a short, private path beneath a trusted home directory, **not `/tmp` or a
symlink**: transport socket tests validate every ancestor's ownership and reject
group/world-writable directories, even sticky `/tmp`. Long paths can also
exceed Unix socket limits. Keep all task resources separate:

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

With all host dependencies prepared, also run the selector/profile self-tests,
race and database commands below directly on the host. Otherwise use the tested
isolated alternative. Native bootstrap and ordinary Go checks do not imply
that host Ruby, race or database prerequisites are available.

### Isolated ARM64 execution environment

The following isolated ARM64 test environment was validated with the native
toolchain when it was written; rerun it for each candidate and keep that
candidate's own evidence. It has no Go of its own: it runs the **same native `.tools/go/bin/go`** installed above.
System packages are confined to the task image. The Debian base is digest-pinned;
APT resolves its maintained Bookworm packages at build time. Retain the build
log, resulting image ID and package versions with each validation record rather
than claiming system-package versions are frozen by `toolchains.lock`.

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

`--init` reaps descendants; run the whole command environment, not a `go` shell
wrapper spawning a container for each invocation. Shell exports and fixtures'
`GOWORK=off` remain within that environment. Pass any deliberately configured
outer environment with explicit Docker `--env` options. Host networking reaches
the scripts' loopback-only database ports, and the identical absolute bind path
makes TLS/temporary files visible to the host Docker daemon. Use a local daemon;
a remote Docker context cannot see these host paths. Docker socket access is
powerful and is only for this trusted, nonproduction validation environment.
The host/container UID:GID match keeps ownership consistent. Do not change
runtime permission assertions, SQL, timeouts or race flags to accommodate it.

Capture command statuses and logs against the candidate SHA. The scripts remove
their own temporary fixtures and database containers; `--rm` removes the test
environment. After retaining evidence, remove only this task's image and private
directory, not shared Docker images/caches or another task's `.tools`. An initial
failure, skipped non-required integration test, simulated platform test, native
Go run and container run must be reported separately.

## SQL artifact acceptance

`scripts/database-artifact-policy.sh all` enforces exactly `schema.sql` and
`upgrade.sql` per engine, absence of legacy manifests, and the shared marker
parser/backend metadata checks. The previous-checkpoint identity is fixed to
v1.2.0, not the current main branch.

The `postgres-snapshot` and `mysql-snapshot` inventories in
`scripts/required-go-tests.txt` require real database execution, final pass and
zero skips. `postgres-snapshot` runs in PostgreSQL smoke and Full; `mysql-snapshot`
is part of `backend-mysql-full` and therefore runs only in Full (the
`mysql-cutover` shard), not in Quick MySQL smoke, so a change to MySQL
`schema.sql`, `upgrade.sql` or the migration runner needs a Full run before it
is trusted. They cover checkpoint/fresh schema, ACL and seed equivalence, current
forward upgrades, unknown/altered databases with zero mutation, raw checksum
mutation, PostgreSQL rollback/locking, and MySQL subprocess kill/recovery at
fresh, transition and cleanup boundaries. The full MySQL inventory includes
these artifact checks alongside retained authentication, audit, scheduler,
telemetry, transaction and permission behavior. Do not replace them with a test
run lacking database credentials.

Each database CI job fetches the fixed release history and checks independent
checkpoint/fresh equivalence with `database-<engine>-snapshot.sh check` (in Full,
MySQL runs it once, in the `services` shard). Full Basic CI additionally runs
PostgreSQL physical (external PostgreSQL) and MySQL logical backup/restore
(`i18-external-postgres-backup-restore-smoke.sh`,
`i18-mysql-backup-restore-smoke.sh`), preserves receipts across restore and
repeat migration, and checks the restored PostgreSQL runtime read and owner-DDL
boundary. PostgreSQL coverage retains cleanup rollback, concurrency and runtime
permissions; in Full, MySQL covers every cleanup DDL interruption boundary,
exact repair, foreign replacement refusal and data rollback. Audit authenticity,
checkpointed-tail transitions and compaction remain required business coverage
after legacy migration removal.

For a major database cutover, qualify the exact candidate with Full Basic CI,
Security, Business integration with the production Signer and resilience, and
build-only Release. Preserve run URLs and commit identities. Branch diagnostics
do not replace main-only [Release Check](release-checks.md); publication requires
separate operator confirmation. The [database migration contract](control-plane.md#current-sql-artifacts-and-bounded-upgrades)
owns the supported checkpoint and receipt identities.

## Bundled PostgreSQL initialization

Run the focused initializer regression only in an authorized, isolated
environment, using a private checkout directory, host root, Python
3, `setpriv`, and a local Docker daemon. Do not use a production host, real
credentials, or existing database volumes. Select the target release's
digest-pinned PostgreSQL 18 image rather than `latest`:

```bash
python3 scripts/test-postgres-init-observation.py \
  --image "${OCSERV_POSTGRES_IMAGE:?set the target PostgreSQL 18 digest}" \
  --expect-argv no
```

The test uses generated task directories, fake credentials, an internal Docker
network and the real image entrypoint. It requires successful argv reads before
checking that passwords and their base64 forms are absent. It also exercises
special characters, existing-role preservation, both role logins, physical
backup verification, invalid passwords, error propagation and no HBA append
after an SQL error. A catalog lock extends the observation window; its duration
is not the duration of an ordinary initialization.

Its log and temporary-file observations are supplementary: collection success
is not asserted, and matching covers complete raw/base64 values, not every
SQL-escaped representation. Negative results do not establish that all output
channels are free of credentials. Keep this limitation with saved results.
Record the candidate commit, script digest, image identity, command status and
JSON output; do not relabel historical runs as executions of a new commit.
The script cleans up its task-owned container, network and temporary directory.
This privileged regression is manual; a green workspace CI does not imply it ran.

## Authoritative references

- The current targets are defined in [`Makefile`](../../Makefile).
- The pull-request job and relevance map are in [GitHub Actions validation](github-actions.md).
