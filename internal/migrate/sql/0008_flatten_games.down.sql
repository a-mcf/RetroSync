-- Roll back the games flatten (dev-only, best-effort). Recreates the games
-- table per the slice-1 shape and restores syncs.game_id as an FK. No original
-- game rows are restored; to keep the FK satisfiable, every existing sync is
-- re-pointed at a single placeholder game carrying its old label as the display.

-- 1. Recreate the games table (slice-1 / 0001_registry shape).
CREATE TABLE games (
    id      text PRIMARY KEY,
    display text NOT NULL,
    system  text NOT NULL,
    notes   text NOT NULL DEFAULT ''
);

-- 2. Seed a placeholder game so the restored FK has a target for every sync.
INSERT INTO games (id, display, system, notes)
VALUES ('unknown', 'Unknown', 'unknown', 'restored by 0008 down migration');

-- 3. Restore the FK column, defaulting every existing row to the placeholder.
ALTER TABLE syncs ADD COLUMN game_id text NOT NULL DEFAULT 'unknown'
    REFERENCES games (id) ON DELETE CASCADE;

-- 4. Drop the free-text label column.
ALTER TABLE syncs DROP COLUMN game;
