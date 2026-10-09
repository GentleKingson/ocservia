# Users, groups and policy

## Desired and observed state

Users and groups are scoped to a node. The control plane records desired state, accepts mutations as asynchronous operations, and keeps agent observations separate. API consumers must display the returned convergence value rather than treating `202 Accepted` as remote success.

Mutations require `Idempotency-Key`, an expected desired version in `If-Match` or the request body, and a reason. Use `revision-0` only when creating a new user or group. A new mutation of the same kind supersedes an older command for the same resource only while that command is still `queued` and holds no node command lease: group apply replaces a queued group apply, password rotation a queued password rotation, disable a queued disable and enable a queued enable. Create never supersedes. While a different kind is queued, or the current revision is dispatched, accepted, running, unknown or superseded, the API returns `409 desired-revision-pending`. After a failed, expired, rolled-back or safely rejected revision, only a same-kind replacement is accepted; another kind returns `409 desired-revision-recovery-required`. Mixed kinds are not merged because a later command does not carry the missing password or lock intent.

Password endpoints accept only a versioned `SealedSecretV1` with the
`user_password` purpose, the enrolled user-password key ID, and an
RSA-OAEP-SHA256 ciphertext. Plaintext passwords and legacy untyped ciphertext
fields are rejected. The P12 password uses a different enrolled key pair and
the `certificate_p12_password` purpose, so ciphertext cannot be replayed across
the two operations. Privd keeps both private keys root-only and independently
checks the signed typed command before selecting the matching key.

The root adapter decrypts with fixed `openssl` arguments and applies a user
password to a same-directory staging copy with the fixed `ocpasswd` executable.
Create atomically requires the authoritative user record to be absent; rotation
atomically requires it to exist. Existing groups and lock state are preserved
before the staging file is fsynced and atomically renamed. Each child process
has a five-second budget while the complete desired mutation has a 20-second
RPC budget. Cancellation-safe cleanup removes its uniquely named staging file,
and privd removes only valid stale adapter staging names before accepting
requests. A conflict, timeout, cancellation, or failed password change leaves
the authoritative record unchanged. Responses, observations, audit events,
logs, and traces never contain password material or password hashes.

Plain authentication stores groups in the second field of each `/etc/ocserv/ocpasswd` record. Group apply updates that authoritative field for every affected user, preserves password hashes and lock markers, and commits with a same-directory, fsynced atomic rename. Observed groups are derived from the same records, not a sidecar file. Usernames, group names, and members use the ASCII pattern `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`; callers cannot supply executable or file paths.

The supported per-node state is bounded to 384 managed users, 384 managed groups, 384 authoritative users, 384 authoritative groups, 384 members in one group, and 384 total non-sentinel user-to-group memberships. Telemetry may report up to 768 groups while empty managed-group tombstones converge alongside disjoint authoritative groups. The state API can return up to 1,536 unioned desired/observed resources when managed and authoritative names are fully disjoint. These limits keep the complete authoritative snapshot, tombstones, and the largest group replacement within the fixed 64 KiB local RPC frame and 512 KiB telemetry batch. With a fresh complete snapshot, the API counts the union of desired and observed users and rejects a create that would exceed the user bound. The root adapter remains authoritative: it rejects a create against a full password file before decrypting the password or invoking `ocpasswd`, then reparses every password-file staging result before preparing recovery evidence or replacing the authoritative file. The API also rejects a larger group replacement or new managed group beyond the bound before creating an operation; an older controller sending an oversized command receives a terminal payload rejection rather than an Unknown outcome. Existing nodes above the authoritative snapshot bound must reduce their `ocpasswd` state before enrollment; the Agent never publishes a partial user/group snapshot that could delete a previously complete observation.

Automatic drift repair is disabled. Privd keeps a bounded, root-only desired-effect store at `/var/lib/ocservia-privd/desired-effects.sqlite3` with a separately generated mode `0600` HMAC key and authenticated store identity. One latest record is retained per mutation kind and resource, up to 65,536 active records. Each record binds command ID, idempotency key, canonical semantic payload hash, revision, expiry, and authenticated before/after authoritative-file states without storing password material or password hashes. Expired applied records are collected only after the corresponding command can no longer pass Agent expiry validation; unresolved prepared evidence is retained.

