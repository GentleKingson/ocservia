# Telemetry and read-only fleet views

I07 adds a read-only node observation path from the unprivileged Agent to the
control-plane API and Web application. The Agent emits a bounded telemetry
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
- PostgreSQL stores raw samples in monthly partitions; MySQL/MariaDB use
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

Run the target's owner-only database initialization and use the explicit
[Controller rollback](../how-to/controller-rollback.md) for a binary rollback.
The current tree provides no database down migrations or historical
cross-version acceptance. MySQL/MariaDB use their own immutable manifests and
execution receipts, not PostgreSQL migration numbers.

Preserve telemetry history, quarantine, durable cursors and node trust through
any backend-specific recovery. Stop affected writers and resolve the underlying
incident before resuming ingestion. Use a forward fix or an explicitly planned
isolated restore; do not clear evidence or reset cursors to make an older
binary start.
