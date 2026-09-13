CREATE INDEX telemetry_rollups_5m_retention_idx ON telemetry_rollups_5m(bucket_at,node_id,metric);
CREATE INDEX telemetry_rollups_1h_retention_idx ON telemetry_rollups_1h(bucket_at,node_id,metric);

CREATE FUNCTION telemetry_prune_rollups(maintenance_time timestamptz) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public SET timezone = 'UTC' AS $$
BEGIN
    IF maintenance_time IS NULL OR NOT isfinite(maintenance_time)
       OR maintenance_time < clock_timestamp() - interval '5 minutes'
       OR maintenance_time > clock_timestamp() + interval '5 minutes' THEN
        RAISE EXCEPTION 'telemetry maintenance clock outside permitted window';
    END IF;
    maintenance_time := least(maintenance_time,clock_timestamp());
    WITH expired AS (
        SELECT node_id,metric,bucket_at FROM public.telemetry_rollups_5m
        WHERE bucket_at < maintenance_time - interval '90 days'
        ORDER BY bucket_at,node_id,metric LIMIT 1000 FOR UPDATE SKIP LOCKED
    ) DELETE FROM public.telemetry_rollups_5m r USING expired e
      WHERE (r.node_id,r.metric,r.bucket_at)=(e.node_id,e.metric,e.bucket_at);
    WITH expired AS (
        SELECT node_id,metric,bucket_at FROM public.telemetry_rollups_1h
        WHERE bucket_at < maintenance_time - interval '13 months'
        ORDER BY bucket_at,node_id,metric LIMIT 1000 FOR UPDATE SKIP LOCKED
    ) DELETE FROM public.telemetry_rollups_1h r USING expired e
      WHERE (r.node_id,r.metric,r.bucket_at)=(e.node_id,e.metric,e.bucket_at);
END;
$$;
REVOKE ALL ON FUNCTION telemetry_prune_rollups(timestamptz) FROM PUBLIC;

CREATE OR REPLACE FUNCTION telemetry_drop_expired_partitions(cutoff timestamptz) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public SET timezone = 'UTC' AS $$
DECLARE
    partition record;
BEGIN
    IF cutoff IS NULL OR NOT isfinite(cutoff)
       OR cutoff > clock_timestamp() - interval '14 days' + interval '5 minutes' THEN
        RAISE EXCEPTION 'telemetry retention cutoff outside permitted window';
    END IF;
    cutoff := least(cutoff,clock_timestamp() - interval '14 days');
    FOR partition IN
        SELECT child.relname FROM pg_catalog.pg_inherits
        JOIN pg_catalog.pg_class parent ON parent.oid=inhparent
        JOIN pg_catalog.pg_namespace parent_namespace ON parent_namespace.oid=parent.relnamespace
        JOIN pg_catalog.pg_class child ON child.oid=inhrelid
        JOIN pg_catalog.pg_namespace child_namespace ON child_namespace.oid=child.relnamespace
        WHERE parent_namespace.nspname='public' AND parent.relname='telemetry_samples'
          AND child_namespace.nspname='public'
          AND child.relname ~ '^telemetry_samples_[0-9]{6}$'
          AND to_timestamp(substring(child.relname from '[0-9]{6}$'),'YYYYMM') + interval '1 month' <= cutoff
        ORDER BY child.relname LIMIT 1
    LOOP
        -- Backfill a whole expired month before dropping it, including when
        -- maintenance was offline longer than the ingestion lateness window.
        EXECUTE format('INSERT INTO public.telemetry_rollups_5m(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) SELECT node_id,metric,date_bin(''5 minutes'',sampled_at,''2000-01-01 00:00:00+00''::timestamptz),count(*),min(value),max(value),avg(value) FROM public.%I WHERE sampled_at >= date_bin(''5 minutes'',clock_timestamp()-interval ''90 days'',''2000-01-01 00:00:00+00''::timestamptz) GROUP BY 1,2,3 ON CONFLICT(node_id,metric,bucket_at) DO UPDATE SET sample_count=EXCLUDED.sample_count,min_value=EXCLUDED.min_value,max_value=EXCLUDED.max_value,avg_value=EXCLUDED.avg_value',partition.relname);
        EXECUTE format('INSERT INTO public.telemetry_rollups_1h(node_id,metric,bucket_at,sample_count,min_value,max_value,avg_value) SELECT node_id,metric,date_bin(''1 hour'',sampled_at,''2000-01-01 00:00:00+00''::timestamptz),count(*),min(value),max(value),avg(value) FROM public.%I WHERE sampled_at >= date_bin(''1 hour'',clock_timestamp()-interval ''13 months'',''2000-01-01 00:00:00+00''::timestamptz) GROUP BY 1,2,3 ON CONFLICT(node_id,metric,bucket_at) DO UPDATE SET sample_count=EXCLUDED.sample_count,min_value=EXCLUDED.min_value,max_value=EXCLUDED.max_value,avg_value=EXCLUDED.avg_value',partition.relname);
        EXECUTE format('DROP TABLE public.%I',partition.relname);
        RETURN 1;
    END LOOP;
    RETURN 0;
END;
$$;
REVOKE ALL ON FUNCTION telemetry_drop_expired_partitions(timestamptz) FROM PUBLIC;

UPDATE controller_schema_compatibility SET "current_schema"=35,minimum_compatible_controller_schema=35 WHERE singleton;
