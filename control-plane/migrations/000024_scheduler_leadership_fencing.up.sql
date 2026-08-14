CREATE TABLE scheduler_leadership (
    id integer PRIMARY KEY CHECK (id = 1),
    instance_id uuid NOT NULL,
    incarnation bigint NOT NULL CHECK (incarnation >= 0),
    epoch bigint NOT NULL CHECK (epoch >= 0),
    lease_until timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The single-row lease starts expired so the first scheduler instance takes
-- over by incrementing the epoch from zero. Epoch values are monotonically
-- increasing and are never reused after a takeover.
INSERT INTO scheduler_leadership (id, instance_id, incarnation, epoch, lease_until)
VALUES (1, '00000000-0000-0000-0000-000000000000', 0, 0, '-infinity');
