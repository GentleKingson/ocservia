DROP TABLE node_capabilities;
DROP TABLE node_endpoint_keys;
DROP TABLE enrollment_tokens;
ALTER TABLE nodes DROP COLUMN policy, DROP COLUMN labels;
ALTER TABLE nodes DROP CONSTRAINT nodes_status_check;
UPDATE nodes SET status = 'approved' WHERE status = 'active';
ALTER TABLE nodes
    ADD CONSTRAINT nodes_status_check
    CHECK (status IN ('pending', 'approved', 'revoked', 'offline'));
