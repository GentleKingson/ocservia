# PR-02 foundation validation

## Current Result

The requested PR-02 migration and real three-backend Controller acceptance
have passed. The [final acceptance record](#final-pr-02-acceptance) is current;
earlier sections retain their original milestone scope and unsuccessful runs.
The PR remains Draft, with MySQL/MariaDB production startup still rejected.

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

## Telemetry Infinity History and Maintenance Follow-Up

Execution date: 2026-09-10. Source starts at `69c1ce7` (the existing v6
attestation milestone), not the earlier reviewed `a0bbe113`. All compilation,
database and Web checks ran on BuildServer in the isolated checkout
`/root/ocservia-telemetry-time.gXtIZO`. No PostgreSQL migration or MySQL/MariaDB
v1-v6 migration artifact was changed. This follow-up needs no physical schema
change: the existing telemetry BIGINT representation already stores infinity.

History now scans logical timestamps directly. The service's history and
maintenance transactions use the common backend, including offline-node
transitions, disconnected events and the final leadership fence. MySQL and
MariaDB preserve infinity during bucketing rather than rounding its encoding.
Shared tests read both infinities and repeat maintenance; the real service
workflow additionally proves a rejected fence rolls back status, events and
rollups, and repeated successful maintenance does not duplicate events.

Telemetry OpenAPI and the regenerated TypeScript client now preserve timestamp
strings. Ordinary dates remain RFC 3339; extended years use signed six-digit
years; infinities are explicit strings. The client no longer coerces these
values into JavaScript Dates or drops microseconds. This is a generated-client
type change (`at` and `since` are strings), not a conversion of other API dates.

| Command | Result | Evidence under BuildServer checkout |
| --- | --- | --- |
| `go test -buildvcs=false -run '^$' ./...` | All Controller packages compile | `artifacts/compile.log` |
| `go test -buildvcs=false -count=1 ./internal/database/value ./internal/database` | Timestamp JSON/extended-year round trips and driver ratchet pass | `artifacts/unit.log` |
| `go test -buildvcs=false -count=1 ./internal/api -run '^TestTelemetryHistoryErrors$'` | Finite/extended/infinite query parsing and error redaction pass | `artifacts/api-telemetry.log` |
| `ENGINE=mysql bash scripts/database-foundation-integration.sh` | Full database suite with race detector, authentication HTTP, telemetry service and startup-gate checks pass | `artifacts/mysql.log` |
| `ENGINE=mariadb bash scripts/database-foundation-integration.sh` | Full database suite with race detector, authentication HTTP, telemetry service and startup-gate checks pass | `artifacts/mariadb.log` |
| `PG_MAJOR=all bash scripts/database-integration.sh` | Full PostgreSQL 17 and 18 scripts pass | `artifacts/postgres.log` |
| `npm run typecheck` | Generated client build and Web typecheck pass | `artifacts/web-typecheck.log` |
| `npx --no-install vitest run test/telemetry-timestamp.test.ts test/openapi-contract.test.ts` | 15 tests pass, including five lossless client timestamp cases | `artifacts/web-telemetry.log` |

The initial PostgreSQL run timed out waiting for artifact-stream acceptance:
rsync had preserved workstation UID 501 on the test checkout directories,
which failed the trusted-owner Unix-socket ancestry check. The isolated copy
was made root-owned and the unchanged full script rerun successfully, including
the previously waiting ownersession tests. No socket validation or test was
relaxed. The first run is retained in
`artifacts/postgres-initial-untrusted-owner.log`. Web checks used Node 24.18.1
with the image's npm 11.16.0 (which warned about the repository's npm 11.19.0
pin); no lockfile or dependencies were changed.

**This is partial acceptance only.** The pre-v4 inventory still has 89 time
fields, five JSONB fields and one array pending. Certificate issuance and
maintenance, other telemetry read models, operation/transport migration and
complete three-backend Controller E2E are not completed by these checks.
In particular, the two maintenance writes to `nodes.updated_at` and
`transport_events.occurred_at` still use their finite legacy DATETIME columns
on MySQL/MariaDB. The driver ratchet was reduced only for removed telemetry
bridges. Controller startup rejection and Draft/no-merge requirements remain.

## Telemetry Reads and Certificate v7 Follow-Up (2026-09-10)

Executed on BuildServer in the isolated `/root/ocservia-telemetry-time.gXtIZO`
checkout, with loopback-only disposable databases and runtime-role workflows:

- PostgreSQL 17, MySQL and MariaDB telemetry workflows pass node/session/IP-ban
  reads, missing snapshots, exact JSON, pagination and infinite heartbeat freshness.
- MySQL and MariaDB artifact workflows pass certificate read/list/operation lookup
  across the complete timestamp endpoints, both infinities and nested DNS JSON;
  recovery of consuming artifacts and certificate expiry maintenance pass with
  idempotent alerts and rejected-fence rollback.
- PostgreSQL 17/18 certificate issuance/download/revoke lifecycle tests pass with
  added infinite/extended read checks and existing root-consumption recovery races.
- Appended v7 manifests were authored on both real engines and both published
  roots. A populated v6 upgrade test passes, preserving nullable fields, all
  microseconds at year 9999, existing positive infinity, nested DNS JSON, and the
  complete v2-v6 revision/step receipts. Published v1-v6 artifacts are unchanged.
- Complete Go compile, value/boundary checks, Web typecheck and focused generated
  client timestamp/OpenAPI tests pass. The client retains certificate timestamps
  as text and does not narrow stored DNS arrays to strings.

Evidence: `artifacts/read-{mysql,mariadb,pg17}.log`,
`artifacts/certificate-v7-{mysql,mariadb}-verified.log`,
`artifacts/certificate-v7-{pg17,pg18}-verified.log`, and
`artifacts/certificate-v7-{mysql,mariadb}.log` (manifest authoring).
One initial recovery fixture omitted mandatory consuming fields after the prior
reset; it was corrected without weakening constraints. A maintenance writer used
a finite time instead of the already-migrated alert timestamp; it was corrected
to the logical codec. Initial v7 verification exposed the missing embed entries;
the explicit manifest embed list was extended and the real tests rerun successfully.

**Still partial acceptance.** 84 original time fields, four JSONB fields and one
text-array remain on earlier storage. Certificate issuance/revocation,
operation/transport, remaining extended-time APIs and full three-backend
Controller E2E still need completion. No startup gate or Draft restriction was
relaxed, and no deployment, push, merge or ready-for-review operation was made.

## Common Certificate Transactions and v8 (2026-09-10)

BuildServer verification additionally passes:

- MySQL/MariaDB actual certificate issuance under the runtime principal: bound
  approval consumption, signer outage/retry, key revocation between signing and
  final persistence, and exactly one intent/success audit pair.
- PostgreSQL 17/18 existing certificate lifecycle with the common issuance,
  revocation and P12 database operations. Secret-reference creation/rotation in
  that lifecycle also passes after its common-store conversion.
- MySQL/MariaDB secret-reference creation, infinite timestamp readback and
  rotation preserving historical creation time; existing long-text bounds remain
  enforced with the migrated timestamp fixtures.
- Both v8 manifests authored from both genuine published roots. Populated v6
  certificate/secret-reference upgrade through v8 preserves NULL, finite
  microseconds, JSON values and prior receipts. No old manifest was rewritten.
- Complete Go compilation, value/boundary checks, Web typecheck and generated
  certificate/artifact/secret timestamp tests pass.

Evidence is in `artifacts/certificate-issue-{mysql,mariadb,pg18}.log`,
`artifacts/certificate-common-pg17.log`, `artifacts/certificate-v8-{mysql,mariadb,pg18}.log`,
`artifacts/certificate-v8-upgrade-{mysql,mariadb}.log`, and
`artifacts/secret-v8-{mysql,mariadb}.log` (authoring). Initial compilation caught
the secret-reference methods' remaining pool dependency; those methods were
migrated with v8 rather than removing that dependency from the ratchet prematurely.

**Full PR-02 acceptance remains open:** 81 original timestamp fields, four JSONB
fields and one array remain; operation creation/dispatch/recovery and transport
still prevent complete MySQL/MariaDB certificate workflows and Controller E2E.
The Draft and startup gates remain unchanged.

