-- Existing default rows remain readable; only the owner may add legacy data.
CREATE FUNCTION telemetry_reject_runtime_default() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
    IF NOT pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid=TG_RELID), 'USAGE') THEN
        RAISE EXCEPTION 'telemetry month is not provisioned' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION telemetry_reject_runtime_default() FROM PUBLIC;
CREATE TRIGGER telemetry_default_owner_only BEFORE INSERT OR UPDATE ON telemetry_samples_default
FOR EACH ROW EXECUTE FUNCTION telemetry_reject_runtime_default();

-- Remove direct legacy grants, including accounts not configured on this run.
DO $$
DECLARE grantee_name text;
BEGIN
    FOR grantee_name IN
        SELECT r.rolname FROM pg_proc p CROSS JOIN LATERAL aclexplode(p.proacl) a
        JOIN pg_roles r ON r.oid=a.grantee
        WHERE p.oid='telemetry_ensure_month_partition(timestamptz)'::regprocedure
          AND a.grantee<>p.proowner
    LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION telemetry_ensure_month_partition(timestamptz) FROM %I',grantee_name);
    END LOOP;
END;
$$;
REVOKE ALL ON FUNCTION telemetry_ensure_month_partition(timestamptz) FROM PUBLIC;
ALTER FUNCTION telemetry_ensure_month_partition(timestamptz) SET timezone='UTC';
UPDATE controller_schema_compatibility SET "current_schema"=36,minimum_compatible_controller_schema=36 WHERE singleton;
