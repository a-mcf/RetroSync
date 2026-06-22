-- Drop game_paths (slice-registry-sync). The registry now manages a game's
-- playable members through syncs + sync_members (the unit of mirroring); the
-- engine and runtime tables have keyed off sync_id since slice 4. game_paths is
-- fully orphaned, so we retire it.
--
-- DEV-ONLY: there is no production data, so we simply drop the table rather than
-- migrate any rows out of it. The down script recreates it per the 0001 shape.

DROP TABLE IF EXISTS game_paths;