Privd durably records `prepared` before the authoritative rename and `applied` afterward. Before another password-file mutation, it resolves every authenticated prepared transition against the current file: a matching after-state is promoted to applied, a matching before-state blocks unrelated work, and any ambiguous state fails closed. Recovery returns one of `APPLIED_EXACT`, `SUPERSEDED_BY_NEWER_REVISION`, `ABSENT`, or `UNKNOWN`. `ABSENT` requires an exact authenticated prepared record whose before-state still matches; a missing record, older snapshot, deleted database, missing store identity, or mismatched file is `UNKNOWN` and can never authorize retry. A newer same-kind revision makes an older payload permanently ineligible for retry. Back up and restore the store and key as a pair; losing either leaves affected Unknown operations for manual reconciliation rather than authorizing replay.

The Agent atomically records a reconciled command result and the last successfully applied desired revision for each user and group in its SQLite journal, then reports that revision with hash-free observations. Convergence requires both that applied revision and the public fingerprint match. An offline node with queued work remains `offline_pending`, including password rotations whose public fingerprint is intentionally unchanged. Operators should inspect the linked operation before deciding whether to issue a new desired revision.

## Quota, expiry and batches

Quota values use integer bytes up to JavaScript's safe integer maximum
(`9007199254740991`). A policy selects receive, transmit, or combined
traffic and either a UTC calendar-month or lifetime period. Monthly counters
start at `00:00:00Z` on the first day of the month. `none` always has a zero
limit. Expiry is an RFC 3339 UTC timestamp ending in `Z`; the parsed instant
must be a whole second so operators and schedulers share one boundary.

Session telemetry is converted from monotonic per-session counters into durable
monthly and lifetime usage. Replayed observations contribute no additional
bytes, and an observation older than the durable session cursor is ignored. A
counter decrease in a newer observation is treated as a new counter epoch and
contributes the new value rather than guessing an outcome.

The scheduler uses a database-backed leadership lease and a reentrant scan. Restarting it simply
replays the scan with stable idempotency keys. Quota or expiry enforcement
creates the existing typed `user_disable` desired-state operation; it never
executes local commands. Node write serialization remains in the command worker.
Unknown outcomes are reconciled by the ordinary command path.
At a new UTC calendar month, a user is re-enabled with another stable operation
only when the policy is monthly and unexpired, an earlier month's quota
enforcement for the same policy version disabled the user (and the user is still
at that disabled version, or that enforcement is still pending), and usage in the
new month is below the quota. Expired users and users disabled for another reason
are not re-enabled.

User batches contain a parent and bounded item list. Each item is independently
authorized and, when allowed, receives a distinct child operation and command.
The default global active remote-command limit is 50. Dispatch reservations are
serialized across workers and count in-flight or unknown commands, while queued
work for an offline node does not consume execution capacity. Parent results
retain forbidden, failed, unknown, and offline-pending child states instead of
reducing the batch to a misleading boolean. Set
`OCSERV_USER_OPERATION_CONCURRENCY` from 1 through 500 to change the limit.
Durable queued work is separately bounded at 500 commands per node and 5,000
per workspace; callers receive a retryable service-unavailable response when a
backlog is full.

Any batch containing a disable action requires independent approval. The client
generates the UUIDv7 batch identifier first, obtains approval for action
`user.batch.disable` on that `batch_operation`, and includes the complete ordered
`batch_items` list in the approval request. The approval response exposes the
canonical SHA-256 and reviewed items. The later batch must match both the batch
identifier and content hash before the approval can be consumed. The client then
has the independent approver read `GET /approval-requests/{approval_id}` and
submit that digest as `expected_request_hash` with the decision. The client then
submits the approval in `X-Approval-ID`. Enable-only batches do not require approval.

`GET /api/v1/user-operations/metrics` returns workspace-scoped pending-policy,
active-item, expired-claim, and unknown-item counters. Alert when
`stale_batch_claim_total` stays nonzero across two scheduler intervals, when
`unknown_batch_item_total` increases, or when `policy_pending_total` grows for
more than two intervals. Each scheduler run emits the
`user_operations.scheduler.run` trace span and a structured completion log. A
failed run emits `alert_kind=user_operations.scheduler_failed` before the
scheduler process exits for supervised restart.

## Recovery

Stop the scheduler and affected API writes, and reconcile active commands.
Rollback is a forward desired-state change: reapply prior group members or use
explicit enable/disable with the current version. Enable/disable changes only
the lock marker, not the password or group field. Preserve desired/observed
records, journals, root effect evidence, and policy, usage, batch and enforcement
history. There are no database down migrations; use a forward fix or a planned
[isolated restore](../operations/incident-recovery.md#database-recovery).
