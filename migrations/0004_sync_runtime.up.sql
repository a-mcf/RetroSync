-- Sync-runtime cutover: re-key the runtime tables (active_bindings, manifest,
-- sync_log) from game_id to sync_id. The unit of mirroring is now a *sync* (a
-- specific set of (node, save-file) members), not a *game*. See
-- docs/data-model.md and docs/state-machine.md.
--
-- This is a DEV-ONLY re-model: there is no production data, so we drop and
-- recreate the runtime tables rather than migrate rows. The registry tables
-- (users, nodes, games, game_paths) and the syncs/sync_members layer are left
-- untouched. game_paths is intentionally KEPT (the registry still manages it;
-- slice-registry-sync drops it later).

DROP TABLE IF EXISTS manifest;
DROP TABLE IF EXISTS sync_log;
DROP TABLE IF EXISTS active_bindings;

-- active_bindings: one row per SYNC that is currently in an active play session.
-- The sync_id PRIMARY KEY is the "one active session per sync" invariant — the
-- most important constraint in the system. A sync with no row here is idle
-- (backup-only); a row makes it active; a non-null conflict_at flags the
-- conflict (paused) state.
--
-- sync_id REFERENCES syncs ON DELETE CASCADE: a sync's runtime binding goes away
-- with the sync. primary_node is left at the default NO ACTION (not CASCADE):
-- the database refuses to delete a node while it is the active primary of a
-- binding (api.md "delete forbidden if active").
--
-- peer_scope is GONE: a sync's members ARE its scope now.
CREATE TABLE active_bindings (
    sync_id      text PRIMARY KEY REFERENCES syncs (id) ON DELETE CASCADE,
    primary_node text NOT NULL REFERENCES nodes (id),
    started_at   timestamptz NOT NULL DEFAULT now(),
    direction    text NOT NULL
        CHECK (direction = 'from-primary' OR direction LIKE 'from-peer-%'),
    conflict_at  timestamptz NULL,
    last_synced  timestamptz NULL
);

-- sync_log: append-only record of every directional copy. One sync pass can
-- produce N rows when fanning out from primary to multiple peers. Drives the UI
-- history panel and conflict diagnostics.
--
-- from_node/to_node are plain text, NOT foreign keys: these are historical
-- records and a node may be removed later. We never want a node deletion to
-- erase or block log history, so they are unconstrained by design.
CREATE TABLE sync_log (
    id        bigserial PRIMARY KEY,
    ts        timestamptz NOT NULL DEFAULT now(),
    sync_id   text NOT NULL REFERENCES syncs (id) ON DELETE CASCADE,
    from_node text,
    to_node   text,
    bytes     bigint,
    src_mtime timestamptz,
    dst_mtime timestamptz,
    outcome   text NOT NULL CHECK (outcome IN ('ok', 'noop', 'conflict', 'error')),
    message   text NOT NULL DEFAULT ''
);

CREATE INDEX sync_log_sync_id_ts_idx ON sync_log (sync_id, ts DESC, id DESC);

-- manifest: last-known file state per (sync, node). The poll loop updates this;
-- conflict detection compares last-known mtime per node with current. Cascades
-- on sync/node deletion since it is purely derived state.
CREATE TABLE manifest (
    sync_id      text NOT NULL REFERENCES syncs (id) ON DELETE CASCADE,
    node_id      text NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    mtime        timestamptz,
    size         bigint,
    sha256       text NULL,
    last_checked timestamptz,
    PRIMARY KEY (sync_id, node_id)
);
