ALTER TABLE commands
    ADD COLUMN details_compacted_at timestamptz,
    ADD COLUMN envelope_sha256 bytea,
    ADD CONSTRAINT commands_compaction_evidence CHECK (
      (details_compacted_at IS NULL AND envelope_sha256 IS NULL) OR
      (details_compacted_at IS NOT NULL AND envelope_sha256 IS NOT NULL AND octet_length(envelope_sha256)=32
       AND state IN ('succeeded','failed','rejected','expired','rolled_back','superseded')));
CREATE INDEX commands_retention_idx ON commands(updated_at,id) WHERE details_compacted_at IS NULL;
COMMENT ON COLUMN commands.details_compacted_at IS 'Expired terminal detail was compacted; envelope is evidence only, never dispatchable. Identity, idempotency and signed fences remain.';
UPDATE controller_schema_compatibility SET "current_schema"=38,minimum_compatible_controller_schema=38 WHERE singleton;
