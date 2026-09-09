# PR-02 Version 4 Controller Workflows

This is still Draft and not final PR-02 acceptance. Controller startup rejects
MySQL/MariaDB; tests explicitly construct backend-owned services and the actual
HTTP router. Passing those routes does not establish that every Controller
route, transport consumer, upgrade or read model is portable.

## Recorded Migration

Version 4 appends to the exact published version-3 checksum on both historical
roots. Versions 1 through 3 and all PostgreSQL migration files are unchanged.
Both engine manifests are authored by executing SQL and checking the resulting
objects and copied values on the pinned real databases, never by adopting live
hashes during application startup.

Thirteen additional business time columns use signed PostgreSQL-epoch
microseconds: four local-auth attempt fields, two scheduler leadership fields,
two audit fields, role-binding creation, bootstrap completion, and three
approval fields. Audit before/after summaries use lossless, arbitrary JSONB
with server-side validation. The migration preserves append-only audit guards
around the temporary removal of update triggers needed for verified copying.
An interrupted migration still refuses startup and requires checksum repair.

Ambiguous historical auth lease/block timestamps require explicit owner
decisions, as documented in [logical time](database-pr02-time-decisions.md).
A normal owner connection cannot change guarded source rows during repair.

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
  preserving durable root-RPC evidence and audit/fence atomicity. Certificate
  issuance and maintenance are not part of this portable download constructor.

No business advisory-lock call remains outside the PostgreSQL adapter package.
MySQL/MariaDB use transaction-owned lock records, not GET_LOCK. This is not the
same as saying every outer business transaction has been migrated: legacy
operation/transport callers still borrow pgx transactions through explicit
bridges. The driver ratchet records only the specific compatibility sites.

The cross-engine HTTP test executes login, authentication, logout, local admin
bootstrap, authorized user creation, unauthorized management denial, password
change/session revocation, user disable and last-administrator protection.
Other tests exercise the real audit/RBAC, scheduler and download service
methods. Approval request/approval creation is still fixture setup in the
audit/RBAC corpus, not an end-to-end approval API acceptance claim.

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

## Remaining Acceptance

Of the pre-v4 inventory, 130 DATETIME fields, nine JSONB fields and one text-array
field remain on their earlier representations. Each needs its writers/readers
ported together with an appended migration. In particular, native artifact
times are still range-limited despite correct transaction-clock usage.

Approval creation/approval, certificate issuance/maintenance, complete
operation/transport/telemetry ingestion and read-model transactions, and full
Controller startup/workflow parity remain outstanding. No deployment, release,
merge, ready-for-review transition or claim of complete support is authorized.
