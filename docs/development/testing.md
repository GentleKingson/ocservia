# Validate a change

Use the smallest validation that covers the code or documentation you changed.
Run local compilation, lint, static checks and tests through `ssh BuildServer`,
in a task-isolated checkout. Do not substitute another host if it is unavailable;
report the blocked checks. GitHub Actions remains authoritative for the exact
pull request commit; local results are not CI results.

## Choose the validation scope

Start with the changed behavior and its direct contracts, not a full-repository
command. Documentation checks, module iteration, cross-module validation,
database regression, real E2E and release acceptance serve different needs.

For documentation-only changes, the focused checks are:

```bash
make docs-check
make policy-check
git diff --check
```

Also inspect changed links and anchors: `docs-check` is not a link checker and
only checks tracked Markdown. New public files need separate inspection until
they are included in the candidate index. Keep local-only material out of that
index and preserve the user's staging state.

For first-time environment preparation, use the supported bootstrap profile
for the selected check and host, with versions from `toolchains.lock`.
`make bootstrap` selects `all`; it is not required for every edit and is not
supported on every architecture. See the [Linux ARM64 dependencies and supported profiles](#linux-arm64-go-validation-on-buildserver)
before preparing BuildServer. Reuse a correctly prepared environment instead
of reinstalling it for each change.

For module-local iteration, keep using `make test-go`, `make test-rust` or
`make test-web`. `make test` intentionally runs all three modules; it is not
the shortest feedback loop for a one-module edit.

## Choose a broader check when needed

Use `make verify` for a complete baseline when the change needs cross-module
validation, not as the default first step for ordinary edits.

`make verify` runs `scripts/lint.sh common` for shared repository/protocol
checks, then the existing Go/Rust/Web checks. Equivalent vet, Clippy and Web
format/lint/type checks run once. Standalone `make lint` still performs all
lint checks. Rust Clippy covers the same workspace/targets/features without a
preceding duplicate `cargo check`; measure the whole cold chain, not the removed
command alone, when assessing its benefit.

`scripts/web-check.sh` builds the generated client once, then uses
`npm --ignore-scripts run lint` and `npx --no-install vue-tsc --noEmit` from
`web`. The hook override applies only to that explicit lint command. Independent
`npm run lint` and `npm run typecheck` still prepare their generated client.

- Quick database feedback on BuildServer: `DATABASE_TEST_SCOPE=smoke PG_MAJOR=17 scripts/database-integration.sh` and `DATABASE_TEST_SCOPE=smoke ENGINE=mysql bash scripts/database-foundation-integration.sh`
- All supported database units: use `DATABASE_TEST_SCOPE=compatibility` for PostgreSQL 17/18, MySQL and MariaDB; this is Full CI's key compatibility scope, not comprehensive acceptance.
- Deep database migrations or failure scenarios, explicitly opt-in: `make database-integration`
- Go and transport local integration: `make integration`
- Browser or runtime behavior: `make e2e`
- Rust behavior or boundaries: `make rust-check`
- Web behavior: `make web-check`
- Real cross-VM behavior: follow [real E2E validation](real-e2e.md); module checks and browser fixtures are not substitutes
- Signed candidate business checks on authorized native runners: [T07 Release Business Smoke](real-business-validation.md); the separate extended profile retains unmatched production-path coverage
- Formal release/readiness: use the G6 workflow and read [G6 readiness](g6-readiness.md)

Do not run the formal G6 harness for an ordinary documentation change unless
the change touches its acceptance contracts or execution paths.

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

Run focused checks on BuildServer in an isolated checkout:

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
`scripts/test-g6-secret-scan-config-runtime.sh` checks that fixed public fixture
values pass the exact-value allowlist while other values remain detectable.

These are raw-wire vectors, not executable or authenticated requests: deprecated
fields remain populated to exercise accepted wire tags, signatures are dummy
bytes, and protocol 1.1 execution still rejects legacy password fields. The
independent canonical signing, fence and receipt goldens remain authoritative
for their own contracts and are not replaced by these fixtures. PR-05 changes
test coverage and its fixture allowlist, not protocol version, ALPN, canonical
bytes or runtime dependencies.

## Linux ARM64 Go validation on BuildServer

Run local validation through `ssh BuildServer`. Bootstrap installs repository
tools, not system packages. Linux `aarch64` supports `go-test` and retains the
existing `rust-basic` and `native-packages` paths. Other ARM64 profiles (including
`all`, `native`, Web, quality and security profiles) fail before installation:
their Node, sccache or quality-tool artifact mappings are not supplied. Linux
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
| `docs-check.sh` | Git; no toolchain or platform self-tests |
| `go-check.sh race` (also the race part of `full`) | `CGO_ENABLED=1`, a C compiler selected by `go env CC`, linker and C development headers; no Docker requirement |
| `database-integration.sh` smoke/compatibility | Go, jq, setsid, Docker CLI and daemon; no race/compiler probe |
| `database-integration.sh` manual regression/full | Also needs race prerequisites, Ruby, Python 3, curl, sha256sum; legacy full also needs patch |
| `database-foundation-integration.sh` | Go, jq, setsid, Python 3, OpenSSL with `req -addext`, Docker CLI and daemon; only manual regression/full need race prerequisites, legacy full diagnostics also use timeout |

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
is not. PostgreSQL 17 and MySQL smoke below do not certify PostgreSQL 18,
MariaDB, full scope or release readiness.

### Native preparation and execution

On the 2026-09-18 BuildServer check (Ubuntu 26.04, `aarch64`), jq, setsid,
Docker, Python 3, OpenSSL and patch were present, but Go, Ruby and a C compiler
were absent. Recheck instead of assuming this inventory remains current.
Do not install global packages with sudo as part of a validation task.

Use a short, private path beneath a trusted home directory, **not `/tmp` or a
symlink**: transport socket tests validate every ancestor's ownership and reject
group/world-writable directories, even sticky `/tmp`. Long paths can also
exceed Unix socket limits. Keep all task resources separate:

```bash
ssh BuildServer
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

The following complete test environment was exercised on BuildServer. It has
no Go of its own: it runs the **same native `.tools/go/bin/go`** installed above.
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
    DATABASE_TEST_SCOPE=smoke PG_MAJOR=17 scripts/database-integration.sh
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
Go run and container run must be reported separately. Earlier ARM64 workaround
measurements remain historical records, not instructions to wrap Go now.

## Bundled PostgreSQL initialization

Run the focused initializer regression only in an authorized, isolated
BuildServer environment, using a private checkout directory, host root, Python
3, `setpriv`, and a local Docker daemon. Do not use a production host, real
credentials, or existing database volumes. Select the target release's
digest-pinned PostgreSQL 17 image rather than `latest`:

```bash
python3 scripts/test-postgres-init-observation.py \
  --image "${OCSERV_POSTGRES_IMAGE:?set the target PostgreSQL 17 digest}" \
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
- Machine-readable release-readiness contracts are in [`acceptance/`](../acceptance/README.md).
