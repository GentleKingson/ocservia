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
The unapproved indexed-text limits and remaining JSON/array/JSON-NUL/time/
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

## Text semantics follow-up

The next Draft revision fixes three concrete accepted-input differences:
`node_sessions.session_id` and `user_usage_cursors.session_id` now admit 256
characters; `secret_provider_refs.key_path` admits 512. Their original CHECK
limits and full unique indexes remain. The backing VARCHAR has one extra
slot so truncating excess trailing spaces cannot bypass those CHECKs.

Every ordinary text column in the two manifests now has SQL-side binary NUL
rejection, including nullable columns. Literal backslash-u0000 text, U+FFFD,
SQL NULL and empty strings are not confused with a NUL byte. This does **not**
validate strings nested inside JSON or complete the array mapping.

All ten distinct schema-34 regex predicates have a shared test corpus. The
PostgreSQL test obtains predicates from the live `pg_constraint` catalog;
the MySQL/MariaDB tests obtain the actual pinned manifest patterns. Both
execute server-side matching, with case/control/Unicode/length/anchor cases.
The corpus also exercises path traversal boundaries. Native table INSERTs
check username case/newline rejection and valid/invalid secret key paths.
Only the current schema's ASCII predicate subset is covered, not arbitrary
regex-engine equivalence.

An initial MariaDB run exposed case-folding differences in parameterized
REGEXP, including `A` matching `[a-z]` and U+017F matching `[A-Za-z]`.
Consequently the schema regex literals explicitly disable case folding on
both engines, in addition to using absolute start/end anchors. Evidence of
that failed run is `artifacts/text-mariadb.log`. Fresh independent authoring
runs on the pinned engines produced SQL checksums and postcondition hashes;
the temporary authoring test was removed before the final regression runs.
The migration runner has no hash-learning or checksum-rewrite mode.

Final verification on BuildServer, 2026-09-09, same isolated checkout and
private TMPDIR as above (all exit 0):

| Command | Result | Evidence under checkout |
| --- | --- | --- |
| `PG_MAJOR=all ARTIFACT_DIR=.../artifacts/text-postgres-final bash scripts/database-integration.sh` | PostgreSQL 17/18 integration, including the live-catalog regex and actual-column length tests, passed | `artifacts/text-postgres-final.log`, `artifacts/text-postgres-final/` |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | MySQL 8.4.10, `-race` passed in 63.829s; config and CLI compilation passed | `artifacts/text-mysql-final.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | MariaDB 12.3.2, `-race` passed in 23.911s; config and CLI compilation passed | `artifacts/text-mariadb-final.log` |
| `bash scripts/docs-check.sh` | Passed | `artifacts/text-docs.log` |

PostgreSQL historical migration diffs remain empty and historical SHA-256
checks passed in both PostgreSQL runs. Worktree `git diff --check` passed.

The indexed-text audit now distinguishes 35 fields already bounded by existing
PostgreSQL CHECKs/FKs, these three fixes, and **eleven still-unapproved limits**.
No length restriction for those eleven fields has been approved or hidden by
a prefix/hash unique index. JSON/arrays, full timestamp/sentinel semantics,
telemetry maintenance parity and business lock call sites remain open. This
is incremental progress, not full PR-02 acceptance; production remains denied.

## Append-only Draft history

The expanded PR-02 scope requires preserving every published Draft artifact.
The original `f6cd0e0` manifests (also used by `6a6e3c5`) are now archived
byte-for-byte, and the `144b660` manifests remain unchanged at their original
paths. Independent appended version-2 manifests identify both exact parent
checksums. This supersedes the earlier Draft behavior of rejecting all prior
Draft databases: known histories now have an explicit forward upgrade, while
unknown/conflicting checksums are still refused, including during repair.

Real-server tests instantiate each published baseline by executing its actual
SQL, insert existing application data, then run concurrent/repeated upgrades.
They compare all original metadata and step receipts, including timestamps,
before/after, and verify preserved application values. The original baseline
executes 58 ALTER steps; the already-corrected parent has zero ALTER receipts.
No synthetic PostgreSQL or prior-Draft execution history is inserted.

Failure tests put an invalid legacy NUL value into the original schema and
verify durable dirty refusal, explicit owner data correction and checksum-bound
repair without rewriting baseline receipts. Another test forwards an ALTER to
the real server, observes its committed response, suppresses that response and
kills the upgrader subprocess. Repair verifies the pinned postcondition without
repeating committed DDL. Conflicting revision/step checksums remain refused.
Runtime can SELECT both new metadata tables but cannot INSERT, UPDATE or DELETE
either; existing owner/runtime/maintenance and audit/DDL denials still run.

Version-2 SQL and postcondition fingerprints were authored independently on
the pinned engines. Physical CHECK ordering differs between the two upgrade
lineages, so the manifests pin each lineage's actual resulting definitions.
The temporary authoring test was removed before regression runs. Evidence is
`artifacts/revision-author.log`; migration has no hash-learning mode.

Final reruns on BuildServer, 2026-09-09, same isolated checkout and
private TMPDIR (exit 0):

| Command | Result | Evidence under checkout |
| --- | --- | --- |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | MySQL 8.4.10, `-race` passed in 102.509s; config and CLI checks passed | `artifacts/revisions-mysql-final.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | MariaDB 12.3.2, `-race` passed in 43.707s; config and CLI checks passed | `artifacts/revisions-mariadb-final.log` |
| `PG_MAJOR=all ARTIFACT_DIR=.../artifacts/revisions-postgres bash scripts/database-integration.sh` | PostgreSQL 17/18 integration, historical SHA-256 checks and driver-boundary tests passed | `artifacts/revisions-postgres.log`, `artifacts/revisions-postgres/` |
| `bash scripts/docs-check.sh` | Passed with source documentation/code paths indexed in the isolated server checkout | `artifacts/revisions-docs.log` |