## Operation Read/Intent Storage and v9-v10 (2026-09-10)

BuildServer verification passes:

- Both engines and both published roots author appended v9/v10 manifests with
  exact metadata/data postconditions. Published v1-v8 manifests are unchanged.
- Real MySQL/MariaDB operation detail, exact full-key idempotent lookup, nullable
  expiry, infinite timestamps, and operation-scoped event sequence cursors.
- Common queued intent writes atomically persist operation, command, outbox and
  event, including infinite clocks; rejected transactions leave none of them.
- Queued expiry preserves positive infinity and every node lease, rolls back
  command/operation/outbox/event writes together, and emits one expiry event.
- A populated v6 upgrade through v10 preserves certificate/secret/operation/
  command/outbox timestamps at microsecond precision, NULLs, JSON and old receipts.
- PostgreSQL 17 creation/replay, controlled capability/observed-target checks,
  audit rejection and backlog recovery regressions; PostgreSQL 18 claimed-command
  expiry and certificate issue/artifact/revocation workflow regressions.
- Complete Go compilation and value/boundary checks, frozen revision checks,
  Web typecheck and 17 generated timestamp/OpenAPI tests.

Evidence: `artifacts/operation-read-{mysql,mariadb,pg17}.log`,
`artifacts/operation-v9-{mysql,mariadb}.log`,
`artifacts/command-v10-{mysql,mariadb}.log`,
`artifacts/command-v10-workflow-mariadb.log`,
`artifacts/operation-expiry-{mysql,mariadb,pg18}.log`,
`artifacts/operation-admission-pg17.log`, and
`artifacts/certificate-intent-pg18.log`.
The broader MySQL test combination exceeded its five-minute harness budget;
its timeout is not a pass. Operation/expiry/populated-upgrade tests were rerun
as a smaller combination and passed. Compilation caught an incorrect use of
pgx-style affected-row access on the common integer result; that was fixed and
the compile and real expiry tests rerun.

**Not complete Controller acceptance:** 69 original timestamp fields, four JSONB
fields and one array remain on earlier storage, and remaining operation/
transport compatibility paths still block full three-backend E2E. Draft and
startup gates are unchanged.

## Configuration Read/Intent and v11 (2026-09-10)

Both real engines and both historical roots pass v11 authoring. Runtime-principal
configuration reads/intents preserve infinite clocks and nested JSONB warning
values, including high-precision decimals, and enforce conditional desired
revision advancement/rollback. Populated v6 upgrades through v11 preserve those
values and prior receipts. PostgreSQL 17/18 configuration plan/apply/result
workflows pass with the common preparation/read/intent stores; the PostgreSQL 18
case also checks extended-time and complete stored-warning reads. Its historical
fixture update was moved to the owner connection rather than expanding runtime
UPDATE permission on immutable plans.

Evidence: `artifacts/config-v11-{mysql,mariadb}.log`,
`artifacts/config-v11-workflow-{mysql,mariadb}.log`,
`artifacts/config-common-{pg17,pg18}.log`, and `artifacts/revision-unit.log`.
Full Go compilation/value/boundary checks pass. This remains partial acceptance:
64 original timestamp columns, three JSONB columns and one array still need
physical conversion, and operation/transport compatibility paths and complete
Controller E2E are not yet done.

## Common Operation Creation (2026-09-10)

The operation creation entry point now owns a common transaction for node and
rollout claim validation, exact replay lookup, capability/observed-target checks,
approval consumption, admission limits, signed command intent, projections,
outbox/event persistence and audit. Both actual MySQL/MariaDB Service workflows
pass signed creation, replay, conflict, one intent audit and superseding creation.
The PostgreSQL 17 operations package also passes its unit/integration cases.

Both MySQL/MariaDB certificate Service workflows now additionally pass CSR
creation/replay, P12 creation and replay without re-exposing token/password,
revocation approval consumption, pending-export revocation and outbox release.
The revocation test initially omitted the reason bound into the approval content;
the fixture was corrected to bind the actual request reason, not bypass approval.
Evidence: `artifacts/operation-create-common-{mysql,mariadb,pg17}.log` and
`artifacts/certificate-operations-{mysql,mariadb}.log`.

Rollout observation reads use logical heartbeat values: missing/negative infinity
is stale and positive infinity is fresh. The still-PostgreSQL rollout mutation
code uses an explicit transaction adapter recorded in the driver ratchet, not
an unrecorded driver bypass. Dispatch/recovery, rollout mutations, transport
result processing and complete Controller E2E remain outstanding.

## Upgrade Reconciliation and v12 (2026-09-10)

Both engines and both historical roots pass v12 authoring. Focused runtime
reconciliation tests pass infinite schedules/heartbeats, reconnect-only progress,
durable success/failure, audit-failure rollback and idempotent retries. The
PostgreSQL 18 operations package also passes. Populated v6-to-v12 upgrades
preserve nullable and year-9999 timestamps and prior migration receipts.

Evidence: `artifacts/upgrade-v12-{mysql,mariadb}.log`,
`artifacts/upgrade-v12-reconcile-{mysql,mariadb}.log`, and
`artifacts/upgrade-common-pg18.log`. The populated upgrade subtest passes in
`artifacts/upgrade-v12-workflow-{mysql,mariadb}.log`; those combined runs failed
because the reconciliation fixture reused a unique node name. The focused
reruns above pass after distinct names and subtest-local failure reporting.
This does not establish complete Controller E2E; 60 original timestamp columns,
three JSONB columns and one array still need physical conversion.

## Rollout Orchestration and v13 (2026-09-10)

The complete rollout orchestration module now uses common transaction stores;
its previous pgx compatibility entries are removed from the driver ratchet.
PostgreSQL 17 creation and PostgreSQL 18 fleet lifecycle tests pass. Read models
and the regenerated client retain infinite clocks and complete exclusions JSON
arrays; the detail view handles nonstandard stored array elements without a
null/object rendering failure. Web typecheck and 19 contract/timestamp tests pass.

Both real engines and historical roots pass v13 authoring. The appended revision
converts four rollout timestamps and exclusions, retaining the 500-element
limit with decimal masking before native JSON inspection. MySQL/MariaDB runtime
Service tests pass creation, exact replay/conflict, audit rollback, infinite
approval expiry, raw JSON/clock reads, infinite dispatch claim fencing,
pause/resume, mandatory canary gating, batch dispatch, reconciliation and one
terminal rollout audit. Receipt projections in these tests are deliberately
seeded; this is not complete transport-backed Controller E2E.

Populated v6-to-v13 upgrades preserve clock/JSON/null values and old receipts.
Evidence: `artifacts/rollout-v13-{mysql,mariadb}.log`,
`artifacts/rollout-v13-workflow-{mysql,mariadb}.log`,
`artifacts/rollout-create-pg17.log`, `artifacts/rollout-advance-pg18.log`,
`artifacts/rollout-read-pg18.log`, and `artifacts/web-telemetry.log`.
The workflow fixture first omitted traceparent and then reused a globally unique
attestation key ID; both fixture errors were corrected before these passing runs.
Remaining physical conversions: 56 timestamp fields, two JSONB fields and one
array. Generic dispatch/recovery, transport, other readers/writers and complete
Controller E2E remain required; Draft status is unchanged.

## Approval Readers and Requested Stop (2026-09-10)

Approval Create/Get/Approve now retain logical timestamp values through the
API and regenerated client. Runtime workflows pass on PostgreSQL 18, MySQL and
MariaDB, including negative-infinity creation, positive-infinity expiry and
expired ancient finite timestamps. The strict expiry-after-creation constraint
is unchanged. The first extended-time fixture attempted equal negative-infinity
creation/expiry and was correctly rejected; the corrected fixture uses the
minimum finite expiry to exercise expiration without weakening the constraint.

Evidence: `artifacts/approval-read-{pg18,mysql,mariadb}.log`. Go compilation and
value/boundary checks pass; Web typecheck and 20 contract/timestamp tests pass.

Work stopped at the user's explicit request after this bounded task. Unconnected
dispatch store scaffolding and candidate v14 source were removed; the active
embedded migration chain remains v13. A MySQL-only v14 authoring experiment
exists in ignored validation artifacts, but is neither published nor part of
acceptance. No complete Controller E2E, merge, push, deployment or Draft
transition has been performed.

