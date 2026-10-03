SET LOCAL statement_timeout = 0;
SET LOCAL lock_timeout = 0;
SET LOCAL idle_in_transaction_session_timeout = 0;
SET LOCAL transaction_timeout = 0;
SET LOCAL client_encoding = 'UTF8';
SET LOCAL standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', true);
SET LOCAL check_function_bodies = false;
SET LOCAL xmloption = content;
SET LOCAL client_min_messages = warning;
SET LOCAL row_security = off;

CREATE FUNCTION public.audit_compact_detail(p_id uuid, p_hash bytea, p_key text, p_mac bytea, p_at timestamp with time zone) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    SET "TimeZone" TO 'UTC'
    AS $$
BEGIN
 UPDATE public.audit_events SET reason=NULL,before_summary=NULL,after_summary=NULL,
  details_compacted_at=p_at,compaction_key_id=p_key,compaction_mac=p_mac
 WHERE id=p_id AND event_hash=p_hash AND details_compacted_at IS NULL
  AND auth_version=1 AND action<>'audit.auth.transition'
  AND occurred_at<=p_at-interval '90 days' AND p_at<=clock_timestamp();
 RETURN FOUND;
END;
$$;

CREATE FUNCTION public.check_audit_compaction() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
    SET "TimeZone" TO 'UTC'
    AS $$
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

CREATE FUNCTION public.reject_audit_checkpoint_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'audit_checkpoints are append-only';
END;
$$;

CREATE FUNCTION public.reject_audit_event_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$;

CREATE FUNCTION public.security_compact_details(p_cutoff timestamp with time zone) RETURNS bigint
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    SET "TimeZone" TO 'UTC'
    AS $$
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

CREATE FUNCTION public.telemetry_drop_expired_partitions(cutoff timestamp with time zone) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    SET "TimeZone" TO 'UTC'
    AS $_$
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
$_$;

CREATE FUNCTION public.telemetry_ensure_month_partition(sample_time timestamp with time zone) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    SET "TimeZone" TO 'UTC'
    AS $$
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

CREATE FUNCTION public.telemetry_prune_rollups(maintenance_time timestamp with time zone) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    SET "TimeZone" TO 'UTC'
    AS $$
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

CREATE FUNCTION public.telemetry_reject_runtime_default() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
    IF NOT pg_has_role(current_user, (SELECT relowner FROM pg_class WHERE oid=TG_RELID), 'USAGE') THEN
        RAISE EXCEPTION 'telemetry month is not provisioned' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;

SET LOCAL default_tablespace = '';

SET LOCAL default_table_access_method = heap;

CREATE TABLE public.agent_command_results (
    event_id uuid CONSTRAINT "agent_command_results_event_id_not_null" NOT NULL,
    command_id uuid CONSTRAINT "agent_command_results_command_id_not_null" NOT NULL,
    idempotency_key uuid CONSTRAINT "agent_command_results_idempotency_key_not_null" NOT NULL,
    payload_sha256 bytea,
    state text CONSTRAINT "agent_command_results_state_not_null" NOT NULL,
    result bytea CONSTRAINT "agent_command_results_result_not_null" NOT NULL,
    error_code text,
    accepted_at timestamp with time zone,
    completed_at timestamp with time zone CONSTRAINT "agent_command_results_completed_at_not_null" NOT NULL,
    replayed boolean CONSTRAINT "agent_command_results_replayed_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "agent_command_results_created_at_not_null" NOT NULL,
    semantic_payload_hash_version smallint DEFAULT 0 CONSTRAINT "agent_command_results_semantic_payload_hash_version_not_null" NOT NULL,
    receipt_verification_status text DEFAULT 'legacy'::text CONSTRAINT "agent_command_results_receipt_verification_status_not_null" NOT NULL,
    receipt_failure_reason text,
    privd_attestation_key_id text,
    effect_record_id bytea,
    effect_sequence bigint,
    receipt_sha256 bytea,
    privileged_result_proof bytea,
    CONSTRAINT agent_command_results_check CHECK (((accepted_at IS NULL) OR (accepted_at <= completed_at))),
    CONSTRAINT agent_command_results_check1 CHECK ((((state = 'succeeded'::text) AND (payload_sha256 IS NOT NULL) AND (accepted_at IS NOT NULL) AND (error_code IS NULL)) OR ((state = 'failed'::text) AND (payload_sha256 IS NOT NULL) AND (accepted_at IS NOT NULL) AND (error_code IS NOT NULL)) OR ((state = 'unknown'::text) AND (payload_sha256 IS NOT NULL) AND (accepted_at IS NOT NULL) AND (error_code IS NOT NULL) AND (octet_length(result) = 0)) OR ((state = 'rejected'::text) AND (accepted_at IS NULL) AND (error_code IS NOT NULL) AND (octet_length(result) = 0)))),
    CONSTRAINT agent_command_results_error_code_check CHECK (((error_code IS NULL) OR ((length(error_code) >= 1) AND (length(error_code) <= 128)))),
    CONSTRAINT agent_command_results_payload_sha256_check CHECK (((payload_sha256 IS NULL) OR (octet_length(payload_sha256) = 32))),
    CONSTRAINT agent_command_results_receipt_failure_reason_check CHECK (((receipt_failure_reason IS NULL) OR ((length(receipt_failure_reason) >= 1) AND (length(receipt_failure_reason) <= 64)))),
    CONSTRAINT agent_command_results_receipt_fields_check CHECK ((((receipt_verification_status = 'verified'::text) AND (receipt_failure_reason IS NULL) AND (privd_attestation_key_id IS NOT NULL) AND (effect_record_id IS NOT NULL) AND ((octet_length(effect_record_id) >= 16) AND (octet_length(effect_record_id) <= 32)) AND (effect_sequence > 0) AND (octet_length(receipt_sha256) = 32) AND ((octet_length(privileged_result_proof) >= 1) AND (octet_length(privileged_result_proof) <= 65536))) OR (receipt_verification_status <> 'verified'::text))),
    CONSTRAINT agent_command_results_receipt_verification_status_check CHECK ((receipt_verification_status = ANY (ARRAY['legacy'::text, 'not_required'::text, 'verified'::text, 'missing'::text, 'invalid'::text, 'unknown_key'::text, 'revoked_key'::text]))),
    CONSTRAINT agent_command_results_result_check CHECK ((octet_length(result) <= 1048576)),
    CONSTRAINT agent_command_results_semantic_payload_hash_version_supported CHECK ((semantic_payload_hash_version = ANY (ARRAY[0, 1, 2]))),
    CONSTRAINT agent_command_results_state_check CHECK ((state = ANY (ARRAY['succeeded'::text, 'failed'::text, 'unknown'::text, 'rejected'::text])))
);

COMMENT ON TABLE public.agent_command_results IS 'Durable Agent journal results; unknown outcomes require observation before retry.';

COMMENT ON COLUMN public.agent_command_results.semantic_payload_hash_version IS 'Algorithm version that produced payload_sha256 (0 = legacy, 1 = frozen v1, 2 = session-authority v2).';

COMMENT ON COLUMN public.agent_command_results.receipt_verification_status IS 'Fail-closed Controller verification outcome for privileged terminal result evidence.';

CREATE TABLE public.agent_rollout_nodes (
    rollout_id uuid CONSTRAINT "agent_rollout_nodes_rollout_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "agent_rollout_nodes_node_id_not_null" NOT NULL,
    ordinal integer CONSTRAINT "agent_rollout_nodes_ordinal_not_null" NOT NULL,
    batch integer CONSTRAINT "agent_rollout_nodes_batch_not_null" NOT NULL,
    state text DEFAULT 'pending'::text CONSTRAINT "agent_rollout_nodes_state_not_null" NOT NULL,
    operation_id uuid,
    from_version text DEFAULT ''::text CONSTRAINT "agent_rollout_nodes_from_version_not_null" NOT NULL,
    failure_code text DEFAULT ''::text CONSTRAINT "agent_rollout_nodes_failure_code_not_null" NOT NULL,
    dispatch_node_version bigint,
    dispatch_attempt integer DEFAULT 0 CONSTRAINT "agent_rollout_nodes_dispatch_attempt_not_null" NOT NULL,
    dispatch_lease_until timestamp with time zone,
    updated_at timestamp with time zone CONSTRAINT "agent_rollout_nodes_updated_at_not_null" NOT NULL,
    CONSTRAINT agent_rollout_nodes_batch_check CHECK ((batch >= 0)),
    CONSTRAINT agent_rollout_nodes_dispatch_attempt_check CHECK ((dispatch_attempt >= 0)),
    CONSTRAINT agent_rollout_nodes_failure_code_check CHECK ((length(failure_code) <= 128)),
    CONSTRAINT agent_rollout_nodes_ordinal_check CHECK ((ordinal >= 0)),
    CONSTRAINT agent_rollout_nodes_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'rolled_back'::text, 'unknown'::text, 'skipped'::text])))
);

COMMENT ON TABLE public.agent_rollout_nodes IS 'Per-node rollout projection; each upgrade reuses the reconciled single-node operation via operation_id.';

CREATE TABLE public.agent_rollouts (
    id uuid CONSTRAINT "agent_rollouts_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "agent_rollouts_workspace_id_not_null" NOT NULL,
    target_version text CONSTRAINT "agent_rollouts_target_version_not_null" NOT NULL,
    state text DEFAULT 'queued'::text CONSTRAINT "agent_rollouts_state_not_null" NOT NULL,
    batch_size integer CONSTRAINT "agent_rollouts_batch_size_not_null" NOT NULL,
    stop_on_failure boolean DEFAULT true CONSTRAINT "agent_rollouts_stop_on_failure_not_null" NOT NULL,
    reason text CONSTRAINT "agent_rollouts_reason_not_null" NOT NULL,
    approval_id uuid CONSTRAINT "agent_rollouts_approval_id_not_null" NOT NULL,
    request_hash bytea CONSTRAINT "agent_rollouts_request_hash_not_null" NOT NULL,
    created_by uuid CONSTRAINT "agent_rollouts_created_by_not_null" NOT NULL,
    actor_session_id uuid CONSTRAINT "agent_rollouts_actor_session_id_not_null" NOT NULL,
    current_batch integer DEFAULT 0 CONSTRAINT "agent_rollouts_current_batch_not_null" NOT NULL,
    pause_code text DEFAULT ''::text CONSTRAINT "agent_rollouts_pause_code_not_null" NOT NULL,
    exclusions jsonb DEFAULT '[]'::jsonb CONSTRAINT "agent_rollouts_exclusions_not_null" NOT NULL,
    idempotency_key text CONSTRAINT "agent_rollouts_idempotency_key_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "agent_rollouts_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "agent_rollouts_updated_at_not_null" NOT NULL,
    CONSTRAINT agent_rollouts_batch_size_check CHECK (((batch_size >= 1) AND (batch_size <= 20))),
    CONSTRAINT agent_rollouts_current_batch_check CHECK ((current_batch >= 0)),
    CONSTRAINT agent_rollouts_exclusions_check CHECK (((jsonb_typeof(exclusions) = 'array'::text) AND (jsonb_array_length(exclusions) <= 500))),
    CONSTRAINT agent_rollouts_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 128))),
    CONSTRAINT agent_rollouts_pause_code_check CHECK ((length(pause_code) <= 128)),
    CONSTRAINT agent_rollouts_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 512))),
    CONSTRAINT agent_rollouts_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT agent_rollouts_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'running'::text, 'paused'::text, 'succeeded'::text, 'failed'::text, 'cancelled'::text])))
);

COMMENT ON TABLE public.agent_rollouts IS 'Durable canary and rolling agent upgrade orchestration; state survives Controller restart and browser closure.';

CREATE TABLE public.agent_upgrade_operations (
    operation_id uuid CONSTRAINT "agent_upgrade_operations_operation_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "agent_upgrade_operations_workspace_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "agent_upgrade_operations_node_id_not_null" NOT NULL,
    target_version text CONSTRAINT "agent_upgrade_operations_target_version_not_null" NOT NULL,
    package_sha256 bytea CONSTRAINT "agent_upgrade_operations_package_sha256_not_null" NOT NULL,
    architecture text CONSTRAINT "agent_upgrade_operations_architecture_not_null" NOT NULL,
    from_version text DEFAULT ''::text CONSTRAINT "agent_upgrade_operations_from_version_not_null" NOT NULL,
    approval_id uuid,
    state text CONSTRAINT "agent_upgrade_operations_state_not_null" NOT NULL,
    scheduled_at timestamp with time zone,
    completed_at timestamp with time zone,
    created_at timestamp with time zone CONSTRAINT "agent_upgrade_operations_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "agent_upgrade_operations_updated_at_not_null" NOT NULL,
    CONSTRAINT agent_upgrade_operations_architecture_check CHECK ((architecture = ANY (ARRAY['amd64'::text, 'arm64'::text]))),
    CONSTRAINT agent_upgrade_operations_package_sha256_check CHECK ((octet_length(package_sha256) = 32)),
    CONSTRAINT agent_upgrade_operations_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'accepted'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'rolled_back'::text, 'unknown'::text])))
);

COMMENT ON TABLE public.agent_upgrade_operations IS 'Per-operation projection for reconciled single-node agent upgrades; mirrors the operations lifecycle.';

CREATE TABLE public.approval_authority_resources (
    approval_id uuid CONSTRAINT "approval_authority_resources_approval_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "approval_authority_resources_workspace_id_not_null" NOT NULL,
    resource_type text CONSTRAINT "approval_authority_resources_resource_type_not_null" NOT NULL,
    resource_id uuid CONSTRAINT "approval_authority_resources_resource_id_not_null" NOT NULL,
    CONSTRAINT approval_authority_resources_resource_type_check CHECK ((resource_type = ANY (ARRAY['workspace'::text, 'node'::text, 'resource'::text, 'secret_ref'::text, 'certificate'::text, 'config_plan'::text, 'batch_operation'::text, 'role_binding'::text])))
);

CREATE TABLE public.approval_batch_items (
    approval_id uuid CONSTRAINT "approval_batch_items_approval_id_not_null" NOT NULL,
    item_index integer CONSTRAINT "approval_batch_items_item_index_not_null" NOT NULL,
    node_id uuid CONSTRAINT "approval_batch_items_node_id_not_null" NOT NULL,
    username text CONSTRAINT "approval_batch_items_username_not_null" NOT NULL,
    action text CONSTRAINT "approval_batch_items_action_not_null" NOT NULL,
    expected_version bigint CONSTRAINT "approval_batch_items_expected_version_not_null" NOT NULL,
    CONSTRAINT approval_batch_items_action_check CHECK ((action = ANY (ARRAY['disable'::text, 'enable'::text]))),
    CONSTRAINT approval_batch_items_expected_version_check CHECK ((expected_version > 0)),
    CONSTRAINT approval_batch_items_item_index_check CHECK ((item_index >= 0)),
    CONSTRAINT approval_batch_items_username_check CHECK ((username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'::text))
);

CREATE TABLE public.approval_requests (
    id uuid CONSTRAINT "approval_requests_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "approval_requests_workspace_id_not_null" NOT NULL,
    requester_id uuid CONSTRAINT "approval_requests_requester_id_not_null" NOT NULL,
    action text CONSTRAINT "approval_requests_action_not_null" NOT NULL,
    resource_type text CONSTRAINT "approval_requests_resource_type_not_null" NOT NULL,
    resource_id uuid CONSTRAINT "approval_requests_resource_id_not_null" NOT NULL,
    reason text CONSTRAINT "approval_requests_reason_not_null" NOT NULL,
    status text CONSTRAINT "approval_requests_status_not_null" NOT NULL,
    approver_id uuid,
    approval_reason text,
    expires_at timestamp with time zone CONSTRAINT "approval_requests_expires_at_not_null" NOT NULL,
    approved_at timestamp with time zone,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone CONSTRAINT "approval_requests_created_at_not_null" NOT NULL,
    request_hash bytea,
    request_summary jsonb,
    authority_snapshot_at timestamp with time zone DEFAULT now() CONSTRAINT "approval_requests_authority_snapshot_at_not_null" NOT NULL,
    CONSTRAINT approval_request_content_pair CHECK (((request_hash IS NULL) = (request_summary IS NULL))),
    CONSTRAINT approval_requests_action_check CHECK (((length(action) >= 1) AND (length(action) <= 128))),
    CONSTRAINT approval_requests_check CHECK ((requester_id IS DISTINCT FROM approver_id)),
    CONSTRAINT approval_requests_check1 CHECK (((status = ANY (ARRAY['approved'::text, 'consumed'::text])) = (approver_id IS NOT NULL))),
    CONSTRAINT approval_requests_check2 CHECK (((status = 'consumed'::text) = (consumed_at IS NOT NULL))),
    CONSTRAINT approval_requests_check3 CHECK ((expires_at > created_at)),
    CONSTRAINT approval_requests_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 512))),
    CONSTRAINT approval_requests_request_hash_check CHECK (((request_hash IS NULL) OR (octet_length(request_hash) = 32))),
    CONSTRAINT approval_requests_request_summary_check CHECK (((request_summary IS NULL) OR (jsonb_typeof(request_summary) = ANY (ARRAY['array'::text, 'object'::text])))),
    CONSTRAINT approval_requests_resource_type_check CHECK (((length(resource_type) >= 1) AND (length(resource_type) <= 64))),
    CONSTRAINT approval_requests_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'rejected'::text, 'expired'::text, 'consumed'::text])))
);

