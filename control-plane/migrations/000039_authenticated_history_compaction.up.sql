ALTER TABLE audit_events
 ADD COLUMN details_compacted_at timestamptz,
 ADD COLUMN compaction_key_id text,
 ADD COLUMN compaction_mac bytea,
 ADD CONSTRAINT audit_compaction_evidence CHECK (
  (details_compacted_at IS NULL AND compaction_key_id IS NULL AND compaction_mac IS NULL) OR
  (details_compacted_at IS NOT NULL AND compaction_key_id IS NOT NULL AND compaction_mac IS NOT NULL
   AND length(compaction_key_id) BETWEEN 1 AND 128 AND compaction_key_id ~ '^[A-Za-z0-9._-]+$'
   AND octet_length(compaction_mac)=32 AND auth_version=1
   AND reason IS NULL AND before_summary IS NULL AND after_summary IS NULL));
CREATE INDEX audit_detail_retention_idx ON audit_events(occurred_at,id) WHERE details_compacted_at IS NULL;

CREATE FUNCTION check_audit_compaction() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public SET timezone='UTC' AS $$
BEGIN
 IF TG_OP='UPDATE' THEN
  IF current_user=(SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid=TG_RELID)
   AND OLD.details_compacted_at IS NULL AND OLD.auth_version=1 AND OLD.action<>'audit.auth.transition'
   AND NEW.details_compacted_at IS NOT NULL
   AND OLD.occurred_at<=NEW.details_compacted_at-interval '90 days'
   AND NEW.details_compacted_at<=clock_timestamp()
   AND NEW.reason IS NULL AND NEW.before_summary IS NULL AND NEW.after_summary IS NULL
   AND (to_jsonb(NEW)-ARRAY['reason','before_summary','after_summary','details_compacted_at','compaction_key_id','compaction_mac'])
       =(to_jsonb(OLD)-ARRAY['reason','before_summary','after_summary','details_compacted_at','compaction_key_id','compaction_mac'])
  THEN RETURN NEW; END IF;
 END IF;
 RAISE EXCEPTION 'audit_events permit only authenticated aged detail compaction';
END;
$$;
REVOKE ALL ON FUNCTION check_audit_compaction() FROM PUBLIC;
DROP TRIGGER audit_events_append_only ON audit_events;
CREATE TRIGGER audit_events_append_only BEFORE UPDATE OR DELETE ON audit_events
FOR EACH ROW EXECUTE FUNCTION check_audit_compaction();
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
FOR EACH STATEMENT EXECUTE FUNCTION reject_audit_event_mutation();

CREATE FUNCTION audit_compact_detail(p_id uuid,p_hash bytea,p_key text,p_mac bytea,p_at timestamptz) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public SET timezone='UTC' AS $$
BEGIN
 UPDATE public.audit_events SET reason=NULL,before_summary=NULL,after_summary=NULL,
  details_compacted_at=p_at,compaction_key_id=p_key,compaction_mac=p_mac
 WHERE id=p_id AND event_hash=p_hash AND details_compacted_at IS NULL
  AND auth_version=1 AND action<>'audit.auth.transition'
  AND occurred_at<=p_at-interval '90 days' AND p_at<=clock_timestamp();
 RETURN FOUND;
END;
$$;
REVOKE ALL ON FUNCTION audit_compact_detail(uuid,bytea,text,bytea,timestamptz) FROM PUBLIC;

ALTER TABLE telemetry_security_events ADD COLUMN details_compacted_at timestamptz,
 ADD COLUMN detail_sha256 bytea,
 ADD CONSTRAINT security_detail_evidence CHECK (
  (details_compacted_at IS NULL AND detail_sha256 IS NULL) OR
  (details_compacted_at IS NOT NULL AND detail_sha256 IS NOT NULL AND octet_length(detail_sha256)=32 AND detail='{}'));
CREATE INDEX security_detail_retention_idx ON telemetry_security_events(observed_at,event_id) WHERE details_compacted_at IS NULL;
CREATE FUNCTION security_compact_details(p_cutoff timestamptz) RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public SET timezone='UTC' AS $$
DECLARE changed bigint;
BEGIN
 WITH batch AS (
  SELECT event_id FROM public.telemetry_security_events
  WHERE details_compacted_at IS NULL AND observed_at<LEAST(p_cutoff,clock_timestamp()-interval '90 days')
  ORDER BY observed_at,event_id LIMIT 32 FOR UPDATE SKIP LOCKED
 ) UPDATE public.telemetry_security_events e
 SET detail_sha256=sha256(convert_to(e.detail::text,'UTF8')),detail='{}',details_compacted_at=clock_timestamp()
 FROM batch b WHERE e.event_id=b.event_id;
 GET DIAGNOSTICS changed=ROW_COUNT;
 RETURN changed;
END;
$$;
REVOKE ALL ON FUNCTION security_compact_details(timestamptz) FROM PUBLIC;
UPDATE controller_schema_compatibility SET "current_schema"=39,minimum_compatible_controller_schema=39 WHERE singleton;
