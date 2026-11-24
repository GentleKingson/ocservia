-- ocservia:artifact=upgrade
-- ocservia:format=1
-- ocservia:engine=postgresql
-- ocservia:epoch=2
-- ocservia:baseline={"checksum":"a870fd98925aed844a62cab28b034a86ee64b9ab8afa942ef5b630233255d305","steps":1,"metadata":{"catalog_sha256":"096ad0fc5ad94f3d8c93fb94ffb9935b65a40bab31bc15cc84b7961d7755800c"}}
-- ocservia:previous-checkpoint-epoch=1
-- ocservia:previous-checkpoint-revision=0
-- ocservia:previous-checkpoint-ref=v1.2.0@169102557cd610847c9f6ac2083336cdcf82c483
-- ocservia:previous-checkpoint-receipt={"checksum":"d837335f22c70858f4e2a5277332e478ecd6032e7b55f57d6512484bfab6f172","steps":1,"metadata":{"catalog_sha256":"a69f2e270c62191be30309d10fffcacc27b960487784a984c18ed69081c84a3b"}}

-- ocservia:transition=1:0->2:0
-- ocservia:step=001:checkpoint
SELECT 1;
-- ocservia:end-step
-- ocservia:step=002:legacy_cleanup
-- ocservia:metadata={"catalog_sha256":"096ad0fc5ad94f3d8c93fb94ffb9935b65a40bab31bc15cc84b7961d7755800c"}
DROP TABLE public.schema_migrations, public.schema_snapshot_origin;
DELETE FROM public.schema_revisions WHERE epoch=1;
-- ocservia:end-step
-- ocservia:end-transition
-- ocservia:revision=1
-- ocservia:checkpoint={"checksum":"672fe4b774df7ff5145be3a6cc78334aee33894a494de24d7a3162c67647b4c2","steps":1}
-- ocservia:step=001:local_auth_attempt_purpose_keys
-- ocservia:metadata={"catalog_sha256":"24b043c5ad24ba23900a65a537b4c6f2c1c769a0a07b8899c2caeb81af43cc69"}
-- Password changes keep attempt state apart from login names (see authstore).
ALTER TABLE public.local_auth_attempts DROP CONSTRAINT local_auth_attempts_username_check, ADD CONSTRAINT local_auth_attempts_username_check CHECK (((octet_length(username) >= 1) AND (octet_length(username) <= 128) AND (((username COLLATE "C") ~ '^[a-z0-9][a-z0-9._-]*$'::text) OR ((username COLLATE "C") ~ '^password-change:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'::text))));
-- ocservia:end-step
-- ocservia:end-revision
-- ocservia:revision=2
-- ocservia:checkpoint={"checksum":"699c3f9a894eca9a2ec2ba0070ed705e21b88b6a9e7621de5f282b04a343af23","steps":1}
-- ocservia:step=001:single_admin_authorization
-- ocservia:metadata={"catalog_sha256":"2701cf9af3ad577f17af4a919ddc49c1b74b03561ade5bc255f9487b1b53be68"}
ALTER TABLE public.agent_rollouts ALTER COLUMN approval_id DROP NOT NULL;
ALTER TABLE public.config_apply_operations ALTER COLUMN approval_id DROP NOT NULL;
ALTER TABLE public.node_privd_attestation_keys ALTER COLUMN registration_credential_id DROP NOT NULL;
-- ocservia:end-step
-- ocservia:end-revision
-- ocservia:revision=3
-- ocservia:checkpoint={"checksum":"6e6297b67be8e0d78e7a884d050d4537028f8296ad991735e946446401a3afaf","steps":1}
-- ocservia:step=001:minimal_closed_loop_cleanup
-- ocservia:metadata={"catalog_sha256":"f021bb005b544187af3888a0ea3c65e5e6d7abec18b8f117a4b4236f90c139d3"}
CREATE TABLE public.workspace_admins (
    workspace_id uuid NOT NULL REFERENCES public.workspaces(id) ON DELETE CASCADE,
    identity_id uuid NOT NULL REFERENCES public.identities(id) ON DELETE RESTRICT,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    PRIMARY KEY (workspace_id, identity_id)
);
INSERT INTO public.workspace_admins (workspace_id, identity_id, created_at)
SELECT DISTINCT ON (rb.workspace_id, rb.identity_id) rb.workspace_id, rb.identity_id, rb.created_at
FROM public.role_bindings rb
WHERE rb.role_name = 'PlatformAdmin' AND rb.resource_type = 'workspace'
ORDER BY rb.workspace_id, rb.identity_id, rb.created_at
ON CONFLICT (workspace_id, identity_id) DO NOTHING;
GRANT SELECT, INSERT ON TABLE public.workspace_admins TO ocservia_app;
ALTER TABLE public.artifact_operations
    ADD COLUMN requester_identity_id uuid REFERENCES public.identities(id) ON DELETE RESTRICT;