## Dispatch, Reconnect Recovery and v14 (2026-09-10)

Work resumed against the existing dirty v13 worktree at the user's request.
Verification used the isolated BuildServer checkout
`/root/ocservia-telemetry-time.gXtIZO`, disposable loopback databases and the
runtime database principal for business workflows. PR #193 remains Open Draft;
no push, merge, deployment or ready-for-review action was performed.

Both engines and both genuine historical roots pass v14 authoring. The four
node-command-lease/command-attempt timestamps now use logical storage. All
embedded v1-v13 artifacts retain their frozen checksums. A populated v6-to-v14
upgrade preserves year-9999 microseconds, NULL attempt completion and prior
receipts, together with the earlier certificate/configuration/rollout values.

Actual MySQL/MariaDB operation Services pass concurrent global capacity,
per-node FIFO and lease exclusion, failed-send retry, infinite lease reads,
exact extension, atomic completion rollback, Unknown priority at full capacity,
reconcile-only state preservation, exact sent frames, stale-owner rejection,
late result-completed MarkSent replay and queue metrics. Reconnect recovery
also passes outer rollback, one committed recovery, replay suppression,
signed reconcile-only envelopes and stale-owner rejection. Durable result rows
in this test are seeded explicitly; this is not complete transport E2E.

| Check | Evidence under BuildServer checkout |
| --- | --- |
| Both v14 manifests authored and postconditions verified on both historical roots | `artifacts/dispatch-v14-{mysql,mariadb}.log` |
| Populated upgrade and dispatch workflows pass with race detector | `artifacts/dispatch-v14-verified-{mysql,mariadb}.log` |
| Expanded dispatch/reconnect Service workflows pass with race detector | `artifacts/reconnect-workflow-{mysql,mariadb}.log` |
| Full PostgreSQL 17 localslice package, including real result-before-MarkSent and takeover recovery, passes | `artifacts/reconnect-results-pg17.log` |
| Full PostgreSQL 18 operations package, including reaping and reconciliation regressions, passes | `artifacts/reconnect-operations-pg18.log` |
| Complete Controller compilation passes | `artifacts/reconnect-compile.log` |
| Timestamp arithmetic, driver-boundary ratchet and frozen revision checks pass | `artifacts/reconnect-unit.log` |
| Documentation checks pass | `artifacts/dispatch-docs.log` |

The first PostgreSQL result-race run found an adapter error: connection-owner
node and connection IDs are bytea, not UUID columns. The guard now passes exact
16-byte values. The unmodified result/takeover tests pass after that correction;
the initial failing evidence remains in `artifacts/dispatch-results-pg17.log`.
No authority predicate or test assertion was relaxed.

**Full PR-02 acceptance remains open.** 52 original timestamp fields, two JSONB
fields and one array remain on earlier physical storage. Lease reaping,
transport/result writers, remaining readers and Controller startup wiring still
need migration and full three-backend Controller E2E. Connection-owner and
result clocks read by the new stores still use their legacy finite physical
representation. The production/startup rejection gate remains unchanged.

## Lease Reaping and Connection Ownership v15 (2026-09-10)

The operation lease reaper now uses common transactions and backend-owned
stores for lost sends, expired configuration applies, missing results and
bounded continuations. Outbox-first locking and SKIP LOCKED preserve concurrent
result ingestion. Runtime MySQL/MariaDB Service tests cover infinite leases,
locked outbox exclusion, first recovery at the attempt cap, unchanged semantic
identity, fixed continuation deadlines, attempt-cap stopping, and complete batch
rollback when a later envelope is corrupt. PostgreSQL operation, configuration
and localslice regressions pass with the race detector.

Both v15 manifests were authored on both genuine historical roots. The two
connection-owner timestamps now use logical storage; embedded v1-v14 checksums
remain frozen. A populated v14-to-v15 upgrade on each engine preserves prior
receipts, raw node/connection identities, epoch/incarnation and finite year-1000
and year-9999 microseconds. No historical value is guessed to be infinity.

Connection ownership and the Session Manager/Observer now use common backend
APIs. Runtime workflows cover exact stored lease deadlines, incarnation fencing,
monotonic epochs, signed-64-bit overflow refusal, infinite lease ordering,
cancelled guard cleanup, simultaneous first acquisition, natural mid-transaction
expiry and expiry during a lock wait. Manager/Observer tests use a controlled
transport registry to prove failed registration leaves stale fences unusable;
this does not establish transportd-backed E2E.

The first MySQL/MariaDB owner test found that an expired filtered shared-lock
read retained the InnoDB record lock and blocked takeover. The adapter now
rejects already-expired terms through a nonlocking read, then takes the exact
term lock and checks a fresh server clock after any wait. The original failing
runs remain in `artifacts/owner-workflow-{mysql,mariadb}.log`; those same runs
also contain the passing populated-upgrade checks. No assertion was weakened.
An initial compile failed after removing an import still needed by dispatch
extension; the import was restored and the complete compile passed.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Reaping Service workflows pass on both runtime principals | `artifacts/reaping-workflow-{mysql,mariadb}.log` |
| PostgreSQL 18 operation and PostgreSQL 17 configuration packages pass | `artifacts/reaping-operations-pg18.log`, `artifacts/reaping-config-pg17.log` |
| Both v15 manifests authored and postconditions verified on both roots | `artifacts/connection-owner-v15-{mysql,mariadb}.log` |
| Populated v14-to-v15 upgrades pass, preserving prior receipts | `TestRealVersionFourteenOwnerUpgrade` in `artifacts/owner-workflow-{mysql,mariadb}.log` |
| Fixed owner and Session Manager/Observer workflows pass with race detector | `TestRealConnectionOwner` in `artifacts/owner-workflow-fixed-{mysql,mariadb}.log` |
| Dispatch/reconnect workflows pass after the owner-clock and lock changes | `TestRealOperationDispatch` in `artifacts/owner-workflow-fixed-{mysql,mariadb}.log` |
| PostgreSQL 17 ownership and PostgreSQL 18 session packages pass | `artifacts/owner-pg17.log`, `artifacts/ownersession-pg18.log` |
| PostgreSQL 18 localslice/result/takeover regressions pass | `artifacts/reaping-owner-localslice-pg18.log` |
| Complete Controller compilation passes | `artifacts/owner-compile-fixed.log` |
| Timestamp, driver-boundary and frozen revision checks pass | `artifacts/owner-boundary.log` |
| Documentation checks pass after preparing isolated Git file metadata | `artifacts/receipt-read-docs-verified.log` |

The PostgreSQL retained-epoch lifecycle test is explicitly skipped because
`OCSERV_TEST_RETAINED_NODE_HEX` was not configured; it is not claimed as passed.
**Full PR-02 acceptance remains open:** 50 original timestamp fields, two JSONB
fields and one array remain on earlier physical storage. Transport/result
writers, remaining readers and Controller startup wiring still require
migration and complete three-backend Controller E2E. The startup rejection gate
remains unchanged. No push, merge, deployment or Draft transition was performed.

## Privileged Receipt Key Reader (2026-09-10)

Command-result and upgrade-result key reads now share the attestation domain
store through a caller-owned common transaction. PostgreSQL receipt APIs are
compatibility wrappers; no business SQL remains in `privdattestation/receipt.go`.
Telemetry upgrade ingestion calls the common verifier directly instead of
owning a duplicate key-read contract. The established finite validity/NULL
domain, canonical signatures and failure classifications remain unchanged.

Actual MySQL/MariaDB runtime workflows verify signed receipts against registered
keys, reject revoked and unknown keys, and fail closed on cancelled key reads.
The existing enrollment, rotation, revocation and exact-microsecond checks also
pass. PostgreSQL 17 localslice/result ingestion and PostgreSQL 18 upgrade
telemetry, including verification after key rotation, pass with the race
detector. A missing transaction is rejected by both the compatibility and
common receipt entry points.