CREATE TABLE public.artifact_operations (
    id uuid CONSTRAINT "artifact_operations_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "artifact_operations_workspace_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "artifact_operations_node_id_not_null" NOT NULL,
    certificate_id uuid CONSTRAINT "artifact_operations_certificate_id_not_null" NOT NULL,
    operation_id uuid CONSTRAINT "artifact_operations_operation_id_not_null" NOT NULL,
    purpose text CONSTRAINT "artifact_operations_purpose_not_null" NOT NULL,
    state text CONSTRAINT "artifact_operations_state_not_null" NOT NULL,
    content_sha256 bytea,
    content_size bigint,
    token_sha256 bytea CONSTRAINT "artifact_operations_token_sha256_not_null" NOT NULL,
    request_hash bytea CONSTRAINT "artifact_operations_request_hash_not_null" NOT NULL,
    expires_at timestamp with time zone CONSTRAINT "artifact_operations_expires_at_not_null" NOT NULL,
    lease_until timestamp with time zone,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone CONSTRAINT "artifact_operations_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "artifact_operations_updated_at_not_null" NOT NULL,
    approval_id uuid,
    certificate_version bigint CONSTRAINT "artifact_operations_certificate_version_not_null" NOT NULL,
    active_grant_id uuid,
    active_grant_subject text,
    active_grant_expires_at timestamp with time zone,
    consume_grant bytea,
    consume_sha256 bytea,
    consume_size bigint,
    consume_actor_id uuid,
    consume_session_id uuid,
    consume_request_id text,
    CONSTRAINT artifact_operations_certificate_version_check CHECK ((certificate_version > 0)),
    CONSTRAINT artifact_operations_check CHECK ((((state = 'ready'::text) AND (content_sha256 IS NOT NULL) AND (content_size IS NOT NULL)) OR (state <> 'ready'::text))),
    CONSTRAINT artifact_operations_consume_check CHECK ((((state = 'consuming'::text) AND (consume_grant IS NOT NULL) AND ((octet_length(consume_grant) >= 1) AND (octet_length(consume_grant) <= 4096)) AND (consume_sha256 IS NOT NULL) AND (octet_length(consume_sha256) = 32) AND (consume_size > 0) AND (consume_actor_id IS NOT NULL) AND (consume_session_id IS NOT NULL) AND (consume_request_id IS NOT NULL) AND ((length(consume_request_id) >= 1) AND (length(consume_request_id) <= 128))) OR (state <> 'consuming'::text))),
    CONSTRAINT artifact_operations_content_sha256_check CHECK (((content_sha256 IS NULL) OR (octet_length(content_sha256) = 32))),
    CONSTRAINT artifact_operations_content_size_check CHECK (((content_size IS NULL) OR ((content_size >= 1) AND (content_size <= 67108864)))),
    CONSTRAINT artifact_operations_grant_check CHECK ((((state = 'leased'::text) AND (active_grant_id IS NOT NULL) AND (active_grant_subject IS NOT NULL) AND (active_grant_expires_at IS NOT NULL)) OR (state <> 'leased'::text))),
    CONSTRAINT artifact_operations_purpose_check CHECK ((purpose = 'certificate_p12'::text)),
    CONSTRAINT artifact_operations_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT artifact_operations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'ready'::text, 'leased'::text, 'consuming'::text, 'consumed'::text, 'expired'::text, 'revoked'::text, 'failed'::text]))),
    CONSTRAINT artifact_operations_token_sha256_check CHECK ((octet_length(token_sha256) = 32))
);

COMMENT ON COLUMN public.artifact_operations.certificate_version IS 'Certificate version bound into the Controller-signed ArtifactGrantV1 and local artifact evidence.';

CREATE TABLE public.audit_checkpoints (
    id uuid CONSTRAINT "audit_checkpoints_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "audit_checkpoints_workspace_id_not_null" NOT NULL,
    through_event_id uuid CONSTRAINT "audit_checkpoints_through_event_id_not_null" NOT NULL,
    through_event_hash bytea CONSTRAINT "audit_checkpoints_through_event_hash_not_null" NOT NULL,
    signature bytea CONSTRAINT "audit_checkpoints_signature_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "audit_checkpoints_created_at_not_null" NOT NULL,
    CONSTRAINT audit_checkpoints_signature_check CHECK ((octet_length(signature) = 32)),
    CONSTRAINT audit_checkpoints_through_event_hash_check CHECK ((octet_length(through_event_hash) = 32))
);

CREATE TABLE public.audit_events (
    id uuid CONSTRAINT "audit_events_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "audit_events_workspace_id_not_null" NOT NULL,
    occurred_at timestamp with time zone CONSTRAINT "audit_events_occurred_at_not_null" NOT NULL,
    actor_type text CONSTRAINT "audit_events_actor_type_not_null" NOT NULL,
    actor_id text CONSTRAINT "audit_events_actor_id_not_null" NOT NULL,
    action text CONSTRAINT "audit_events_action_not_null" NOT NULL,
    resource_type text CONSTRAINT "audit_events_resource_type_not_null" NOT NULL,
    resource_id uuid,
    request_id text CONSTRAINT "audit_events_request_id_not_null" NOT NULL,
    trace_id text,
    result text CONSTRAINT "audit_events_result_not_null" NOT NULL,
    reason text,
    previous_event_hash bytea,
    event_hash bytea CONSTRAINT "audit_events_event_hash_not_null" NOT NULL,
    source_session_id uuid,
    node_id uuid,
    command_id uuid,
    approval_id uuid,
    before_summary jsonb,
    after_summary jsonb,
    error_type text,
    auth_version smallint DEFAULT 0 CONSTRAINT "audit_events_auth_version_not_null" NOT NULL,
    event_key_id text,
    event_mac bytea,
    details_compacted_at timestamp with time zone,
    compaction_key_id text,
    compaction_mac bytea,
    CONSTRAINT audit_compaction_evidence CHECK ((((details_compacted_at IS NULL) AND (compaction_key_id IS NULL) AND (compaction_mac IS NULL)) OR ((details_compacted_at IS NOT NULL) AND (compaction_key_id IS NOT NULL) AND (compaction_mac IS NOT NULL) AND ((length(compaction_key_id) >= 1) AND (length(compaction_key_id) <= 128)) AND (compaction_key_id ~ '^[A-Za-z0-9._-]+$'::text) AND (octet_length(compaction_mac) = 32) AND (auth_version = 1) AND (reason IS NULL) AND (before_summary IS NULL) AND (after_summary IS NULL)))),
    CONSTRAINT audit_event_auth_fields CHECK ((((auth_version = 0) AND (event_key_id IS NULL) AND (event_mac IS NULL)) OR ((auth_version = 1) AND ((length(event_key_id) >= 1) AND (length(event_key_id) <= 128)) AND (event_key_id ~ '^[A-Za-z0-9._-]+$'::text) AND (octet_length(event_mac) = 32)))),
    CONSTRAINT audit_event_auth_version CHECK ((auth_version = ANY (ARRAY[0, 1]))),
    CONSTRAINT audit_event_hash_size CHECK ((octet_length(event_hash) = 32)),
    CONSTRAINT audit_events_result_check CHECK ((result = ANY (ARRAY['intent'::text, 'succeeded'::text, 'failed'::text]))),
    CONSTRAINT audit_previous_hash_size CHECK (((previous_event_hash IS NULL) OR (octet_length(previous_event_hash) = 32)))
);

COMMENT ON TABLE public.audit_events IS 'Append-only security audit records; secrets and full request bodies are forbidden.';

COMMENT ON COLUMN public.audit_events.auth_version IS 'Version 0 marks checkpoint-anchored legacy rows; version 1 requires application-origin HMAC authentication.';

COMMENT ON COLUMN public.audit_events.event_key_id IS 'Identifier of the Controller audit-event HMAC key; never contains key material.';

COMMENT ON COLUMN public.audit_events.event_mac IS 'HMAC-SHA256 over the domain-separated event_hash using the Controller audit-event key.';

CREATE TABLE public.auth_sessions (
    id uuid CONSTRAINT "auth_sessions_id_not_null" NOT NULL,
    identity_id uuid CONSTRAINT "auth_sessions_identity_id_not_null" NOT NULL,
    expires_at timestamp with time zone CONSTRAINT "auth_sessions_expires_at_not_null" NOT NULL,
    revoked_at timestamp with time zone,
    break_glass boolean DEFAULT false CONSTRAINT "auth_sessions_break_glass_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "auth_sessions_created_at_not_null" NOT NULL,
    CONSTRAINT auth_sessions_check CHECK ((expires_at > created_at))
);

CREATE TABLE public.batch_operation_items (
    batch_id uuid CONSTRAINT "batch_operation_items_batch_id_not_null" NOT NULL,
    item_index integer CONSTRAINT "batch_operation_items_item_index_not_null" NOT NULL,
    node_id uuid CONSTRAINT "batch_operation_items_node_id_not_null" NOT NULL,
    username text CONSTRAINT "batch_operation_items_username_not_null" NOT NULL,
    action text CONSTRAINT "batch_operation_items_action_not_null" NOT NULL,
    expected_version bigint CONSTRAINT "batch_operation_items_expected_version_not_null" NOT NULL,
    state text CONSTRAINT "batch_operation_items_state_not_null" NOT NULL,
    child_operation_id uuid,
    error_type text,
    lease_owner uuid,
    lease_until timestamp with time zone,
    updated_at timestamp with time zone CONSTRAINT "batch_operation_items_updated_at_not_null" NOT NULL,
    CONSTRAINT batch_operation_items_action_check CHECK ((action = ANY (ARRAY['disable'::text, 'enable'::text]))),
    CONSTRAINT batch_operation_items_check CHECK (((lease_owner IS NULL) = (lease_until IS NULL))),
    CONSTRAINT batch_operation_items_expected_version_check CHECK ((expected_version > 0)),
    CONSTRAINT batch_operation_items_item_index_check CHECK ((item_index >= 0)),
    CONSTRAINT batch_operation_items_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'submitting'::text, 'submitted'::text, 'succeeded'::text, 'failed'::text, 'unknown'::text, 'offline_pending'::text, 'forbidden'::text]))),
    CONSTRAINT batch_operation_items_username_check CHECK ((username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'::text))
);

COMMENT ON TABLE public.batch_operation_items IS 'Each item is authorized independently and owns a distinct child operation and command.';

CREATE TABLE public.batch_operations (
    id uuid CONSTRAINT "batch_operations_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "batch_operations_workspace_id_not_null" NOT NULL,
    state text CONSTRAINT "batch_operations_state_not_null" NOT NULL,
    actor_identity_id uuid,
    actor_session_id uuid,
    approval_id uuid,
    actor_id text CONSTRAINT "batch_operations_actor_id_not_null" NOT NULL,
    reason text CONSTRAINT "batch_operations_reason_not_null" NOT NULL,
    request_id text CONSTRAINT "batch_operations_request_id_not_null" NOT NULL,
    traceparent text CONSTRAINT "batch_operations_traceparent_not_null" NOT NULL,
    idempotency_key text CONSTRAINT "batch_operations_idempotency_key_not_null" NOT NULL,
    request_hash bytea CONSTRAINT "batch_operations_request_hash_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "batch_operations_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "batch_operations_updated_at_not_null" NOT NULL,
    CONSTRAINT batch_operations_actor_id_check CHECK (((length(actor_id) >= 1) AND (length(actor_id) <= 256))),
    CONSTRAINT batch_operations_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 128))),
    CONSTRAINT batch_operations_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 512))),
    CONSTRAINT batch_operations_request_hash_check CHECK ((octet_length(request_hash) = 32)),
    CONSTRAINT batch_operations_request_id_check CHECK (((length(request_id) >= 1) AND (length(request_id) <= 128))),
    CONSTRAINT batch_operations_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'running'::text, 'succeeded'::text, 'partial_failed'::text, 'failed'::text]))),
    CONSTRAINT batch_operations_traceparent_check CHECK ((traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'::text))
);

CREATE TABLE public.break_glass_uses (
    credential_fingerprint bytea CONSTRAINT "break_glass_uses_credential_fingerprint_not_null" NOT NULL,
    identity_id uuid CONSTRAINT "break_glass_uses_identity_id_not_null" NOT NULL,
    used_at timestamp with time zone CONSTRAINT "break_glass_uses_used_at_not_null" NOT NULL,
    source_session_id uuid CONSTRAINT "break_glass_uses_source_session_id_not_null" NOT NULL,
    rotation_required boolean DEFAULT true CONSTRAINT "break_glass_uses_rotation_required_not_null" NOT NULL,
    CONSTRAINT break_glass_uses_credential_fingerprint_check CHECK ((octet_length(credential_fingerprint) = 32))
);

CREATE TABLE public.certificates (
    id uuid CONSTRAINT "certificates_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "certificates_workspace_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "certificates_node_id_not_null" NOT NULL,
    operation_id uuid CONSTRAINT "certificates_operation_id_not_null" NOT NULL,
    common_name text CONSTRAINT "certificates_common_name_not_null" NOT NULL,
    dns_names jsonb DEFAULT '[]'::jsonb CONSTRAINT "certificates_dns_names_not_null" NOT NULL,
    key_bits integer CONSTRAINT "certificates_key_bits_not_null" NOT NULL,
    state text CONSTRAINT "certificates_state_not_null" NOT NULL,
    csr_der bytea,
    public_key_sha256 bytea,
    certificate_chain_pem bytea,
    serial_number text,
    not_before timestamp with time zone,
    not_after timestamp with time zone,
    revoked_at timestamp with time zone,
    revocation_reason text,
    issue_approval_id uuid,
    issue_request_hash bytea,
    issue_actor_identity_id uuid,
    created_at timestamp with time zone CONSTRAINT "certificates_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "certificates_updated_at_not_null" NOT NULL,
    version bigint DEFAULT 1 CONSTRAINT "certificates_version_not_null" NOT NULL,
    csr_receipt_legacy boolean DEFAULT false CONSTRAINT "certificates_csr_receipt_legacy_not_null" NOT NULL,
    csr_receipt_verified_at timestamp with time zone,
    csr_receipt_sha256 bytea,
    csr_privd_attestation_key_id text,
    csr_effect_record_id bytea,
    csr_der_sha256 bytea,
    csr_requested_subject_sha256 bytea,
    issue_certificate_version bigint,
    CONSTRAINT certificates_certificate_chain_pem_check CHECK (((certificate_chain_pem IS NULL) OR ((octet_length(certificate_chain_pem) >= 64) AND (octet_length(certificate_chain_pem) <= 262144)))),
    CONSTRAINT certificates_common_name_check CHECK (((length(common_name) >= 1) AND (length(common_name) <= 253))),
    CONSTRAINT certificates_csr_der_check CHECK (((csr_der IS NULL) OR ((octet_length(csr_der) >= 64) AND (octet_length(csr_der) <= 65536)))),
    CONSTRAINT certificates_csr_receipt_check CHECK ((((state = ANY (ARRAY['csr_ready'::text, 'signing'::text, 'signer_unavailable'::text, 'issued'::text, 'expiring'::text, 'expired'::text, 'revoking'::text, 'revocation_unknown'::text, 'revoked'::text])) AND (csr_receipt_verified_at IS NOT NULL) AND (octet_length(csr_receipt_sha256) = 32) AND (csr_privd_attestation_key_id IS NOT NULL) AND ((octet_length(csr_effect_record_id) >= 16) AND (octet_length(csr_effect_record_id) <= 32)) AND (octet_length(csr_der_sha256) = 32) AND (octet_length(csr_requested_subject_sha256) = 32)) OR csr_receipt_legacy OR (state = ANY (ARRAY['csr_pending'::text, 'failed'::text, 'unknown'::text])))),
    CONSTRAINT certificates_dns_names_check CHECK ((jsonb_typeof(dns_names) = 'array'::text)),
    CONSTRAINT certificates_issue_certificate_version_check CHECK ((issue_certificate_version > 0)),
    CONSTRAINT certificates_issue_request_hash_check CHECK (((issue_request_hash IS NULL) OR (octet_length(issue_request_hash) = 32))),
    CONSTRAINT certificates_key_bits_check CHECK ((key_bits = ANY (ARRAY[2048, 3072, 4096]))),
    CONSTRAINT certificates_public_key_sha256_check CHECK (((public_key_sha256 IS NULL) OR (octet_length(public_key_sha256) = 32))),
    CONSTRAINT certificates_revocation_reason_check CHECK (((revocation_reason IS NULL) OR ((length(revocation_reason) >= 1) AND (length(revocation_reason) <= 128)))),
    CONSTRAINT certificates_serial_number_check CHECK (((serial_number IS NULL) OR ((length(serial_number) >= 1) AND (length(serial_number) <= 128)))),
    CONSTRAINT certificates_state_check CHECK ((state = ANY (ARRAY['csr_pending'::text, 'csr_ready'::text, 'signing'::text, 'signer_unavailable'::text, 'issued'::text, 'expiring'::text, 'expired'::text, 'revoking'::text, 'revocation_unknown'::text, 'revoked'::text, 'failed'::text, 'unknown'::text]))),
    CONSTRAINT certificates_version_check CHECK ((version > 0))
);

CREATE TABLE public.command_attempts (
    id uuid CONSTRAINT "command_attempts_id_not_null" NOT NULL,
    command_id uuid CONSTRAINT "command_attempts_command_id_not_null" NOT NULL,
    outbox_event_id uuid CONSTRAINT "command_attempts_outbox_event_id_not_null" NOT NULL,
    worker_id uuid CONSTRAINT "command_attempts_worker_id_not_null" NOT NULL,
    attempt_number integer CONSTRAINT "command_attempts_attempt_number_not_null" NOT NULL,
    state text CONSTRAINT "command_attempts_state_not_null" NOT NULL,
    started_at timestamp with time zone CONSTRAINT "command_attempts_started_at_not_null" NOT NULL,
    finished_at timestamp with time zone,
    error_code text,
    CONSTRAINT command_attempts_attempt_number_check CHECK ((attempt_number > 0)),
    CONSTRAINT command_attempts_check CHECK (((state = 'sending'::text) = (finished_at IS NULL))),
    CONSTRAINT command_attempts_state_check CHECK ((state = ANY (ARRAY['sending'::text, 'sent'::text, 'failed'::text, 'unknown'::text])))
);

COMMENT ON TABLE public.command_attempts IS 'Immutable dispatch-attempt history; sending attempts become unknown when their lease expires.';

CREATE TABLE public.commands (
    id uuid CONSTRAINT "commands_id_not_null" NOT NULL,
    operation_id uuid CONSTRAINT "commands_operation_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "commands_workspace_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "commands_node_id_not_null" NOT NULL,
    state text CONSTRAINT "commands_state_not_null" NOT NULL,
    payload_type text CONSTRAINT "commands_payload_type_not_null" NOT NULL,
    envelope bytea CONSTRAINT "commands_envelope_not_null" NOT NULL,
    idempotency_key text CONSTRAINT "commands_idempotency_key_not_null" NOT NULL,
    expected_version bigint CONSTRAINT "commands_expected_version_not_null" NOT NULL,
    sequence bigint DEFAULT 1 CONSTRAINT "commands_sequence_not_null" NOT NULL,
    traceparent text CONSTRAINT "commands_traceparent_not_null" NOT NULL,
    expires_at timestamp with time zone CONSTRAINT "commands_expires_at_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "commands_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "commands_updated_at_not_null" NOT NULL,
    resource_type text,
    resource_key text,
    details_compacted_at timestamp with time zone,
    envelope_sha256 bytea,
    CONSTRAINT commands_check CHECK ((expires_at > created_at)),
    CONSTRAINT commands_compaction_evidence CHECK ((((details_compacted_at IS NULL) AND (envelope_sha256 IS NULL)) OR ((details_compacted_at IS NOT NULL) AND (envelope_sha256 IS NOT NULL) AND (octet_length(envelope_sha256) = 32) AND (state = ANY (ARRAY['succeeded'::text, 'failed'::text, 'rejected'::text, 'expired'::text, 'rolled_back'::text, 'superseded'::text]))))),
    CONSTRAINT commands_envelope_check CHECK (((octet_length(envelope) >= 1) AND (octet_length(envelope) <= 1048576))),
    CONSTRAINT commands_expected_version_check CHECK ((expected_version >= 0)),
    CONSTRAINT commands_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 128))),
    CONSTRAINT commands_payload_type_check CHECK ((payload_type = ANY (ARRAY['synthetic_noop'::text, 'synthetic_echo'::text, 'session_disconnect'::text, 'session_terminate'::text, 'ip_ban_remove'::text, 'service_reload'::text, 'user_create'::text, 'user_disable'::text, 'user_enable'::text, 'user_password_rotate'::text, 'group_apply'::text, 'config_plan'::text, 'config_apply'::text, 'certificate_csr'::text, 'certificate_p12'::text, 'certificate_revoke'::text, 'agent_upgrade'::text]))),
    CONSTRAINT commands_resource_identity CHECK ((((resource_type IS NULL) = (resource_key IS NULL)) AND ((resource_type IS NULL) OR (resource_type = ANY (ARRAY['user'::text, 'group'::text]))))),
    CONSTRAINT commands_sequence_check CHECK ((sequence > 0)),
    CONSTRAINT commands_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'dispatched'::text, 'accepted'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'rejected'::text, 'unknown'::text, 'expired'::text, 'rolled_back'::text, 'superseded'::text]))),
    CONSTRAINT commands_traceparent_check CHECK ((traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'::text))
);

COMMENT ON COLUMN public.commands.envelope IS 'Serialized typed command. Password commands may contain only client-sealed ciphertext, never plaintext.';

COMMENT ON COLUMN public.commands.details_compacted_at IS 'Expired terminal detail was compacted; envelope is evidence only, never dispatchable. Identity, idempotency and signed fences remain.';

COMMENT ON CONSTRAINT commands_payload_type_check ON public.commands IS 'Only typed command payloads are dispatchable; raw shell, file, occtl, and systemctl operations are forbidden.';

CREATE TABLE public.config_apply_operations (
    operation_id uuid CONSTRAINT "config_apply_operations_operation_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "config_apply_operations_workspace_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "config_apply_operations_node_id_not_null" NOT NULL,
    plan_id uuid CONSTRAINT "config_apply_operations_plan_id_not_null" NOT NULL,
    approval_id uuid CONSTRAINT "config_apply_operations_approval_id_not_null" NOT NULL,
    expected_revision bigint CONSTRAINT "config_apply_operations_expected_revision_not_null" NOT NULL,
    desired_revision bigint CONSTRAINT "config_apply_operations_desired_revision_not_null" NOT NULL,
    candidate_hash bytea CONSTRAINT "config_apply_operations_candidate_hash_not_null" NOT NULL,
    previous_hash bytea CONSTRAINT "config_apply_operations_previous_hash_not_null" NOT NULL,
    state text CONSTRAINT "config_apply_operations_state_not_null" NOT NULL,
    failure_code text,
    created_at timestamp with time zone CONSTRAINT "config_apply_operations_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "config_apply_operations_updated_at_not_null" NOT NULL,
    CONSTRAINT config_apply_operations_candidate_hash_check CHECK ((octet_length(candidate_hash) = 32)),
    CONSTRAINT config_apply_operations_desired_revision_check CHECK (((expected_revision >= 0) AND (desired_revision > expected_revision))),
    CONSTRAINT config_apply_operations_expected_revision_check CHECK ((expected_revision >= 0)),
    CONSTRAINT config_apply_operations_failure_code_check CHECK (((failure_code IS NULL) OR ((length(failure_code) >= 1) AND (length(failure_code) <= 128)))),
    CONSTRAINT config_apply_operations_previous_hash_check CHECK ((octet_length(previous_hash) = 32)),
    CONSTRAINT config_apply_operations_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'dispatched'::text, 'accepted'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'rolled_back'::text, 'failed_critical'::text, 'unknown'::text, 'expired'::text])))
);

