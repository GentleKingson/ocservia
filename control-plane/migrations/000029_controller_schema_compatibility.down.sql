BEGIN;
DROP TABLE IF EXISTS controller_schema_compatibility;
DELETE FROM schema_migrations WHERE version = 29;
COMMIT;
