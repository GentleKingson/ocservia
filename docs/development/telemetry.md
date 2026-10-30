# Telemetry and read-only fleet views

The unprivileged Agent streams read-only node observations to the Controller.
Every 30 seconds it emits a bounded telemetry batch over a dedicated
unidirectional Iroh stream. The Web UI queries the persisted data through the
HTTP API.

A node appears offline once its most recent heartbeat is older than 90 seconds
or if it has never reported one. The API computes freshness (`fresh`, `stale`,
`never`) at read time. Background scheduler maintenance persists the offline
state transition separately.

## Data classes and limits

- Telemetry separates security observations, current health snapshots,
  aggregate metrics, and raw history into distinct priority tiers.
- The local Agent buffer is capped at 64 MiB, and buffered telemetry is
  eligible for offline recovery for at most five minutes. When the buffer fills,
  the Agent evicts the oldest raw history before dropping aggregate, health, or
  security records, and reports the resulting drop counts.
- Each wire batch is bounded to 512 KiB. High-cardinality session, username,
  and client IP attributes belong only to the node session read model and are
  never attached as metric labels.
- In PostgreSQL, raw telemetry samples land in monthly table partitions. MySQL
  uses owner-managed monthly shard tables tracked in a durable shard catalog
  that runtime writers never create or drop directly.
- Background scheduler maintenance computes 5-minute and 1-hour rollups and
  enforces the 14-day, 90-day, and 13-month retention windows idempotently.
  Each rollup pass recomputes the full 14-day lateness window starting from the
  resolution's complete UTC bucket. Each retention pass deletes up to 1,000
  expired rows per rollup table and retires at most one expired month.
  Retention cutoffs are evaluated against server time, cannot advance into the
  future, and always aggregate the oldest retirement candidate before its raw
  partition or shard is retired.
- On ingress, the Controller accepts snapshot, metric, and
  security-observation timestamps ranging from 14 days in the past up to five
  minutes in the future. It rejects out-of-window events before selecting a
  backend partition or shard.

## Transport ingestion recovery

Before advancing its durable event cursor, the Controller classifies any
authenticated transport ingestion failure. Transient database or transaction
errors leave the cursor at its previous position and trigger bounded reconnect
backoff.

When an event carries a permanently invalid business payload, the Controller
rolls back the payload write, records bounded quarantine metadata without the
raw payload, and advances the durable cursor within the same transaction. It
raises a high-severity security alert for each quarantined event. This keeps a
malformed payload from one node from stalling ingestion for the rest of the
fleet.

Clients read current fleet and node state from `GET /api/v1/nodes`,
`GET /api/v1/nodes/{node_id}`, and the node `sessions` resource.

Historical series are queried from the node `telemetry` resource with a
required metric name, a resolution of `raw`, `5m` (default), or `1h`, and an
optional RFC 3339 `since` timestamp (defaulting to 24 hours ago). The resource
returns up to 2,000 data points, ordered from oldest to newest starting at
`since`.

The SSE stream is only an invalidation signal, so clients fetch authoritative
state via REST after connecting or reconnecting. A reconnecting client may
supply its last seen event ID as a cursor. The request returns `400`
(`invalid-cursor`) if that event is no longer visible within the caller's
authorized scope.

## Upgrade and rollback

Schema setup and upgrades must run through the target backend's owner-only
initialization path. Migration numbers are not interchangeable across database
engines, and [Binary rollback](../how-to/controller-lifecycle.md#rollback)
provides neither down migrations nor cross-version schema compatibility.

During [recovery](../operations/incident-recovery.md#database-recovery), stop
affected writers and preserve telemetry history, quarantine records, ingestion
cursors, and node trust state. Resolve the underlying fault before resuming
ingestion, and never wipe stored evidence to make an older binary start.
