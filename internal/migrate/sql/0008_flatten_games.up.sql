-- Flatten the games entity (slice-21). See docs/data-model.md.
--
-- The games table held no paths or used metadata — just a label you had to
-- create before you could make a sync. So we drop it: the *sync* becomes the
-- atomic entity and "game" becomes a free-text LABEL on the sync (a display
-- grouping; a later slice infers it from save filenames). No games table, no FK.
--
-- DEV-ONLY: there is no production data. We add syncs.game, copy each sync's
-- label from its old game's display where convenient, then drop the game_id FK
-- column and the games table.

-- 1. Add the free-text label column (default '' so existing rows are valid).
ALTER TABLE syncs ADD COLUMN game text NOT NULL DEFAULT '';

-- 2. Migrate the label from the old game's display (best-effort, dev-only).
UPDATE syncs sy
   SET game = g.display
  FROM games g
 WHERE sy.game_id = g.id;

-- 3. Drop the FK column; the sync no longer references a game row.
ALTER TABLE syncs DROP COLUMN game_id;

-- 4. Drop the now-orphaned games table (nothing references it anymore).
DROP TABLE IF EXISTS games;
