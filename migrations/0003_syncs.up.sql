-- Syncs (the data-model redesign foundation). See docs/data-model.md.
--
-- A *game* is just a title; the thing that actually mirrors together is a
-- *sync*: a specific set of (node, save-file) members. One game can have many
-- independent syncs as long as they do not share a (device, file): e.g. Super
-- Metroid's two unrelated streams {bob-deck<->bob-mister} and Alice's own
-- stream must never cross.
--
-- This migration is ADDITIVE: it adds the syncs/sync_members layer alongside
-- the existing game-based tables (game_paths, active_bindings, manifest,
-- sync_log), which keep working unchanged. Later slices re-point the engine,
-- runtime tables, and web onto syncs.

CREATE TABLE syncs (
    id      text PRIMARY KEY,
    game_id text NOT NULL REFERENCES games (id) ON DELETE CASCADE,
    name    text NOT NULL DEFAULT ''
);

-- sync_members: the (node, file) members of a sync.
--
-- PRIMARY KEY (sync_id, node_id) -> a node appears at most once in a given sync.
--
-- UNIQUE (node_id, path) across ALL sync_members -> a given (device, file) lives
-- in at most one sync. THIS is the core invariant: it lets multi-slot work
-- (same node + a different path -> a different sync) while forbidding the same
-- file from being claimed by two syncs.
CREATE TABLE sync_members (
    sync_id text NOT NULL REFERENCES syncs (id) ON DELETE CASCADE,
    node_id text NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    path    text NOT NULL,
    PRIMARY KEY (sync_id, node_id),
    UNIQUE (node_id, path)
);
