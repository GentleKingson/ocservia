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

## Version 3 storage and transaction milestone

The appended version-3 plans remove the eleven remaining text length limits
using exact full-value uniqueness tables. They introduce actual identity,
observed-state, command-admission and telemetry-history Store adapters, typed
JSONB/array/timestamp representations, and FK-preserving monthly telemetry
tables. See [storage adapters](database-pr02-storage-adapters.md) for the exact
ported fields, production call sites and remaining acceptance boundaries.

Published PostgreSQL migrations and MySQL/MariaDB version-1/version-2 artifacts
are unchanged. Version-3 candidates were independently authored against both
pinned engines and both genuine baseline lineages. Integration review found a
source-copy race before publication: temporary migration-owned writer guards
and immediate pre-switch value checks were added to the unpublished candidate.
Earlier candidate artifacts remain on BuildServer; runtime never learns hashes.

Final verification on BuildServer, 2026-09-09, in
`/root/ocservia-pr02.YX4Vwr`, with a private TMPDIR; all commands exited 0:

| Command | Result | Evidence under checkout |
| --- | --- | --- |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | MySQL 8.4.10 full script, `-race` package passed in 624.490s; config/CLI checks passed | `artifacts/pr02-expanded-mysql-final.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | MariaDB 12.3.2 full script, `-race` package passed in 457.992s; config/CLI checks passed | `artifacts/pr02-expanded-mariadb-final.log` |
| `PG_MAJOR=all ARTIFACT_DIR=.../artifacts/pr02-expanded-postgres-final bash scripts/database-integration.sh` | Full PostgreSQL 17/18 integration, historical migration and driver-boundary checks passed | `artifacts/pr02-expanded-postgres-final.log`, `artifacts/pr02-expanded-postgres-final/` |
| `bash scripts/test-bootstrap-profiles.sh` | CI profile contract passed, with Ruby and jq supplied by the server container | `artifacts/pr02-expanded-profiles-verified.log` |
| `bash scripts/docs-check.sh` | Passed with source paths indexed in the isolated server checkout | `artifacts/pr02-expanded-docs.log` |

After the full matrix compiled, repair was additionally tightened to reject
a missing previously recorded writer guard. The focused real-server
`TestRealMigrationProtectsVerifiedSourceCopy` rerun passed on MySQL (30.96s)
and MariaDB (22.56s), including guard restoration, stale-copy refusal and
explicit correction. Evidence: `artifacts/pr02-writer-guard-mysql.log` and
`artifacts/pr02-writer-guard-mariadb.log`. This follow-up did not change SQL or
manifest bytes. The full database scripts were not filtered.

Earlier unsuccessful attempts are retained. An older drift fixture incorrectly
marked all revisions running and was corrected to mark only the latest one.
The first PostgreSQL attempt exposed the need for an owner connection for
fixture deletion, without widening runtime privileges. A subsequent server
checkout copied with workstation UID ownership failed secure Unix-socket
ancestry validation; restoring server checkout ownership fixed that environment
failure. The isolated checkout has no Git HEAD, so server Go invocations disable
VCS stamping only. Initial profile runs lacked Ruby, then jq; both dependencies
were supplied before the successful unmodified contract test.

This is not full PR-02 acceptance. The remaining JSON/array/time columns,
sentinel semantics, outer Controller transactions and advisory-lock workflows,
automatic legacy telemetry migration, and authenticated cross-engine Controller
workflows remain outstanding. Draft and production refusal stay in force.

## Version 4 Controller and history milestone

This follow-up starts at `c3148a473169e1654a33f57ae1fc66c400cc12a1`.
The appended v4 artifacts were executed and fingerprinted independently on
MySQL 8.4.10 and MariaDB 12.3.2 against both published baseline lineages.
PostgreSQL migrations and all published v1-v3 artifacts remain byte-for-byte
unchanged. Final candidate authoring evidence is under
`artifacts/v4-author-retention/` and
`artifacts/pr02-v4-author-retention-{mysql,mariadb}.log` on BuildServer.

The exact ported service boundaries, fields and remaining gaps are recorded in
[Controller workflows](database-pr02-controller-workflows.md) and
[time decisions](database-pr02-time-decisions.md). Authentication now owns its
outer transactions through the common backend, including local administration,
session revocation and audited break-glass. Audit/RBAC, scheduler leadership and
certificate download methods use their actual domain Stores. Approval
consumption joins the caller's transaction; approval creation is not yet ported.

Thirteen more business time columns and both audit JSON summaries receive
verified read/write representations. Ambiguous historical sentinel values
require explicit owner decisions, with guarded source rows, dirty refusal and
repair. Historical telemetry now has an owner-only automatic discovery/copy
workflow, atomic source removal, recovery receipts, finite-write guards and a
retention gate. Completion cannot silently discard a concurrent legacy write.

The cross-engine HTTP test uses the real router for local login, cookie
authentication/logout, bootstrap, authorized creation, denied management,
password changes, disable/session revocation and last-administrator protection.
It does not bypass the production startup gate or stand in for every Controller
route, transport consumer, read model or certificate lifecycle.

Unsuccessful integration runs are retained, not counted as passes:

- `artifacts/pr02-v4-full-postgres.log` exposed a new latest-schema audit test
  being run against the historical schema-33 fixture. The database adapter
  tests now use the separately preserved latest schema; historical checks
  remain unchanged.
- `artifacts/pr02-v4-full-postgres-final.log` exposed a cancellation tracer
  fixture replacing only the legacy pool, not the newly used backend. Both
  handles now refer to the traced pool; the cancellation assertions remain.
- `artifacts/pr02-v4-full-postgres-final2.log` then exposed a genuine cancellation
  regression: a completed credential read was hidden by the subsequently
  cancelled read-only commit, skipping failed-password accounting. Credential
  lookup again uses a single query through a backend-owned reader; session
  issuance still rechecks credentials under transaction locks.
- The first full MariaDB run exposed its explicit serialization conflict when
  history completion races a committed writer. The test now accepts only that
  mapped conflict, verifies the receipt stays running and the source remains,
  then exercises explicit owner recovery. No production retry was introduced.
- Earlier full new-backend runs also exposed a regex fixture still writing
  native time values into the v4 auth-attempt BIGINT columns. It now uses the
  actual typed timestamp values; all username rejection assertions remain.
- Independent review found session TTL calculation moved before acquiring a
  transaction. It is restored to the previous post-acquisition point, so pool
  waits do not consume a newly issued session's lifetime.

Final verification uses the same isolated BuildServer checkout and root-owned
TMPDIR. The Go wrapper disables VCS stamping because this exported checkout has
no HEAD; no database test names are filtered from the full scripts.

| Command | Result | Evidence under checkout |
| --- | --- | --- |
| `PG_MAJOR=all bash scripts/database-integration.sh` | Exit 0; full PostgreSQL 17/18 integration, including actual authentication HTTP routes, cancellation accounting, historical SQL checks and driver ratchet | `artifacts/pr02-v4-full-postgres-final3.log` |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | Exit 0; full MySQL 8.4.10 database suite with `-race` passed in 1326.788s; actual authentication HTTP workflow and config/CLI checks passed | `artifacts/pr02-v4-full-mysql-final2.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | Exit 0; full MariaDB 12.3.2 database suite with `-race` passed in 1135.238s; actual authentication HTTP workflow and config/CLI checks passed | `artifacts/pr02-v4-full-mariadb-final2.log` |
| `go test -buildvcs=false -run '^$' ./...` | Exit 0; all Controller packages compile | `artifacts/pr02-v4-compile-final.log` |
| `bash scripts/docs-check.sh` | Exit 0 | `artifacts/pr02-v4-docs-final.log` |
| `bash scripts/test-bootstrap-profiles.sh` | Exit 0; unmodified contract executed with container Ruby/jq dependencies | `artifacts/pr02-v4-profiles-final.log` |

