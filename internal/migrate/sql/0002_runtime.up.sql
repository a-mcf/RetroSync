-- Runtime state tables (fast-changing, machine-edited). See docs/data-model.md
-- and docs/state-machine.md.

-- active_bindings: one row per game that is currently in an active play
-- session. The game_id PRIMARY KEY is the "one active session per game"
-- invariant — the most important constraint in the system. A game with no row
-- here is idle (backup-only); a row makes it active; a non-null conflict_at
-- flags the conflict (paused) state.
--
-- The game_id and primary_node foreign keys are intentionally left at the
-- default NO ACTION (not CASCADE): the database refuses to delete a game or a
-- node while it is referenced by an active binding. This enforces api.md's
-- "delete forbidden if active" at the storage layer for both impls.
CREATE TABLE active_bindings (
    game_id      text PRIMARY KEY REFERENCES games (id),
    primary_node text NOT NULL REFERENCES nodes (id),
    started_at   timestamptz NOT NULL DEFAULT now(),
    direction    text NOT NULL
        CHECK (direction = 'from-primary' OR direction LIKE 'from-peer-%'),
    peer_scope   text NOT NULL DEFAULT 'all-configured',
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
    game_id   text NOT NULL REFERENCES games (id) ON DELETE CASCADE,
    from_node text,
    to_node   text,
    bytes     bigint,
    src_mtime timestamptz,
    dst_mtime timestamptz,
    outcome   text NOT NULL CHECK (outcome IN ('ok', 'noop', 'conflict', 'error')),
    message   text NOT NULL DEFAULT ''
);

CREATE INDEX sync_log_game_id_ts_idx ON sync_log (game_id, ts DESC, id DESC);

-- manifest: last-known file state per (game, node). The poll loop updates this;
-- conflict detection compares last-known mtime per node with current. Cascades
-- on game/node deletion since it is purely derived state.
CREATE TABLE manifest (
    game_id      text NOT NULL REFERENCES games (id) ON DELETE CASCADE,
    node_id      text NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    mtime        timestamptz,
    size         bigint,
    sha256       text NULL,
    last_checked timestamptz,
    PRIMARY KEY (game_id, node_id)
);
