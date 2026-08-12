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
- Raw samples are monthly PostgreSQL partitions. Scheduler maintenance builds
  5-minute and 1-hour rollups and applies the 14-day, 90-day, and 13-month
  retention periods idempotently.
- The Controller accepts snapshot, metric, and security-observation timestamps
  from the preceding 14 days through five minutes in the future. Events outside
  that window are rejected before PostgreSQL partition selection.

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

Apply database migration `000005_telemetry_observed` before deploying the new
Controller, transportd, Agent, or Web images. The protocol change is additive,
so older Agents continue to connect without emitting telemetry.

Migration `000022_transport_event_quarantine` adds the durable global cursor
and quarantine evidence. Deploy it before a Controller using the resilient
ingestion path. Its down migration refuses to discard existing quarantine
evidence. It also refuses to remove a durable cursor that the previous
Controller cannot recover from `transport_events`. Before an explicit schema
rollback, preserve and clear the quarantine evidence, resolve the underlying
incident, and let the new Controller commit a later valid event. The down
migration verifies that this accepted event is both the durable cursor and the
latest legacy cursor; an archived quarantined tail alone is not sufficient.

For a Controller rollback across migration `000022`, first stop event-ingestion
writers while the new Controller is still the schema authority, satisfy the
cursor compatibility guard, and apply the `000022` down migration. Only then
start the previous Controller binary. The Agent does not need to roll back for
this database-only protocol change.

If the older I07 telemetry schema must also be removed, preserve any needed
history, stop I07 writers and scheduler roles, then apply
`000005_telemetry_observed.down.sql`. This removes I07 telemetry history and
read models; it does not alter node trust or enrollment state.