Evidence: `artifacts/receipt-read-{mysql,mariadb}.log`,
`artifacts/receipt-read-localslice-pg17.log`,
`artifacts/receipt-read-telemetry-pg18.log`, `artifacts/receipt-read-nil.log`.
Complete Controller compilation and the receipt/driver-boundary tests pass in
`artifacts/receipt-read-compile.log` and `artifacts/receipt-read-unit.log`.
This is a reader migration, not complete transport or Controller E2E acceptance;
the v15 remaining inventory and startup gate are unchanged.

The first documentation check returned exit zero despite reporting missing Git
metadata in this isolated checkout; that result is not valid evidence. An empty
Git repository and intent-to-add entries for the source checkout's relevant
tracked and untracked files were prepared only in the BuildServer test copy.
The unchanged documentation script then passed with no diagnostics in
`artifacts/receipt-read-docs-verified.log`. No source-worktree Git index, commit
or remote was changed by this verification setup.

## Transport Results and Operation Readers v16 (2026-09-10)

Transport ingress, quarantine, durable cursor and command-result handling now
use caller-owned common transactions. A fixed savepoint separates permanently
invalid evidence from business state; transient errors still roll back the
entire event, including the cursor. Telemetry ingestion, reconnect recovery,
receipt verification, configuration/certificate/artifact/upgrade projections,
outbox completion, dispatch closure and audit share the event transaction.

Both v16 manifests were authored against both genuine historical roots. Three
result timestamps now use logical storage. Populated v15-to-v16 upgrades preserve
every prior revision/step receipt, NULL acceptance for rejected results, and
finite year-1000/year-9999 microseconds. Dispatch compares result creation and
attempt times directly in their common logical epoch. Earlier manifests remain
unchanged.

Runtime-principal workflows pass on MySQL and MariaDB for queued-result refusal,
result-before-MarkSent, late completion, duplicate events, quarantine replay and
evidence collision, endpoint rejection, reconcile-only and verified-absence
retry modes, unchanged infinite command update clocks, and injected result
failure with complete rollback and retry. Signed privileged results cover CSR
readiness, P12 readiness, revocation, nonterminal upgrade scheduling, successful
configuration application, healthy rollback and critical rollback failure.
Receipt replay does not increment projection versions. Forged terminal evidence
cannot change success and retains a failed verification audit.

Projection fixtures seed already-dispatched commands and approved trust keys;
P12/revoke fixtures use pre-existing legacy-issued certificates. These are
runtime Service/store checks, not a full certificate lifecycle, Agent/privd
round trip or Controller E2E. Separate real operation creation and dispatch
workflows continue to cover their own boundaries.

The first runtime runs exposed two adapter defects: no-op duplicate updates
required forbidden UPDATE access to append-only event identity, and alert
writes incorrectly supplied a native time to the already-v5 logical column.
Both were corrected without changing grants or published schema. Subsequent
projection fixture failures caught duplicate test credentials and incomplete
P12/revocation bindings; fixtures were corrected to satisfy existing production
constraints. No production validation or assertion was weakened. The initial
compile failure was an unused import left by extraction, removed before the
passing PostgreSQL runs.

Operation detail/list/summary reads now reuse the common operation store and
preserve logical clocks, nullable IDs, descending pagination and workspace
isolation. API not-found handling uses the common error. Runtime tests exercise
infinite timestamps and page boundaries; the PostgreSQL localslice regression
also verifies complete state-class summaries and transport watch recovery.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Both v16 manifests authored on both roots | `artifacts/result-v16-{mysql,mariadb}.log` |
| Populated v15-to-v16 upgrade passes | `TestRealVersionFifteenResultUpgrade` in `artifacts/transport-fixed-{mysql,mariadb}.log`; later transport tests in those initial logs failed |
| Result/attempt clock dispatch regression passes | `TestRealOperationDispatch` in `artifacts/transport-{mysql,mariadb}.log`; later transport tests in those initial logs failed |
| Complete runtime ingress/projection workflows and logical operation readers pass | `artifacts/transport-read-final-{mysql,mariadb}.log` |
| PostgreSQL 18 ingress/results and PostgreSQL 17 ingress/results/readers pass with race detector | `artifacts/transport-pg18.log`, `artifacts/transport-read-pg17.log` |
| Complete Controller compilation passes | `artifacts/transport-read-compile.log` |
| Driver-boundary/frozen manifest and matching API operation unit checks pass | `artifacts/transport-read-boundary.log` |

**Full PR-02 acceptance remains open.** 47 original timestamp fields, two JSONB
fields and one array remain on earlier physical storage. Simulator jobs,
event listings/gap reconciliation, other remaining readers/writers, startup
wiring and complete three-backend Controller E2E still need completion. Node,
transport-event, quarantine and cursor clocks currently retain finite physical
storage on MySQL/MariaDB. The production startup rejection gate is unchanged.
No push, merge, deployment or Draft transition was performed.

## Simulator Lifecycle and Event Clocks v17 (2026-09-10)

The localslice service now owns common transactions throughout simulator
creation, claim/dispatch/retry/expiry, event listing, gap reconciliation,
ingress, results and operation reads. Its PostgreSQL constructors are adapters;
no business SQL remains in localslice. Workspace reuse uses exact slug equality
and the existing transaction-owned business lock on MySQL/MariaDB, rather than
assuming trigger-owned uniqueness is a native upsert key.

Appended v17 converts eight timestamps: event occurrence and receipt,
quarantine observation, cursor update and four simulator-job clocks. Both
manifests were authored on both historical roots. The populated upgrade test
now runs v15 through v17 and checks exact finite year-1000/year-9999 values,
NULL dispatch timestamps, and unchanged prior receipts. Versions 1-16 remain
immutable. The remaining original inventory is 39 timestamp fields, two JSONB
fields and one text-array field.

Both genuine runtime-principal workflows cover exact workspace reuse,
SKIP LOCKED job claim, dispatch retry and duplicate refusal, endpoint-bound heartbeat
ingress, scoped sequence pagination and durable cursor reads. Registry failures
leave state unchanged. An injected synthetic event failure rolls back the gap
transaction, and an injected job failure leaves no orphan simulator node.
Successful gap reconciliation distinguishes connected and disconnected nodes,
marks ambiguous dispatched operations unknown, invalidates cursors and appends
only the required synthetic disconnect. Infinite event occurrence and job
availability/expiry retain their logical ordering.

Platform-event OpenAPI timestamps now reuse the established logical timestamp
schema. The pinned generator produced the checked client with schema validation
enabled; events retain strings, microseconds, expanded years and infinities.
Development and overview views do not coerce those strings into JavaScript Dates.
The first generator attempt referenced a nonexistent schema; typechecking caught
it and it was replaced with the existing schema. BuildServer has no native Java,
so generation used its existing isolated Java container and checksum-verified
7.24.0 jar. The initial runtime test compile had an unused import, removed before
the passing runs.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Both v17 manifests authored on both roots | `artifacts/transport-v17-{mysql,mariadb}.log` |
| Runtime lifecycle, populated v15-to-v17 upgrade and signed result projections pass with race detector | `artifacts/local-v17-fixed-{mysql,mariadb}.log` |
| PostgreSQL 18 complete localslice regression passes with race detector | `artifacts/local-v17-pg18.log` |
| Complete Controller compilation passes | `artifacts/local-v17-compile-final.log` |
| Driver boundary and immutable manifest checks pass | `artifacts/local-v17-boundary.log` |
| Validated pinned client generation passes | `artifacts/local-v17-client-validated.log` |
| Client/Vue typechecks, 33 focused frontend tests and changed-file formatting pass | `artifacts/local-v17-web-fixed.log` |

These are runtime service workflows, not complete Controller startup E2E.
Workspace/node/endpoint clock migration and other remaining workflows are still
open. No push, merge, deployment or Draft transition was performed.

## Controller HTTP Readers and Diagnostics (2026-09-10)

The actual HTTP server now accepts the common backend rather than retaining a
PostgreSQL pool. Workspace/audit lists and upgrade-target reads use the existing
RBAC/audit stores; the API package has no business SQL. Audit responses retain
infinities, the maximum finite timestamp, nullable IDs/text and exact ordering.
Lists reject incomplete row reads. Configuration, rollout and other migrated
error handlers use the common not-found classification.