Worktree `git diff --check` passed. PostgreSQL historical migrations and all
published version-1 manifest bytes are unchanged by this update.

This evidence establishes the appended-history milestone, not the expanded
PR-02 acceptance. Eleven unapproved text limits, lossless JSON/array/time
representations with actual adapters, telemetry physical storage/retention,
common business transactions and real three-engine Controller workflows remain
unfinished. Draft status and production refusal remain in force.

## Usage transaction prerequisite

The shared real-server corpus now invokes the production
`userusage.RecordTransaction` function through `database.Within` on actual
migrated tables with runtime credentials on all engines. The backend-owned
store locks the node before any cursor read, including missing cursors. Tests
do not supply a substitute business algorithm or acquire the lock for it.

Coverage includes the existing 256-character session range, replay/stale
observation suppression, counter reset, UTC month rollover, exact BIGINT
saturation, username-conflict rollback of the whole batch, a later business
error, request cancellation followed by independent rollback, and concurrent
first observations over multiple physical connections.

The production telemetry call resolves the usage store from its own wrapped
transaction. The `useroperations.RecordUsageTx` compatibility entry point now
accepts the common Tx directly; its existing integration test uses that boundary.
Its call graph currently contains only test callers, so it is not counted as
another production Controller workflow. Driver-boundary allowances were reduced,
not expanded to permit new business-layer driver dependencies.

The first MySQL attempt failed during fixture provisioning before Go tests:
the readiness probe had accepted the image's temporary socket-only server,
which then shut down. Inspection of the pinned image's entrypoint confirmed
its temporary server uses `--skip-networking`. The script now waits for the
final TCP listener and provisions accounts through TCP. The failed run remains
at `artifacts/usage-mysql.log`.

Final verification on BuildServer, 2026-09-09, same isolated checkout/private
TMPDIR; all commands exited 0:

| Command | Result | Evidence under checkout |
| --- | --- | --- |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | MySQL 8.4.10, `-race` passed in 109.366s; config/CLI checks passed | `artifacts/usage-node-lock-mysql.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | MariaDB 12.3.2, `-race` passed in 46.080s; config/CLI checks passed | `artifacts/usage-node-lock-mariadb.log` |
| `PG_MAJOR=all ARTIFACT_DIR=.../artifacts/usage-node-lock-postgres bash scripts/database-integration.sh` | PostgreSQL 17/18 integration, shared usage tests, driver-boundary and historical migration checks passed | `artifacts/usage-node-lock-postgres.log`, `artifacts/usage-node-lock-postgres/` |
| `bash scripts/docs-check.sh` | Passed with new source paths indexed in the isolated server checkout | `artifacts/usage-docs.log` |

Worktree `git diff --check` passed. No PostgreSQL migration or published
MySQL/MariaDB migration artifact changed.

This is a usage-storage prerequisite only. It does not complete telemetry
ingestion/maintenance, the eleven length restrictions, JSON/arrays, full time
semantics or the advisory-lock transaction ports. MySQL/MariaDB Controller
startup is still refused and expanded PR-02 acceptance remains incomplete.
