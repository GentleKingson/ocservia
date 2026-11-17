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