UPDATE public.artifact_operations a
SET requester_identity_id = r.requester_id
FROM public.approval_requests r
WHERE r.id = a.approval_id;
ALTER TABLE public.artifact_operations
    ALTER COLUMN requester_identity_id SET NOT NULL,
    DROP COLUMN approval_id;
ALTER TABLE public.audit_events DROP CONSTRAINT audit_events_approval_id_fkey;
ALTER TABLE public.certificates DROP COLUMN issue_approval_id;
ALTER TABLE public.config_apply_operations DROP COLUMN approval_id;
ALTER TABLE public.agent_rollouts DROP COLUMN approval_id;
ALTER TABLE public.agent_upgrade_operations DROP COLUMN approval_id;
ALTER TABLE public.node_privd_attestation_keys DROP COLUMN registration_credential_id;
ALTER TABLE public.local_auth_bootstrap DROP CONSTRAINT local_initialization_state, DROP COLUMN approver_identity_id, DROP COLUMN completion_pending, DROP COLUMN completed_at;
DROP TABLE public.approval_batch_items;
DROP TABLE public.approval_authority_resources;
DROP TABLE public.batch_operation_items;
DROP TABLE public.batch_operations;
DROP TABLE public.role_bindings;
DROP TABLE public.roles;
DROP TABLE public.approval_requests;
DROP TABLE public.scheduler_leases;
DROP TABLE public.enrollment_tokens;
DROP TABLE public.privd_attestation_enrollment_credentials;
DROP TABLE public.upstream_sync_records;
-- ocservia:end-step
-- ocservia:end-revision
-- ocservia:revision=4
-- ocservia:checkpoint={"checksum":"847e1c10216a38d0ac5cf1585c12664f66961bb7424c573127d1120c986e3549","steps":1}
-- ocservia:step=001:backlog_and_expiry_partial_indexes
-- ocservia:metadata={"catalog_sha256":"892cda01819ab42b44f5bcbfa5fe0e9fc7f5e644cc658d9f8b214d419d67001a"}
CREATE INDEX commands_queued_expiry_idx ON public.commands USING btree (expires_at) WHERE (state = 'queued'::text);
CREATE INDEX operations_node_backlog_idx ON public.operations USING btree (node_id) WHERE (state = ANY (ARRAY['queued'::text, 'offline_pending'::text, 'dispatched'::text, 'accepted'::text, 'running'::text, 'unknown'::text]));
CREATE INDEX operations_workspace_backlog_idx ON public.operations USING btree (workspace_id) WHERE (state = ANY (ARRAY['queued'::text, 'offline_pending'::text, 'dispatched'::text, 'accepted'::text, 'running'::text, 'unknown'::text]));
-- ocservia:end-step
-- ocservia:end-revision
-- ocservia:revision=5
-- ocservia:checkpoint={"checksum":"fef9ae411b86cb145d61ee93e3b468711767f6b274b07dc9ae309e32d544971c","steps":1}
-- ocservia:step=001:vpn_session_history
-- ocservia:metadata={"catalog_sha256":"02236adc015c56f8c8cd7e79d687565ea941736cce80e33e21d3f3ee25ca6614"}
ALTER TABLE public.node_sessions ADD COLUMN vpn_ip inet;
CREATE TABLE public.vpn_session_history (
    node_id uuid NOT NULL REFERENCES public.nodes(id) ON DELETE CASCADE,
    session_id text NOT NULL,
    connected_at timestamptz NOT NULL,
    username text NOT NULL,
    client_ip inet NOT NULL,
    vpn_ip inet,
    last_seen_at timestamptz NOT NULL,
    absent_at timestamptz,
    bytes_in bigint NOT NULL CHECK (bytes_in >= 0),
    bytes_out bigint NOT NULL CHECK (bytes_out >= 0),
    PRIMARY KEY (node_id, session_id, connected_at)
);
CREATE INDEX vpn_session_history_open_idx ON public.vpn_session_history(node_id) WHERE absent_at IS NULL;
CREATE INDEX vpn_session_history_closed_idx ON public.vpn_session_history(absent_at) WHERE absent_at IS NOT NULL;
CREATE INDEX vpn_session_history_user_time_idx ON public.vpn_session_history(username, connected_at DESC);
CREATE TABLE public.vpn_session_events (node_id uuid NOT NULL REFERENCES public.nodes(id) ON DELETE CASCADE,
 event_id uuid NOT NULL, session_id text NOT NULL, username text NOT NULL, client_ip inet NOT NULL,
 vpn_ip inet NOT NULL, connected_at timestamptz, observed_at timestamptz NOT NULL,
 bytes_in bigint CHECK(bytes_in>=0), bytes_out bigint CHECK(bytes_out>=0), PRIMARY KEY(node_id,event_id), event_type text NOT NULL CHECK(event_type IN ('connect','disconnect')));
