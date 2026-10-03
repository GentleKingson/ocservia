# Telemetry and read-only fleet views

The unprivileged Agent sends read-only observations to the control-plane API
and Web application. It emits a bounded telemetry
batch every 30 seconds on a dedicated Iroh unidirectional stream. A node is
shown offline after its latest heartbeat is more than 90 seconds old.

## Data classes and limits

- Security observations, current health, aggregate metrics, and raw history
  have separate priorities.
- The Agent persists at most 64 MiB. Buffered telemetry is eligible for offline
  recovery for at most five minutes. It evicts oldest raw history before
  aggregate, health, and security data and reports drop counts.
- A wire batch is limited to 512 KiB. Session, username, and client IP fields
  are stored in the node session read model and must not be metric labels.
- PostgreSQL stores raw samples in monthly partitions; MySQL uses
  owner-managed monthly shard tables and a durable shard catalog. Runtime
  writers do not create or drop these objects. Scheduler maintenance builds
  5-minute and 1-hour rollups and applies the 14-day, 90-day, and 13-month
  retention periods idempotently. Rollups recompute the full accepted 14-day
  lateness window starting at each resolution's complete UTC bucket, and each
  retention invocation deletes at most 1,000 expired rows per rollup table and
  retires at most one expired month. Retention cutoffs are checked against
  server time and cannot move into the future; the oldest retirement candidate
  is aggregated before its raw data becomes unavailable.
- The Controller accepts snapshot, metric, and security-observation timestamps
  from the preceding 14 days through five minutes in the future. Events outside
  that window are rejected before backend partition/shard selection.

## Transport ingestion recovery

The Controller classifies authenticated transport ingestion failures before it
updates the retained event cursor. Transaction or database failures retain the
previous cursor and use the bounded reconnect backoff. Permanently invalid
business payloads are rolled back, recorded as bounded metadata without their
raw payload, and advance the durable cursor in the same transaction. A high
severity security alert identifies each newly quarantined event so one node
cannot silently block later events from other nodes.

Current state is available from `GET /api/v1/nodes`,
`GET /api/v1/nodes/{node_id}`, and the node `sessions` resource. Bounded
history queries use the node `telemetry` resource with a metric, resolution,
and optional RFC 3339 start time. SSE is only an invalidation signal: clients
rebuild authoritative state through REST after connecting or reconnecting.

## Upgrade and rollback

Run the target's owner-only initialization; backend migration numbers are not
interchangeable. [Binary rollback](../how-to/controller-lifecycle.md#rollback) supplies
neither down migrations nor cross-version acceptance. For
[recovery](../operations/incident-recovery.md#database-recovery), stop affected
writers and preserve telemetry history, quarantine, cursors and node trust.
Resolve the incident before resuming ingestion; never reset evidence to make
an older binary start.