Runtime diagnostics provide pool counters and bounded attestation-key labels.
Readiness retains its two-second dependency deadline. MySQL/MariaDB validate
metadata shape, every immutable baseline/revision receipt, predecessor checksums,
dirty state and compatibility while excluding concurrent owner migration. The
owner's full physical ValidateSchema remains unchanged and separately required;
runtime readiness does not inspect business trigger/routine definitions. Runtime
DDL and metadata-write privileges remain denied. Application startup still
rejects these engines until the remaining service and process workflows pass.

Real HTTP reads pass on PostgreSQL 17, MySQL and MariaDB using actual login
cookies and runtime principals: scoped/empty/development workspace lists,
unauthenticated and cross-workspace refusal, audit limits and lossless values,
unobserved/observed/missing upgrade targets, heartbeat-timeout maintenance and
disconnected-event reads, key/pool diagnostics, compatible/incompatible readiness
and unavailable-database diagnostics. Legacy audit fixture rows test the reader,
not authenticated audit creation or chain verification.

The first maintenance attempt correctly refused a fixture whose owner had not
completed historical telemetry migration. The fixture now runs that required
owner step. The first readiness implementation incorrectly reused owner-only
physical inspection; it was replaced with the complete runtime receipt check,
not broader grants. Receipt-corruption coverage initially selected a no-op v2
root with no step row; it now alters a populated v3 predecessor and asserts the
fixture update actually changed one row. No acceptance assertion was removed.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Existing three-backend authentication HTTP workflow passes | `artifacts/api-read-{mysql,mariadb,pg18}.log` |
| New MySQL/MariaDB HTTP reads, maintenance and readiness pass with race detector | `TestControllerReadsBackendHTTPIntegration` in `artifacts/api-read-diagnostics-{mysql,mariadb}.log`; subsequent initial privilege fixture checks in those logs failed |
| PostgreSQL 17 readers, readiness compatibility and upgrade/rollout regressions pass | `artifacts/api-read-new-pg17.log` |
| Runtime denies DDL/metadata writes and rejects dirty/incompatible/reordered/checksum-altered receipts; migration-lock deadline, retry and owner physical validation pass | `artifacts/api-diagnostics-final-{mysql,mariadb}.log` |
| Complete Controller compilation and focused not-found/diagnostic/boundary/frozen checks pass after the final receipt-check extraction | `artifacts/api-read-complete-compile.log`, `artifacts/api-read-complete-boundary.log` |
| Documentation checks pass with the isolated checkout's source-file metadata | `artifacts/api-read-docs-final.log` |

The remaining original physical inventory is unchanged at 39 timestamp fields,
two JSONB fields and one text-array field. Enrollment/trust convergence,
desired-state/policy/batch workflows, startup wiring and complete three-backend
Controller process E2E remain open. No push, merge, deployment or Draft
transition was performed.

## Enrollment Trust Convergence (2026-09-10)

The common trust store covers enqueue, claim, separate update/close completion,
unlock and retry release. The worker keeps its external fenced executor and
stable update IDs. Only its transaction and SQL boundary changed; full enrollment
still uses its PostgreSQL compatibility constructor and transaction bridge.

Both v18 manifests were authored against both genuine historical roots. The
revision converts available, lease, creation and update clocks without changing
earlier artifacts. It retains nullable lock pairs, the pending index and the
complete logical timestamp domain. The original physical inventory now has 35
timestamp fields, two JSONB fields and one text-array field left to port.

The first new integration fixtures reused a node name within one workspace.
Existing uniqueness enforcement correctly rejected them. The fixtures now use
distinct names; no schema or acceptance constraint was loosened.

Runtime-principal workflows on both engines cover skipped locks, rollback of
claimed work and superseding enqueue, repeated matched-row updates, exact
worker/revision guards, monotone concurrent first enqueue, separate update/close
retry progress and bounded exponential backoff. They retain infinite and full
finite timestamps and reject broken lock pairs or out-of-domain timestamps.
The fencing probe checks binding propagation, stable operation IDs and refusal
before transport invocation; it is not a replacement for signed real-owner
transport E2E. Populated v17 upgrades preserve microsecond clocks, nullable
leases, owner IDs and all earlier revision receipts, then pass owner physical
schema validation.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Both v18 manifests authored on both roots | `artifacts/trust-v18-{mysql,mariadb}.log` |
| MySQL/MariaDB runtime convergence and populated v17-to-v18 upgrades pass with race detector | `artifacts/trust-v18-fixed-{mysql,mariadb}.log` |
| Complete Controller compilation passes | `artifacts/trust-v18-compile.log` |
| Driver boundary and immutable manifest checks pass | `artifacts/trust-v18-boundary.log` |
| PostgreSQL 18 enrollment regression and PostgreSQL 17 enrollment/trust lifecycle pass with race detector | `artifacts/trust-v18-pg18.log`, `artifacts/trust-v18-pg17.log` |

These checks do not establish full Controller startup E2E. Enrollment token,
node/endpoint and session-authority migration, desired-state/policy/batch
workflows and complete three-backend process acceptance remain outstanding.
No push, merge, deployment or Draft transition was performed.

## Enrollment Service and Token Storage (2026-09-10)

The complete enrollment service now uses the common backend: initial/bootstrap
token issuance and validation, atomic single-use consumption, pending-node reuse,
sealing-key binding, approvals/revocations, endpoint checks, trust snapshots and
session authorization. Its only PostgreSQL references are constructor adapters.
All enrollment SQL lives in backend-owned stores, and trust queue changes use
the same common caller transaction as approval consumption and audit.

Appended v19 converts six token timestamps. Both manifests were authored against
both historical roots; v1-v18 artifacts remain frozen. Populated v18 upgrades
retain years 1000/9999 with exact microseconds, NULL consumption, consumed-node
and endpoint bindings, earlier revision receipts and full physical validation.

The real runtime-principal workflow passes on MySQL, MariaDB and PostgreSQL 18.
It checks signed endpoint proof rejection, eight-way single-use enrollment,
scoped independent approval, content substitution refusal and replay, active and
revoked endpoint behavior, trust snapshots, authenticated audit chains, bound
bootstrap retry after expiry, extended token clocks, legacy pending-node reuse
and one-time sealing-key retrofit. The real owner-session manager issues signed
grants/fences and demonstrates exact lease cleanup after authorization commit
cancellation. Held node/endpoint shared locks block revocation. Registration is
an in-process test double; this is not full transportd/Agent process E2E.

The new test initially had an incorrect result arity, fixed before execution.
Its next run omitted approver role bindings and was correctly rejected by the
existing approval authority check. The fixture now grants the appropriate scoped
roles; no production permission, state or acceptance check was weakened.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Both v19 manifests authored on both roots | `artifacts/enrollment-v19-author-{mysql,mariadb}.log` |
| Populated v18-to-v19 upgrades pass with race detector | `artifacts/enrollment-v19-upgrade-{mysql,mariadb}.log` |
| Runtime enrollment, real approval and owner-session workflows pass with race detector | `artifacts/enrollment-v19-complete-{mysql,mariadb}.log` |
| PostgreSQL 18 complete enrollment suite, including existing contention and commit-cleanup regressions, passes with race detector | `artifacts/enrollment-v19-complete-pg18.log` |
| PostgreSQL 17 common enrollment workflow and PostgreSQL 18 bootstrap HTTP/permission/error/TTL regressions pass with race detector | `artifacts/enrollment-v19-pg17.log`, `artifacts/enrollment-v19-api-pg18.log` |
| Complete Controller compilation, driver boundary and frozen artifact checks pass | `artifacts/enrollment-v19-compile-final.log`, `artifacts/enrollment-v19-boundary.log` |
| Documentation checks pass against the isolated checkout's updated source-file metadata | `artifacts/enrollment-v19-docs.log` |

The original physical inventory now has 29 timestamp fields, two JSONB fields
and one text-array field still to port. Node/endpoint/sealing-key finite bridges,
desired user/group and policy/batch workflows, shared startup wiring and complete
three-backend Controller process acceptance remain outstanding. The startup gate
has not been relaxed. No push, merge, deployment or Draft transition occurred.

## Desired User/Group State and Storage (2026-09-10)