CREATE TABLE public.vpn_network_history (node_id uuid NOT NULL REFERENCES public.nodes(id) ON DELETE CASCADE,
 event_id uuid NOT NULL, session_id text NOT NULL, username text NOT NULL, client_ip inet NOT NULL,
 vpn_ip inet NOT NULL, connected_at timestamptz, observed_at timestamptz NOT NULL,
 bytes_in bigint CHECK(bytes_in>=0), bytes_out bigint CHECK(bytes_out>=0), PRIMARY KEY(node_id,event_id), destination_ip inet NOT NULL, destination_port integer CHECK(destination_port BETWEEN 0 AND 65535), protocol text NOT NULL, flow_started_at timestamptz NOT NULL);
CREATE INDEX vpn_session_events_time_idx ON public.vpn_session_events(observed_at DESC);
CREATE INDEX vpn_network_time_idx ON public.vpn_network_history(observed_at DESC);
CREATE INDEX vpn_network_destination_time_idx ON public.vpn_network_history(destination_ip,observed_at DESC);
CREATE INDEX vpn_network_node_time_idx ON public.vpn_network_history(node_id,observed_at DESC,event_id DESC);
CREATE INDEX vpn_network_user_time_idx ON public.vpn_network_history(username,observed_at DESC);
CREATE INDEX vpn_session_events_node_time_idx ON public.vpn_session_events(node_id,observed_at DESC,event_id DESC);
CREATE INDEX vpn_session_events_user_time_idx ON public.vpn_session_events(username,observed_at DESC);
CREATE INDEX vpn_session_history_node_time_idx ON public.vpn_session_history(node_id,connected_at DESC,session_id DESC);
CREATE FUNCTION public.prune_vpn_history() RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path TO pg_catalog, public AS $$
 DELETE FROM public.vpn_session_history WHERE (node_id,session_id,connected_at) IN (
   SELECT node_id,session_id,connected_at FROM public.vpn_session_history
   WHERE absent_at < statement_timestamp() - interval '90 days'
   ORDER BY absent_at LIMIT 1024 FOR UPDATE SKIP LOCKED
 );
 DELETE FROM public.vpn_network_history WHERE (node_id,event_id) IN (
   SELECT node_id,event_id FROM public.vpn_network_history WHERE observed_at < statement_timestamp()-interval '30 days'
   ORDER BY observed_at LIMIT 4096 FOR UPDATE SKIP LOCKED);
 DELETE FROM public.vpn_session_events WHERE (node_id,event_id) IN (
   SELECT node_id,event_id FROM public.vpn_session_events WHERE observed_at < statement_timestamp()-interval '90 days'
   ORDER BY observed_at LIMIT 1024 FOR UPDATE SKIP LOCKED);
$$;
REVOKE ALL ON FUNCTION public.prune_vpn_history() FROM PUBLIC;
-- ocservia:end-step
-- ocservia:end-revision
-- ocservia:revision=6
-- ocservia:checkpoint={"checksum":"b8f7bcb21262ae608412255146dd0fec5f756d3c661ce4126591624f2d53e4e9","steps":1}
-- ocservia:step=001:telemetry_partition_maintenance
-- ocservia:metadata={"catalog_sha256":"84e4ca0586c9c1647e193af7430649b9d058e323f8044dfda56c3092451b6221"}
-- Remove the DEFAULT partition from the routing table while retaining its historical rows.
ALTER TABLE public.telemetry_samples DETACH PARTITION public.telemetry_samples_default;
GRANT SELECT ON public.telemetry_samples_default TO ocservia_app;
DROP FUNCTION public.telemetry_drop_expired_partitions(timestamptz);

CREATE TABLE public.telemetry_partition_retirements (
    partition_name text PRIMARY KEY
        CHECK (partition_name ~ '^telemetry_samples_[1-9][0-9]{3}(0[1-9]|1[0-2])$'),
    relation_oid regclass NOT NULL UNIQUE
);

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
REVOKE ALL ON FUNCTION public.telemetry_ensure_upcoming_partitions() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.telemetry_ensure_upcoming_partitions() TO ocservia_app;

CREATE TABLE public.telemetry_rollup_queue (
    bucket_at timestamptz NOT NULL,
    node_id uuid NOT NULL,
    metric text NOT NULL,
    PRIMARY KEY (bucket_at, node_id, metric)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON public.telemetry_rollup_queue TO ocservia_app;

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
-- ocservia:end-step
-- ocservia:end-revision
