-- +goose Up
-- Remove the DEFAULT partition from the routing table while retaining its historical rows.
ALTER TABLE public.telemetry_samples DETACH PARTITION public.telemetry_samples_default;
GRANT SELECT ON public.telemetry_samples_default TO ocservia_app;
DROP FUNCTION public.telemetry_drop_expired_partitions(timestamptz);

CREATE TABLE public.telemetry_partition_retirements (
    partition_name text PRIMARY KEY
        CHECK (partition_name ~ '^telemetry_samples_[1-9][0-9]{3}(0[1-9]|1[0-2])$'),
    relation_oid regclass NOT NULL UNIQUE
);

-- +goose StatementBegin
CREATE FUNCTION public.telemetry_ensure_upcoming_partitions() RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    SET "TimeZone" TO 'UTC'
AS $$
DECLARE
    month_start timestamptz := date_trunc('month', now());
    sample_month timestamptz;
    partition_name text;
BEGIN
    FOR month_offset IN 0..2 LOOP
        sample_month := month_start + make_interval(months => month_offset);
        partition_name := 'telemetry_samples_' || to_char(sample_month, 'YYYYMM');
        IF to_regclass('public.' || partition_name || '_query_idx') IS NULL OR NOT EXISTS (
            SELECT 1 FROM pg_inherits
            WHERE inhparent='public.telemetry_samples'::regclass
            AND inhrelid=to_regclass('public.' || partition_name)
            AND NOT inhdetachpending
        ) THEN
            PERFORM public.telemetry_ensure_month_partition(sample_month);
        END IF;
    END LOOP;
END;
$$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION public.telemetry_ensure_upcoming_partitions() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.telemetry_ensure_upcoming_partitions() TO ocservia_app;

CREATE TABLE public.telemetry_rollup_queue (
    bucket_at timestamptz NOT NULL,
    node_id uuid NOT NULL,
    metric text NOT NULL,
    PRIMARY KEY (bucket_at, node_id, metric)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON public.telemetry_rollup_queue TO ocservia_app;

-- +goose StatementBegin
CREATE FUNCTION public.telemetry_queue_rollups() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
AS $$
BEGIN
    INSERT INTO public.telemetry_rollup_queue(bucket_at,node_id,metric)
    SELECT DISTINCT date_bin(interval '5 minutes',sampled_at,'2000-01-01 00:00:00+00'::timestamptz),node_id,metric
    FROM inserted_samples WHERE sampled_at > '-infinity'::timestamptz
    ORDER BY 1,2,3
    ON CONFLICT (bucket_at,node_id,metric) DO UPDATE SET metric=EXCLUDED.metric;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER telemetry_queue_rollups AFTER INSERT ON public.telemetry_samples
    REFERENCING NEW TABLE AS inserted_samples FOR EACH STATEMENT
    EXECUTE FUNCTION public.telemetry_queue_rollups();
CREATE TRIGGER telemetry_queue_rollups AFTER INSERT ON public.telemetry_samples_default
    REFERENCING NEW TABLE AS inserted_samples FOR EACH STATEMENT
    EXECUTE FUNCTION public.telemetry_queue_rollups();

INSERT INTO public.telemetry_rollup_queue(bucket_at,node_id,metric)
SELECT DISTINCT date_bin(interval '5 minutes',sampled_at,'2000-01-01 00:00:00+00'::timestamptz),node_id,metric
FROM (
    SELECT sampled_at,node_id,metric FROM public.telemetry_samples
    UNION ALL
    SELECT sampled_at,node_id,metric FROM public.telemetry_samples_default
) samples
WHERE sampled_at >= date_bin(interval '5 minutes',now()-interval '14 days','2000-01-01 00:00:00+00'::timestamptz)
ON CONFLICT DO NOTHING;

CREATE INDEX audit_checkpoints_latest_idx ON public.audit_checkpoints(workspace_id,created_at DESC,id DESC);

ALTER TABLE public.outbox_events ADD COLUMN receipt_recovery_attempt_limit integer
    CHECK (receipt_recovery_attempt_limit > 0);
