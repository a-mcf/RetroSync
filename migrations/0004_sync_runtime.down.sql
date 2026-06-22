-- Roll back the sync-runtime cutover: drop the sync-keyed runtime tables and
-- recreate the game-keyed runtime tables exactly as migration 0002 created them.
-- (Dev-only re-model; no data is preserved either way.)

DROP TABLE IF EXISTS manifest;
DROP TABLE IF EXISTS sync_log;
DROP TABLE IF EXISTS active_bindings;

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

CREATE TABLE manifest (
    game_id      text NOT NULL REFERENCES games (id) ON DELETE CASCADE,
    node_id      text NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    mtime        timestamptz,
    size         bigint,
    sha256       text NULL,
    last_checked timestamptz,
    PRIMARY KEY (game_id, node_id)
);
