CREATE TABLE IF NOT EXISTS scheduler_leadership (
    id integer PRIMARY KEY CHECK (id = 1),
    instance_id uuid NOT NULL,
    incarnation bigint NOT NULL CHECK (incarnation >= 0),
    epoch bigint NOT NULL CHECK (epoch >= 0),
    lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The single-row lease starts expired so the first scheduler instance takes
-- over by incrementing the epoch from zero. Epoch values are monotonically
-- increasing and are never reused after a takeover, so a schema rollback
-- retains this table and its epoch state (see the down script). Re-running
-- this migration over that retained state must re-register version 24
-- without re-seeding the epoch: the ON CONFLICT guard keeps the existing
-- row exactly as the last leader left it.
INSERT INTO scheduler_leadership (id, instance_id, incarnation, epoch, lease_until)
VALUES (1, '00000000-0000-0000-0000-000000000000', 0, 0, '-infinity')
ON CONFLICT (id) DO NOTHING;