This remains **incomplete PR-02 acceptance**: 130 earlier DATETIME fields, nine
JSONB fields, one array field, approval creation/approval, certificate issuance
and maintenance, and remaining Controller operation/transport/read-model
transactions still require migration with their real call chains. The PR must
remain Draft and MySQL/MariaDB production startup must remain refused.

## Appended Version 5 Milestone

Version 5 extends both historical roots through the exact published v4 parents.
The MySQL manifest SHA256 is
`0446b47cc83175873680e140e1bd20dd284a89fcd2907a79a81573152ba80ad1`;
the MariaDB manifest SHA256 is
`3483d0f381fd4b1b18235eef1e21a681c96b39c227f91487da09d696c281da77`.
Both were authored against real pinned servers, including source-copy checks.
Evidence is retained under `artifacts/v5-author-all/` and
`artifacts/pr02-v5-author-{mysql,mariadb}3.log` on the same BuildServer checkout.
Versions 1 through 4 and PostgreSQL migrations remain unchanged.

This milestone converts 33 additional time fields and four JSONB fields,
ports approval creation/approval and the actual telemetry ingestion transaction,
and extends the real HTTP router workflow through independent approval of a
password reset, replay denial and session revocation. Artifact download and
authentication exercise explicit infinity/finite-extreme decisions rather than
silently mapping them to native DATETIME. A populated v4 upgrade also checks
unchanged historical receipts and exact finite/NULL preservation.

