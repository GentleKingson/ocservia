-- Per-node fencing epochs must never be reused across the database's whole
-- life, so this rollback is deliberately expand-only: the
-- connection_owner_fencing table and every node's epoch row survive, and
-- only the version registration is removed by the caller. Dropping the table
-- would let a later re-upgrade restart epochs at one and hand historical
-- epoch values to new connection owners. The matching up script tolerates
-- the retained table with IF NOT EXISTS and never seeds rows.
SELECT 1;
