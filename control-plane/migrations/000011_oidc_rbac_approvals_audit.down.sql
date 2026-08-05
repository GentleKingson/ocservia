DROP TRIGGER IF EXISTS audit_checkpoints_append_only ON audit_checkpoints;
DROP FUNCTION IF EXISTS reject_audit_checkpoint_mutation();
DROP TABLE IF EXISTS break_glass_uses;
DROP TABLE IF EXISTS security_alerts;
DROP TABLE IF EXISTS audit_checkpoints;
ALTER TABLE audit_events
    DROP CONSTRAINT IF EXISTS audit_event_hash_size,
    DROP CONSTRAINT IF EXISTS audit_previous_hash_size,
    DROP COLUMN IF EXISTS error_type,
    DROP COLUMN IF EXISTS after_summary,
    DROP COLUMN IF EXISTS before_summary,
    DROP COLUMN IF EXISTS approval_id,
    DROP COLUMN IF EXISTS command_id,
    DROP COLUMN IF EXISTS node_id,
    DROP COLUMN IF EXISTS source_session_id;
DROP TABLE IF EXISTS approval_requests;
DROP TABLE IF EXISTS role_bindings;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS auth_sessions;
DROP TABLE IF EXISTS identities;
