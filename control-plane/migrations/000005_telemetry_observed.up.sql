CREATE TABLE telemetry_ingest_batches (
    batch_id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    sequence bigint NOT NULL CHECK (sequence >= 0),
    kind text NOT NULL CHECK (kind IN ('security', 'current_health', 'aggregate', 'raw_history')),
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    payload_bytes integer NOT NULL CHECK (payload_bytes BETWEEN 0 AND 524288)
);

ALTER TABLE transport_events DROP CONSTRAINT transport_events_event_type_check;
ALTER TABLE transport_events ADD CONSTRAINT transport_events_event_type_check
    CHECK (event_type IN ('connected','disconnected','command_result','heartbeat','error','path_changed','telemetry'));

CREATE TABLE node_observed_snapshots (
    node_id uuid PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    boot_id text NOT NULL CHECK (length(boot_id) BETWEEN 1 AND 128),
    agent_instance_id uuid NOT NULL,
    agent_version text NOT NULL CHECK (length(agent_version) BETWEEN 1 AND 128),
    ocserv_version text NOT NULL CHECK (length(ocserv_version) BETWEEN 1 AND 128),
    os_release text NOT NULL CHECK (length(os_release) BETWEEN 1 AND 128),
    ocserv jsonb NOT NULL,
    system jsonb NOT NULL,
    path jsonb NOT NULL,
    last_heartbeat_at timestamptz NOT NULL,
    dropped_security bigint NOT NULL DEFAULT 0 CHECK (dropped_security >= 0),
    dropped_health bigint NOT NULL DEFAULT 0 CHECK (dropped_health >= 0),
    dropped_aggregate bigint NOT NULL DEFAULT 0 CHECK (dropped_aggregate >= 0),
    dropped_raw bigint NOT NULL DEFAULT 0 CHECK (dropped_raw >= 0),
    CHECK (jsonb_typeof(ocserv) = 'object'),
    CHECK (jsonb_typeof(system) = 'object'),
    CHECK (jsonb_typeof(path) = 'object')
);
CREATE INDEX node_observed_freshness_idx ON node_observed_snapshots (last_heartbeat_at);

CREATE TABLE node_sessions (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    session_id text NOT NULL CHECK (length(session_id) BETWEEN 1 AND 256),
    username text NOT NULL CHECK (length(username) BETWEEN 1 AND 256),
    client_ip inet NOT NULL,
    connected_at timestamptz NOT NULL,
    bytes_in bigint NOT NULL CHECK (bytes_in >= 0),
    bytes_out bigint NOT NULL CHECK (bytes_out >= 0),
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, session_id)
);
CREATE INDEX node_sessions_observed_idx ON node_sessions (node_id, observed_at DESC, session_id);

CREATE TABLE telemetry_security_events (
    event_id uuid PRIMARY KEY,
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    observed_at timestamptz NOT NULL,
    severity text NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    event_type text NOT NULL CHECK (length(event_type) BETWEEN 1 AND 128),
    detail jsonb NOT NULL CHECK (jsonb_typeof(detail) = 'object')
);
CREATE INDEX telemetry_security_node_time_idx ON telemetry_security_events (node_id, observed_at DESC);

CREATE TABLE telemetry_samples (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    batch_id uuid NOT NULL REFERENCES telemetry_ingest_batches(batch_id) ON DELETE CASCADE,
    sampled_at timestamptz NOT NULL,
    metric text NOT NULL CHECK (metric IN ('cpu_usage_ratio', 'memory_used_bytes', 'network_rx_bytes', 'network_tx_bytes', 'session_count', 'connection_rtt_ms')),
    value double precision NOT NULL CHECK (value NOT IN ('Infinity'::double precision, '-Infinity'::double precision, 'NaN'::double precision)),
    PRIMARY KEY (sampled_at, node_id, batch_id, metric)
) PARTITION BY RANGE (sampled_at);
CREATE TABLE telemetry_samples_default PARTITION OF telemetry_samples DEFAULT;
CREATE INDEX telemetry_samples_default_query_idx ON telemetry_samples_default (node_id, metric, sampled_at DESC);

CREATE TABLE telemetry_rollups_5m (
    node_id uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    metric text NOT NULL,
    bucket_at timestamptz NOT NULL,
    sample_count bigint NOT NULL CHECK (sample_count > 0),
    min_value double precision NOT NULL,
    max_value double precision NOT NULL,
    avg_value double precision NOT NULL,
    PRIMARY KEY (node_id, metric, bucket_at)
);
CREATE TABLE telemetry_rollups_1h (LIKE telemetry_rollups_5m INCLUDING ALL);

CREATE OR REPLACE FUNCTION telemetry_ensure_month_partition(sample_time timestamptz) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    start_at timestamptz := date_trunc('month', sample_time AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';
    end_at timestamptz := start_at + interval '1 month';
    partition_name text := 'telemetry_samples_' || to_char(start_at, 'YYYYMM');
BEGIN
    IF start_at < date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - interval '1 month'
       OR start_at > date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' + interval '2 months' THEN
        RAISE EXCEPTION 'telemetry partition date outside permitted window';
    END IF;
    EXECUTE format('CREATE TABLE IF NOT EXISTS public.%I PARTITION OF public.telemetry_samples FOR VALUES FROM (%L) TO (%L)', partition_name, start_at, end_at);
    EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON public.%I (node_id, metric, sampled_at DESC)', partition_name || '_query_idx', partition_name);
END;
$$;
REVOKE ALL ON FUNCTION telemetry_ensure_month_partition(timestamptz) FROM PUBLIC;

CREATE OR REPLACE FUNCTION telemetry_drop_expired_partitions(cutoff timestamptz) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    partition record;
    partition_month timestamptz;
    dropped integer := 0;
BEGIN
    IF cutoff < now() - interval '90 days' OR cutoff > now() THEN
        RAISE EXCEPTION 'telemetry retention cutoff outside permitted window';
    END IF;
    FOR partition IN
        SELECT child.relname
        FROM pg_catalog.pg_inherits
        JOIN pg_catalog.pg_class parent ON parent.oid = inhparent
        JOIN pg_catalog.pg_namespace parent_namespace ON parent_namespace.oid = parent.relnamespace
        JOIN pg_catalog.pg_class child ON child.oid = inhrelid
        JOIN pg_catalog.pg_namespace child_namespace ON child_namespace.oid = child.relnamespace
        WHERE parent_namespace.nspname = 'public'
          AND parent.relname = 'telemetry_samples'
          AND child_namespace.nspname = 'public'
          AND child.relname ~ '^telemetry_samples_[0-9]{6}$'
    LOOP
        partition_month := to_timestamp(substring(partition.relname from '[0-9]{6}$'), 'YYYYMM');
        IF partition_month + interval '1 month' <= cutoff THEN
            EXECUTE format('DROP TABLE IF EXISTS public.%I', partition.relname);
            dropped := dropped + 1;
        END IF;
    END LOOP;
    RETURN dropped;
END;
$$;
REVOKE ALL ON FUNCTION telemetry_drop_expired_partitions(timestamptz) FROM PUBLIC;

COMMENT ON TABLE node_sessions IS 'High-cardinality session, username, and client IP data; these fields must never become Prometheus labels.';
COMMENT ON TABLE telemetry_samples IS 'Monthly-partitioned raw telemetry retained independently from current observed state and rollups.';
