# PR-02 Controller Workflows

The requested business-store migrations and real three-backend Controller
certificate/operation process acceptance are complete. Controller test/development
startup selects all three backends through one business-service wiring path.
The PR remains Draft and MySQL/MariaDB production startup remains rejected.
See [final acceptance](database-pr02-validation.md#final-pr-02-acceptance) for
evidence and boundaries; earlier domain/HTTP milestones below are historical.

## Recorded Migration

Version 5 appends to the exact published version-4 checksum on both historical
roots. Versions 1 through 4 and all PostgreSQL migration files are unchanged.
Both engine manifests are authored by executing SQL and checking the resulting
objects and copied values on the pinned real databases, never by adopting live
hashes during application startup.

Version 4 added thirteen business time columns using signed PostgreSQL-epoch
microseconds: four local-auth attempt fields, two scheduler leadership fields,
two audit fields, role-binding creation, bootstrap completion, and three
approval fields. Audit before/after summaries use lossless, arbitrary JSONB
with server-side validation. The migration preserves append-only audit guards
around the temporary removal of update triggers needed for verified copying.
An interrupted migration still refuses startup and requires checksum repair.

Ambiguous historical auth lease/block timestamps require explicit owner
decisions, as documented in [logical time](database-pr02-time-decisions.md).
A normal owner connection cannot change guarded source rows during repair.

Version 5 adds 33 more time columns: 13 identity/authentication fields, seven
artifact/certificate fields, two approval fields, and 11 telemetry ingestion
fields. Approval request summaries and the three snapshot JSON objects receive
lossless storage and server validation. Populated v4 upgrade coverage checks
that the prior migration receipts remain unchanged and that finite historical
year-1000 values remain finite, rather than being guessed to be infinity.

Version 6 appends to the exact published version-5 checksum on both roots and
converts the eight privd attestation times on
`privd_attestation_enrollment_credentials` and `node_privd_attestation_keys`.
The enrollment-credential table name exceeds the identifier budget of the
generic migration-writer guard trigger names, so this version's guards use an
explicit shorter prefix while keeping the exclusive-writer contract. A
populated v5 upgrade proves receipts and every finite historical value,
including NULL validity and consumption, survive the switch unchanged.

## Actual Business Boundaries

- Authentication owns its entire transaction through `database.Within`:
  credential provisioning, login and lease cleanup, session checks/logout,
  local bootstrap/completion, management mutations, self-service password
  changes, and audited break-glass. The original PostgreSQL constructor is a
  compatibility adapter, not a separate transaction implementation.
- Authentication and RBAC retain management-lock key 734821032; account
  admission retains 734821033. Identity/session/credential ordering and the
  independent cancelled-request completion context remain explicit. MySQL
  rechecks the server clock after waiting for an expiring session or lease.
- The audit Manager and RBAC public methods use domain Stores. Audit append
  takes the original workspace scope before a current locking read. A stale
  repeatable-read snapshot cannot fork the chain. Approval consumption joins
  the caller's transaction and preserves PostgreSQL's transaction-time expiry
  semantics; it does not silently switch to a statement-start clock.
- Scheduler Runner acquisition, renewal and fencing use common transactions.
  Certificate download open/abort/consume/finalize uses a common artifact Store,
  preserving durable root-RPC evidence and audit/fence atomicity. At version 5,
  issuance and maintenance were not part of the portable download constructor;
  their follow-up migrations are recorded below.
- Privd attestation owns credential issuance, signed registration with
  rotation overlap, and key revocation through `database.Within` and its own
  domain Store. Registration consumes the one-time credential, grants the
  capability and advances the node authorization revision inside the same
  transaction. The original PostgreSQL constructor remains a compatibility
  adapter; the telemetry ingestion key read uses the same typed contract.

No business advisory-lock call remains outside the PostgreSQL adapter package.
MySQL/MariaDB use transaction-owned lock records, not GET_LOCK. This is not the
same as migrating every outer business transaction. The enrollment, user-state
and batch-operation follow-ups are recorded below. The driver ratchet records
only the specific remaining compatibility sites.

The cross-engine HTTP test executes login, authentication, logout, local admin
bootstrap, authorized user creation, unauthorized management denial, password
change/session revocation, user disable and last-administrator protection.
Other tests exercise the real audit/RBAC, scheduler and download service
methods. Version 5 also runs the real approval service create/approve/consume
chain and extends HTTP coverage to independently approved password resets,
self-approval refusal, replay refusal and session revocation. Approval creation,
approval and validation now use the common Store boundary.

Artifact download tests cover NULL, minimum/maximum finite timestamps and both
infinities. Authentication honors an infinite stored session expiry without
extending the independently signed finite cookie lifetime. These are explicit
domain decisions, not general JSON API support for extended timestamps.

Audit canonicalization retains every previously signable v1 payload byte and
extends only its formerly rejected large-number domain. This permits summaries
such as `1e1000` without invalidating historical signatures. The existing v1
float64 canonicalization of ordinary large integers is not changed; lossless
storage is not a claim that this historical authentication ambiguity is solved.

## Historical Telemetry

The owner-only `telemetry-migrate-history` CLI automatically discovers all
finite historical months and resumes planned or partially completed work.
Each month copies, verifies and removes the source atomically; a same-key
different-value conflict is refused, never overwritten. Extended month names
cover the complete PostgreSQL finite timestamp range. Infinity remains in
the default table, outside finite monthly tables.

Completion requires verified migration history, pinned static objects, valid
dynamic shards and no finite legacy rows. The completion receipt and legacy
write guards prevent finite rows from returning to the default table. Retention
cannot retire shards while historical migration is pending. Owner copying uses
NOWAIT where catalog ownership could otherwise wait on an existing business
row lock; failure preserves evidence and requires an explicit rerun.

Runtime receives only SELECT on the completion receipt, never mutation or DDL.
The owner must grant newly created monthly tables through the existing explicit
test-account provisioning command. Existing inconsistent retired/dropped shard
history is refused rather than silently revived or discarded.

The actual telemetry ingest service now owns common transactions, including
wire decoding, batch deduplication, node locking, snapshots, sessions, bans,
users/groups, usage accounting and history writes. Its cross-engine workflow
checks outer rollback/retry, concurrent duplicate delivery, stale snapshots,
usage-conflict rollback, JSON rejection and cancellation. The PostgreSQL
transport transaction bridge remains. At version 5 the MySQL constructor was
ingestion-only; the follow-up below extends it without claiming all telemetry
service methods are portable.

### Telemetry History and Maintenance Follow-Up

History and maintenance now own common transactions on all three backends.
Maintenance includes heartbeat timeout status changes, disconnected events,
rollups, retention and the final leadership fence in the same transaction.
The PostgreSQL ingress transport bridge remains a compatibility site. The backend
constructor now also supports node listing/detail, upgrade eligibility, sessions
and IP bans through a common read store. Missing snapshots retain the previous
never-observed state. Heartbeat freshness compares logical timestamps directly:
positive infinity is fresh and negative infinity is stale.

History bounds and points retain logical timestamps instead of converting
infinity to `time.Time`. The telemetry API accepts and emits `infinity` and
`-infinity`; ordinary years retain RFC 3339 strings and extended years use
signed six-digit ISO 8601 years. The generated client keeps these values as
strings, including all six fractional digits, rather than JavaScript Dates.
Observed/heartbeat and session timestamps use the same API representation;
the UI localizes ordinary timestamps without coercing expanded or infinite values.

Rollup bucketing follows PostgreSQL `date_bin`: positive infinity is its own
bucket and never rounded as a finite BIGINT. Negative infinity sorts before
the finite 48-hour recomputation window and is not newly aggregated. Existing
rollups are retained/deleted by normal timestamp ordering; positive infinity
does not age out. Raw infinities remain in the default table, outside finite
monthly retention. Neither infinity is dropped or replaced with a finite
sentinel to make reads succeed.

The disconnected event uses v17 logical storage. Appended v23 completes the
shared `nodes.updated_at` migration, including activation and maintenance writes.

### Certificate Recovery and Maintenance

Durable consumption recovery now reads through the artifact store and compares
the logical expiry directly. An expired root grant returns an unexpired artifact
to ready, including positive infinity, while an expired artifact becomes expired.
Confirmation/consumption still runs inside the owner fencing interval; resetting
the durable record and expiry maintenance assert the scheduler fence before commit.

Certificate expiry maintenance is a common transaction with backend-specific
locking and updates. Negative infinity is expired; positive infinity and NULL
do not enter a finite expiry window. Each state transition increments the version
and emits one alert atomically. Appended v7 converts the five remaining certificate
timestamps and DNS JSONB array to logical storage. Certificate detail/list and
operation lookup use common read stores and preserve extended timestamps and
the complete JSON array.

Issuance now uses common transactions for approval consumption, intent audit,
signing state, pre/post-signature receipt/key checks and final issue audit. The
external signer remains outside those transactions. Revocation and P12 database
reads/writes and their operation creation use common transactions. Transport
result ingestion also uses common stores. Outbox release uses the logical clock
after v10 rather than narrowing it to a finite DATETIME.

Appended v8 converts the three secret-provider reference timestamps. Creation,
rotation, lookup and resource resolution share the common store and keep the
audit write in the same transaction. Certificate and secret-reference API read
models, including artifact replay expiry, preserve logical timestamps.

### Operation Intent and Expiry

Appended v9 converts the four operation timestamps and the event timestamp;
v10 converts three command and four outbox timestamps without rewriting prior
receipts. Detail, idempotent replay, and sequence-based event reads use the common
store. The API/client retains infinite and extended-range timestamps as text.

Operation/command/outbox/event intent persistence and certificate CSR/artifact
intent writes use backend stores on one common transaction. The creation entry
point, node admission, capability/observed-target checks, approval consumption,
supersede and audit are now common. Both real MySQL/MariaDB Service workflows
cover signed creation/replay/conflict and certificate CSR/P12/revocation intents.
Ordinary queued-command expiry uses a common transaction; a command with any
node lease is excluded, even when the lease timestamp has elapsed. Dispatch
ambiguity still belongs to reconciliation, not ordinary TTL cleanup.

Result ingestion and its projections now use common transactions as described
below. These conversions do not establish complete transport-backed
certificate workflow support.

### Dispatch Claim and Completion

Claim, sent/failed completion, exact pre-send lease extension and queue metrics
now use common transactions and backend-owned stores. Appended v14 converts
the four node-lease/command-attempt clocks without changing v1-v13 artifacts.
Unknown work retains priority and can use the already-counted active capacity;
ordinary queued work is bounded by the remaining global budget. Ranking still
selects only one command per node and respects earlier resource revisions.

Completion guards the exact sent owner term and locks the outbox in a separate
statement before reading result state. A late MarkSent preserves authoritative
terminal results and pending reconcile-only envelopes. Publishing, projections,
attempt closure and lease release commit together. Infinite stored leases are
live; finite extension uses a fresh server clock after any row-lock wait.

Result clocks now use v16 logical storage; dispatch compares creation and
attempt clocks directly in the same epoch. Connection-owner clocks use v15
logical storage.

Reconnect recovery now joins the caller's common transaction. It guards the
authoritative owner term, locks outbox rows before command/operation projections,
preserves verified upgrade scheduling acknowledgements and atomically schedules
signed reconcile-only work. Recovery projection writes for configuration,
certificates and artifacts also use the backend stores. The transport caller
now supplies its common transaction without a PostgreSQL bridge. There is no
backend-specific SQL left in the reconnect orchestration module.

### Lease Reaping and Connection Ownership

The complete operation Worker lifecycle now owns common transactions, including
expired sending leases, expired configuration applies, missing results and
bounded reconciliation continuations. Reaping locks the outbox before command
and projection rows, skips rows held by result ingestion, preserves semantic
command identity, strips stale owner proofs and never extends a continuation's
deadline. Corrupt later candidates roll back the complete batch. Infinite lease
and attempt timestamps are compared directly without conversion to finite time.

Appended v15 converts `connection_owner_fencing.lease_until` and `updated_at`.
Acquire, renew, exact-term release, assertion, state reads and observed-fence
guards use common transactions. Session Manager and Observer constructors now
accept the common backend, with legacy PostgreSQL constructors as adapters.
New acquisition and renewal deadlines are finite server-generated values;
stored infinite leases remain valid for authority reads and ordering.

MySQL/MariaDB serialize first acquisition through the existing transaction-owned
business lock, increment epochs under the authority row lock, and refuse signed
64-bit epoch overflow. Their assertion first rejects an already-expired term
without a lock: InnoDB can retain locks even for filtered-out rows. The locking
read and fresh post-wait server clock then recheck the exact term. Successful
guards hold the row until the bounded external action finishes; cancelled guard
cleanup uses an independent context. A rejected post-wait assertion requires
the caller to finish its transaction, as with other transaction failures.

Runtime workflows cover stale/replaced terms, infinite leases, concurrent first
acquisition, natural expiry inside an older transaction, expiry while a shared
lock waits, cancelled guard release and failed fence registration. A stale fence
still served by the transport registry cannot authorize an Observer mutation.
The registry in this test is a controlled double, not full transportd E2E.

### Privileged Result Key Reads

Privileged command and durable upgrade receipt verification now read key
material through the existing attestation domain store inside the caller's
common transaction. Telemetry upgrade ingestion uses this same entry point;
command-result ingress also calls the common verifier directly. Legacy
PostgreSQL verification entry points remain compatibility wrappers.
Signature transcripts, validity comparisons and error classifications are
unchanged. This domain still requires finite activation/validity timestamps,
with NULL for unlimited validity, as recorded for v6. This reader migration
does not reinterpret infinity as a finite key-validity sentinel.

### Transport Ingress and Results

Ingress now owns a common transaction and a fixed backend-owned savepoint.
Node/endpoint trust checks, event insertion, telemetry/result processing,
reconnect recovery and the durable cursor commit together. Permanent malformed
events roll back business changes to the savepoint, then append quarantine
evidence and advance the cursor. Transient failures roll back the entire event.
MySQL/MariaDB detect duplicate insert errors without requiring UPDATE access
to immutable events or quarantine evidence; runtime grants are unchanged.

Command results lock the outbox in a separate statement before reading current
dispatch state. Verification, receipt replay checks, command/operation state,
configuration/CSR/revocation/artifact/upgrade projections, dispatch closure,
audit and recovery scheduling share that transaction. Upgrade acknowledgements
remain nonterminal. A matched Unknown command remains mutable when its state
and infinite update clock are unchanged, independently of MySQL changed-row
counts. Configuration success retains a newer desired revision and reads the
immutable redacted plan before its upsert.

Appended v16 converts accepted, completed and created result clocks, preserving
NULL acceptance for rejected results and finite historical microseconds.
Appended v17 converts event occurrence/receipt, quarantine observation, cursor
update and the four simulator-job timestamps. The common constructor supports
ingress, cursor reads and operation
detail/list/summary reads. Those operation readers reuse the existing operation
store, preserve logical clocks and retain descending ID pagination, nullable
node/command IDs and workspace isolation.

Simulator creation, job claim/dispatch/retry/expiry, event listings and gap
reconciliation now use backend-owned stores. Creation and audit commit together;
exact workspace slugs serialize through the existing transaction-owned business
lock. Claim skips locked jobs, and a rejected dispatch start rolls back any
operation transition. Gap reconciliation performs registry reads before opening
its write transaction, then atomically invalidates cursors, marks ambiguous
dispatched work and records disconnected nodes. A failed registry read or
synthetic event insert leaves the prior database state intact.

Event pagination orders by ingest sequence, not occurrence time. API and
generated clients preserve logical timestamps including both infinities and
microseconds. Appended v23 also converts workspace, node and endpoint clocks;
simulator creation and status changes write their logical representation.

### Controller HTTP Readers and Diagnostics

The HTTP server accepts a common backend; its PostgreSQL constructor is a
compatibility adapter. Workspace and audit listings and upgrade-target reads
use the existing domain stores. Lists retain scope, order, limits and nullable
fields, and reject incomplete row reads. Audit timestamps preserve the logical
domain without narrowing to time.Time. Configuration and rollout not-found
problems recognize the common store error, as do the other migrated handlers.
No business SQL remains in the API package.

Readiness and development pool/key-state metrics use backend diagnostics and
the attestation store. MySQL/MariaDB readiness validates every baseline and
revision receipt, metadata shape, dirty state and declared compatibility under
the migration lock, within the existing request deadline. It does not inspect
physical business triggers/routines: that remains the owner's full
ValidateSchema operation. Runtime receives no DDL or metadata-write privileges.
This runtime receipt check is not a replacement for owner physical validation.
At this milestone other service ports and process E2E were incomplete. The
startup and real-process follow-ups below close those gaps for test/development;
the production admission gate remains in place.

### Enrollment Trust Convergence

The trust queue producer and worker use a shared transaction-scoped store.
PostgreSQL constructors remain as compatibility adapters. Enqueue replaces only older authorization
revisions and commits with its caller's transaction. MySQL/MariaDB serialize
first insertion with the existing node row, then lock the queue row before
comparing revisions. Existing attempt counts and original creation times remain.

Claims skip locked rows, increment attempts and take a ten-second lease using
the transaction clock. Completion and release require the exact node, revision
and worker; a superseding enqueue clears ownership and makes stale updates
ineffective. Update and close progress remain separate, so failed closes retry
without replaying completed trust updates. Administrative transport calls retain
the existing fenced executor and stable state-update operation IDs.

Appended v18 converts all four trust-convergence timestamps, preserving nullable
leases, the lock-pair constraint and the pending index. Existing manifests stay
immutable. The Controller's common-backend startup wiring remains separate.

### Enrollment Service

Token issuance and consumption, initial and bootstrap enrollment, pending-node
reuse, sealing-key binding, approvals/revocations, endpoint checks, trust
snapshots and session authorization now use common transactions and typed
backend-owned stores. No SQL or borrowed pgx transaction remains in enrollment.
The PostgreSQL constructors delegate to the common constructors.

Single-use enrollment still locks the token before consuming it with the node,
endpoint, keys, capabilities and audit record. The workspace audit lock still
serializes pending-node admission. Bootstrap replay remains bound to the original
endpoint and pending node. Existing sealing keys cannot be replaced by reusing
an enrollment token. Approval content, consumed-approval replay and protocol
capability exclusions retain their prior checks.

Session authorization retains repeatable-read isolation and shared node/endpoint
locks through signed grant and owner-fence issuance and commit. A failed commit
closes the exact opened owner term using an independent cleanup deadline.
MySQL/MariaDB use the same owner-session manager, not a process-local substitute.

Appended v19 converts the six token clocks, retaining expiry ordering, nullable
consumption/binding constraints and workspace-expiry indexes. Readers compare
logical expiry values directly, including extended finite dates and infinities;
newly issued tokens still have the existing maximum fifteen-minute TTL.
Appended v23 also ports shared node/endpoint/sealing-key clocks and node labels.
Enrollment writes no longer convert logical clocks through finite time.Time.
New protocol-generated clocks and validated approval labels retain their
existing request contracts.

## Configuration, Upgrade and Rollout Migrations

Appended v11 converts configuration-plan/apply/state timestamps and the stored
warnings JSONB array. Configuration service preparation, detail/resource reads,
apply preparation and operation-side configuration intent use common stores.
The API preserves the complete warning array and logical plan/approval-summary
expiry. Result projection writers now use the common ingress transaction.

Appended v12 converts the four agent-upgrade projection timestamps. Upgrade
reconciliation uses common transactions and logical deadlines/observations,
including infinite schedules and heartbeats. Lost conditional state fences and
audit failures roll back the complete transition.

Appended v13 converts rollout header/node clocks and the exclusions JSONB array,
preserving the 500-element constraint without converting decimal tokens to
native floating point. Rollout creation, replay, workspace/detail/list reads,
pause/resume, canary/batch advancement and claim/dispatch recording now use
common stores and transactions. No PostgreSQL driver references remain in the
upgrade/rollout orchestration module. MySQL/MariaDB workflow tests cover signed
per-node operation creation and seeded durable upgrade reconciliation.

Appended v23 finishes the pre-v4 physical type inventory: none of its DATETIME,
native JSONB approximations or text arrays remain unconverted. Converted
physical columns alone do not establish complete process-level support.

The subsequent startup and real-process follow-ups complete the requested
three-backend certificate and operation/transport acceptance. No deployment,
release, merge or ready-for-review transition is authorized by that result.

## Desired Users and Groups

User/group mutations and desired/observed reads now use a common user-state
Store. Node locking and capability checks reuse the operation Store; desired
state, resource-bound signed commands, outbox records, operation events and audit
remain in one transaction. The final scheduler assertion also fences idempotent
replays. The old PostgreSQL constructors remain compatibility adapters only.

Version conflicts, same-kind failed/expired/rejected revision recovery, capacity
limits, password key binding and normalization are unchanged. Queued command
coalescing locks outbox before command and refuses locked outboxes or command
leases; replacements reuse the unapplied revision. Membership capacity unions
desired and fresh observed members, preserving NULL, binary text equality and
multidimensional unnest semantics without bounded text casts.

Appended v20 converts desired user/group creation and update clocks plus the
last native text array, `desired_groups.members`. Verified shadows and exclusive
writer guards protect conversion; logical array triggers retain the 4096-member
constraint. Earlier manifests and migration receipts remain unchanged.

Resource responses preserve full observation timestamps and NULL array members.
Ordinary membership lists retain their existing JSON representation. Optional
`desired_member_dimensions` and `observed_member_dimensions` carry nonstandard
bounds and multidimensional shapes in row-major order. Mutation requests still
accept only bounded, valid Ocserv membership names, not arbitrary stored arrays.
The OpenAPI 3.1 string/null item union generates nullable member types directly;
converters continue to pass primitive members through exactly.

Approval creation/detail/decision readers now preserve logical creation and
expiry values rather than converting them back to finite time.Time. The
existing strict expires_at > created_at constraint remains: negative infinity
can be a creation time, but cannot be a valid expiry. Positive-infinity expiry
can be approved; an ancient finite expiry is rejected as expired.

## User Policies and Batches

Policy mutation/read/metrics, approval-bound batch creation/replay, child claims,
submission, refresh and quota/expiry/monthly-reset scheduling now use typed
stores on common transactions. Only compatibility constructors retain pgxpool.
Approvals, audit, idempotency hashes, source versions, bounded scheduling and
owner-qualified claim completion retain their existing contracts. Every common
scheduler write and replay asserts the current leadership term before commit.

Appended v21 converts twelve clocks in desired policies, policy mutations,
batch headers/items, scheduler leases and enforcement receipts. The enforcement
natural key contains period_start: migration verifies its private exact-key
registry before conversion, then re-encodes that registry under writer guards.
Existing migration artifacts and receipts remain unchanged. Claim lease pairs,
indexes, NULL and the full logical timestamp domain are preserved.

Crash recovery now includes a pending enforcement whose child advanced the user
version by one, then resolves the original child by its stable operation key.
Monthly reset only uses that recovery branch when the user is already enabled;
it does not reinterpret a newer manual disable as a reset to resume. A completed
receipt still prevents duplicate enforcement or reset commands.

Policy and batch response clocks are textual logical timestamps. The mutation
API still accepts only finite, whole-second UTC expiry. The form displays
unsupported stored expiries without clearing or truncating them and requires an
explicit finite replacement or removal before saving.

## Usage Cursors and Totals

Appended v22 converts all four usage/cursor clocks, including the connected_at
and period_start primary-key components. Verified shadows and writer guards
preserve existing rows, exact keys, counters and earlier migration receipts.
Both stores now use logical timestamps for cursor reads/writes and usage totals.
Policy reads and quota joins compare those values directly; their temporary
native DATETIME conversions have been removed.

Incoming protocol samples remain finite. Replay ordering uses their persisted
microsecond precision, including samples that differ only in submicroseconds.
An existing cursor can retain extended finite observations or either infinity.
The caller-owned transaction still covers cursor updates, both usage periods
and subsequent business writes, with node-before-cursor locking, reset handling,
counter saturation and username binding unchanged.

At this milestone, common startup and complete transportd/Agent process
acceptance were still outstanding; the startup follow-up is recorded below.

## Shared Storage

Appended v23 converts the remaining nine clocks in workspaces, nodes, endpoint
keys, sealing keys and upstream synchronization records, plus node labels and
synchronization classification JSONB. Verified shadows and exclusive writer
guards retain earlier rows, exact natural keys and migration receipts. Endpoint
revocation remains paired with a non-NULL revocation time. Node labels retain the
full JSONB value domain; classification still requires an object. Decimal tokens
are validated without passing their values through native floating point.

Enrollment, local simulation, transport status changes, telemetry activation/
offline maintenance and attestation revision updates write the logical clocks.
Workspace reuse does not reset creation/update times; telemetry activation still
uses GREATEST to avoid regressing an existing node clock, including infinity.

Upstream synchronization records have no Controller business mutation in the
current source tree. Their writer is the owner-only migration seed; runtime
principals retain SELECT only. The appended migration preserves that seed and
its exact-key registry rather than adding a new synchronization workflow.

### Controller Startup Follow-Up

The normal `ocserv-control` entrypoint now uses the common constructors for
authentication, authorization, audit, enrollment/trust, owner sessions, operations,
certificates, configuration, local ingress/results, telemetry and user policies.
Database selection, driver ownership and owner migrations live in the explicit
`database/connection` composition root, not business services. The driver ratchet
continues to cover application wiring and removes its prior pgx allowance.

Unset `OCSERV_DATABASE_BACKEND` still means PostgreSQL. Explicit `mysql` and
`mariadb` require test/development, the pinned server, the restricted driver DSN
and the existing verified-TLS policy. The database secret-file contract is
unchanged. PostgreSQL TLS configuration remains in its URL.

`--migrate-only` has a bounded ten-minute owner deadline. PostgreSQL keeps its
existing migration lock, audit preflight and role grants. MySQL/MariaDB keep the
immutable revision runner, refuse implicit dirty repair, migrate historical
telemetry and provision the last-14-day ingestion window through two future
months before explicit runtime grants. Re-running the owner command advances
that horizon; runtime never performs DDL. Owner-only history/provision/GC commands
remain available for explicit lifecycle work.

For MySQL/MariaDB, `OCSERV_RUNTIME_DATABASE_ROLE` names a pre-created `user@host`
account, without SQL quoting. Only the existing exact table/column/procedure
privileges and verified telemetry shards are granted. The command neither
creates accounts nor changes maintenance privileges. Accounts must be fresh or
independently audited: grants do not revoke previously held privileges.

Schema-only checks and normal startup use runtime-readable immutable receipts;
owner migration still verifies physical objects. API readiness uses the same
selected backend. G6's test-only completion recorder now borrows the common
transaction and preserves the final exact-term fence; its journal/routine remain
owner-installed test fixtures, not business migrations or runtime DDL.

### Real Process Acceptance

The same `TestControllerTransportBackendE2E` passes against PostgreSQL 18,
MySQL 8.4.10 and MariaDB 12.3.2 through `scripts/database-controller-e2e.sh` on
BuildServer. It starts the real Controller CLI, transportd, Agent and privd as
separate UNIX principals, two authenticated TLS relays, and native Ocserv 1.3.0.
Only the initial workspace is seeded; identities, approvals, enrollment, root
attestation, telemetry, commands and artifacts are created through real protocols.

The workflow verifies fenced sessions, independently approved CSR issuance,
signed certificate chains, one-use P12 download with OpenSSL verification,
revocation, user creation/replay, password rotation, disable/enable, native
OpenConnect authentication and group convergence. Operation/event/audit readers
and audit-chain/checkpoint verification run after these mutations. The runtime
processes never receive the harness's owner database credentials.

Configuration, upgrade/rollout, policy, recovery and maintenance stores retain
the focused real-database service coverage recorded in the validation history;
they are not all claimed as native process scenarios in this E2E. The external
PKI is a TLS signing fixture, and a strict service proxy replaces systemd PID 1
inside the disposable container. This is not production installation, a real
external-CA integration, or a two-failure-domain G6 run. Production admission,
Draft status and the prohibition on merge/deployment are unchanged.
