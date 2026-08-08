# Telemetry and read-only fleet views

I07 adds a read-only node observation path from the unprivileged Agent to the
control-plane API and Web application. The Agent emits a bounded telemetry
batch every 30 seconds on a dedicated Iroh unidirectional stream. A node is
shown offline after its latest heartbeat is more than 90 seconds old.

## Data classes and limits

- Security observations, current health, aggregate metrics, and raw history
  have separate priorities.
- The Agent persists at most 64 MiB for at most five minutes. It evicts oldest raw
  history before aggregate, health, and security data and reports drop counts.
- A wire batch is limited to 512 KiB. Session, username, and client IP fields
  are stored in the node session read model and must not be metric labels.
- Raw samples are monthly PostgreSQL partitions. Scheduler maintenance builds
  5-minute and 1-hour rollups and applies the 14-day, 90-day, and 13-month
  retention periods idempotently.

Current state is available from `GET /api/v1/nodes`,
`GET /api/v1/nodes/{node_id}`, and the node `sessions` resource. Bounded
history queries use the node `telemetry` resource with a metric, resolution,
and optional RFC 3339 start time. SSE is only an invalidation signal: clients
rebuild authoritative state through REST after connecting or reconnecting.

## Upgrade and rollback

Apply database migration `000005_telemetry_observed` before deploying the new
Controller, transportd, Agent, or Web images. The protocol change is additive,
so older Agents continue to connect without emitting telemetry.

To roll back application binaries, deploy the previous Controller and Agent
versions first. The telemetry tables can remain in place for forward recovery.
If schema rollback is explicitly required, preserve any needed history, stop
I07 writers and scheduler roles, then apply
`000005_telemetry_observed.down.sql`. This removes I07 telemetry history and
read models; it does not alter node trust or enrollment state.