The user-state service now owns a common transaction for desired changes, signed
resource commands, outbox/event records and audit. Its PostgreSQL references are
constructor adapters only. Typed backend stores preserve optimistic versions,
same-kind revision recovery, locked-outbox/lease exclusions, queued coalescing,
fresh observed capacity and the final real scheduler fence, including replays.

Appended v20 converts four desired user/group timestamps and the last native
text array, `desired_groups.members`. Both manifests were authored on both
historical roots. Populated v19 upgrades retain exact year-1000/year-9999 times,
empty arrays, NULL/text members, revisions and earlier receipts. Invalid logical
arrays and out-of-range timestamps remain rejected; owner physical validation
passes. Versions 1-19 were not modified.

Runtime-principal workflows pass on MySQL, MariaDB and PostgreSQL 17/18. They
check Ed25519 command signatures and applied revisions, idempotency conflict and
replay, eight-way version contention, coalescing, locked outboxes, failed-create
recovery and real scheduler fence rejection/rollback after epoch advancement.
Capacity checks retain
NULL membership deduplication, multidimensional flattening, byte-exact case and
trailing-space distinctions, and fresh/stale infinite observation clocks. Lists
and JSON preserve dimensions, lower bounds, NULL members and infinite times.

One fixture initially requested exactly the remaining capacity rather than
exceeding it; another left MySQL's reserved `system` column unquoted. Both were
corrected without loosening constraints. Repeated identical desired UPDATEs
also exposed MySQL/MariaDB changed-row counts; the store now performs a locking
existence check after zero changed rows, matching PostgreSQL semantics.

Membership mutation requests remain limited to 384 valid names. Read responses
can represent all 4096 stored members and optional nonstandard dimensions. The
OpenAPI 3.1 string/null union generates correct nullable TypeScript arrays with
the unchanged pinned generator, without custom templates or type mappings.
The existing capacity contract test now distinguishes write and read limits.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Both v20 manifests authored on both roots | `artifacts/desired-v20-author-{mysql,mariadb}.log` |
| Populated v19-to-v20 upgrades pass with race detector | `artifacts/desired-v20-upgrade-{mysql,mariadb}.log` |
| MySQL and MariaDB runtime workflows pass with race detector | `artifacts/desired-v20-runtime-final-mysql.log`, `artifacts/desired-v20-runtime-complete-mariadb.log` |
| PostgreSQL 17 common workflow and PostgreSQL 18 complete user-state suite pass with race detector | `artifacts/desired-v20-runtime-fixed-pg17.log`, `artifacts/desired-v20-runtime-complete-pg18.log` |
| Complete Controller compilation and boundary/frozen-artifact checks pass | `artifacts/desired-v20-compile-final.log`, `artifacts/desired-v20-boundary.log` |
| Standard client regeneration | `artifacts/desired-v20-generate-oas31.log` |
| Client/Vue typecheck, 21 focused OpenAPI/value round-trip tests and touched frontend formatting pass | `artifacts/desired-v20-client-final.log` |
| Documentation checks pass | `artifacts/desired-v20-docs.log` |

Full OpenAPI lint is not green: `/auth/methods` has no documented 4XX response,
and `AgentRolloutExclusion` is unused. Replacing only the v20 schema changes with
their prior definitions reproduces the same two errors. Evidence:
`artifacts/desired-v20-openapi-lint.log` and
`artifacts/desired-v20-openapi-before.log`. No lint rule was disabled. These
remain acceptance follow-ups, not successful checks.

The original physical inventory now has 25 timestamp and two JSONB fields left;
no native text-array field remains. Shared node storage, user policies/batches,
startup wiring and complete three-backend Controller/transport process E2E are
still outstanding. The startup gate and PR Draft state remain unchanged.

## User Policies, Batches and Enforcement (2026-09-10)

The user-operations service now uses common transactions and typed stores for
policy writes/reads/metrics, approval-bound batches, scheduler leases, claims,
child submission/refresh and expiry/quota/monthly-reset enforcement. PostgreSQL
references remain only in compatibility constructors. Approval consumption,
audit and batch persistence share one transaction; scheduler writes and
idempotent replays assert the real leadership fence before commit.

Appended v21 converts twelve timestamps. The enforcement natural key includes
period_start, so its exact-key registry is verified before conversion and
re-encoded under the same migration guards. Populated v20 upgrades retain
year-1000/year-9999 microseconds, NULL expiry/leases, lease-pair checks, indexes
and prior receipts. Long enforcement names remain byte-exact; duplicate keys
after migration are rejected. Extended finite values and both infinities work,
and owner physical schema validation passes. Artifacts v1-v20 stay frozen.

The first migration-author run used MySQL's reserved STORED word as an alias;
it was corrected before freezing v21. A subsequent MariaDB author attempt
refused to overwrite its earlier draft output, so the corrected manifest was
written to a fresh artifact directory. One upgrade fixture incorrectly used an
unbounded enforcement name for a bounded desired-user column; it now separates
those two contracts. A runtime SQL alias also needed to avoid reserved USAGE.

Runtime coverage verifies policy hash/version conflicts and replay, durable
usage-based quota decisions, child-command and monthly-reset crash recovery,
manual disable preservation, real independently approved batch consumption,
content-substitution rejection, batch child recovery, offline/unknown/success
refresh, metrics, lease ownership, concurrent SKIP LOCKED claims, claim-owner
checks, release/failure transitions, stale leadership rollback and audit chains.
It exposed an existing recovery gap: after a child committed, the advanced user
version excluded the pending enforcement. The candidate queries now admit
exactly that next-version recovery, still resolving the stable child key and
never using the reset recovery branch for a newer manual disable.

Policy/batch response timestamps are logical strings in OpenAPI and the standard
generated client. Expiry mutation remains finite, UTC and whole-second. The form
preserves unsupported stored expiries visibly and rejects saving them unchanged
rather than silently clearing them or losing precision. Client/Vue typechecking
and all 27 focused adapter, timestamp round-trip and OpenAPI contract tests pass.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Both v21 manifests authored on both roots | `artifacts/user-operations-v21-author-mysql.log`, `artifacts/user-operations-v21-author-fixed-mariadb.log` |
| Populated v20-to-v21 upgrades pass with race detector | `artifacts/user-operations-v21-upgrade-fixed-{mysql,mariadb}.log` |
| Final runtime workflows pass with race detector | `artifacts/user-operations-v21-runtime-final-{mysql,mariadb,pg17}.log` |
| Complete PostgreSQL 18 user-operations suite, including existing manual-hold and refresh-fairness regressions, passes with race detector | `artifacts/user-operations-v21-runtime-final-pg18.log` |
| Complete Controller compilation and boundary/frozen-artifact checks pass | `artifacts/user-operations-v21-compile-final.log`, `artifacts/user-operations-v21-boundary.log` |
| Standard client regeneration, typechecking and focused tests pass | `artifacts/user-operations-v21-generate.log`, `artifacts/user-operations-v21-client.log` |
| Documentation checks pass | `artifacts/user-operations-v21-docs.log` |

An intermediate MariaDB run lost its disposable container before testing;
the final isolated rerun completed successfully. It is not counted as a pass.
The remaining inventory is 13 timestamps and two JSONB fields. Four of those
timestamps are in usage/cursor storage: policy readers currently convert the
native usage dates to logical microseconds in SQL, but the writer/cursor bridge
still needs its own migration. Shared node storage, startup wiring, the two
previously recorded OpenAPI lint findings and complete three-backend Controller/
transport process E2E remain outstanding. No startup gate, Draft state, merge or
deployment changed.

## Usage Storage and Contract Checks (2026-09-10)

Appended v22 converts connected_at/observed_at in usage cursors and
period_start/observed_at in accumulated usage. Both timestamp-bearing primary
keys are rebuilt in the verified switch under writer guards. Both manifests
were authored on both historical roots. Populated v21 upgrades preserve native
year-1000/year-9999 microseconds, counters, exact session/period keys and all
earlier revision/step receipts. Migration replay and owner physical validation
pass; versions 1-21 remain unchanged.

The shared usage store now uses logical timestamps for every persisted cursor
and period value. Finite protocol samples are normalized to persisted
microseconds before replay comparison. Direct store fixtures round-trip both
infinities and finite domain endpoints; ordinary ingestion handles existing
infinite observations without converting them to time.Time. MySQL policy reads
and quota joins no longer convert native usage dates in SQL.