COMMENT ON TABLE public.config_apply_operations IS 'Approved configuration transactions and secret-safe apply/rollback outcomes.';

CREATE TABLE public.config_plans (
    id uuid CONSTRAINT "config_plans_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "config_plans_workspace_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "config_plans_node_id_not_null" NOT NULL,
    operation_id uuid CONSTRAINT "config_plans_operation_id_not_null" NOT NULL,
    template_name text CONSTRAINT "config_plans_template_name_not_null" NOT NULL,
    expected_revision bigint CONSTRAINT "config_plans_expected_revision_not_null" NOT NULL,
    candidate_hash bytea CONSTRAINT "config_plans_candidate_hash_not_null" NOT NULL,
    candidate_redacted text CONSTRAINT "config_plans_candidate_redacted_not_null" NOT NULL,
    warnings jsonb DEFAULT '[]'::jsonb CONSTRAINT "config_plans_warnings_not_null" NOT NULL,
    expires_at timestamp with time zone CONSTRAINT "config_plans_expires_at_not_null" NOT NULL,
    created_by uuid,
    created_at timestamp with time zone CONSTRAINT "config_plans_created_at_not_null" NOT NULL,
    CONSTRAINT config_plans_candidate_hash_check CHECK ((octet_length(candidate_hash) = 32)),
    CONSTRAINT config_plans_candidate_redacted_check CHECK (((octet_length(candidate_redacted) >= 1) AND (octet_length(candidate_redacted) <= 262144))),
    CONSTRAINT config_plans_expected_revision_check CHECK ((expected_revision >= 0)),
    CONSTRAINT config_plans_template_name_check CHECK (((length(template_name) >= 1) AND (length(template_name) <= 128))),
    CONSTRAINT config_plans_warnings_check CHECK ((jsonb_typeof(warnings) = 'array'::text))
);

COMMENT ON TABLE public.config_plans IS 'Immutable, redacted, side-effect-free configuration plan snapshots.';

COMMENT ON COLUMN public.config_plans.candidate_redacted IS 'Rendered candidate with every SecretRef represented by a non-secret placeholder.';

CREATE TABLE public.connection_owner_fencing (
    node_id bytea CONSTRAINT "connection_owner_fencing_node_id_not_null" NOT NULL,
    owner_instance_id uuid CONSTRAINT "connection_owner_fencing_owner_instance_id_not_null" NOT NULL,
    owner_incarnation bigint CONSTRAINT "connection_owner_fencing_owner_incarnation_not_null" NOT NULL,
    connection_id bytea CONSTRAINT "connection_owner_fencing_connection_id_not_null" NOT NULL,
    owner_epoch bigint CONSTRAINT "connection_owner_fencing_owner_epoch_not_null" NOT NULL,
    lease_until timestamp with time zone CONSTRAINT "connection_owner_fencing_lease_until_not_null" NOT NULL,
    updated_at timestamp with time zone DEFAULT now() CONSTRAINT "connection_owner_fencing_updated_at_not_null" NOT NULL,
    CONSTRAINT connection_owner_fencing_connection_id_check CHECK ((octet_length(connection_id) = 16)),
    CONSTRAINT connection_owner_fencing_node_id_check CHECK ((octet_length(node_id) = 16)),
    CONSTRAINT connection_owner_fencing_owner_epoch_check CHECK ((owner_epoch >= 1)),
    CONSTRAINT connection_owner_fencing_owner_incarnation_check CHECK ((owner_incarnation >= 0))
);

CREATE TABLE public.controller_schema_compatibility (
    singleton boolean DEFAULT true CONSTRAINT "controller_schema_compatibility_singleton_not_null" NOT NULL,
    "current_schema" bigint NOT NULL,
    minimum_compatible_controller_schema bigint CONSTRAINT controller_schema_compatibi_minimum_compatible_control_not_null NOT NULL,
    CONSTRAINT controller_schema_compatibil_minimum_compatible_controlle_check CHECK ((minimum_compatible_controller_schema > 0)),
    CONSTRAINT controller_schema_compatibility_check CHECK ((minimum_compatible_controller_schema <= "current_schema")),
    CONSTRAINT controller_schema_compatibility_current_schema_check CHECK (("current_schema" > 0)),
    CONSTRAINT controller_schema_compatibility_singleton_check CHECK (singleton)
);

COMMENT ON TABLE public.controller_schema_compatibility IS 'Authoritative Controller schema compatibility range; not a backup or PITR marker.';

