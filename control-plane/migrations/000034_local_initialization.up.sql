ALTER TABLE local_auth_bootstrap
    ADD COLUMN completion_pending boolean NOT NULL DEFAULT false,
    ADD COLUMN completed_at timestamptz,
    ADD COLUMN approver_identity_id uuid REFERENCES identities(id) ON DELETE RESTRICT;

-- Freeze upgrade eligibility once, not whenever a deployment loses an admin.
-- Historical/disabled elevated bindings also close this exception.
UPDATE local_auth_bootstrap b SET completion_pending = true
WHERE EXISTS (
    SELECT 1 FROM identities i JOIN local_credentials c ON c.identity_id=i.id
    JOIN role_bindings r ON r.identity_id=i.id
    WHERE i.id=b.identity_id AND i.issuer='local' AND i.subject=c.username
      AND i.disabled_at IS NULL AND r.workspace_id=b.workspace_id
      AND r.resource_type='workspace' AND r.role_name='PlatformAdmin'
) AND NOT EXISTS (
    SELECT 1 FROM role_bindings r WHERE r.workspace_id=b.workspace_id
      AND r.role_name IN ('PlatformAdmin','SecurityAdmin') AND r.identity_id<>b.identity_id
) AND EXISTS (
    SELECT 1 FROM audit_events a WHERE a.workspace_id=b.workspace_id
      AND a.resource_id=b.identity_id AND a.action='local_user.bootstrap'
      AND a.result='succeeded'
);

UPDATE local_auth_bootstrap SET completed_at=now() WHERE NOT completion_pending;

ALTER TABLE local_auth_bootstrap ADD CONSTRAINT local_initialization_state
    CHECK ((completion_pending AND completed_at IS NULL AND approver_identity_id IS NULL)
        OR (NOT completion_pending AND completed_at IS NOT NULL));

-- Do not allow old Controllers to bypass account protection or create new
-- single-admin bootstrap states after this migration.
-- Keep the runner's minimum-compatible schema at 34.