The first runtime run caught a new cursor initialization regression: a missing
cursor incorrectly retained the incoming counters and produced zero first-use
delta. Both stores now initialize only its lookup key before scanning. The
corrected runs verify first-use accounting, replay, counter reset, month
boundaries, saturation, username conflicts, concurrent first observations,
cross-store rollback, extended finite ingestion and submicrosecond replay order.
Real telemetry ingestion and policy/enforcement/batch workflows also pass using
runtime principals, not migration-owner connections.

The two previously recorded OpenAPI lint failures are resolved without disabling
rules or narrowing lossless rollout exclusions. The authentication-method route
now documents its existing JSON Problem 405 response for unsupported methods.
The unused, formerly restrictive exclusion schema and its generated model were
removed. Standard client regeneration changes only that model and its export;
the remaining generated source matches exactly. Full OpenAPI lint, client/Vue
typechecking and all 27 focused contract/value/adapter tests pass.

The authorization layer no longer constructs or recognizes PostgreSQL-specific
not-found errors; all resource services already return the common database
error. The existing real-login HTTP workflow now verifies Problem 404 responses
for both an invalid node identifier and a valid missing node on all backends.

A broader PostgreSQL authorization run did not pass: the existing certificate
and configuration-approval fixtures submit multiple parameterized statements
as one prepared statement and fail before their service calls. This is outside
the not-found mapping change; the fixtures were not rewritten. Evidence is
`artifacts/usage-v22-authorization-pg18.log`. The narrower common HTTP workflow
and routing checks passed separately; they do not establish that the two older
fixture-based scenarios pass.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Both v22 manifests authored on both roots | `artifacts/usage-v22-author-{mysql,mariadb}.log` |
| Populated v21 upgrades and common usage workflows pass with race detector | `artifacts/usage-v22-runtime-upgrade-fixed-mysql.log`, `artifacts/usage-v22-runtime-upgrade-mariadb.log` |
| PostgreSQL 17/18 common usage and cross-store transaction workflows pass with race detector | `artifacts/usage-v22-runtime-pg17.log`, `artifacts/usage-v22-runtime-fixed-pg18.log` |
| Runtime policy/enforcement/batch workflow passes with race detector | `artifacts/usage-v22-policy-{mysql,mariadb,pg18}.log` |
| Real telemetry ingestion workflow passes with race detector | `artifacts/usage-v22-telemetry-{mysql,mariadb,pg18}.log` |
| Common authentication/authorization HTTP workflow passes with race detector | `artifacts/usage-v22-authorization-{mysql,mariadb}.log`, `artifacts/usage-v22-authorization-final-pg18.log` |
| Complete Controller compilation and boundary/frozen-artifact checks pass | `artifacts/usage-v22-compile-final.log`, `artifacts/usage-v22-boundary.log` |
| Standard client regeneration, exact source comparison and contract checks pass | `artifacts/usage-v22-generate.log`, `artifacts/usage-v22-generated-diff.log`, `artifacts/usage-v22-client.log` |
| Existing routing Problem response test, including authentication methods, passes | `artifacts/usage-v22-route.log` |
| Documentation checks pass | `artifacts/usage-v22-docs.log` |

The remaining physical inventory is nine timestamps and two JSONB fields in
shared workspace/node/key storage and upstream synchronization records. Startup
wiring and complete three-backend Controller/transportd/Agent process acceptance
remain outstanding. No startup gate, Draft state, merge or deployment changed.

## Shared Workspace, Node and Key Storage (2026-09-10)

Appended v23 converts the last nine business timestamps and two JSONB fields in
the pre-v4 physical inventory. Workspace creation/update/archive, node creation/
update, endpoint binding/revocation, sealing-key creation and synchronization
times now retain the complete PostgreSQL timestamp domain. Labels accept any
JSONB value, while synchronization classification retains its object constraint.
No prior manifests, checksums or migration receipts were rewritten.

Both manifests were authored on both historical roots. Populated v22 upgrades
preserve year-1000/year-9999 microseconds, historical arrays/objects and large
integer JSON values. All nine clocks round-trip both infinities and the finite
domain endpoints. Invalid times, required NULL values, invalid Unicode/decimal
JSON and broken endpoint revocation pairs are rejected. Nullable archive and
revocation values, long workspace exact keys, replay and owner physical schema
validation remain intact.

Enrollment, local-simulation workspace/node/key creation, gap disconnection,
transport status changes, telemetry activation/offline maintenance and attestation
revision changes no longer bridge shared storage through native DATETIME. Fresh
fixtures were adapted only at their shared-column writes; historical upgrade
fixtures still seed the original native representation. A shared runtime-store
workflow exercises workspace reuse, node/endpoint/sealing-key insertion, touch,
activation, gap disconnection and revocation with extended and infinite clocks.
Its labels include scalar/NULL/array JSON and decimal values beyond native JSON
number range. Telemetry explicitly preserves an infinite node clock during
activation and writes the correct logical maintenance clock after a fenced
rollback/retry.

Upstream synchronization has no Controller business writer in the current
source tree: the pinned migration seed is owner-only and runtime retains SELECT
only. The old bounded-key inventory's pending sync-workflow note was corrected;
no new workflow or privilege was introduced.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Both v23 manifests authored on both roots | `artifacts/shared-v23-author-{mysql,mariadb}.log` |
| Populated v22-to-v23 upgrades pass with race detector | `artifacts/shared-v23-upgrade-{mysql,mariadb}.log` |
| Existing enrollment/approval/trust workflow passes with race detector | `artifacts/shared-v23-enrollment-{mysql,mariadb}.log` |
| Shared logical-clock/JSON writer workflow passes with race detector | `artifacts/shared-v23-logical-{mysql,mariadb}.log` |
| PostgreSQL 18 existing enrollment and shared writer workflows, plus PostgreSQL 17 shared writer workflow, pass with race detector | `artifacts/shared-v23-enrollment-pg18.log`, `artifacts/shared-v23-logical-pg17.log` |
| Real telemetry activation/maintenance clock checks pass with race detector | `artifacts/shared-v23-telemetry-{mysql,mariadb,pg18}.log` |
| Real attestation revision changes, simulator lifecycle/transport ingress and long exact-key writes pass with race detector | `artifacts/shared-v23-shared-writers-{mysql,mariadb}.log` |
| Complete Controller compilation and boundary/frozen-artifact checks pass | `artifacts/shared-v23-compile-final.log`, `artifacts/shared-v23-boundary.log` |
| Documentation checks pass | `artifacts/shared-v23-docs.log` |

The original physical type inventory has no remaining native approximations.
This does not complete PR-02: common startup wiring and complete three-backend
Controller/transportd/Agent process acceptance remain outstanding. The two older
PostgreSQL authorization fixture failures recorded above are not resolved by
these storage checks. The startup gate and Draft state are unchanged; nothing
was merged or deployed.

## Controller Process Startup

The ordinary `ocserv-control` binary now selects PostgreSQL, MySQL or MariaDB
through one common service-wiring path. MySQL/MariaDB selection remains confined
to test/development; production is still rejected by both configuration and the
driver opener. This adds no migration version and changes no frozen artifact.

The new subprocess test builds the real CLI and executes owner `--migrate-only`,
runtime schema compatibility, rejection of unsupported schema versions, rejection
of runtime migration/DDL, an idempotent owner rerun, runtime local-admin bootstrap,
normal HTTP readiness/login/authorized node listing, and graceful SIGTERM exit.
MySQL/MariaDB use a newly created, uniquely named runtime account rather than the
hard-coded fixture account, exercising the new explicit `user@host` grants.

The owner command also completes historical telemetry and provisions the active
14-day ingestion window through two future months; runtime receives only the
existing exact privileges and verified shard grants. Normal readiness does not
gain owner physical-inspection privileges. PostgreSQL retains its migration
preflight and grant implementation.

