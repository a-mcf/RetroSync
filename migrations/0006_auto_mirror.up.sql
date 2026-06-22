-- Auto-mirror cutover (slice-18). The explicit primary/binding/take-over model
-- is retired: every sync now auto-mirrors. The daemon watches EVERY sync each
-- poll, finds which member changed vs the manifest, and fans the lone changed
-- member out to the others (0 changed = noop, 1 = propagate, 2+ = conflict +
-- pause). There is no "active session", no primary, no direction — so the
-- active_bindings table is gone and the per-sync runtime state (conflict +
-- last-synced) moves onto the syncs table itself.
--
-- DEV-ONLY re-model: there is no production data, so we drop active_bindings
-- outright rather than migrate rows. manifest (sync_id, node_id, ...) and
-- sync_log (sync_id, ...) are unchanged — they already key off sync_id.

-- Per-sync runtime state, formerly on active_bindings:
--   conflict_at  non-null => the sync forked (2+ members changed); mirroring is
--                paused until a human resolves it. Nil = mirroring normally.
--   last_synced  time of the last successful mirror pass; nil if never synced.
ALTER TABLE syncs ADD COLUMN conflict_at timestamptz NULL;
ALTER TABLE syncs ADD COLUMN last_synced timestamptz NULL;

-- active_bindings is fully orphaned (no primary/direction/session anymore).
DROP TABLE IF EXISTS active_bindings;
