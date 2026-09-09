# PR-01 validation record

## Source

- `git fetch origin main`: succeeded after requesting permission to write
  `.git/FETCH_HEAD` (the initial sandboxed fetch was denied).
- `git rev-parse HEAD origin/main a9ead272f5e5e88efce1919b11b160ee15fe3072`:
  all three were `a9ead272f5e5e88efce1919b11b160ee15fe3072`.
- `git status --short --branch`: initially clean `main...origin/main`.
- `git switch -c pr-01-database-boundary`: succeeded; no reset/rebase.
- `git diff --name-only a9ead272f5e5e88efce1919b11b160ee15fe3072 -- control-plane/migrations`:
  empty. Historical SQL and runner are unchanged.
- `git diff --check`: passed.

The access inventory was generated with `rg -l -i` over
`control-plane/internal control-plane/migrations control-plane/cmd scripts deploy`,
matching pgx, SQL statements, grants, partitions, psql, pg_dump, pg_restore,
pg_basebackup, advisory locks and database configuration, then sorted.
The 252-file candidate list includes test fixtures. Follow-up targeted searches
covered SQLSTATE/error checks, row/advisory locks, time functions, SECURITY
DEFINER, grants and non-importing API/worker SQL. See
`database-boundary-pr01.md` for findings and removal ownership.

## BuildServer setup

All tests ran via `ssh BuildServer`, in a new, isolated directory:
`/root/ocservia-pr01.A8SNio`. Source was transferred using rsync, excluding
`.git`, caches, toolchains, node_modules, target and tmp. No existing checkout
or database was replaced. Subsequent transfers used `--no-owner --no-group`.

Initial targeted tests exposed an incorrect relative path in the new boundary
test; it was corrected. Existing secure-key tests also rejected source ancestry
because rsync preserved the workstation UID. The isolated server checkout was
changed to `root:root`, and `TMPDIR` was set to its private mode-0700 `test-tmp`.
The configuration tests then passed without weakening their security checks.

The first matrix attempt could not run the existing `-race` tests because the
host has no C compiler. The successful setup uses a checkout-local `go` wrapper
that runs `golang:1.26.6-bookworm` with host networking, the checkout mounted at
the same absolute path, the current working directory and test/cache environment
variables. This supplies cgo inside the container, not a global host install.
The wrapper does not change the Go arguments or filter tests.

Images present on BuildServer:

| Image | Repository digest |
| --- | --- |
| postgres:17-bookworm | sha256:051f7b7b3abdd564d5d1bd1e8c4b9c1b6e77087d1dd22020ede611c096a272e0 |
| postgres:18-bookworm | sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af |
| golang:1.26.6-bookworm | sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 |

## Commands and results

Commands below ran in the server checkout, with `source scripts/env.sh` before
Go commands and the private TMPDIR described above. Logs are retained under
`/root/ocservia-pr01.A8SNio/artifacts` on BuildServer.

| Command | Result / evidence |
| --- | --- |
| `cd control-plane; go test -count=1 ./internal/database/... ./internal/userusage ./internal/platform/config` | Passed; `unit.log`. DB integration tests skip without a DSN here and run against real databases below. |
| `cd control-plane; bash ../scripts/required-go-tests.sh unit -p 1 ./internal/auth ./internal/api ./internal/platform/app ./internal/platform/config` | Passed; 39/39 required tests, zero required skips; `required-unit.log`. |
| `cd control-plane; go test -count=1 -v ./internal/database/...` with the PostgreSQL 17 runtime test URL | Passed including the legacy transaction bridge test; `boundary-pg17.log`. |
| `PG_MAJOR=all ARTIFACT_DIR=/root/ocservia-pr01.A8SNio/artifacts bash scripts/database-integration.sh` | Passed, exit 0; `database-integration.log`. PostgreSQL 17 and 18 both completed, followed by the legacy upgrade fixture. |
| `bash scripts/test-bootstrap-profiles.sh` | Passed in a BuildServer Ruby 3.4 container with jq installed in that disposable container; `ci-profile.log`. Covers the updated 17/18 CI matrix contract. |
| `gofmt -l` on all changed Go files and `bash -n scripts/database-integration.sh scripts/test-bootstrap-profiles.sh` | Passed with no output. |

The matrix command also executes `sha256sum -c docs/database-migrations.sha256`
before opening test databases. All 68 historical SQL hashes passed.

Both major versions passed schema compatibility, historical initialization,
rollback/fencing boundaries and the existing business integration tests. On each
major the enforced manifests report database-auth 22/22, database-api 8/8 and
database-lifecycle 1/1, with zero required skips. The new database suite passed
on both majors; the final legacy bridge test also passed in the explicit PG17
rerun. All containers for run `database-local-1-job-20260909t062327z-401668`
were removed by the script's cleanup; a final filtered `docker ps` was empty.

No merge, production backend registration, deployment or release is performed.