CREATE TABLE public.desired_groups (
    node_id uuid CONSTRAINT "desired_groups_node_id_not_null" NOT NULL,
    group_name text CONSTRAINT "desired_groups_group_name_not_null" NOT NULL,
    members text[] CONSTRAINT "desired_groups_members_not_null" NOT NULL,
    version bigint CONSTRAINT "desired_groups_version_not_null" NOT NULL,
    revision bigint CONSTRAINT "desired_groups_revision_not_null" NOT NULL,
    fingerprint bytea CONSTRAINT "desired_groups_fingerprint_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "desired_groups_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "desired_groups_updated_at_not_null" NOT NULL,
    CONSTRAINT desired_groups_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT desired_groups_group_name_check CHECK ((group_name ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'::text)),
    CONSTRAINT desired_groups_members_check CHECK ((cardinality(members) <= 4096)),
    CONSTRAINT desired_groups_revision_check CHECK ((revision > 0)),
    CONSTRAINT desired_groups_version_check CHECK ((version > 0))
);

CREATE TABLE public.desired_user_policies (
    node_id uuid CONSTRAINT "desired_user_policies_node_id_not_null" NOT NULL,
    username text CONSTRAINT "desired_user_policies_username_not_null" NOT NULL,
    quota_period text CONSTRAINT "desired_user_policies_quota_period_not_null" NOT NULL,
    quota_direction text CONSTRAINT "desired_user_policies_quota_direction_not_null" NOT NULL,
    quota_bytes bigint CONSTRAINT "desired_user_policies_quota_bytes_not_null" NOT NULL,
    expires_at timestamp with time zone,
    version bigint CONSTRAINT "desired_user_policies_version_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "desired_user_policies_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "desired_user_policies_updated_at_not_null" NOT NULL,
    CONSTRAINT desired_user_policies_check CHECK (((quota_period = 'none'::text) = (quota_bytes = 0))),
    CONSTRAINT desired_user_policies_quota_bytes_check CHECK (((quota_bytes >= 0) AND (quota_bytes <= '9007199254740991'::bigint))),
    CONSTRAINT desired_user_policies_quota_direction_check CHECK ((quota_direction = ANY (ARRAY['rx'::text, 'tx'::text, 'rxtx'::text]))),
    CONSTRAINT desired_user_policies_quota_period_check CHECK ((quota_period = ANY (ARRAY['none'::text, 'monthly'::text, 'lifetime'::text]))),
    CONSTRAINT desired_user_policies_username_check CHECK ((username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'::text)),
    CONSTRAINT desired_user_policies_version_check CHECK ((version > 0))
);

COMMENT ON COLUMN public.desired_user_policies.quota_bytes IS 'Integer bytes. Monthly periods reset at 00:00:00 UTC on the first day of each calendar month.';

CREATE TABLE public.desired_users (
    node_id uuid CONSTRAINT "desired_users_node_id_not_null" NOT NULL,
    username text CONSTRAINT "desired_users_username_not_null" NOT NULL,
    enabled boolean CONSTRAINT "desired_users_enabled_not_null" NOT NULL,
    version bigint CONSTRAINT "desired_users_version_not_null" NOT NULL,
    revision bigint CONSTRAINT "desired_users_revision_not_null" NOT NULL,
    fingerprint bytea CONSTRAINT "desired_users_fingerprint_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "desired_users_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "desired_users_updated_at_not_null" NOT NULL,
    CONSTRAINT desired_users_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT desired_users_revision_check CHECK ((revision > 0)),
    CONSTRAINT desired_users_username_check CHECK ((username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'::text)),
    CONSTRAINT desired_users_version_check CHECK ((version > 0))
);

COMMENT ON TABLE public.desired_users IS 'Node-scoped desired user state. Password material is never stored here.';

CREATE TABLE public.enrollment_tokens (
    id uuid CONSTRAINT "enrollment_tokens_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "enrollment_tokens_workspace_id_not_null" NOT NULL,
    token_hash bytea CONSTRAINT "enrollment_tokens_token_hash_not_null" NOT NULL,
    expected_environment text CONSTRAINT "enrollment_tokens_expected_environment_not_null" NOT NULL,
    expected_node_name text,
    expected_endpoint_id bytea,
    expires_at timestamp with time zone CONSTRAINT "enrollment_tokens_expires_at_not_null" NOT NULL,
    consumed_at timestamp with time zone,
    consumed_node_id uuid,
    created_by text CONSTRAINT "enrollment_tokens_created_by_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "enrollment_tokens_created_at_not_null" NOT NULL,
    CONSTRAINT enrollment_tokens_check CHECK ((expires_at > created_at)),
    CONSTRAINT enrollment_tokens_check1 CHECK (((consumed_at IS NULL) = (consumed_node_id IS NULL))),
    CONSTRAINT enrollment_tokens_created_by_check CHECK (((length(created_by) >= 1) AND (length(created_by) <= 256))),
    CONSTRAINT enrollment_tokens_expected_endpoint_id_check CHECK (((expected_endpoint_id IS NULL) OR (octet_length(expected_endpoint_id) = 32))),
    CONSTRAINT enrollment_tokens_expected_environment_check CHECK (((length(expected_environment) >= 1) AND (length(expected_environment) <= 64))),
    CONSTRAINT enrollment_tokens_expected_node_name_check CHECK (((expected_node_name IS NULL) OR ((length(expected_node_name) >= 1) AND (length(expected_node_name) <= 128)))),
    CONSTRAINT enrollment_tokens_token_hash_check CHECK ((octet_length(token_hash) = 32))
);

COMMENT ON COLUMN public.enrollment_tokens.token_hash IS 'SHA-256 digest only; plaintext enrollment tokens must never be persisted.';

CREATE TABLE public.identities (
    id uuid CONSTRAINT "identities_id_not_null" NOT NULL,
    issuer text CONSTRAINT "identities_issuer_not_null" NOT NULL,
    subject text CONSTRAINT "identities_subject_not_null" NOT NULL,
    email text,
    display_name text,
    disabled_at timestamp with time zone,
    created_at timestamp with time zone CONSTRAINT "identities_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "identities_updated_at_not_null" NOT NULL
);

CREATE TABLE public.local_auth_attempts (
    username text CONSTRAINT "local_auth_attempts_username_not_null" NOT NULL,
    failures integer DEFAULT 0 CONSTRAINT "local_auth_attempts_failures_not_null" NOT NULL,
    window_until timestamp with time zone CONSTRAINT "local_auth_attempts_window_until_not_null" NOT NULL,
    blocked_until timestamp with time zone DEFAULT '-infinity'::timestamp with time zone CONSTRAINT "local_auth_attempts_blocked_until_not_null" NOT NULL,
    lease_id uuid,
    lease_until timestamp with time zone DEFAULT '-infinity'::timestamp with time zone CONSTRAINT "local_auth_attempts_lease_until_not_null" NOT NULL,
    expires_at timestamp with time zone CONSTRAINT "local_auth_attempts_expires_at_not_null" NOT NULL,
    CONSTRAINT local_auth_attempts_failures_check CHECK (((failures >= 0) AND (failures <= 14))),
    CONSTRAINT local_auth_attempts_username_check CHECK ((((octet_length(username) >= 1) AND (octet_length(username) <= 128)) AND ((username COLLATE "C") ~ '^[a-z0-9][a-z0-9._-]*$'::text)))
);

CREATE TABLE public.local_auth_bootstrap (
    singleton boolean DEFAULT true CONSTRAINT "local_auth_bootstrap_singleton_not_null" NOT NULL,
    identity_id uuid CONSTRAINT "local_auth_bootstrap_identity_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "local_auth_bootstrap_workspace_id_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "local_auth_bootstrap_created_at_not_null" NOT NULL,
    completion_pending boolean DEFAULT false CONSTRAINT "local_auth_bootstrap_completion_pending_not_null" NOT NULL,
    completed_at timestamp with time zone,
    approver_identity_id uuid,
    CONSTRAINT local_auth_bootstrap_singleton_check CHECK (singleton),
    CONSTRAINT local_initialization_state CHECK (((completion_pending AND (completed_at IS NULL) AND (approver_identity_id IS NULL)) OR ((NOT completion_pending) AND (completed_at IS NOT NULL))))
);

COMMENT ON TABLE public.local_auth_bootstrap IS 'One-shot Local initialization marker and fixed RBAC management workspace. Never cleared by password changes or disable.';

CREATE TABLE public.local_credentials (
    identity_id uuid CONSTRAINT "local_credentials_identity_id_not_null" NOT NULL,
    username text CONSTRAINT "local_credentials_username_not_null" NOT NULL,
    password_hash text CONSTRAINT "local_credentials_password_hash_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "local_credentials_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "local_credentials_updated_at_not_null" NOT NULL,
    password_changed_at timestamp with time zone CONSTRAINT "local_credentials_password_changed_at_not_null" NOT NULL,
    CONSTRAINT local_credentials_password_hash_check CHECK (((octet_length(password_hash) >= 1) AND (octet_length(password_hash) <= 512))),
    CONSTRAINT local_credentials_username_check CHECK ((((octet_length(username) >= 1) AND (octet_length(username) <= 128)) AND ((username COLLATE "C") ~ '^[a-z0-9][a-z0-9._-]*$'::text)))
);

CREATE TABLE public.local_slice_jobs (
    operation_id uuid CONSTRAINT "local_slice_jobs_operation_id_not_null" NOT NULL,
    command_envelope bytea CONSTRAINT "local_slice_jobs_command_envelope_not_null" NOT NULL,
    traceparent text CONSTRAINT "local_slice_jobs_traceparent_not_null" NOT NULL,
    attempts integer DEFAULT 0 CONSTRAINT "local_slice_jobs_attempts_not_null" NOT NULL,
    available_at timestamp with time zone CONSTRAINT "local_slice_jobs_available_at_not_null" NOT NULL,
    expires_at timestamp with time zone CONSTRAINT "local_slice_jobs_expires_at_not_null" NOT NULL,
    dispatched_at timestamp with time zone,
    last_error text,
    created_at timestamp with time zone CONSTRAINT "local_slice_jobs_created_at_not_null" NOT NULL,
    CONSTRAINT local_slice_jobs_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT local_slice_jobs_command_envelope_check CHECK (((octet_length(command_envelope) >= 1) AND (octet_length(command_envelope) <= 1048576))),
    CONSTRAINT local_slice_jobs_traceparent_check CHECK ((traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'::text))
);

COMMENT ON TABLE public.local_slice_jobs IS 'Development-only I03 simulator dispatch queue; it has no remote side effects.';

CREATE TABLE public.node_agent_upgrade_results (
    operation_id uuid CONSTRAINT "node_agent_upgrade_results_operation_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "node_agent_upgrade_results_node_id_not_null" NOT NULL,
    state text CONSTRAINT "node_agent_upgrade_results_state_not_null" NOT NULL,
    target_version text CONSTRAINT "node_agent_upgrade_results_target_version_not_null" NOT NULL,
    detail text DEFAULT ''::text CONSTRAINT "node_agent_upgrade_results_detail_not_null" NOT NULL,
    completed_at timestamp with time zone CONSTRAINT "node_agent_upgrade_results_completed_at_not_null" NOT NULL,
    reported_at timestamp with time zone CONSTRAINT "node_agent_upgrade_results_reported_at_not_null" NOT NULL,
    privileged_result_proof bytea CONSTRAINT "node_agent_upgrade_results_privileged_result_proof_not_null" NOT NULL,
    CONSTRAINT node_agent_upgrade_results_detail_check CHECK ((length(detail) <= 160)),
    CONSTRAINT node_agent_upgrade_results_privileged_result_proof_check CHECK (((octet_length(privileged_result_proof) >= 1) AND (octet_length(privileged_result_proof) <= 65536))),
    CONSTRAINT node_agent_upgrade_results_state_check CHECK ((state = ANY (ARRAY['succeeded'::text, 'failed'::text, 'rolled_back'::text])))
);

COMMENT ON TABLE public.node_agent_upgrade_results IS 'Durable local upgrader outcomes reported read-only through agent telemetry; first report per operation wins.';

CREATE TABLE public.node_bootstrap_tokens (
    id uuid CONSTRAINT "node_bootstrap_tokens_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "node_bootstrap_tokens_workspace_id_not_null" NOT NULL,
    token_hash bytea CONSTRAINT "node_bootstrap_tokens_token_hash_not_null" NOT NULL,
    expected_environment text CONSTRAINT "node_bootstrap_tokens_expected_environment_not_null" NOT NULL,
    expected_node_name text,
    expires_at timestamp with time zone CONSTRAINT "node_bootstrap_tokens_expires_at_not_null" NOT NULL,
    bound_endpoint_id bytea,
    consumed_node_id uuid,
    consumed_at timestamp with time zone,
    created_by text CONSTRAINT "node_bootstrap_tokens_created_by_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "node_bootstrap_tokens_created_at_not_null" NOT NULL,
    expected_endpoint_id bytea,
    CONSTRAINT node_bootstrap_tokens_bound_endpoint_id_check CHECK (((bound_endpoint_id IS NULL) OR (octet_length(bound_endpoint_id) = 32))),
    CONSTRAINT node_bootstrap_tokens_check CHECK ((expires_at > created_at)),
    CONSTRAINT node_bootstrap_tokens_check1 CHECK (((bound_endpoint_id IS NULL) = (consumed_at IS NULL))),
    CONSTRAINT node_bootstrap_tokens_check2 CHECK (((consumed_at IS NULL) = (consumed_node_id IS NULL))),
    CONSTRAINT node_bootstrap_tokens_created_by_check CHECK (((length(created_by) >= 1) AND (length(created_by) <= 256))),
    CONSTRAINT node_bootstrap_tokens_expected_endpoint_id_check CHECK (((expected_endpoint_id IS NULL) OR (octet_length(expected_endpoint_id) = 32))),
    CONSTRAINT node_bootstrap_tokens_expected_environment_check CHECK (((length(expected_environment) >= 1) AND (length(expected_environment) <= 64))),
    CONSTRAINT node_bootstrap_tokens_expected_node_name_check CHECK (((expected_node_name IS NULL) OR ((length(expected_node_name) >= 1) AND (length(expected_node_name) <= 128)))),
    CONSTRAINT node_bootstrap_tokens_token_hash_check CHECK ((octet_length(token_hash) = 32))
);

COMMENT ON COLUMN public.node_bootstrap_tokens.token_hash IS 'SHA-256 digest only; plaintext node bootstrap tokens must never be persisted.';

CREATE TABLE public.node_capabilities (
    node_id uuid CONSTRAINT "node_capabilities_node_id_not_null" NOT NULL,
    capability text CONSTRAINT "node_capabilities_capability_not_null" NOT NULL,
    approved boolean DEFAULT false CONSTRAINT "node_capabilities_approved_not_null" NOT NULL,
    CONSTRAINT node_capabilities_capability_check CHECK (((length(capability) >= 1) AND (length(capability) <= 128)))
);

CREATE TABLE public.node_command_leases (
    node_id uuid CONSTRAINT "node_command_leases_node_id_not_null" NOT NULL,
    command_id uuid CONSTRAINT "node_command_leases_command_id_not_null" NOT NULL,
    lease_token uuid CONSTRAINT "node_command_leases_lease_token_not_null" NOT NULL,
    worker_id uuid CONSTRAINT "node_command_leases_worker_id_not_null" NOT NULL,
    leased_until timestamp with time zone CONSTRAINT "node_command_leases_leased_until_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "node_command_leases_created_at_not_null" NOT NULL
);

CREATE TABLE public.node_config_state (
    node_id uuid CONSTRAINT "node_config_state_node_id_not_null" NOT NULL,
    revision bigint DEFAULT 0 CONSTRAINT "node_config_state_revision_not_null" NOT NULL,
    candidate_hash bytea,
    redacted_config text DEFAULT ''::text CONSTRAINT "node_config_state_redacted_config_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "node_config_state_updated_at_not_null" NOT NULL,
    automation_locked boolean DEFAULT false CONSTRAINT "node_config_state_automation_locked_not_null" NOT NULL,
    automation_lock_reason text,
    last_apply_operation_id uuid,
    desired_revision bigint DEFAULT 0 CONSTRAINT "node_config_state_desired_revision_not_null" NOT NULL,
    CONSTRAINT node_config_state_automation_lock_reason_check CHECK (((automation_lock_reason IS NULL) OR ((length(automation_lock_reason) >= 1) AND (length(automation_lock_reason) <= 128)))),
    CONSTRAINT node_config_state_candidate_hash_check CHECK (((candidate_hash IS NULL) OR (octet_length(candidate_hash) = 32))),
    CONSTRAINT node_config_state_check CHECK ((desired_revision >= revision)),
    CONSTRAINT node_config_state_redacted_config_check CHECK ((octet_length(redacted_config) <= 262144)),
    CONSTRAINT node_config_state_revision_check CHECK ((revision >= 0))
);

COMMENT ON COLUMN public.node_config_state.desired_revision IS 'Highest Controller-issued configuration effect revision, independent of the last applied revision.';

CREATE TABLE public.node_endpoint_keys (
    node_id uuid CONSTRAINT "node_endpoint_keys_node_id_not_null" NOT NULL,
    endpoint_id bytea CONSTRAINT "node_endpoint_keys_endpoint_id_not_null" NOT NULL,
    state text CONSTRAINT "node_endpoint_keys_state_not_null" NOT NULL,
    bound_at timestamp with time zone CONSTRAINT "node_endpoint_keys_bound_at_not_null" NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT node_endpoint_keys_check CHECK (((state = 'revoked'::text) = (revoked_at IS NOT NULL))),
    CONSTRAINT node_endpoint_keys_endpoint_id_check CHECK ((octet_length(endpoint_id) = 32)),
    CONSTRAINT node_endpoint_keys_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'active'::text, 'revoked'::text])))
);

CREATE TABLE public.node_ip_bans (
    node_id uuid CONSTRAINT "node_ip_bans_node_id_not_null" NOT NULL,
    ip inet CONSTRAINT "node_ip_bans_ip_not_null" NOT NULL,
    seconds_remaining bigint,
    observed_at timestamp with time zone CONSTRAINT "node_ip_bans_observed_at_not_null" NOT NULL,
    CONSTRAINT node_ip_bans_seconds_remaining_check CHECK (((seconds_remaining IS NULL) OR (seconds_remaining >= 0)))
);

COMMENT ON TABLE public.node_ip_bans IS 'Current typed Ocserv ban observations; addresses must not be used as metric labels.';

CREATE TABLE public.node_observed_snapshots (
    node_id uuid CONSTRAINT "node_observed_snapshots_node_id_not_null" NOT NULL,
    observed_at timestamp with time zone CONSTRAINT "node_observed_snapshots_observed_at_not_null" NOT NULL,
    received_at timestamp with time zone DEFAULT now() CONSTRAINT "node_observed_snapshots_received_at_not_null" NOT NULL,
    boot_id text CONSTRAINT "node_observed_snapshots_boot_id_not_null" NOT NULL,
    agent_instance_id uuid CONSTRAINT "node_observed_snapshots_agent_instance_id_not_null" NOT NULL,
    agent_version text CONSTRAINT "node_observed_snapshots_agent_version_not_null" NOT NULL,
    ocserv_version text CONSTRAINT "node_observed_snapshots_ocserv_version_not_null" NOT NULL,
    os_release text CONSTRAINT "node_observed_snapshots_os_release_not_null" NOT NULL,
    ocserv jsonb CONSTRAINT "node_observed_snapshots_ocserv_not_null" NOT NULL,
    system jsonb CONSTRAINT "node_observed_snapshots_system_not_null" NOT NULL,
    path jsonb CONSTRAINT "node_observed_snapshots_path_not_null" NOT NULL,
    last_heartbeat_at timestamp with time zone CONSTRAINT "node_observed_snapshots_last_heartbeat_at_not_null" NOT NULL,
    dropped_security bigint DEFAULT 0 CONSTRAINT "node_observed_snapshots_dropped_security_not_null" NOT NULL,
    dropped_health bigint DEFAULT 0 CONSTRAINT "node_observed_snapshots_dropped_health_not_null" NOT NULL,
    dropped_aggregate bigint DEFAULT 0 CONSTRAINT "node_observed_snapshots_dropped_aggregate_not_null" NOT NULL,
    dropped_raw bigint DEFAULT 0 CONSTRAINT "node_observed_snapshots_dropped_raw_not_null" NOT NULL,
    architecture text DEFAULT ''::text CONSTRAINT "node_observed_snapshots_architecture_not_null" NOT NULL,
    CONSTRAINT node_observed_snapshots_agent_version_check CHECK (((length(agent_version) >= 1) AND (length(agent_version) <= 128))),
    CONSTRAINT node_observed_snapshots_boot_id_check CHECK (((length(boot_id) >= 1) AND (length(boot_id) <= 128))),
    CONSTRAINT node_observed_snapshots_dropped_aggregate_check CHECK ((dropped_aggregate >= 0)),
    CONSTRAINT node_observed_snapshots_dropped_health_check CHECK ((dropped_health >= 0)),
    CONSTRAINT node_observed_snapshots_dropped_raw_check CHECK ((dropped_raw >= 0)),
    CONSTRAINT node_observed_snapshots_dropped_security_check CHECK ((dropped_security >= 0)),
    CONSTRAINT node_observed_snapshots_ocserv_check CHECK ((jsonb_typeof(ocserv) = 'object'::text)),
    CONSTRAINT node_observed_snapshots_ocserv_version_check CHECK (((length(ocserv_version) >= 1) AND (length(ocserv_version) <= 128))),
    CONSTRAINT node_observed_snapshots_os_release_check CHECK (((length(os_release) >= 1) AND (length(os_release) <= 128))),
    CONSTRAINT node_observed_snapshots_path_check CHECK ((jsonb_typeof(path) = 'object'::text)),
    CONSTRAINT node_observed_snapshots_system_check CHECK ((jsonb_typeof(system) = 'object'::text))
);

CREATE TABLE public.node_privd_attestation_keys (
    node_id uuid CONSTRAINT "node_privd_attestation_keys_node_id_not_null" NOT NULL,
    key_id text CONSTRAINT "node_privd_attestation_keys_key_id_not_null" NOT NULL,
    algorithm text CONSTRAINT "node_privd_attestation_keys_algorithm_not_null" NOT NULL,
    public_key bytea CONSTRAINT "node_privd_attestation_keys_public_key_not_null" NOT NULL,
    state text CONSTRAINT "node_privd_attestation_keys_state_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "node_privd_attestation_keys_created_at_not_null" NOT NULL,
    approved_at timestamp with time zone CONSTRAINT "node_privd_attestation_keys_approved_at_not_null" NOT NULL,
    activated_at timestamp with time zone CONSTRAINT "node_privd_attestation_keys_activated_at_not_null" NOT NULL,
    valid_until timestamp with time zone,
    revoked_at timestamp with time zone,
    predecessor_key_id text,
    successor_key_id text,
    registration_credential_id uuid CONSTRAINT "node_privd_attestation_keys_registration_credential_id_not_null" NOT NULL,
    CONSTRAINT node_privd_attestation_keys_algorithm_check CHECK ((algorithm = 'ed25519'::text)),
    CONSTRAINT node_privd_attestation_keys_check CHECK (((valid_until IS NULL) OR (valid_until >= activated_at))),
    CONSTRAINT node_privd_attestation_keys_check1 CHECK (((state = 'revoked'::text) = (revoked_at IS NOT NULL))),
    CONSTRAINT node_privd_attestation_keys_check2 CHECK (((predecessor_key_id IS NULL) OR (predecessor_key_id <> key_id))),
    CONSTRAINT node_privd_attestation_keys_check3 CHECK (((successor_key_id IS NULL) OR (successor_key_id <> key_id))),
    CONSTRAINT node_privd_attestation_keys_key_id_check CHECK ((key_id ~ '^ed25519-sha256:[0-9a-f]{64}$'::text)),
    CONSTRAINT node_privd_attestation_keys_public_key_check CHECK ((octet_length(public_key) = 32)),
    CONSTRAINT node_privd_attestation_keys_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'active'::text, 'revoked'::text])))
);

COMMENT ON TABLE public.node_privd_attestation_keys IS 'Root-authenticated per-node privd Ed25519 trust anchors with bounded rotation overlap.';

CREATE TABLE public.node_sealing_keys (
    node_id uuid CONSTRAINT "node_sealing_keys_node_id_not_null" NOT NULL,
    purpose smallint CONSTRAINT "node_sealing_keys_purpose_not_null" NOT NULL,
    version smallint CONSTRAINT "node_sealing_keys_version_not_null" NOT NULL,
    key_id text CONSTRAINT "node_sealing_keys_key_id_not_null" NOT NULL,
    public_key_sha256 bytea CONSTRAINT "node_sealing_keys_public_key_sha256_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "node_sealing_keys_created_at_not_null" NOT NULL,
    CONSTRAINT node_sealing_keys_key_id_check CHECK ((key_id ~ '^[A-Za-z0-9_.-]{1,128}$'::text)),
    CONSTRAINT node_sealing_keys_public_key_sha256_check CHECK ((octet_length(public_key_sha256) = 32)),
    CONSTRAINT node_sealing_keys_purpose_check CHECK ((purpose = ANY (ARRAY[1, 2]))),
    CONSTRAINT node_sealing_keys_version_check CHECK ((version = 1))
);

CREATE TABLE public.node_sessions (
    node_id uuid CONSTRAINT "node_sessions_node_id_not_null" NOT NULL,
    session_id text CONSTRAINT "node_sessions_session_id_not_null" NOT NULL,
    username text CONSTRAINT "node_sessions_username_not_null" NOT NULL,
    client_ip inet CONSTRAINT "node_sessions_client_ip_not_null" NOT NULL,
    connected_at timestamp with time zone CONSTRAINT "node_sessions_connected_at_not_null" NOT NULL,
    bytes_in bigint CONSTRAINT "node_sessions_bytes_in_not_null" NOT NULL,
    bytes_out bigint CONSTRAINT "node_sessions_bytes_out_not_null" NOT NULL,
    observed_at timestamp with time zone CONSTRAINT "node_sessions_observed_at_not_null" NOT NULL,
    CONSTRAINT node_sessions_bytes_in_check CHECK ((bytes_in >= 0)),
    CONSTRAINT node_sessions_bytes_out_check CHECK ((bytes_out >= 0)),
    CONSTRAINT node_sessions_session_id_check CHECK (((length(session_id) >= 1) AND (length(session_id) <= 256))),
    CONSTRAINT node_sessions_username_check CHECK (((length(username) >= 1) AND (length(username) <= 256)))
);

COMMENT ON TABLE public.node_sessions IS 'High-cardinality session, username, and client IP data; these fields must never become Prometheus labels.';

CREATE TABLE public.node_trust_convergence (
    node_id uuid CONSTRAINT "node_trust_convergence_node_id_not_null" NOT NULL,
    endpoint_id bytea CONSTRAINT "node_trust_convergence_endpoint_id_not_null" NOT NULL,
    desired_state text CONSTRAINT "node_trust_convergence_desired_state_not_null" NOT NULL,
    revision bigint CONSTRAINT "node_trust_convergence_revision_not_null" NOT NULL,
    reason text CONSTRAINT "node_trust_convergence_reason_not_null" NOT NULL,
    update_applied boolean DEFAULT false CONSTRAINT "node_trust_convergence_update_applied_not_null" NOT NULL,
    close_required boolean CONSTRAINT "node_trust_convergence_close_required_not_null" NOT NULL,
    close_applied boolean DEFAULT false CONSTRAINT "node_trust_convergence_close_applied_not_null" NOT NULL,
    available_at timestamp with time zone CONSTRAINT "node_trust_convergence_available_at_not_null" NOT NULL,
    locked_by uuid,
    locked_until timestamp with time zone,
    attempts integer DEFAULT 0 CONSTRAINT "node_trust_convergence_attempts_not_null" NOT NULL,
    last_error text,
    created_at timestamp with time zone CONSTRAINT "node_trust_convergence_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "node_trust_convergence_updated_at_not_null" NOT NULL,
    CONSTRAINT node_trust_convergence_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT node_trust_convergence_check CHECK (((locked_by IS NULL) = (locked_until IS NULL))),
    CONSTRAINT node_trust_convergence_check1 CHECK (((NOT close_applied) OR close_required)),
    CONSTRAINT node_trust_convergence_check2 CHECK (((desired_state = 'revoked'::text) OR (NOT close_required))),
    CONSTRAINT node_trust_convergence_desired_state_check CHECK ((desired_state = ANY (ARRAY['active'::text, 'revoked'::text]))),
    CONSTRAINT node_trust_convergence_endpoint_id_check CHECK ((octet_length(endpoint_id) = 32)),
    CONSTRAINT node_trust_convergence_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 1024))),
    CONSTRAINT node_trust_convergence_revision_check CHECK ((revision > 0))
);

