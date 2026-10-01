-- Test-only completion marker for startup, role and independent Controller
-- Rebind checks. Installed by the test owner after schema validation.
CREATE TABLE IF NOT EXISTS public.test_scheduler_maintenance_history (
    maintenance_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    instance_id uuid NOT NULL,
    incarnation bigint NOT NULL CHECK (incarnation >= 1),
    epoch bigint NOT NULL CHECK (epoch >= 1),
    completed_at timestamptz NOT NULL
);

REVOKE ALL ON TABLE public.test_scheduler_maintenance_history FROM PUBLIC;

CREATE OR REPLACE FUNCTION public.test_record_scheduler_maintenance(
    requested_instance_id uuid,
    requested_incarnation bigint,
    requested_epoch bigint
)
RETURNS bigint
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    recorded_id bigint;
BEGIN
    PERFORM 1
    FROM public.scheduler_leadership AS leadership
    WHERE leadership.id = 1
      AND leadership.instance_id = requested_instance_id
      AND leadership.incarnation = requested_incarnation
      AND leadership.epoch = requested_epoch
      AND leadership.lease_until > pg_catalog.clock_timestamp()
    FOR SHARE OF leadership;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'scheduler maintenance term is not the exact live leader'
            USING ERRCODE = '55000';
    END IF;

    INSERT INTO public.test_scheduler_maintenance_history (
        instance_id,
        incarnation,
        epoch,
        completed_at
    ) VALUES (
        requested_instance_id,
        requested_incarnation,
        requested_epoch,
        pg_catalog.clock_timestamp()
    )
    RETURNING maintenance_id INTO recorded_id;
    RETURN recorded_id;
END;
$$;

REVOKE ALL ON FUNCTION public.test_record_scheduler_maintenance(uuid, bigint, bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.test_record_scheduler_maintenance(uuid, bigint, bigint) TO ocservia_app;

