# Validate a change

Use the smallest validation that covers the code or documentation you changed.
GitHub Actions remains authoritative for the exact pull request commit.

## Most changes

```bash
make bootstrap
make verify
```

For documentation-only changes, the focused checks are:

```bash
make docs-check
make policy-check
git diff --check
```

## Choose a broader check when needed

- Database migrations or database behavior: `make database-integration`
- Go and transport local integration: `make integration`
- Browser or runtime behavior: `make e2e`
- Rust behavior or boundaries: `make rust-check`
- Web behavior: `make web-check`
- Formal release/readiness: use the G6 workflow and read [G6 readiness](g6-readiness.md)

Do not run the formal G6 harness for an ordinary documentation change unless
the change touches its acceptance contracts or execution paths.

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
| `go-check.sh standard` | Installed Go/gofmt, jq, `setsid` (util-linux); the required-test wrapper also uses tee and mktemp |
| `test-required-go-tests.sh` | Go, jq, setsid, Ruby, Python 3; includes real standalone `GOWORK=off` fixtures and signal/timeout tests |
| `test-bootstrap-profiles.sh` / `docs-check.sh` | Ruby, tar, gzip and a SHA-256 utility for disposable platform/preflight fixtures; profile/workflow assertions also need jq |
| `go-check.sh race` (also the race part of `full`) | `CGO_ENABLED=1`, a C compiler selected by `go env CC`, linker and C development headers; no Docker requirement |
| `database-integration.sh` | Go/race prerequisites, jq, setsid, Ruby, Python 3, curl, sha256sum, Docker CLI and daemon access; full scope also needs patch, and PostgreSQL 18/all full needs shasum |
| `database-foundation-integration.sh` | Go/race prerequisites, jq, setsid, Python 3, OpenSSL with `req -addext`, Docker CLI and daemon access; full-scope diagnostics also use timeout |

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
is not. PostgreSQL 17 and MySQL regression below do not certify PostgreSQL 18,
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
    scripts/go-check.sh race
    DATABASE_TEST_SCOPE=regression PG_MAJOR=17 scripts/database-integration.sh
    ENGINE=mysql DATABASE_TEST_SCOPE=regression bash scripts/database-foundation-integration.sh
    bash scripts/i14-quota-expiry-backport.sh --contract-only
    bash scripts/i15-config-plan.sh --contract-only
    bash scripts/i16-config-apply.sh --contract-only
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

## Authoritative references

- The current targets are defined in [`Makefile`](../../Makefile).
- The pull-request job and relevance map are in [GitHub Actions validation](github-actions.md).
- Machine-readable release-readiness contracts are in [`acceptance/`](../acceptance/README.md).