COMMENT ON TABLE public.node_trust_convergence IS 'Durable Controller-to-transport trust convergence; node trust changes and retry work commit atomically.';

CREATE TABLE public.nodes (
    id uuid CONSTRAINT "nodes_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "nodes_workspace_id_not_null" NOT NULL,
    name text CONSTRAINT "nodes_name_not_null" NOT NULL,
    status text CONSTRAINT "nodes_status_not_null" NOT NULL,
    version bigint DEFAULT 1 CONSTRAINT "nodes_version_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "nodes_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "nodes_updated_at_not_null" NOT NULL,
    labels jsonb DEFAULT '{}'::jsonb CONSTRAINT "nodes_labels_not_null" NOT NULL,
    policy text,
    authorization_revision bigint DEFAULT 1 CONSTRAINT "nodes_authorization_revision_not_null" NOT NULL,
    CONSTRAINT nodes_authorization_revision_check CHECK ((authorization_revision > 0)),
    CONSTRAINT nodes_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'active'::text, 'revoked'::text, 'offline'::text]))),
    CONSTRAINT nodes_version_check CHECK ((version > 0))
);

COMMENT ON COLUMN public.nodes.authorization_revision IS 'Monotonic authority epoch changed only by trust or capability authorization transitions.';

CREATE TABLE public.observed_groups (
    node_id uuid CONSTRAINT "observed_groups_node_id_not_null" NOT NULL,
    group_name text CONSTRAINT "observed_groups_group_name_not_null" NOT NULL,
    members text[] CONSTRAINT "observed_groups_members_not_null" NOT NULL,
    revision bigint CONSTRAINT "observed_groups_revision_not_null" NOT NULL,
    fingerprint bytea CONSTRAINT "observed_groups_fingerprint_not_null" NOT NULL,
    observed_at timestamp with time zone CONSTRAINT "observed_groups_observed_at_not_null" NOT NULL,
    CONSTRAINT observed_groups_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT observed_groups_group_name_check CHECK ((group_name ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'::text)),
    CONSTRAINT observed_groups_members_check CHECK ((cardinality(members) <= 4096)),
    CONSTRAINT observed_groups_revision_check CHECK ((revision >= 0))
);

CREATE TABLE public.observed_user_usage (
    node_id uuid CONSTRAINT "observed_user_usage_node_id_not_null" NOT NULL,
    username text CONSTRAINT "observed_user_usage_username_not_null" NOT NULL,
    period text CONSTRAINT "observed_user_usage_period_not_null" NOT NULL,
    period_start timestamp with time zone CONSTRAINT "observed_user_usage_period_start_not_null" NOT NULL,
    rx_bytes bigint DEFAULT 0 CONSTRAINT "observed_user_usage_rx_bytes_not_null" NOT NULL,
    tx_bytes bigint DEFAULT 0 CONSTRAINT "observed_user_usage_tx_bytes_not_null" NOT NULL,
    observed_at timestamp with time zone CONSTRAINT "observed_user_usage_observed_at_not_null" NOT NULL,
    CONSTRAINT observed_user_usage_period_check CHECK ((period = ANY (ARRAY['monthly'::text, 'lifetime'::text]))),
    CONSTRAINT observed_user_usage_rx_bytes_check CHECK ((rx_bytes >= 0)),
    CONSTRAINT observed_user_usage_tx_bytes_check CHECK ((tx_bytes >= 0)),
    CONSTRAINT observed_user_usage_username_check CHECK ((username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'::text))
);

CREATE TABLE public.observed_users (
    node_id uuid CONSTRAINT "observed_users_node_id_not_null" NOT NULL,
    username text CONSTRAINT "observed_users_username_not_null" NOT NULL,
    enabled boolean CONSTRAINT "observed_users_enabled_not_null" NOT NULL,
    revision bigint CONSTRAINT "observed_users_revision_not_null" NOT NULL,
    fingerprint bytea CONSTRAINT "observed_users_fingerprint_not_null" NOT NULL,
    observed_at timestamp with time zone CONSTRAINT "observed_users_observed_at_not_null" NOT NULL,
    CONSTRAINT observed_users_fingerprint_check CHECK ((octet_length(fingerprint) = 32)),
    CONSTRAINT observed_users_revision_check CHECK ((revision >= 0)),
    CONSTRAINT observed_users_username_check CHECK ((username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'::text))
);

COMMENT ON TABLE public.observed_users IS 'Agent-reported user state without password hashes or password material.';

CREATE TABLE public.operation_events (
    sequence bigint CONSTRAINT "operation_events_sequence_not_null" NOT NULL,
    id uuid CONSTRAINT "operation_events_id_not_null" NOT NULL,
    operation_id uuid CONSTRAINT "operation_events_operation_id_not_null" NOT NULL,
    state text CONSTRAINT "operation_events_state_not_null" NOT NULL,
    occurred_at timestamp with time zone CONSTRAINT "operation_events_occurred_at_not_null" NOT NULL,
    CONSTRAINT operation_events_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'dispatched'::text, 'accepted'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'unknown'::text, 'expired'::text, 'rolled_back'::text, 'superseded'::text])))
);

ALTER TABLE public.operation_events ALTER COLUMN sequence ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.operation_events_sequence_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.operations (
    id uuid CONSTRAINT "operations_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "operations_workspace_id_not_null" NOT NULL,
    node_id uuid,
    command_id uuid,
    state text CONSTRAINT "operations_state_not_null" NOT NULL,
    version bigint DEFAULT 1 CONSTRAINT "operations_version_not_null" NOT NULL,
    request_id text CONSTRAINT "operations_request_id_not_null" NOT NULL,
    trace_id text,
    created_at timestamp with time zone CONSTRAINT "operations_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "operations_updated_at_not_null" NOT NULL,
    idempotency_key text,
    request_hash bytea,
    expires_at timestamp with time zone,
    completed_at timestamp with time zone,
    CONSTRAINT operations_idempotency_pair CHECK (((idempotency_key IS NULL) = (request_hash IS NULL))),
    CONSTRAINT operations_request_hash_size CHECK (((request_hash IS NULL) OR (octet_length(request_hash) = 32))),
    CONSTRAINT operations_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'queued'::text, 'dispatched'::text, 'accepted'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'unknown'::text, 'expired'::text, 'rolled_back'::text, 'offline_pending'::text, 'drifted'::text, 'superseded'::text]))),
    CONSTRAINT operations_version_check CHECK ((version > 0))
);

CREATE TABLE public.outbox_events (
    id uuid CONSTRAINT "outbox_events_id_not_null" NOT NULL,
    command_id uuid CONSTRAINT "outbox_events_command_id_not_null" NOT NULL,
    event_type text CONSTRAINT "outbox_events_event_type_not_null" NOT NULL,
    payload bytea CONSTRAINT "outbox_events_payload_not_null" NOT NULL,
    available_at timestamp with time zone CONSTRAINT "outbox_events_available_at_not_null" NOT NULL,
    locked_by uuid,
    locked_until timestamp with time zone,
    published_at timestamp with time zone,
    attempts integer DEFAULT 0 CONSTRAINT "outbox_events_attempts_not_null" NOT NULL,
    last_error text,
    created_at timestamp with time zone CONSTRAINT "outbox_events_created_at_not_null" NOT NULL,
    CONSTRAINT outbox_events_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT outbox_events_check CHECK (((locked_by IS NULL) = (locked_until IS NULL))),
    CONSTRAINT outbox_events_check1 CHECK (((published_at IS NULL) OR (locked_by IS NULL))),
    CONSTRAINT outbox_events_event_type_check CHECK ((event_type = 'command.dispatch'::text)),
    CONSTRAINT outbox_events_payload_check CHECK (((octet_length(payload) >= 1) AND (octet_length(payload) <= 1048576)))
);

COMMENT ON TABLE public.outbox_events IS 'Transactional outbox; delivery correctness never depends on LISTEN/NOTIFY.';

CREATE TABLE public.privd_attestation_enrollment_credentials (
    id uuid CONSTRAINT "privd_attestation_enrollment_credentials_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "privd_attestation_enrollment_credentials_node_id_not_null" NOT NULL,
    secret_sha256 bytea CONSTRAINT "privd_attestation_enrollment_credentials_secret_sha256_not_null" NOT NULL,
    controller_nonce bytea CONSTRAINT privd_attestation_enrollment_credenti_controller_nonce_not_null NOT NULL,
    credential_context_sha256 bytea CONSTRAINT privd_attestation_enrollment_credential_context_sha256_not_null NOT NULL,
    expires_at timestamp with time zone CONSTRAINT "privd_attestation_enrollment_credentials_expires_at_not_null" NOT NULL,
    consumed_at timestamp with time zone,
    created_by_identity_id uuid CONSTRAINT privd_attestation_enrollment_cr_created_by_identity_id_not_null NOT NULL,
    created_by_session_id uuid CONSTRAINT privd_attestation_enrollment_cre_created_by_session_id_not_null NOT NULL,
    created_at timestamp with time zone DEFAULT now() CONSTRAINT "privd_attestation_enrollment_credentials_created_at_not_null" NOT NULL,
    CONSTRAINT privd_attestation_enrollment_cr_credential_context_sha256_check CHECK ((octet_length(credential_context_sha256) = 32)),
    CONSTRAINT privd_attestation_enrollment_credentials_check CHECK ((expires_at > created_at)),
    CONSTRAINT privd_attestation_enrollment_credentials_check1 CHECK (((consumed_at IS NULL) OR (consumed_at >= created_at))),
    CONSTRAINT privd_attestation_enrollment_credentials_controller_nonce_check CHECK ((octet_length(controller_nonce) = 32)),
    CONSTRAINT privd_attestation_enrollment_credentials_id_check CHECK ((uuid_extract_version(id) = 7)),
    CONSTRAINT privd_attestation_enrollment_credentials_secret_sha256_check CHECK ((octet_length(secret_sha256) = 32))
);

CREATE TABLE public.role_bindings (
    id uuid CONSTRAINT "role_bindings_id_not_null" NOT NULL,
    identity_id uuid CONSTRAINT "role_bindings_identity_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "role_bindings_workspace_id_not_null" NOT NULL,
    role_name text CONSTRAINT "role_bindings_role_name_not_null" NOT NULL,
    resource_type text DEFAULT 'workspace'::text CONSTRAINT "role_bindings_resource_type_not_null" NOT NULL,
    resource_id uuid,
    created_by uuid,
    created_at timestamp with time zone CONSTRAINT "role_bindings_created_at_not_null" NOT NULL,
    approval_id uuid,
    CONSTRAINT role_bindings_check CHECK (((resource_type = 'workspace'::text) = (resource_id IS NULL))),
    CONSTRAINT role_bindings_resource_type_check CHECK ((resource_type = ANY (ARRAY['workspace'::text, 'node'::text, 'resource'::text, 'secret_ref'::text, 'certificate'::text, 'config_plan'::text, 'batch_operation'::text, 'role_binding'::text])))
);

CREATE TABLE public.roles (
    name text CONSTRAINT "roles_name_not_null" NOT NULL,
    CONSTRAINT roles_name_check CHECK ((name = ANY (ARRAY['Viewer'::text, 'Operator'::text, 'UserManager'::text, 'ConfigManager'::text, 'Auditor'::text, 'SecurityAdmin'::text, 'PlatformAdmin'::text])))
);

CREATE TABLE public.scheduler_leadership (
    id integer CONSTRAINT "scheduler_leadership_id_not_null" NOT NULL,
    instance_id uuid CONSTRAINT "scheduler_leadership_instance_id_not_null" NOT NULL,
    incarnation bigint CONSTRAINT "scheduler_leadership_incarnation_not_null" NOT NULL,
    epoch bigint CONSTRAINT "scheduler_leadership_epoch_not_null" NOT NULL,
    lease_until timestamp with time zone CONSTRAINT "scheduler_leadership_lease_until_not_null" NOT NULL,
    updated_at timestamp with time zone DEFAULT now() CONSTRAINT "scheduler_leadership_updated_at_not_null" NOT NULL,
    CONSTRAINT scheduler_leadership_epoch_check CHECK ((epoch >= 0)),
    CONSTRAINT scheduler_leadership_id_check CHECK ((id = 1)),
    CONSTRAINT scheduler_leadership_incarnation_check CHECK ((incarnation >= 0))
);

CREATE TABLE public.scheduler_leases (
    lease_name text CONSTRAINT "scheduler_leases_lease_name_not_null" NOT NULL,
    owner_id uuid CONSTRAINT "scheduler_leases_owner_id_not_null" NOT NULL,
    lease_until timestamp with time zone CONSTRAINT "scheduler_leases_lease_until_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "scheduler_leases_updated_at_not_null" NOT NULL,
    CONSTRAINT scheduler_leases_lease_name_check CHECK (((length(lease_name) >= 1) AND (length(lease_name) <= 128)))
);

CREATE TABLE public.schema_migrations (
    version bigint CONSTRAINT "schema_migrations_version_not_null" NOT NULL,
    name text CONSTRAINT "schema_migrations_name_not_null" NOT NULL,
    checksum bytea CONSTRAINT "schema_migrations_checksum_not_null" NOT NULL,
    applied_at timestamp with time zone DEFAULT now() CONSTRAINT "schema_migrations_applied_at_not_null" NOT NULL,
    snapshot_covered boolean DEFAULT false CONSTRAINT "schema_migrations_snapshot_covered_not_null" NOT NULL
);

CREATE TABLE public.schema_snapshot_origin (
    singleton boolean DEFAULT true CONSTRAINT "schema_snapshot_origin_singleton_not_null" NOT NULL,
    covered_version bigint CONSTRAINT "schema_snapshot_origin_covered_version_not_null" NOT NULL,
    history_sha256 bytea CONSTRAINT "schema_snapshot_origin_history_sha256_not_null" NOT NULL,
    schema_sha256 bytea CONSTRAINT "schema_snapshot_origin_schema_sha256_not_null" NOT NULL,
    receipt_sha256 bytea CONSTRAINT "schema_snapshot_origin_receipt_sha256_not_null" NOT NULL,
    initialized_at timestamp with time zone DEFAULT now() CONSTRAINT "schema_snapshot_origin_initialized_at_not_null" NOT NULL,
    CONSTRAINT schema_snapshot_origin_covered_version_check CHECK ((covered_version > 0)),
    CONSTRAINT schema_snapshot_origin_history_sha256_check CHECK ((octet_length(history_sha256) = 32)),
    CONSTRAINT schema_snapshot_origin_receipt_sha256_check CHECK ((octet_length(receipt_sha256) = 32)),
    CONSTRAINT schema_snapshot_origin_schema_sha256_check CHECK ((octet_length(schema_sha256) = 32)),
    CONSTRAINT schema_snapshot_origin_singleton_check CHECK (singleton)
);

COMMENT ON TABLE public.schema_snapshot_origin IS 'Atomic snapshot coverage provenance; covered schema_migrations rows are coverage, not individually executed migrations.';

CREATE TABLE public.secret_provider_refs (
    id uuid CONSTRAINT "secret_provider_refs_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "secret_provider_refs_workspace_id_not_null" NOT NULL,
    provider text CONSTRAINT "secret_provider_refs_provider_not_null" NOT NULL,
    key_path text CONSTRAINT "secret_provider_refs_key_path_not_null" NOT NULL,
    version text CONSTRAINT "secret_provider_refs_version_not_null" NOT NULL,
    state text CONSTRAINT "secret_provider_refs_state_not_null" NOT NULL,
    rotated_at timestamp with time zone,
    created_at timestamp with time zone CONSTRAINT "secret_provider_refs_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "secret_provider_refs_updated_at_not_null" NOT NULL,
    CONSTRAINT secret_provider_refs_key_path_check CHECK ((((length(key_path) >= 1) AND (length(key_path) <= 512)) AND (key_path !~ '(^|/)\.\.(/|$)'::text))),
    CONSTRAINT secret_provider_refs_provider_check CHECK ((provider ~ '^[A-Za-z0-9._-]{1,64}$'::text)),
    CONSTRAINT secret_provider_refs_state_check CHECK ((state = ANY (ARRAY['active'::text, 'rotating'::text, 'disabled'::text, 'unavailable'::text]))),
    CONSTRAINT secret_provider_refs_version_check CHECK (((length(version) >= 1) AND (length(version) <= 128)))
);

COMMENT ON TABLE public.secret_provider_refs IS 'Opaque external secret references only; secret values are never stored in PostgreSQL.';

CREATE TABLE public.security_alerts (
    id uuid CONSTRAINT "security_alerts_id_not_null" NOT NULL,
    workspace_id uuid,
    severity text CONSTRAINT "security_alerts_severity_not_null" NOT NULL,
    kind text CONSTRAINT "security_alerts_kind_not_null" NOT NULL,
    source_session_id uuid,
    acknowledged_at timestamp with time zone,
    created_at timestamp with time zone CONSTRAINT "security_alerts_created_at_not_null" NOT NULL,
    node_id uuid,
    resource_type text,
    resource_id uuid,
    CONSTRAINT security_alerts_kind_check CHECK (((length(kind) >= 1) AND (length(kind) <= 128))),
    CONSTRAINT security_alerts_resource_type_check CHECK (((resource_type IS NULL) OR ((length(resource_type) >= 1) AND (length(resource_type) <= 64)))),
    CONSTRAINT security_alerts_severity_check CHECK ((severity = ANY (ARRAY['high'::text, 'critical'::text])))
);

