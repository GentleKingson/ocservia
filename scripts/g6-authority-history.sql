BEGIN;

-- Install and seed the journals without leaving a mutation between the seed
-- snapshot and trigger creation. This lock exists only during harness setup,
-- before worker, scheduler, API, or Agent processes start.
LOCK TABLE public.connection_owner_fencing, public.scheduler_leadership
IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE IF NOT EXISTS public.g6_connection_owner_history (
    history_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    node_id bytea NOT NULL CHECK (octet_length(node_id) = 16),
    owner_instance_id uuid NOT NULL,
    owner_incarnation bigint NOT NULL CHECK (owner_incarnation >= 0),
    connection_id bytea NOT NULL CHECK (octet_length(connection_id) = 16),
    owner_epoch bigint NOT NULL CHECK (owner_epoch >= 1),
    lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS public.g6_scheduler_leadership_history (
    history_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    instance_id uuid NOT NULL,
    incarnation bigint NOT NULL CHECK (incarnation >= 1),
    epoch bigint NOT NULL CHECK (epoch >= 1),
    lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS public.g6_scheduler_maintenance_history (
    maintenance_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    instance_id uuid NOT NULL,
    incarnation bigint NOT NULL CHECK (incarnation >= 1),
    epoch bigint NOT NULL CHECK (epoch >= 1),
    completed_at timestamptz NOT NULL
);

-- These tables are part of the failure-domain evidence source and must follow
-- the primary through physical replication and promotion.
ALTER TABLE public.g6_connection_owner_history SET LOGGED;
ALTER TABLE public.g6_scheduler_leadership_history SET LOGGED;
ALTER TABLE public.g6_scheduler_maintenance_history SET LOGGED;

REVOKE ALL ON TABLE public.g6_connection_owner_history FROM PUBLIC;
REVOKE ALL ON TABLE public.g6_scheduler_leadership_history FROM PUBLIC;
REVOKE ALL ON TABLE public.g6_scheduler_maintenance_history FROM PUBLIC;

CREATE OR REPLACE FUNCTION public.g6_capture_connection_owner_history()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    -- The authoritative node row already serializes terms for that node. Do
    -- not add a global journal lock to the managed-node renewal hot path.
    INSERT INTO public.g6_connection_owner_history (
        node_id,
        owner_instance_id,
        owner_incarnation,
        connection_id,
        owner_epoch,
        lease_until,
        updated_at
    ) VALUES (
        NEW.node_id,
        NEW.owner_instance_id,
        NEW.owner_incarnation,
        NEW.connection_id,
        NEW.owner_epoch,
        NEW.lease_until,
        NEW.updated_at
    );
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION public.g6_capture_scheduler_leadership_history()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    -- The migration's epoch-zero row is a seed, not an acquired authority
    -- term. Only real scheduler epochs belong in the readiness history.
    IF NEW.epoch < 1 THEN
        RETURN NEW;
    END IF;
    INSERT INTO public.g6_scheduler_leadership_history (
        instance_id,
        incarnation,
        epoch,
        lease_until,
        updated_at
    ) VALUES (
        NEW.instance_id,
        NEW.incarnation,
        NEW.epoch,
        NEW.lease_until,
        NEW.updated_at
    );
    RETURN NEW;
END;
$$;

REVOKE ALL ON FUNCTION public.g6_capture_connection_owner_history() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.g6_capture_scheduler_leadership_history() FROM PUBLIC;

-- The scheduler calls this only after its real maintenance body completed.
-- The exact leadership tuple is checked and share-locked in the same
-- transaction as the marker insert, so a stale process cannot manufacture a
-- successful completion and a takeover cannot pass the fence before commit.
CREATE OR REPLACE FUNCTION public.g6_record_scheduler_maintenance(
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

    INSERT INTO public.g6_scheduler_maintenance_history (
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

REVOKE ALL ON FUNCTION public.g6_record_scheduler_maintenance(uuid, bigint, bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.g6_record_scheduler_maintenance(uuid, bigint, bigint) TO ocservia_app;

CREATE OR REPLACE FUNCTION public.g6_reject_authority_history_mutation()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog
AS $$
BEGIN
    RAISE EXCEPTION '% is an append-only G6 evidence journal', TG_TABLE_NAME;
END;
$$;

REVOKE ALL ON FUNCTION public.g6_reject_authority_history_mutation() FROM PUBLIC;

DROP TRIGGER IF EXISTS g6_connection_owner_history_append_only
ON public.g6_connection_owner_history;
CREATE TRIGGER g6_connection_owner_history_append_only
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.g6_connection_owner_history
FOR EACH STATEMENT
EXECUTE FUNCTION public.g6_reject_authority_history_mutation();

DROP TRIGGER IF EXISTS g6_scheduler_history_append_only
ON public.g6_scheduler_leadership_history;
CREATE TRIGGER g6_scheduler_history_append_only
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.g6_scheduler_leadership_history
FOR EACH STATEMENT
EXECUTE FUNCTION public.g6_reject_authority_history_mutation();

DROP TRIGGER IF EXISTS g6_scheduler_maintenance_history_append_only
ON public.g6_scheduler_maintenance_history;
CREATE TRIGGER g6_scheduler_maintenance_history_append_only
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.g6_scheduler_maintenance_history
FOR EACH STATEMENT
EXECUTE FUNCTION public.g6_reject_authority_history_mutation();

-- A normal G6 database has no acquired authority at installation time. Seed
-- nevertheless, under the base-table lock, so a resumed setup cannot create
-- a silent gap between a pre-existing term and the first trigger event.
INSERT INTO public.g6_connection_owner_history (
    node_id,
    owner_instance_id,
    owner_incarnation,
    connection_id,
    owner_epoch,
    lease_until,
    updated_at
)
SELECT
    current.node_id,
    current.owner_instance_id,
    current.owner_incarnation,
    current.connection_id,
    current.owner_epoch,
    current.lease_until,
    current.updated_at
FROM public.connection_owner_fencing AS current
WHERE NOT EXISTS (
    SELECT 1 FROM public.g6_connection_owner_history
);

INSERT INTO public.g6_scheduler_leadership_history (
    instance_id,
    incarnation,
    epoch,
    lease_until,
    updated_at
)
SELECT
    current.instance_id,
    current.incarnation,
    current.epoch,
    current.lease_until,
    current.updated_at
FROM public.scheduler_leadership AS current
WHERE current.id = 1
  AND current.epoch >= 1
  AND NOT EXISTS (
      SELECT 1 FROM public.g6_scheduler_leadership_history
  );

DROP TRIGGER IF EXISTS g6_journal_connection_owner ON public.connection_owner_fencing;
CREATE TRIGGER g6_journal_connection_owner
AFTER INSERT OR UPDATE ON public.connection_owner_fencing
FOR EACH ROW
EXECUTE FUNCTION public.g6_capture_connection_owner_history();

DROP TRIGGER IF EXISTS g6_journal_scheduler_leadership ON public.scheduler_leadership;
CREATE TRIGGER g6_journal_scheduler_leadership
AFTER INSERT OR UPDATE ON public.scheduler_leadership
FOR EACH ROW
EXECUTE FUNCTION public.g6_capture_scheduler_leadership_history();

COMMIT;
