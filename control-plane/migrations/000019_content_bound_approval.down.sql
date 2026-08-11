DROP TABLE approval_batch_items;
ALTER TABLE certificates DROP COLUMN version;
ALTER TABLE artifact_operations DROP COLUMN approval_id;
ALTER TABLE role_bindings DROP COLUMN approval_id;
ALTER TABLE role_bindings DROP CONSTRAINT role_bindings_resource_type_check;
ALTER TABLE role_bindings ADD CONSTRAINT role_bindings_resource_type_check CHECK (resource_type IN ('workspace','node','resource'));
DROP TABLE approval_authority_resources;
ALTER TABLE approval_requests DROP COLUMN authority_snapshot_at;
ALTER TABLE approval_requests DROP CONSTRAINT approval_requests_request_summary_check;
ALTER TABLE approval_requests ADD CONSTRAINT approval_requests_request_summary_check CHECK (
    request_summary IS NULL OR jsonb_typeof(request_summary) IN ('array','object')
);