CREATE TABLE public.telemetry_ingest_batches (
    batch_id uuid CONSTRAINT "telemetry_ingest_batches_batch_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "telemetry_ingest_batches_node_id_not_null" NOT NULL,
    sequence bigint CONSTRAINT "telemetry_ingest_batches_sequence_not_null" NOT NULL,
    kind text CONSTRAINT "telemetry_ingest_batches_kind_not_null" NOT NULL,
    observed_at timestamp with time zone CONSTRAINT "telemetry_ingest_batches_observed_at_not_null" NOT NULL,
    received_at timestamp with time zone DEFAULT now() CONSTRAINT "telemetry_ingest_batches_received_at_not_null" NOT NULL,
    payload_bytes integer CONSTRAINT "telemetry_ingest_batches_payload_bytes_not_null" NOT NULL,
    CONSTRAINT telemetry_ingest_batches_kind_check CHECK ((kind = ANY (ARRAY['security'::text, 'current_health'::text, 'aggregate'::text, 'raw_history'::text]))),
    CONSTRAINT telemetry_ingest_batches_payload_bytes_check CHECK (((payload_bytes >= 0) AND (payload_bytes <= 524288))),
    CONSTRAINT telemetry_ingest_batches_sequence_check CHECK ((sequence >= 0))
);

CREATE TABLE public.telemetry_rollups_1h (
    node_id uuid CONSTRAINT telemetry_rollups_5m_node_id_not_null NOT NULL,
    metric text CONSTRAINT telemetry_rollups_5m_metric_not_null NOT NULL,
    bucket_at timestamp with time zone CONSTRAINT telemetry_rollups_5m_bucket_at_not_null NOT NULL,
    sample_count bigint CONSTRAINT telemetry_rollups_5m_sample_count_not_null NOT NULL,
    min_value double precision CONSTRAINT telemetry_rollups_5m_min_value_not_null NOT NULL,
    max_value double precision CONSTRAINT telemetry_rollups_5m_max_value_not_null NOT NULL,
    avg_value double precision CONSTRAINT telemetry_rollups_5m_avg_value_not_null NOT NULL,
    CONSTRAINT telemetry_rollups_5m_sample_count_check CHECK ((sample_count > 0))
);

CREATE TABLE public.telemetry_rollups_5m (
    node_id uuid CONSTRAINT "telemetry_rollups_5m_node_id_not_null" NOT NULL,
    metric text CONSTRAINT "telemetry_rollups_5m_metric_not_null" NOT NULL,
    bucket_at timestamp with time zone CONSTRAINT "telemetry_rollups_5m_bucket_at_not_null" NOT NULL,
    sample_count bigint CONSTRAINT "telemetry_rollups_5m_sample_count_not_null" NOT NULL,
    min_value double precision CONSTRAINT "telemetry_rollups_5m_min_value_not_null" NOT NULL,
    max_value double precision CONSTRAINT "telemetry_rollups_5m_max_value_not_null" NOT NULL,
    avg_value double precision CONSTRAINT "telemetry_rollups_5m_avg_value_not_null" NOT NULL,
    CONSTRAINT telemetry_rollups_5m_sample_count_check CHECK ((sample_count > 0))
);

CREATE TABLE public.telemetry_samples (
    node_id uuid CONSTRAINT "telemetry_samples_node_id_not_null" NOT NULL,
    batch_id uuid CONSTRAINT "telemetry_samples_batch_id_not_null" NOT NULL,
    sampled_at timestamp with time zone CONSTRAINT "telemetry_samples_sampled_at_not_null" NOT NULL,
    metric text CONSTRAINT "telemetry_samples_metric_not_null" NOT NULL,
    value double precision CONSTRAINT "telemetry_samples_value_not_null" NOT NULL,
    CONSTRAINT telemetry_samples_metric_check CHECK ((metric = ANY (ARRAY['cpu_usage_ratio'::text, 'memory_used_bytes'::text, 'network_rx_bytes'::text, 'network_tx_bytes'::text, 'session_count'::text, 'connection_rtt_ms'::text]))),
    CONSTRAINT telemetry_samples_value_check CHECK ((value <> ALL (ARRAY['Infinity'::double precision, '-Infinity'::double precision, 'NaN'::double precision])))
)
PARTITION BY RANGE (sampled_at);

COMMENT ON TABLE public.telemetry_samples IS 'Monthly-partitioned raw telemetry retained independently from current observed state and rollups.';

CREATE TABLE public.telemetry_samples_default (
    node_id uuid CONSTRAINT telemetry_samples_node_id_not_null NOT NULL,
    batch_id uuid CONSTRAINT telemetry_samples_batch_id_not_null NOT NULL,
    sampled_at timestamp with time zone CONSTRAINT telemetry_samples_sampled_at_not_null NOT NULL,
    metric text CONSTRAINT telemetry_samples_metric_not_null NOT NULL,
    value double precision CONSTRAINT telemetry_samples_value_not_null NOT NULL,
    CONSTRAINT telemetry_samples_metric_check CHECK ((metric = ANY (ARRAY['cpu_usage_ratio'::text, 'memory_used_bytes'::text, 'network_rx_bytes'::text, 'network_tx_bytes'::text, 'session_count'::text, 'connection_rtt_ms'::text]))),
    CONSTRAINT telemetry_samples_value_check CHECK ((value <> ALL (ARRAY['Infinity'::double precision, '-Infinity'::double precision, 'NaN'::double precision])))
);

CREATE TABLE public.telemetry_security_events (
    event_id uuid CONSTRAINT "telemetry_security_events_event_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "telemetry_security_events_node_id_not_null" NOT NULL,
    observed_at timestamp with time zone CONSTRAINT "telemetry_security_events_observed_at_not_null" NOT NULL,
    severity text CONSTRAINT "telemetry_security_events_severity_not_null" NOT NULL,
    event_type text CONSTRAINT "telemetry_security_events_event_type_not_null" NOT NULL,
    detail jsonb CONSTRAINT "telemetry_security_events_detail_not_null" NOT NULL,
    details_compacted_at timestamp with time zone,
    detail_sha256 bytea,
    CONSTRAINT security_detail_evidence CHECK ((((details_compacted_at IS NULL) AND (detail_sha256 IS NULL)) OR ((details_compacted_at IS NOT NULL) AND (detail_sha256 IS NOT NULL) AND (octet_length(detail_sha256) = 32) AND (detail = '{}'::jsonb)))),
    CONSTRAINT telemetry_security_events_detail_check CHECK ((jsonb_typeof(detail) = 'object'::text)),
    CONSTRAINT telemetry_security_events_event_type_check CHECK (((length(event_type) >= 1) AND (length(event_type) <= 128))),
    CONSTRAINT telemetry_security_events_severity_check CHECK ((severity = ANY (ARRAY['info'::text, 'warning'::text, 'critical'::text])))
);

CREATE TABLE public.transport_event_cursor (
    singleton boolean DEFAULT true CONSTRAINT "transport_event_cursor_singleton_not_null" NOT NULL,
    event_id uuid CONSTRAINT "transport_event_cursor_event_id_not_null" NOT NULL,
    valid boolean DEFAULT true CONSTRAINT "transport_event_cursor_valid_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "transport_event_cursor_updated_at_not_null" NOT NULL,
    CONSTRAINT transport_event_cursor_singleton_check CHECK (singleton)
);

COMMENT ON TABLE public.transport_event_cursor IS 'Durable cursor for both accepted and quarantined transport events.';

