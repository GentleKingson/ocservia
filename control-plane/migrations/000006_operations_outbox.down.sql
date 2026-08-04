DROP TABLE IF EXISTS operation_events;
DROP TABLE IF EXISTS node_command_leases;
DROP TABLE IF EXISTS command_attempts;
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS commands;
DROP INDEX IF EXISTS operations_workspace_idempotency_idx;
ALTER TABLE operations
    DROP CONSTRAINT IF EXISTS operations_request_hash_size,
    DROP CONSTRAINT IF EXISTS operations_idempotency_pair,
    DROP COLUMN IF EXISTS completed_at,
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS request_hash,
    DROP COLUMN IF EXISTS idempotency_key;
