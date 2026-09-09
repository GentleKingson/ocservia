# PR-02 foundation validation

## Scope

Execution date: 2026-09-09. All Go compilation/tests and database execution were
on BuildServer through `ssh BuildServer`, in the isolated checkout
`/root/ocservia-pr02.YX4Vwr`. No production database or existing server checkout
was replaced. The workstation was used for editing, git and artifact transfer,
not database or Go tests.

Source base: `65026afc0beac680983532e1d5a0bea41dfa1a82` (merged PR-01 #192),
confirmed equal to fetched `origin/main`. The original worktree was clean.
PostgreSQL migration SQL and runner are unchanged.

The server uses a checkout-local Go wrapper running `golang:1.26.6-bookworm`
with host networking and the checkout mounted at the same absolute path. It
forwards test environment variables, including PR02 settings, and does not
filter test names. `TMPDIR` is the checkout's private, root-owned `test-tmp`.
This supplies a C compiler for `-race` without altering the host toolchain.

## Results

Commands run from the server checkout, with the private TMPDIR above:

| Command | Result | Evidence under checkout |
| --- | --- | --- |
| `PG_MAJOR=all ARTIFACT_DIR=.../artifacts/postgres bash scripts/database-integration.sh` | Exit 0; PostgreSQL 17 and 18 both completed, including the existing historical/rollback and required authentication checks | `artifacts/postgres.log`, `artifacts/postgres/` |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | Exit 0; real MySQL 8.4.10, database tests with `-race` passed in 54.540s; config passed | `artifacts/mysql-final.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | Exit 0; real MariaDB 12.3.2, database tests with `-race` passed in 25.577s; config passed | `artifacts/mariadb-final.log` |
| `go test -count=1 ./cmd/ocserv-db-foundation ./internal/platform/config` | Exit 0; new command compiled (no command-package tests), config/secret-file tests passed | `artifacts/foundation-command.log` |
| `git diff --check` | Passed | Worktree check |
| `git diff -- control-plane/migrations` | Empty | Historical SQL/runner unchanged; historical SHA-256 checks also run in PostgreSQL script |

The three database families were running during the same validation session;
MySQL/MariaDB are separate servers/jobs, not an interchangeable compatibility
label. The script pins both version and image digest:

- MySQL `8.4.10`: `sha256:8dbcf531a03aade657e181b9cf2f1d1803ce621a1d55610cb44cb531ab7d7db6`
- MariaDB `12.3.2`: `sha256:a02fe89cb597d4375812b2eac90cf9d0775d4686daa7f7cc750ebbcad7525bbc`

## What the new real-database tests exercise

- Empty initialization, repeated migration, exact logical schema 34 gate and
  stored manifest checksum conflict.
- Two competing migration runners using server locks and dedicated connections.
- A subprocess forcibly killed after a DDL commit but before step verification;
  default dirty refusal, wrong repair checksum refusal, and successful explicit
  forward recovery of the already-created object.
- Rejection of repair over a foreign/altered schema, without force-cleaning it.
- Real connection cancellation during `SLEEP` and subsequent connection-ID
  change. Real RELEASE_LOCK returning an unconfirmed result, reported as an
  error, followed by proof that the original physical connection was discarded.
- Verified TLS against the real server using an ephemeral test certificate;
  a connection without the trusted CA is rejected. TLS/session tests run
  independently on both engines.
- Session UTC, READ COMMITTED, binary collation, and microsecond truncation of
  both Go time values and server-side string-to-DATETIME conversion.
- Transaction-scoped business-row locks: different keys do not conflict,
  matching keys block, cancellation is preserved, rollback releases the lock.
- Fresh separate owner/runtime/maintenance accounts; table and column allows
  and denials, migration metadata protection, DDL/TRUNCATE denial, audit
  mutation denial and maintenance authority limits.
- Case/trailing-space distinctions, exact duplicate rejection, NULL uniqueness,
  enforced CHECKs and foreign keys.

`TestCrashChild` is intentionally skipped in the parent test process and is
launched/killed by `TestRealCrashAndRepair`; it is not a skipped acceptance
case. Database integration tests skip in the PostgreSQL-only package run when
PR02_DSN is absent, and execute in the separate new-backend jobs where the
script always sets the real DSN.

## Failures found and corrected

Early authoring runs exposed MySQL boolean CHECK syntax, a parenthesis in the
inet constraint, a reserved identifier, and cross-table CHECK name collisions.
Both manifests were subsequently created and fingerprinted independently on
their pinned servers. The temporary authoring helper is not shipped.

Fresh owner-account execution exposed MySQL binlog's trigger-creation
prerequisite. The isolated server explicitly enables
`log_bin_trust_function_creators`; no SUPER grant was added to owner/runtime.
A nondeterministic charset/collation SET ordering was fixed by not resetting
`character_set_connection` after `collation_connection`. The MariaDB crash test
also observed the server already closing the killed client's connection; only
the precise unknown-thread result is accepted for that cleanup race.

Final result is **foundation-test success, not complete PR-02 acceptance**.
The unapproved indexed-text limits and remaining JSON/array/NUL/regexp/time/
partition semantics are explicit blockers in `database-pr02-foundation.md`.
No MySQL/MariaDB Controller business workflow or production startup is enabled
or claimed as tested. The PR must remain Draft.

## PR #193 review corrections

The two additional review findings against `f6cd0e0` were reproduced and fixed
in the same isolated BuildServer checkout on 2026-09-09:

- The original bootstrap contract script exits 1 against the PR workflow:
  `database-smoke must run only basic commands`. Its matrix, environment and
  backend command routing assertions now match all four independent jobs.
  The corrected `bash scripts/test-bootstrap-profiles.sh` exits 0 in an
  ephemeral `ruby:3.4-bookworm` container with jq installed. Evidence:
  `artifacts/review-bootstrap-before.log` and `review-bootstrap-after.log`.
- Real TCP proxy tests blackhole ROLLBACK, blackhole COMMIT, or forward COMMIT
  and drop its response. With the original `backend.go` exported from
  `f6cd0e0` into a disposable source copy, all three tests fail because the
  finalizer ignores its context. The copy and isolated MySQL container were
  removed after the test. Evidence: `artifacts/review-transaction-before.log`.
- The corrected finalizers use a 300ms deadline in those tests while the
  driver's I/O timeout remains 30s. Tests require bounded completion, actual
  socket closure and a different subsequent connection ID. The lost-response
  case explicitly verifies that COMMIT was durable despite returning a timeout.
- Additional tests exercise request cancellation followed by independent
  rollback, pre-cancelled Commit/Rollback, successful connection reuse after
  cleanup-context cancellation, each isolation selector, and `Within` cleanup
  after callback errors and panics.

Final reruns (same private TMPDIR, all exit 0):

| Command | Result | Evidence under checkout |
| --- | --- | --- |
| `PG_MAJOR=all ARTIFACT_DIR=.../artifacts/review-postgres bash scripts/database-integration.sh` | PostgreSQL 17 and 18 integration passed | `artifacts/review-postgres.log`, `artifacts/review-postgres/` |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | MySQL 8.4.10, `-race` passed in 46.511s; config and CLI compilation passed | `artifacts/review-mysql.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | MariaDB 12.3.2, `-race` passed in 17.225s; config and CLI compilation passed | `artifacts/review-mariadb.log` |

These corrections do not resolve the schema/semantic acceptance blockers
listed above and do not make this Draft ready for another acceptance review.
