-- Fencing epochs must never be reused across the database's whole life, so
-- this rollback is deliberately expand-only: the scheduler_leadership table
-- and its epoch row survive, and only the version registration is removed by
-- the caller. Dropping the table would let a later re-upgrade re-seed epoch
-- zero and hand historical epoch values to new leaders. The matching up
-- script tolerates the retained table and row with IF NOT EXISTS and ON
-- CONFLICT DO NOTHING.
SELECT 1;
