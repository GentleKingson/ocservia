ALTER TABLE transport_events DROP CONSTRAINT IF EXISTS transport_events_event_type_check;
DELETE FROM transport_events WHERE event_type = 'telemetry';
ALTER TABLE transport_events ADD CONSTRAINT transport_events_event_type_check
    CHECK (event_type IN ('connected','disconnected','command_result','heartbeat','error','path_changed'));
DROP FUNCTION IF EXISTS telemetry_drop_expired_partitions(timestamptz);
DROP FUNCTION IF EXISTS telemetry_ensure_month_partition(timestamptz);
DROP TABLE IF EXISTS telemetry_rollups_1h;
DROP TABLE IF EXISTS telemetry_rollups_5m;
DROP TABLE IF EXISTS telemetry_samples;
DROP TABLE IF EXISTS telemetry_security_events;
DROP TABLE IF EXISTS node_sessions;
DROP TABLE IF EXISTS node_observed_snapshots;
DROP TABLE IF EXISTS telemetry_ingest_batches;
