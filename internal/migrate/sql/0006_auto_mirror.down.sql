-- Roll back the auto-mirror cutover: recreate active_bindings per the 0004
-- shape and drop the per-sync runtime columns. (Dev-only re-model; no data is
-- restored either way — the table comes back empty.)

CREATE TABLE active_bindings (
    sync_id      text PRIMARY KEY REFERENCES syncs (id) ON DELETE CASCADE,
    primary_node text NOT NULL REFERENCES nodes (id),
    started_at   timestamptz NOT NULL DEFAULT now(),
    direction    text NOT NULL
        CHECK (direction = 'from-primary' OR direction LIKE 'from-peer-%'),
    conflict_at  timestamptz NULL,
    last_synced  timestamptz NULL
);

ALTER TABLE syncs DROP COLUMN IF EXISTS last_synced;
ALTER TABLE syncs DROP COLUMN IF EXISTS conflict_at;
