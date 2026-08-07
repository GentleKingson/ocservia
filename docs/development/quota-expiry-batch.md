# Quota, expiry, and batch operations

Quota values use integer bytes up to JavaScript's safe integer maximum
(`9007199254740991`). A policy selects receive, transmit, or combined
traffic and either a UTC calendar-month or lifetime period. Monthly counters
start at `00:00:00Z` on the first day of the month. `none` always has a zero
limit. Expiry is an exact RFC 3339 UTC instant; offset timestamps and fractional
seconds are rejected so operators and schedulers share one boundary.

Session telemetry is converted from monotonic per-session counters into durable
monthly and lifetime usage. Replayed observations contribute no additional
bytes, and an observation older than the durable session cursor is ignored. A
counter decrease in a newer observation is treated as a new counter epoch and
contributes the new value rather than guessing an outcome.

The scheduler uses a PostgreSQL lease and a reentrant scan. Restarting it simply
replays the scan with stable idempotency keys. Quota or expiry enforcement
creates the existing typed `user_disable` desired-state operation; it never
executes local commands. Node write serialization remains in the command worker.
Unknown outcomes are reconciled by the ordinary command path.
At a new UTC calendar month, only users disabled by an earlier enforcement of
the same policy are re-enabled, with another stable operation. Expired users and
users disabled for another reason are not re-enabled.

User batches contain a parent and bounded item list. Each item is independently
authorized and, when allowed, receives a distinct child operation and command.
The default global submission bound is 50 per scan. Parent results retain
forbidden, failed, unknown, and offline-pending child states instead of reducing
the batch to a misleading boolean. Set `OCSERV_USER_OPERATION_CONCURRENCY` from
1 through 500 to change the global per-scan bound.

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

Rollback disables the scheduler/API version first, waits for active command
leases to settle, and reverts the standalone I14 change. Migration `000013`
may be rolled down only before policy, usage, batch, or enforcement history is
relied upon; otherwise retain the additive tables and forward-fix.