CREATE TABLE public.transport_event_quarantine (
    event_id uuid CONSTRAINT "transport_event_quarantine_event_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "transport_event_quarantine_node_id_not_null" NOT NULL,
    event_type integer CONSTRAINT "transport_event_quarantine_event_type_not_null" NOT NULL,
    payload_sha256 bytea CONSTRAINT "transport_event_quarantine_payload_sha256_not_null" NOT NULL,
    reason_code text CONSTRAINT "transport_event_quarantine_reason_code_not_null" NOT NULL,
    reason_detail text CONSTRAINT "transport_event_quarantine_reason_detail_not_null" NOT NULL,
    observed_at timestamp with time zone CONSTRAINT "transport_event_quarantine_observed_at_not_null" NOT NULL,
    CONSTRAINT transport_event_quarantine_payload_sha256_check CHECK ((octet_length(payload_sha256) = 32)),
    CONSTRAINT transport_event_quarantine_reason_code_check CHECK ((reason_code ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT transport_event_quarantine_reason_detail_check CHECK (((octet_length(reason_detail) >= 1) AND (octet_length(reason_detail) <= 256)))
);

COMMENT ON TABLE public.transport_event_quarantine IS 'Bounded metadata for permanently invalid transport events; raw attacker payloads are never retained.';

CREATE TABLE public.transport_events (
    event_id uuid CONSTRAINT "transport_events_event_id_not_null" NOT NULL,
    ingest_sequence bigint CONSTRAINT "transport_events_ingest_sequence_not_null" NOT NULL,
    node_id uuid CONSTRAINT "transport_events_node_id_not_null" NOT NULL,
    event_type text CONSTRAINT "transport_events_event_type_not_null" NOT NULL,
    occurred_at timestamp with time zone CONSTRAINT "transport_events_occurred_at_not_null" NOT NULL,
    traceparent text CONSTRAINT "transport_events_traceparent_not_null" NOT NULL,
    payload bytea CONSTRAINT "transport_events_payload_not_null" NOT NULL,
    transport_cursor_valid boolean DEFAULT true CONSTRAINT "transport_events_transport_cursor_valid_not_null" NOT NULL,
    received_at timestamp with time zone DEFAULT now() CONSTRAINT "transport_events_received_at_not_null" NOT NULL,
    CONSTRAINT transport_events_event_type_check CHECK ((event_type = ANY (ARRAY['connected'::text, 'disconnected'::text, 'command_result'::text, 'heartbeat'::text, 'error'::text, 'path_changed'::text, 'telemetry'::text, 'simulation_result'::text]))),
    CONSTRAINT transport_events_payload_check CHECK ((octet_length(payload) <= 1048576)),
    CONSTRAINT transport_events_traceparent_check CHECK ((traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$'::text))
);

COMMENT ON TABLE public.transport_events IS 'Idempotently ingested typed transport events used by REST and SSE rebuilds.';

ALTER TABLE public.transport_events ALTER COLUMN ingest_sequence ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.transport_events_ingest_sequence_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.upstream_sync_records (
    id uuid CONSTRAINT "upstream_sync_records_id_not_null" NOT NULL,
    repository text CONSTRAINT "upstream_sync_records_repository_not_null" NOT NULL,
    old_ref text CONSTRAINT "upstream_sync_records_old_ref_not_null" NOT NULL,
    old_commit text CONSTRAINT "upstream_sync_records_old_commit_not_null" NOT NULL,
    new_ref text CONSTRAINT "upstream_sync_records_new_ref_not_null" NOT NULL,
    new_commit text CONSTRAINT "upstream_sync_records_new_commit_not_null" NOT NULL,
    classification jsonb CONSTRAINT "upstream_sync_records_classification_not_null" NOT NULL,
    rollback_ref text CONSTRAINT "upstream_sync_records_rollback_ref_not_null" NOT NULL,
    synced_at timestamp with time zone CONSTRAINT "upstream_sync_records_synced_at_not_null" NOT NULL,
    CONSTRAINT upstream_sync_records_classification_check CHECK ((jsonb_typeof(classification) = 'object'::text)),
    CONSTRAINT upstream_sync_records_new_commit_check CHECK ((new_commit ~ '^[0-9a-f]{40}$'::text)),
    CONSTRAINT upstream_sync_records_old_commit_check CHECK ((old_commit ~ '^[0-9a-f]{40}$'::text))
);

COMMENT ON TABLE public.upstream_sync_records IS 'Pinned, auditable upstream comparison metadata; local execution and privileged deployment code are never imported.';

CREATE TABLE public.user_policy_enforcements (
    node_id uuid CONSTRAINT "user_policy_enforcements_node_id_not_null" NOT NULL,
    username text CONSTRAINT "user_policy_enforcements_username_not_null" NOT NULL,
    policy_version bigint CONSTRAINT "user_policy_enforcements_policy_version_not_null" NOT NULL,
    cause text CONSTRAINT "user_policy_enforcements_cause_not_null" NOT NULL,
    period_start timestamp with time zone CONSTRAINT "user_policy_enforcements_period_start_not_null" NOT NULL,
    source_user_version bigint CONSTRAINT "user_policy_enforcements_source_user_version_not_null" NOT NULL,
    operation_id uuid,
    resulting_user_version bigint,
    created_at timestamp with time zone CONSTRAINT "user_policy_enforcements_created_at_not_null" NOT NULL,
    CONSTRAINT user_policy_enforcements_cause_check CHECK ((cause = ANY (ARRAY['quota'::text, 'expiry'::text, 'quota_reset'::text]))),
    CONSTRAINT user_policy_enforcements_check CHECK (((operation_id IS NULL) = (resulting_user_version IS NULL))),
    CONSTRAINT user_policy_enforcements_policy_version_check CHECK ((policy_version > 0)),
    CONSTRAINT user_policy_enforcements_resulting_user_version_check CHECK ((resulting_user_version > 0)),
    CONSTRAINT user_policy_enforcements_source_user_version_check CHECK ((source_user_version > 0))
);

CREATE TABLE public.user_policy_mutations (
    id uuid CONSTRAINT "user_policy_mutations_id_not_null" NOT NULL,
    workspace_id uuid CONSTRAINT "user_policy_mutations_workspace_id_not_null" NOT NULL,
    node_id uuid CONSTRAINT "user_policy_mutations_node_id_not_null" NOT NULL,
    username text CONSTRAINT "user_policy_mutations_username_not_null" NOT NULL,
    idempotency_key text CONSTRAINT "user_policy_mutations_idempotency_key_not_null" NOT NULL,
    request_hash bytea CONSTRAINT "user_policy_mutations_request_hash_not_null" NOT NULL,
    policy_version bigint CONSTRAINT "user_policy_mutations_policy_version_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "user_policy_mutations_created_at_not_null" NOT NULL,
    CONSTRAINT user_policy_mutations_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 128))),
    CONSTRAINT user_policy_mutations_policy_version_check CHECK ((policy_version > 0)),
    CONSTRAINT user_policy_mutations_request_hash_check CHECK ((octet_length(request_hash) = 32))
);

CREATE TABLE public.user_usage_cursors (
    node_id uuid CONSTRAINT "user_usage_cursors_node_id_not_null" NOT NULL,
    session_id text CONSTRAINT "user_usage_cursors_session_id_not_null" NOT NULL,
    connected_at timestamp with time zone CONSTRAINT "user_usage_cursors_connected_at_not_null" NOT NULL,
    username text CONSTRAINT "user_usage_cursors_username_not_null" NOT NULL,
    rx_bytes bigint CONSTRAINT "user_usage_cursors_rx_bytes_not_null" NOT NULL,
    tx_bytes bigint CONSTRAINT "user_usage_cursors_tx_bytes_not_null" NOT NULL,
    observed_at timestamp with time zone CONSTRAINT "user_usage_cursors_observed_at_not_null" NOT NULL,
    CONSTRAINT user_usage_cursors_rx_bytes_check CHECK ((rx_bytes >= 0)),
    CONSTRAINT user_usage_cursors_session_id_check CHECK (((length(session_id) >= 1) AND (length(session_id) <= 256))),
    CONSTRAINT user_usage_cursors_tx_bytes_check CHECK ((tx_bytes >= 0)),
    CONSTRAINT user_usage_cursors_username_check CHECK ((username ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'::text))
);

CREATE TABLE public.workspaces (
    id uuid CONSTRAINT "workspaces_id_not_null" NOT NULL,
    name text CONSTRAINT "workspaces_name_not_null" NOT NULL,
    slug text CONSTRAINT "workspaces_slug_not_null" NOT NULL,
    version bigint DEFAULT 1 CONSTRAINT "workspaces_version_not_null" NOT NULL,
    created_at timestamp with time zone CONSTRAINT "workspaces_created_at_not_null" NOT NULL,
    updated_at timestamp with time zone CONSTRAINT "workspaces_updated_at_not_null" NOT NULL,
    archived_at timestamp with time zone,
    CONSTRAINT workspaces_version_check CHECK ((version > 0))
);

ALTER TABLE ONLY public.telemetry_samples ATTACH PARTITION public.telemetry_samples_default DEFAULT;

ALTER TABLE ONLY public.agent_command_results
    ADD CONSTRAINT agent_command_results_pkey PRIMARY KEY (event_id);

ALTER TABLE ONLY public.agent_rollout_nodes
    ADD CONSTRAINT agent_rollout_nodes_pkey PRIMARY KEY (rollout_id, node_id);

ALTER TABLE ONLY public.agent_rollout_nodes
    ADD CONSTRAINT agent_rollout_nodes_rollout_id_ordinal_key UNIQUE (rollout_id, ordinal);

ALTER TABLE ONLY public.agent_rollouts
    ADD CONSTRAINT agent_rollouts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.agent_rollouts
    ADD CONSTRAINT agent_rollouts_workspace_id_idempotency_key_key UNIQUE (workspace_id, idempotency_key);

ALTER TABLE ONLY public.agent_upgrade_operations
    ADD CONSTRAINT agent_upgrade_operations_pkey PRIMARY KEY (operation_id);

ALTER TABLE ONLY public.approval_authority_resources
    ADD CONSTRAINT approval_authority_resources_pkey PRIMARY KEY (approval_id, resource_type, resource_id);

ALTER TABLE ONLY public.approval_batch_items
    ADD CONSTRAINT approval_batch_items_pkey PRIMARY KEY (approval_id, item_index);

ALTER TABLE ONLY public.approval_requests
    ADD CONSTRAINT approval_requests_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.artifact_operations
    ADD CONSTRAINT artifact_operations_operation_id_key UNIQUE (operation_id);

ALTER TABLE ONLY public.artifact_operations
    ADD CONSTRAINT artifact_operations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.audit_checkpoints
    ADD CONSTRAINT audit_checkpoints_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.audit_checkpoints
    ADD CONSTRAINT audit_checkpoints_workspace_id_through_event_id_key UNIQUE (workspace_id, through_event_id);

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.auth_sessions
    ADD CONSTRAINT auth_sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.batch_operation_items
    ADD CONSTRAINT batch_operation_items_pkey PRIMARY KEY (batch_id, item_index);

ALTER TABLE ONLY public.batch_operations
    ADD CONSTRAINT batch_operations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.batch_operations
    ADD CONSTRAINT batch_operations_workspace_id_idempotency_key_key UNIQUE (workspace_id, idempotency_key);

ALTER TABLE ONLY public.break_glass_uses
    ADD CONSTRAINT break_glass_uses_pkey PRIMARY KEY (credential_fingerprint);

ALTER TABLE ONLY public.certificates
    ADD CONSTRAINT certificates_operation_id_key UNIQUE (operation_id);

ALTER TABLE ONLY public.certificates
    ADD CONSTRAINT certificates_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.command_attempts
    ADD CONSTRAINT command_attempts_command_id_attempt_number_key UNIQUE (command_id, attempt_number);

ALTER TABLE ONLY public.command_attempts
    ADD CONSTRAINT command_attempts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.commands
    ADD CONSTRAINT commands_operation_id_key UNIQUE (operation_id);

ALTER TABLE ONLY public.commands
    ADD CONSTRAINT commands_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.commands
    ADD CONSTRAINT commands_workspace_id_idempotency_key_key UNIQUE (workspace_id, idempotency_key);

ALTER TABLE ONLY public.config_apply_operations
    ADD CONSTRAINT config_apply_operations_pkey PRIMARY KEY (operation_id);

ALTER TABLE ONLY public.config_apply_operations
    ADD CONSTRAINT config_apply_operations_plan_id_key UNIQUE (plan_id);

ALTER TABLE ONLY public.config_plans
    ADD CONSTRAINT config_plans_operation_id_key UNIQUE (operation_id);

ALTER TABLE ONLY public.config_plans
    ADD CONSTRAINT config_plans_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.connection_owner_fencing
    ADD CONSTRAINT connection_owner_fencing_pkey PRIMARY KEY (node_id);

ALTER TABLE ONLY public.controller_schema_compatibility
    ADD CONSTRAINT controller_schema_compatibility_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY public.desired_groups
    ADD CONSTRAINT desired_groups_pkey PRIMARY KEY (node_id, group_name);

ALTER TABLE ONLY public.desired_user_policies
    ADD CONSTRAINT desired_user_policies_pkey PRIMARY KEY (node_id, username);

ALTER TABLE ONLY public.desired_users
    ADD CONSTRAINT desired_users_pkey PRIMARY KEY (node_id, username);

ALTER TABLE ONLY public.enrollment_tokens
    ADD CONSTRAINT enrollment_tokens_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.enrollment_tokens
    ADD CONSTRAINT enrollment_tokens_token_hash_key UNIQUE (token_hash);

ALTER TABLE ONLY public.identities
    ADD CONSTRAINT identities_issuer_subject_key UNIQUE (issuer, subject);

ALTER TABLE ONLY public.identities
    ADD CONSTRAINT identities_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.local_auth_attempts
    ADD CONSTRAINT local_auth_attempts_pkey PRIMARY KEY (username);

ALTER TABLE ONLY public.local_auth_bootstrap
    ADD CONSTRAINT local_auth_bootstrap_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY public.local_credentials
    ADD CONSTRAINT local_credentials_pkey PRIMARY KEY (identity_id);

ALTER TABLE ONLY public.local_credentials
    ADD CONSTRAINT local_credentials_username_key UNIQUE (username);

ALTER TABLE ONLY public.local_slice_jobs
    ADD CONSTRAINT local_slice_jobs_pkey PRIMARY KEY (operation_id);

ALTER TABLE ONLY public.node_agent_upgrade_results
    ADD CONSTRAINT node_agent_upgrade_results_pkey PRIMARY KEY (operation_id);

ALTER TABLE ONLY public.node_bootstrap_tokens
    ADD CONSTRAINT node_bootstrap_tokens_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.node_bootstrap_tokens
    ADD CONSTRAINT node_bootstrap_tokens_token_hash_key UNIQUE (token_hash);

ALTER TABLE ONLY public.node_capabilities
    ADD CONSTRAINT node_capabilities_pkey PRIMARY KEY (node_id, capability);

ALTER TABLE ONLY public.node_command_leases
    ADD CONSTRAINT node_command_leases_command_id_key UNIQUE (command_id);

ALTER TABLE ONLY public.node_command_leases
    ADD CONSTRAINT node_command_leases_lease_token_key UNIQUE (lease_token);

ALTER TABLE ONLY public.node_command_leases
    ADD CONSTRAINT node_command_leases_pkey PRIMARY KEY (node_id);

ALTER TABLE ONLY public.node_config_state
    ADD CONSTRAINT node_config_state_pkey PRIMARY KEY (node_id);

ALTER TABLE ONLY public.node_endpoint_keys
    ADD CONSTRAINT node_endpoint_keys_endpoint_id_key UNIQUE (endpoint_id);

ALTER TABLE ONLY public.node_endpoint_keys
    ADD CONSTRAINT node_endpoint_keys_pkey PRIMARY KEY (node_id);

ALTER TABLE ONLY public.node_ip_bans
    ADD CONSTRAINT node_ip_bans_pkey PRIMARY KEY (node_id, ip);

ALTER TABLE ONLY public.node_observed_snapshots
    ADD CONSTRAINT node_observed_snapshots_pkey PRIMARY KEY (node_id);

ALTER TABLE ONLY public.node_privd_attestation_keys
    ADD CONSTRAINT node_privd_attestation_keys_key_id_key UNIQUE (key_id);

ALTER TABLE ONLY public.node_privd_attestation_keys
    ADD CONSTRAINT node_privd_attestation_keys_pkey PRIMARY KEY (node_id, key_id);

ALTER TABLE ONLY public.node_privd_attestation_keys
    ADD CONSTRAINT node_privd_attestation_keys_public_key_key UNIQUE (public_key);

ALTER TABLE ONLY public.node_privd_attestation_keys
    ADD CONSTRAINT node_privd_attestation_keys_registration_credential_id_key UNIQUE (registration_credential_id);

ALTER TABLE ONLY public.node_sealing_keys
    ADD CONSTRAINT node_sealing_keys_node_id_key_id_key UNIQUE (node_id, key_id);

ALTER TABLE ONLY public.node_sealing_keys
    ADD CONSTRAINT node_sealing_keys_node_id_public_key_sha256_key UNIQUE (node_id, public_key_sha256);

ALTER TABLE ONLY public.node_sealing_keys
    ADD CONSTRAINT node_sealing_keys_pkey PRIMARY KEY (node_id, purpose);

ALTER TABLE ONLY public.node_sessions
    ADD CONSTRAINT node_sessions_pkey PRIMARY KEY (node_id, session_id);

ALTER TABLE ONLY public.node_trust_convergence
    ADD CONSTRAINT node_trust_convergence_pkey PRIMARY KEY (node_id);

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_workspace_id_id_key UNIQUE (workspace_id, id);

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_workspace_id_name_key UNIQUE (workspace_id, name);

ALTER TABLE ONLY public.observed_groups
    ADD CONSTRAINT observed_groups_pkey PRIMARY KEY (node_id, group_name);

ALTER TABLE ONLY public.observed_user_usage
    ADD CONSTRAINT observed_user_usage_pkey PRIMARY KEY (node_id, username, period, period_start);

ALTER TABLE ONLY public.observed_users
    ADD CONSTRAINT observed_users_pkey PRIMARY KEY (node_id, username);

ALTER TABLE ONLY public.operation_events
    ADD CONSTRAINT operation_events_id_key UNIQUE (id);

ALTER TABLE ONLY public.operation_events
    ADD CONSTRAINT operation_events_pkey PRIMARY KEY (sequence);

ALTER TABLE ONLY public.operations
    ADD CONSTRAINT operations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.outbox_events
    ADD CONSTRAINT outbox_events_command_id_key UNIQUE (command_id);

ALTER TABLE ONLY public.outbox_events
    ADD CONSTRAINT outbox_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.privd_attestation_enrollment_credentials
    ADD CONSTRAINT privd_attestation_enrollment_credentials_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.privd_attestation_enrollment_credentials
    ADD CONSTRAINT privd_attestation_enrollment_credentials_secret_sha256_key UNIQUE (secret_sha256);

ALTER TABLE ONLY public.role_bindings
    ADD CONSTRAINT role_bindings_identity_id_workspace_id_role_name_resource_t_key UNIQUE NULLS NOT DISTINCT (identity_id, workspace_id, role_name, resource_type, resource_id);

ALTER TABLE ONLY public.role_bindings
    ADD CONSTRAINT role_bindings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.roles
    ADD CONSTRAINT roles_pkey PRIMARY KEY (name);

ALTER TABLE ONLY public.scheduler_leadership
    ADD CONSTRAINT scheduler_leadership_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.scheduler_leases
    ADD CONSTRAINT scheduler_leases_pkey PRIMARY KEY (lease_name);

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);

ALTER TABLE ONLY public.schema_snapshot_origin
    ADD CONSTRAINT schema_snapshot_origin_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY public.secret_provider_refs
    ADD CONSTRAINT secret_provider_refs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.secret_provider_refs
    ADD CONSTRAINT secret_provider_refs_workspace_id_provider_key_path_key UNIQUE (workspace_id, provider, key_path);

ALTER TABLE ONLY public.security_alerts
    ADD CONSTRAINT security_alerts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.telemetry_ingest_batches
    ADD CONSTRAINT telemetry_ingest_batches_pkey PRIMARY KEY (batch_id);

ALTER TABLE ONLY public.telemetry_rollups_1h
    ADD CONSTRAINT telemetry_rollups_1h_pkey PRIMARY KEY (node_id, metric, bucket_at);

ALTER TABLE ONLY public.telemetry_rollups_5m
    ADD CONSTRAINT telemetry_rollups_5m_pkey PRIMARY KEY (node_id, metric, bucket_at);

ALTER TABLE ONLY public.telemetry_samples
    ADD CONSTRAINT telemetry_samples_pkey PRIMARY KEY (sampled_at, node_id, batch_id, metric);

ALTER TABLE ONLY public.telemetry_samples_default
    ADD CONSTRAINT telemetry_samples_default_pkey PRIMARY KEY (sampled_at, node_id, batch_id, metric);

ALTER TABLE ONLY public.telemetry_security_events
    ADD CONSTRAINT telemetry_security_events_pkey PRIMARY KEY (event_id);

ALTER TABLE ONLY public.transport_event_cursor
    ADD CONSTRAINT transport_event_cursor_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY public.transport_event_quarantine
    ADD CONSTRAINT transport_event_quarantine_pkey PRIMARY KEY (event_id);

ALTER TABLE ONLY public.transport_events
    ADD CONSTRAINT transport_events_ingest_sequence_key UNIQUE (ingest_sequence);

ALTER TABLE ONLY public.transport_events
    ADD CONSTRAINT transport_events_pkey PRIMARY KEY (event_id);

ALTER TABLE ONLY public.upstream_sync_records
    ADD CONSTRAINT upstream_sync_records_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.upstream_sync_records
    ADD CONSTRAINT upstream_sync_records_repository_old_commit_new_commit_key UNIQUE (repository, old_commit, new_commit);

ALTER TABLE ONLY public.user_policy_enforcements
    ADD CONSTRAINT user_policy_enforcements_pkey PRIMARY KEY (node_id, username, policy_version, cause, period_start);

ALTER TABLE ONLY public.user_policy_mutations
    ADD CONSTRAINT user_policy_mutations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.user_policy_mutations
    ADD CONSTRAINT user_policy_mutations_workspace_id_idempotency_key_key UNIQUE (workspace_id, idempotency_key);

ALTER TABLE ONLY public.user_usage_cursors
    ADD CONSTRAINT user_usage_cursors_pkey PRIMARY KEY (node_id, session_id, connected_at);

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.workspaces
    ADD CONSTRAINT workspaces_slug_key UNIQUE (slug);

CREATE INDEX agent_command_results_command_created_idx ON public.agent_command_results USING btree (command_id, created_at);

CREATE UNIQUE INDEX agent_command_results_effect_receipt_unique_idx ON public.agent_command_results USING btree (privd_attestation_key_id, effect_record_id, effect_sequence) WHERE (receipt_verification_status = 'verified'::text);

CREATE INDEX agent_command_results_receipt_idx ON public.agent_command_results USING btree (command_id, effect_record_id, effect_sequence) WHERE (receipt_verification_status = 'verified'::text);

CREATE INDEX agent_rollout_nodes_active_idx ON public.agent_rollout_nodes USING btree (rollout_id, batch) WHERE (state = ANY (ARRAY['pending'::text, 'running'::text]));

CREATE INDEX agent_rollout_nodes_operation_idx ON public.agent_rollout_nodes USING btree (operation_id) WHERE (operation_id IS NOT NULL);

CREATE INDEX agent_rollouts_active_idx ON public.agent_rollouts USING btree (created_at) WHERE (state = ANY (ARRAY['queued'::text, 'running'::text]));

CREATE INDEX agent_rollouts_workspace_created_idx ON public.agent_rollouts USING btree (workspace_id, created_at DESC);

CREATE INDEX agent_upgrade_operations_operation_idx ON public.agent_upgrade_operations USING btree (operation_id);

CREATE INDEX agent_upgrade_operations_pending_idx ON public.agent_upgrade_operations USING btree (node_id) WHERE ((completed_at IS NULL) AND (state = ANY (ARRAY['queued'::text, 'accepted'::text, 'running'::text, 'unknown'::text])));

CREATE INDEX approval_requests_scope_idx ON public.approval_requests USING btree (workspace_id, resource_type, resource_id, action, status, expires_at);

CREATE UNIQUE INDEX artifact_operations_active_grant_idx ON public.artifact_operations USING btree (active_grant_id) WHERE (active_grant_id IS NOT NULL);

CREATE INDEX artifact_operations_expiry_idx ON public.artifact_operations USING btree (expires_at) WHERE (state = ANY (ARRAY['pending'::text, 'ready'::text, 'leased'::text]));

CREATE UNIQUE INDEX artifact_operations_one_live_certificate_idx ON public.artifact_operations USING btree (certificate_id) WHERE (state = ANY (ARRAY['pending'::text, 'ready'::text, 'leased'::text, 'consuming'::text]));

CREATE INDEX audit_detail_retention_idx ON public.audit_events USING btree (occurred_at, id) WHERE (details_compacted_at IS NULL);

CREATE INDEX audit_events_workspace_time_idx ON public.audit_events USING btree (workspace_id, occurred_at DESC, id DESC);

CREATE INDEX auth_sessions_expiry_idx ON public.auth_sessions USING btree (expires_at) WHERE (revoked_at IS NULL);

CREATE INDEX batch_operation_items_claim_idx ON public.batch_operation_items USING btree (updated_at, batch_id, item_index) WHERE (state = ANY (ARRAY['queued'::text, 'submitting'::text]));

CREATE INDEX batch_operations_active_idx ON public.batch_operations USING btree (updated_at, id) WHERE (state = ANY (ARRAY['queued'::text, 'running'::text, 'partial_failed'::text]));

CREATE INDEX certificates_node_expiry_idx ON public.certificates USING btree (node_id, not_after) WHERE (state = ANY (ARRAY['issued'::text, 'expiring'::text]));

CREATE INDEX commands_node_state_idx ON public.commands USING btree (node_id, state, created_at, id);

CREATE INDEX commands_pending_resource_idx ON public.commands USING btree (node_id, resource_type, resource_key, created_at) WHERE ((state = 'queued'::text) AND (resource_type IS NOT NULL));

CREATE INDEX commands_retention_idx ON public.commands USING btree (updated_at, id) WHERE (details_compacted_at IS NULL);

CREATE INDEX config_apply_operations_node_created_idx ON public.config_apply_operations USING btree (node_id, created_at DESC, operation_id DESC);

CREATE UNIQUE INDEX config_apply_operations_one_active_node_idx ON public.config_apply_operations USING btree (node_id) WHERE (state = ANY (ARRAY['queued'::text, 'dispatched'::text, 'accepted'::text, 'running'::text, 'unknown'::text]));

CREATE INDEX config_plans_node_created_idx ON public.config_plans USING btree (node_id, created_at DESC, id DESC);

CREATE INDEX enrollment_tokens_workspace_expiry_idx ON public.enrollment_tokens USING btree (workspace_id, expires_at DESC);

CREATE INDEX local_auth_attempts_expiry ON public.local_auth_attempts USING btree (expires_at);

CREATE INDEX local_slice_jobs_dispatch_idx ON public.local_slice_jobs USING btree (available_at, operation_id) WHERE (dispatched_at IS NULL);

CREATE INDEX node_agent_upgrade_results_node_idx ON public.node_agent_upgrade_results USING btree (node_id);

CREATE INDEX node_bootstrap_tokens_workspace_expiry_idx ON public.node_bootstrap_tokens USING btree (workspace_id, expires_at DESC);

CREATE INDEX node_command_leases_expiry_idx ON public.node_command_leases USING btree (leased_until);

CREATE INDEX node_observed_freshness_idx ON public.node_observed_snapshots USING btree (last_heartbeat_at);

CREATE INDEX node_privd_attestation_keys_active_idx ON public.node_privd_attestation_keys USING btree (node_id, state, activated_at) WHERE (state = 'active'::text);

CREATE INDEX node_sessions_observed_idx ON public.node_sessions USING btree (node_id, observed_at DESC, session_id);

CREATE INDEX node_trust_convergence_pending_idx ON public.node_trust_convergence USING btree (available_at, node_id) WHERE ((NOT update_applied) OR (close_required AND (NOT close_applied)));

CREATE INDEX operation_events_operation_sequence_idx ON public.operation_events USING btree (operation_id, sequence);

CREATE INDEX operations_active_command_limit_idx ON public.operations USING btree (state, id) WHERE (state = ANY (ARRAY['dispatched'::text, 'accepted'::text, 'running'::text, 'unknown'::text]));

CREATE INDEX operations_queued_backlog_idx ON public.operations USING btree (workspace_id, node_id, id) WHERE (state = ANY (ARRAY['queued'::text, 'offline_pending'::text]));

CREATE INDEX operations_workspace_created_idx ON public.operations USING btree (workspace_id, created_at DESC, id DESC);

CREATE UNIQUE INDEX operations_workspace_idempotency_idx ON public.operations USING btree (workspace_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);

CREATE INDEX outbox_events_dispatch_idx ON public.outbox_events USING btree (available_at, id) WHERE (published_at IS NULL);

CREATE INDEX privd_attestation_credentials_node_active_idx ON public.privd_attestation_enrollment_credentials USING btree (node_id, expires_at) WHERE (consumed_at IS NULL);

CREATE INDEX role_bindings_authorization_idx ON public.role_bindings USING btree (identity_id, workspace_id, resource_type, resource_id, role_name);

CREATE INDEX security_detail_retention_idx ON public.telemetry_security_events USING btree (observed_at, event_id) WHERE (details_compacted_at IS NULL);

CREATE INDEX telemetry_rollups_1h_retention_idx ON public.telemetry_rollups_1h USING btree (bucket_at, node_id, metric);

CREATE INDEX telemetry_rollups_5m_retention_idx ON public.telemetry_rollups_5m USING btree (bucket_at, node_id, metric);

CREATE INDEX telemetry_samples_default_query_idx ON public.telemetry_samples_default USING btree (node_id, metric, sampled_at DESC);

CREATE INDEX telemetry_security_node_time_idx ON public.telemetry_security_events USING btree (node_id, observed_at DESC);

CREATE INDEX transport_event_quarantine_node_time_idx ON public.transport_event_quarantine USING btree (node_id, observed_at DESC);

ALTER INDEX public.telemetry_samples_pkey ATTACH PARTITION public.telemetry_samples_default_pkey;

CREATE TRIGGER audit_checkpoints_append_only BEFORE DELETE OR UPDATE OR TRUNCATE ON public.audit_checkpoints FOR EACH STATEMENT EXECUTE FUNCTION public.reject_audit_checkpoint_mutation();

CREATE TRIGGER audit_events_append_only BEFORE DELETE OR UPDATE ON public.audit_events FOR EACH ROW EXECUTE FUNCTION public.check_audit_compaction();

CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON public.audit_events FOR EACH STATEMENT EXECUTE FUNCTION public.reject_audit_event_mutation();

CREATE TRIGGER telemetry_default_owner_only BEFORE INSERT OR UPDATE ON public.telemetry_samples_default FOR EACH ROW EXECUTE FUNCTION public.telemetry_reject_runtime_default();

ALTER TABLE ONLY public.agent_command_results
    ADD CONSTRAINT agent_command_results_command_id_fkey FOREIGN KEY (command_id) REFERENCES public.commands(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.agent_command_results
    ADD CONSTRAINT agent_command_results_event_id_fkey FOREIGN KEY (event_id) REFERENCES public.transport_events(event_id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.agent_rollout_nodes
    ADD CONSTRAINT agent_rollout_nodes_rollout_id_fkey FOREIGN KEY (rollout_id) REFERENCES public.agent_rollouts(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.agent_rollouts
    ADD CONSTRAINT agent_rollouts_approval_id_fkey FOREIGN KEY (approval_id) REFERENCES public.approval_requests(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.agent_rollouts
    ADD CONSTRAINT agent_rollouts_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.agent_upgrade_operations
    ADD CONSTRAINT agent_upgrade_operations_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.agent_upgrade_operations
    ADD CONSTRAINT agent_upgrade_operations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.approval_authority_resources
    ADD CONSTRAINT approval_authority_resources_approval_id_fkey FOREIGN KEY (approval_id) REFERENCES public.approval_requests(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.approval_authority_resources
    ADD CONSTRAINT approval_authority_resources_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.approval_batch_items
    ADD CONSTRAINT approval_batch_items_approval_id_fkey FOREIGN KEY (approval_id) REFERENCES public.approval_requests(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.approval_batch_items
    ADD CONSTRAINT approval_batch_items_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.approval_requests
    ADD CONSTRAINT approval_requests_approver_id_fkey FOREIGN KEY (approver_id) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.approval_requests
    ADD CONSTRAINT approval_requests_requester_id_fkey FOREIGN KEY (requester_id) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.approval_requests
    ADD CONSTRAINT approval_requests_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.artifact_operations
    ADD CONSTRAINT artifact_operations_approval_id_fkey FOREIGN KEY (approval_id) REFERENCES public.approval_requests(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.artifact_operations
    ADD CONSTRAINT artifact_operations_certificate_id_fkey FOREIGN KEY (certificate_id) REFERENCES public.certificates(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.artifact_operations
    ADD CONSTRAINT artifact_operations_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.artifact_operations
    ADD CONSTRAINT artifact_operations_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.artifact_operations
    ADD CONSTRAINT artifact_operations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.audit_checkpoints
    ADD CONSTRAINT audit_checkpoints_through_event_id_fkey FOREIGN KEY (through_event_id) REFERENCES public.audit_events(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.audit_checkpoints
    ADD CONSTRAINT audit_checkpoints_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_approval_id_fkey FOREIGN KEY (approval_id) REFERENCES public.approval_requests(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.audit_events
    ADD CONSTRAINT audit_events_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.auth_sessions
    ADD CONSTRAINT auth_sessions_identity_id_fkey FOREIGN KEY (identity_id) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.batch_operation_items
    ADD CONSTRAINT batch_operation_items_batch_id_fkey FOREIGN KEY (batch_id) REFERENCES public.batch_operations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.batch_operation_items
    ADD CONSTRAINT batch_operation_items_child_operation_id_fkey FOREIGN KEY (child_operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.batch_operation_items
    ADD CONSTRAINT batch_operation_items_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.batch_operations
    ADD CONSTRAINT batch_operations_actor_identity_id_fkey FOREIGN KEY (actor_identity_id) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.batch_operations
    ADD CONSTRAINT batch_operations_actor_session_id_fkey FOREIGN KEY (actor_session_id) REFERENCES public.auth_sessions(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.batch_operations
    ADD CONSTRAINT batch_operations_approval_id_fkey FOREIGN KEY (approval_id) REFERENCES public.approval_requests(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.batch_operations
    ADD CONSTRAINT batch_operations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.break_glass_uses
    ADD CONSTRAINT break_glass_uses_identity_id_fkey FOREIGN KEY (identity_id) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.certificates
    ADD CONSTRAINT certificates_csr_privd_key_fk FOREIGN KEY (node_id, csr_privd_attestation_key_id) REFERENCES public.node_privd_attestation_keys(node_id, key_id);

ALTER TABLE ONLY public.certificates
    ADD CONSTRAINT certificates_issue_actor_identity_id_fkey FOREIGN KEY (issue_actor_identity_id) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.certificates
    ADD CONSTRAINT certificates_issue_approval_id_fkey FOREIGN KEY (issue_approval_id) REFERENCES public.approval_requests(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.certificates
    ADD CONSTRAINT certificates_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.certificates
    ADD CONSTRAINT certificates_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.certificates
    ADD CONSTRAINT certificates_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.certificates
    ADD CONSTRAINT certificates_workspace_id_node_id_fkey FOREIGN KEY (workspace_id, node_id) REFERENCES public.nodes(workspace_id, id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.command_attempts
    ADD CONSTRAINT command_attempts_command_id_fkey FOREIGN KEY (command_id) REFERENCES public.commands(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.command_attempts
    ADD CONSTRAINT command_attempts_outbox_event_id_fkey FOREIGN KEY (outbox_event_id) REFERENCES public.outbox_events(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.commands
    ADD CONSTRAINT commands_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.commands
    ADD CONSTRAINT commands_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.commands
    ADD CONSTRAINT commands_workspace_id_node_id_fkey FOREIGN KEY (workspace_id, node_id) REFERENCES public.nodes(workspace_id, id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_apply_operations
    ADD CONSTRAINT config_apply_operations_approval_id_fkey FOREIGN KEY (approval_id) REFERENCES public.approval_requests(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_apply_operations
    ADD CONSTRAINT config_apply_operations_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_apply_operations
    ADD CONSTRAINT config_apply_operations_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_apply_operations
    ADD CONSTRAINT config_apply_operations_plan_id_fkey FOREIGN KEY (plan_id) REFERENCES public.config_plans(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_apply_operations
    ADD CONSTRAINT config_apply_operations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_apply_operations
    ADD CONSTRAINT config_apply_operations_workspace_id_node_id_fkey FOREIGN KEY (workspace_id, node_id) REFERENCES public.nodes(workspace_id, id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_plans
    ADD CONSTRAINT config_plans_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_plans
    ADD CONSTRAINT config_plans_id_fkey FOREIGN KEY (id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_plans
    ADD CONSTRAINT config_plans_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_plans
    ADD CONSTRAINT config_plans_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_plans
    ADD CONSTRAINT config_plans_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.config_plans
    ADD CONSTRAINT config_plans_workspace_id_node_id_fkey FOREIGN KEY (workspace_id, node_id) REFERENCES public.nodes(workspace_id, id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.desired_groups
    ADD CONSTRAINT desired_groups_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.desired_user_policies
    ADD CONSTRAINT desired_user_policies_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.desired_user_policies
    ADD CONSTRAINT desired_user_policies_node_id_username_fkey FOREIGN KEY (node_id, username) REFERENCES public.desired_users(node_id, username) ON DELETE CASCADE;

ALTER TABLE ONLY public.desired_users
    ADD CONSTRAINT desired_users_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.enrollment_tokens
    ADD CONSTRAINT enrollment_tokens_workspace_id_consumed_node_id_fkey FOREIGN KEY (workspace_id, consumed_node_id) REFERENCES public.nodes(workspace_id, id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.enrollment_tokens
    ADD CONSTRAINT enrollment_tokens_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.local_auth_bootstrap
    ADD CONSTRAINT local_auth_bootstrap_approver_identity_id_fkey FOREIGN KEY (approver_identity_id) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.local_auth_bootstrap
    ADD CONSTRAINT local_auth_bootstrap_identity_id_fkey FOREIGN KEY (identity_id) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.local_auth_bootstrap
    ADD CONSTRAINT local_auth_bootstrap_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.local_credentials
    ADD CONSTRAINT local_credentials_identity_id_fkey FOREIGN KEY (identity_id) REFERENCES public.identities(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.local_slice_jobs
    ADD CONSTRAINT local_slice_jobs_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_bootstrap_tokens
    ADD CONSTRAINT node_bootstrap_tokens_workspace_id_consumed_node_id_fkey FOREIGN KEY (workspace_id, consumed_node_id) REFERENCES public.nodes(workspace_id, id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.node_bootstrap_tokens
    ADD CONSTRAINT node_bootstrap_tokens_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.node_capabilities
    ADD CONSTRAINT node_capabilities_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_command_leases
    ADD CONSTRAINT node_command_leases_command_id_fkey FOREIGN KEY (command_id) REFERENCES public.commands(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_command_leases
    ADD CONSTRAINT node_command_leases_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_config_state
    ADD CONSTRAINT node_config_state_last_apply_operation_id_fkey FOREIGN KEY (last_apply_operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.node_config_state
    ADD CONSTRAINT node_config_state_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.node_endpoint_keys
    ADD CONSTRAINT node_endpoint_keys_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.node_ip_bans
    ADD CONSTRAINT node_ip_bans_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_observed_snapshots
    ADD CONSTRAINT node_observed_snapshots_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_privd_attestation_keys
    ADD CONSTRAINT node_privd_attestation_keys_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_privd_attestation_keys
    ADD CONSTRAINT node_privd_attestation_keys_registration_credential_id_fkey FOREIGN KEY (registration_credential_id) REFERENCES public.privd_attestation_enrollment_credentials(id);

ALTER TABLE ONLY public.node_sealing_keys
    ADD CONSTRAINT node_sealing_keys_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.node_sessions
    ADD CONSTRAINT node_sessions_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_trust_convergence
    ADD CONSTRAINT node_trust_convergence_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.observed_groups
    ADD CONSTRAINT observed_groups_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.observed_user_usage
    ADD CONSTRAINT observed_user_usage_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.observed_users
    ADD CONSTRAINT observed_users_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.operation_events
    ADD CONSTRAINT operation_events_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.operations
    ADD CONSTRAINT operations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.operations
    ADD CONSTRAINT operations_workspace_id_node_id_fkey FOREIGN KEY (workspace_id, node_id) REFERENCES public.nodes(workspace_id, id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.outbox_events
    ADD CONSTRAINT outbox_events_command_id_fkey FOREIGN KEY (command_id) REFERENCES public.commands(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.privd_attestation_enrollment_credentials
    ADD CONSTRAINT privd_attestation_enrollment_creden_created_by_identity_id_fkey FOREIGN KEY (created_by_identity_id) REFERENCES public.identities(id);

ALTER TABLE ONLY public.privd_attestation_enrollment_credentials
    ADD CONSTRAINT privd_attestation_enrollment_credent_created_by_session_id_fkey FOREIGN KEY (created_by_session_id) REFERENCES public.auth_sessions(id);

ALTER TABLE ONLY public.privd_attestation_enrollment_credentials
    ADD CONSTRAINT privd_attestation_enrollment_credentials_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.role_bindings
    ADD CONSTRAINT role_bindings_approval_id_fkey FOREIGN KEY (approval_id) REFERENCES public.approval_requests(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.role_bindings
    ADD CONSTRAINT role_bindings_created_by_fkey FOREIGN KEY (created_by) REFERENCES public.identities(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.role_bindings
    ADD CONSTRAINT role_bindings_identity_id_fkey FOREIGN KEY (identity_id) REFERENCES public.identities(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.role_bindings
    ADD CONSTRAINT role_bindings_role_name_fkey FOREIGN KEY (role_name) REFERENCES public.roles(name) ON DELETE RESTRICT;

ALTER TABLE ONLY public.role_bindings
    ADD CONSTRAINT role_bindings_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.secret_provider_refs
    ADD CONSTRAINT secret_provider_refs_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.security_alerts
    ADD CONSTRAINT security_alerts_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.security_alerts
    ADD CONSTRAINT security_alerts_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.telemetry_ingest_batches
    ADD CONSTRAINT telemetry_ingest_batches_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.telemetry_rollups_5m
    ADD CONSTRAINT telemetry_rollups_5m_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE public.telemetry_samples
    ADD CONSTRAINT telemetry_samples_batch_id_fkey FOREIGN KEY (batch_id) REFERENCES public.telemetry_ingest_batches(batch_id) ON DELETE CASCADE;

ALTER TABLE public.telemetry_samples
    ADD CONSTRAINT telemetry_samples_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.telemetry_security_events
    ADD CONSTRAINT telemetry_security_events_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.transport_events
    ADD CONSTRAINT transport_events_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.user_policy_enforcements
    ADD CONSTRAINT user_policy_enforcements_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.user_policy_enforcements
    ADD CONSTRAINT user_policy_enforcements_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES public.operations(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.user_policy_mutations
    ADD CONSTRAINT user_policy_mutations_node_id_username_fkey FOREIGN KEY (node_id, username) REFERENCES public.desired_user_policies(node_id, username) ON DELETE RESTRICT;

ALTER TABLE ONLY public.user_policy_mutations
    ADD CONSTRAINT user_policy_mutations_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES public.workspaces(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.user_usage_cursors
    ADD CONSTRAINT user_usage_cursors_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

REVOKE ALL ON FUNCTION public.audit_compact_detail(p_id uuid, p_hash bytea, p_key text, p_mac bytea, p_at timestamp with time zone) FROM PUBLIC;

REVOKE ALL ON FUNCTION public.check_audit_compaction() FROM PUBLIC;

REVOKE ALL ON FUNCTION public.reject_audit_checkpoint_mutation() FROM PUBLIC;

REVOKE ALL ON FUNCTION public.reject_audit_event_mutation() FROM PUBLIC;

REVOKE ALL ON FUNCTION public.security_compact_details(p_cutoff timestamp with time zone) FROM PUBLIC;

REVOKE ALL ON FUNCTION public.telemetry_drop_expired_partitions(cutoff timestamp with time zone) FROM PUBLIC;

REVOKE ALL ON FUNCTION public.telemetry_ensure_month_partition(sample_time timestamp with time zone) FROM PUBLIC;

REVOKE ALL ON FUNCTION public.telemetry_prune_rollups(maintenance_time timestamp with time zone) FROM PUBLIC;

REVOKE ALL ON FUNCTION public.telemetry_reject_runtime_default() FROM PUBLIC;

GRANT SELECT,INSERT ON TABLE public.approval_authority_resources TO ocservia_app;

GRANT SELECT,INSERT ON TABLE public.approval_batch_items TO ocservia_app;

GRANT SELECT,INSERT,UPDATE ON TABLE public.artifact_operations TO ocservia_app;

GRANT SELECT,INSERT,UPDATE ON TABLE public.certificates TO ocservia_app;

GRANT SELECT,INSERT ON TABLE public.node_sealing_keys TO ocservia_app;

GRANT SELECT,INSERT,UPDATE ON TABLE public.secret_provider_refs TO ocservia_app;

SET LOCAL statement_timeout = 0;
SET LOCAL lock_timeout = 0;
SET LOCAL idle_in_transaction_session_timeout = 0;
SET LOCAL transaction_timeout = 0;
SET LOCAL client_encoding = 'UTF8';
SET LOCAL standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', true);
SET LOCAL check_function_bodies = false;
SET LOCAL xmloption = content;
SET LOCAL client_min_messages = warning;
SET LOCAL row_security = off;

INSERT INTO public.controller_schema_compatibility (singleton, "current_schema", minimum_compatible_controller_schema) VALUES (true, 39, 39);

INSERT INTO public.roles (name) VALUES ('Viewer');
INSERT INTO public.roles (name) VALUES ('Operator');
INSERT INTO public.roles (name) VALUES ('UserManager');
INSERT INTO public.roles (name) VALUES ('ConfigManager');
INSERT INTO public.roles (name) VALUES ('Auditor');
INSERT INTO public.roles (name) VALUES ('SecurityAdmin');
INSERT INTO public.roles (name) VALUES ('PlatformAdmin');

INSERT INTO public.upstream_sync_records (id, repository, old_ref, old_commit, new_ref, new_commit, classification, rollback_ref, synced_at) VALUES ('019fdc5b-b939-72a1-ae67-8efd197e5688', 'mmtaee/ocserv-dashboard', 'v4.9', 'b8f59026c4d879f40c1da43dc00d97e34f9790bc', 'master', '4d25478580d899b77460bdf0cf0a590cfdd26030', '{"A": [], "B": ["web/src/components/auth/SetupForm.vue"], "C": ["quota and expiry semantics mapped to node-scoped desired policy and scheduler"], "D": ["Docker/native occtl execution", "local cron journal", "direct password/config files", "permanent deletion"]}', 'publication: revert PR15 independently; implementation: stop I14 scheduler/API, reconcile commands, revert PR14, then apply migration 000013 down only when policy and batch data need not be retained', '2026-08-07 16:41:52+00');

INSERT INTO public.scheduler_leadership(id,instance_id,incarnation,epoch,lease_until) VALUES(1,'00000000-0000-0000-0000-000000000000',0,0,'-infinity');