Development failures are retained, not counted as acceptance passes:

- Initial authoring exposed overlapping snapshot time/JSON writer guards and
  uppercase trigger names. The unpublished candidate uses one guarded sequence
  and valid lowercase identifiers. No published checksums were changed.
- The first focused workflow runs exposed missing v5 embed declarations.
  Both manifests are now explicitly embedded and checksum-pinned.
- The second MySQL focused run passed the database and authentication HTTP
  tests, then failed telemetry snapshot insertion because `system` is reserved.
  The adapter and readback query now quote that column; the failure remains in
  `artifacts/pr02-v5-workflows-mysql2.log`.
- Early PostgreSQL attempts failed in the exported-checkout VCS wrapper and
  during a source update while compiling embed inputs. These were setup/build
  failures, not successful database runs.

The focused MariaDB workflow script passed database authentication/approval/
artifact tests, the actual HTTP test and the actual telemetry test in
`artifacts/pr02-v5-workflows-mariadb2.log`. Full-script verification is recorded
separately below; focused runs are not substitutes for the full matrix.

All full scripts below ran without filtering database test names, using the
root-owned TMPDIR and the exported-checkout `-buildvcs=false` wrapper. All
commands exited 0. MySQL/MariaDB use distinct digest-pinned real containers.

| Command | Result | Evidence under BuildServer checkout |
| --- | --- | --- |
| `PG_MAJOR=all bash scripts/database-integration.sh` | Full PostgreSQL 17/18 integration passed, including the new approval service, authentication HTTP and telemetry ingestion workflows | `artifacts/pr02-v5-postgres3.log` |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | MySQL 8.4.10 database suite with `-race` passed in 1515.060s; actual HTTP workflow in 32.69s and telemetry workflow in 23.65s; config/CLI checks passed | `artifacts/pr02-v5-full-mysql.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | MariaDB 12.3.2 database suite with `-race` passed in 1112.700s; actual HTTP workflow in 29.74s and telemetry workflow in 18.96s; config/CLI checks passed | `artifacts/pr02-v5-full-mariadb.log` |
| `go test -buildvcs=false -run '^$' ./...` | All Controller packages compile | `artifacts/pr02-v5-compile-final.log` |
| `go test -buildvcs=false -count=1 ./internal/audit` | Unit regressions, including legacy payload/signature preservation and large JSONB numbers, passed | `artifacts/pr02-v5-audit-final.log` |
| `bash scripts/docs-check.sh` | Passed | `artifacts/pr02-v5-docs-final.log` |
| `bash scripts/test-bootstrap-profiles.sh` | Passed using container Ruby/jq dependencies; matrix/backend contract unchanged | `artifacts/pr02-v5-profiles-final.log` |

This is still **not complete PR-02 acceptance**. Of the historical pre-v4
inventory, 97 time fields, five JSONB fields and one array remain on earlier
representations. Certificate issuance/maintenance, remaining Controller outer
transactions, telemetry transport/read models/maintenance and extended-time
API representations still need their real adapters and workflows. The ingest
workflow does not yet establish signed upgrade-result/key-rotation coverage or
equivalence for caller-owned old RepeatableRead snapshots. Audit v1 retains its
existing ordinary-number float64 canonicalization; the narrow large-number
fallback preserves old signatures, not a claim to solve that older ambiguity.
Draft and the production rejection gate remain mandatory.

## Appended Version 6 Milestone

Version 6 extends both historical roots through the exact published v5 parents.
The MySQL manifest SHA256 is
`8519e36210c309f4e070199db7cee6d7c4e6838cacd6e58a9239b46bcc2ba365`;
the MariaDB manifest SHA256 is
`63d54a9b8ef56ce50d7a7581f00f0a05e4acf6d341f5e5ba5aa06d5593bcd540`.
Both were authored against real pinned servers (authoring passed in
`artifacts/v6-author/author-mysql.log` in 57.731s and
`artifacts/v6-author/author-mariadb.log` in 32.618s on the same BuildServer
checkout). Versions 1 through 5 and PostgreSQL migrations remain unchanged.

This milestone converts the eight privd attestation time fields and ports the
remaining privd attestation Controller transactions. Credential issuance,
signed registration (one-time consumption, capability grant and authorization
revision advance in one transaction) and key revocation now run through
`database.Within` on a domain Store; the legacy pgxpool constructor is a
compatibility adapter, and telemetry ingestion reads the key with the same
typed timestamp contract. Because the enrollment-credential table name
exceeds the generic guard-trigger identifier budget, this version's
exclusive-writer guards use explicit shorter names. A populated v5 upgrade
proves migration receipts and every finite historical value, including NULL
validity and consumption, survive the switch unchanged. All of these times are
business-finite; unlimited validity stays NULL and no infinity decision is
needed.

Development failures are retained, not counted as acceptance passes:

- The first focused workflow iterations failed for test-reason errors, not
  adapter errors: rotation was attempted with the already-consumed credential,
  the overlap assertion used the first activation instead of the successor
  registration instant, `consumed_at IS NULL` was scanned into the typed
  timestamp, `authorization_revision` ignored the nodes default, and
  `ValidateSchema` was first run under the runtime user, who cannot see
  triggers in `information_schema.TRIGGERS`; it now validates on the owner
  backend. The corrected focused runs are retained under
  `artifacts/v6-focused/mysql.log` (80.069s) and
  `artifacts/v6-focused/mariadb.log` (44.275s).
- The first revision-six populated-upgrade fixture failed on placeholder-count
  mismatches in the seed inserts, key IDs containing NUL bytes (hex pairs are
  used instead), a unique `registration_credential_id` collision (the revoked
  key now references the pending credential) and a wrong expected creation
  instant. These were corrected before authoring; the published manifests were
  never regenerated for them.
- Moving the service to the backend constructor tripped the driver-boundary
  ratchet as designed; the baseline records the reduced pgxpool references and
  the new compatibility adapter, and the ratchet test passes inside the full
  suite.
- The first full-matrix run failed in the config tests because the macOS rsync
  had left uid-501 file ownership, tripping the key-ancestry check. This was a
  checkout-environment failure; after `chown -R root:root` the complete matrix
  rerun below passed. Bootstrap-profile checks were not rerun for this
  increment because the bootstrap/profile/backend contract is unchanged.

All full scripts below ran without filtering database test names, using the
root-owned TMPDIR and the exported-checkout `-buildvcs=false` wrapper. All
commands exited 0. MySQL/MariaDB use distinct digest-pinned real containers.

| Command | Result | Evidence under BuildServer checkout |
| --- | --- | --- |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | MySQL 8.4.10 database suite with `-race` passed in 1322.489s; privd attestation workflow in 27.00s and populated v5 upgrade in 23.60s; API, telemetry and config/CLI checks passed | `artifacts/v6-full/mysql.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | MariaDB 12.3.2 database suite with `-race` passed in 799.310s; privd attestation workflow in 15.22s and populated v5 upgrade in 14.76s; API, telemetry and config/CLI checks passed | `artifacts/v6-full/mariadb.log` |
| `PG_MAJOR=all ARTIFACT_DIR=.../artifacts/v6-full/postgres bash scripts/database-integration.sh` | Full PostgreSQL 17/18 integration passed, including historical migration, authentication, telemetry and driver-boundary checks | `artifacts/v6-full/postgres.log`, `artifacts/v6-full/postgres/` |
| `go test -buildvcs=false -run '^$' ./...` | All Controller packages compile | `artifacts/v6-full/compile.log` |
| `bash scripts/docs-check.sh` | Passed | `artifacts/v6-full/docs.log` |

This is still **not complete PR-02 acceptance**. Of the historical pre-v4
inventory, 89 time fields, five JSONB fields and one array remain on earlier
representations. Certificate issuance/maintenance, the remaining Controller
outer transactions, telemetry transport/read models/maintenance and
extended-time API representations still need their real adapters and
workflows. Draft and the production rejection gate remain mandatory.
