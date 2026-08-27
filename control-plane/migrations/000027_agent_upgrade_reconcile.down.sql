BEGIN;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM agent_upgrade_operations)
       OR EXISTS (SELECT 1 FROM node_agent_upgrade_results) THEN
        RAISE EXCEPTION 'cannot roll back agent upgrade reconciliation while agent upgrade history exists';
    END IF;
END
$$;

ALTER TABLE node_observed_snapshots DROP COLUMN architecture;

DROP TABLE node_agent_upgrade_results;
DROP TABLE agent_upgrade_operations;

COMMIT;
