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
the batch to a misleading boolean.

Any batch containing a disable action requires independent approval. The client
generates the UUIDv7 batch identifier first, obtains approval for action
`user.batch.disable` on that `batch_operation`, then submits the identifier and
approval in `X-Approval-ID`. Enable-only batches do not require approval.

Rollback disables the scheduler/API version first, waits for active command
leases to settle, and reverts the standalone I14 change. Migration `000013`
may be rolled down only before policy, usage, batch, or enforcement history is
relied upon; otherwise retain the additive tables and forward-fix.