The all-role process must finish its actual policy, rollout, telemetry,
certificate-maintenance and audit-checkpoint body before a G6 completion marker
appears. The marker's common transaction retains its final live-term fence.
PostgreSQL uses the existing G6 SQL fixture; MySQL/MariaDB use an owner-installed
test journal/procedure with runtime EXECUTE only. These are not claimed to port
the complete two-failure-domain G6 harness.

| Check | Evidence under `/root/ocservia-telemetry-time.gXtIZO` on BuildServer |
| --- | --- |
| Real Controller subprocess workflow passes on MySQL and MariaDB | `artifacts/controller-startup-{mysql,mariadb}.log` |
| Same subprocess workflow passes on PostgreSQL 17 and 18 | `artifacts/controller-startup-{pg17,pg18}.log` |
| Selector/TLS policy, production gate, account quoting, bootstrap ordering, driver ratchet and immutable artifacts pass | `artifacts/controller-startup-unit.log` |
| Full configuration package and foundation CLI compilation pass | `artifacts/controller-startup-config.log` |
| Existing exact runtime/maintenance privilege denials and telemetry shard lifecycle pass on both new backends | `artifacts/controller-startup-privileges-{mysql,mariadb}.log` |
| Complete Controller compilation and documentation checks pass | `artifacts/controller-startup-compile-final.log`, `artifacts/controller-startup-docs.log` |
| Existing G6 runtime-adapter, pipeline, secret-policy, artifact and shell checks pass | `artifacts/controller-startup-g6-static-pass.log` |
| Earlier PostgreSQL certificate/config-plan authorization fixtures pass on 17 and 18 | `artifacts/controller-startup-authorization-final-{pg17,pg18}.log` |

The G6 script ran in a disposable BuildServer Ruby container with Node 24.18.1,
jq, GNU awk, archive tools and ShellCheck; its initial missing-tool/awk failures
were environment issues, not suppressed assertions. This remains a script/fixture
check, not a real two-failure-domain failover run.

The two earlier PostgreSQL fixture failures are resolved. Their parameterized
multi-statement setup/cleanup uses pgx's explicit per-call simple protocol;
business queries retain the production extended-protocol default. The download
authorization fixture explicitly identifies its pre-attestation issued
certificate as legacy, rather than pretending to supply a verified current CSR
receipt. Current CSR issuance/attestation remains covered separately by the
certificate workflows, not by that authorization fixture. No schema, permission,
receipt-verification or fencing constraint was relaxed.

The Go integration harness runs with `-race`; the subprocess is the ordinary
compiled Controller binary. No transportd or Agent is substituted into this
startup check. Full three-backend transportd/Agent certificate/operation process
acceptance remains outstanding. PR #193 was rechecked as OPEN and Draft after
these checks; no merge or deployment occurred.

## Real Controller, Agent and Privileged Workflows

`scripts/database-controller-e2e.sh` runs the same real-process test against a
fresh isolated database on BuildServer. The workload uses the production Rust
feature partitions, an actual Controller CLI build, native Ocserv 1.3.0,
OpenSSL and OpenConnect. Controller, transportd, Agent and privd run as separate
UNIX principals. Only the initial empty workspace is seeded: enrollment,
approvals, sessions, root-key registration, command results and artifacts are
created through their real protocols rather than injected into the database.

The network has no host-published ports or public discovery connectivity. Two
dedicated authenticated TLS relays provide addressing; Iroh may subsequently
select an authenticated direct path. The external PKI fixture validates and
signs real root-generated CSRs and seals passwords with purpose-separated RSA
keys. It is not a replacement Controller, transport, Agent or privd. Because
the disposable container has no systemd PID 1, its fixed service adapter checks
the actual Ocserv PID and native `occtl` status and sends a real reload signal.
This does not claim to test systemd installation or two-failure-domain G6.

Two real transport defects were exposed by this workflow:

- Agent enrollment now uses the same dedicated-relay addressing hints as
  regular sessions, without changing the authenticated Controller identity.
- Artifact relay consumes QUIC FIN before forwarding the terminal chunk.
  Previously dropping the unread transport stream sent STOP_SENDING, causing
  the real Agent to reject an otherwise complete P12 transfer. The existing
  artifact-boundary regression now also asserts clean sender acknowledgement.

PostgreSQL 18 passed the real workflow in 127.75 seconds, with evidence in
`artifacts/controller-e2e-pg-artifact/workflow.log` under the BuildServer
checkout. Coverage includes independent local identities and self-approval
denial, authenticated enrollment, one-use root-registration credentials,
verified fenced sessions, real CSR receipts, certificate issuance, P12
generation and one-use download, OpenSSL verification, revocation, user
creation/replay/password rotation/disable/enable, actual Ocserv authentication,
native group membership and telemetry convergence. Operation, event and audit
readers and audit-chain/checkpoint verification also pass. Runtime processes
do not inherit the harness's owner/admin database credentials.

MySQL and MariaDB subsequently passed the identical workflow. The final transport
regression also passed; the combined acceptance evidence is recorded below.

## Final PR-02 Acceptance

All execution below was on BuildServer in
`/root/ocservia-telemetry-time.gXtIZO`. Each engine used an isolated fresh database,
the same real-process workflow and scoped container/network cleanup. No database
ports were published on the host. A required test-level PASS marker prevents a
skipped or unmatched test from being accepted as a successful E2E.

| Check | Result | Evidence under the final checkout |
| --- | --- | --- |
| PostgreSQL 18 real Controller/transportd/Agent/privd workflow, final-image rerun | PASS, 132.38s | `artifacts/controller-e2e-postgres-final/workflow.log` |
| MySQL 8.4.10 identical real-process workflow | PASS, 194.35s | `artifacts/controller-e2e-mysql/workflow.log` |
| MariaDB 12.3.2 identical real-process workflow | PASS, 169.52s | `artifacts/controller-e2e-mariadb/workflow.log` |
| Rust artifact fencing and clean QUIC sender acknowledgement regression | PASS, 1 test, 0 ignored | `artifacts/controller-e2e-rust-unit.log` |
| Agent/transportd Rust format checks | PASS | `artifacts/controller-e2e-rust-format-final.log` |
| Complete Controller package compilation, driver boundary, immutable artifacts, revision sequence and startup safeguards | PASS | `artifacts/pr02-acceptance-go.log` |
| Documentation, shell syntax, whitespace and common runtime payload checks | PASS | `artifacts/pr02-acceptance-final-static.log` |

The E2E harness uses `go test -race`; its child Controller is the ordinary
compiled binary, not a race-instrumented subprocess. Rust production feature
partitions remain separate. Each E2E directory also records build logs and exact
runtime image IDs. The final Go command compiles all packages but runs only
the five named boundary/history/startup checks, not every unit or integration
test. Earlier focused migration, permission, service, PostgreSQL 17/18 and web
contract results remain listed in their respective sections above.

An initial byte comparison of the exported image-ID files failed because each
BuildKit export has a distinct provenance-attestation manifest. This is retained
in `artifacts/pr02-acceptance-static.log`, not counted as a pass. All three final
`workflow-build.log` files record the same runtime image manifest
`sha256:bc86fe65dda42b15dd82c1e8e405b45e9bf575892a75fd80be1ed9d4e0b5d16f`
and configuration `sha256:3a9744bc90e6339906f86d79d1da2a1cc504647c4922eb4de2292134e585a0cc`;
the corrected check compares those payload identities, not the provenance-bearing
manifest-list IDs.

The acceptance audit confirmed that the ordinary application wiring selects
common stores for readers, writers, certificates, operations, transport ingress,
enrollment, telemetry and user workflows. No business SQL remains in the API,
certificate, operation, local-slice, enrollment, configuration or user-service
modules. Remaining PostgreSQL constructors are compatibility wrappers; owner-only
PostgreSQL migration preflight is deliberately unchanged. The original physical
type inventory is fully migrated through appended v23, all frozen artifacts
pass their checksum checks, and `control-plane/migrations` has no diff.

This closes the requested PR-02 migration and three-backend Controller E2E work,
not production release acceptance. Native configuration/upgrade/failover scenarios
are not added to the process coverage by inference: their existing service and
static checks remain distinct. No real two-failure-domain G6, systemd installation,
production database migration, deployment, merge or ready-for-review transition
was performed. MySQL/MariaDB production admission remains rejected and PR #193
remains Draft.
