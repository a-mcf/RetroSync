-- Registry tables (slow-changing, human-edited). See docs/data-model.md.

CREATE TABLE users (
    id      text PRIMARY KEY,
    display text NOT NULL,
    pw_hash text NOT NULL,
    role    text NOT NULL CHECK (role IN ('user', 'admin'))
);

CREATE TABLE nodes (
    id            text PRIMARY KEY,
    owner_user_id text NULL REFERENCES users (id),
    display       text NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('deck', 'mister', 'anbernic', 'generic')),
    reach         text NOT NULL CHECK (reach IN ('syncthing-share', 'ssh')),
    -- reach_config holds only non-secret reach info plus a secret_ref pointer
    -- for ssh nodes. NEVER a cleartext password or key (see docs/auth.md).
    reach_config  jsonb NOT NULL,
    last_seen_at  timestamptz NULL
);

CREATE TABLE games (
    id      text PRIMARY KEY,
    display text NOT NULL,
    system  text NOT NULL,
    notes   text NOT NULL DEFAULT ''
);

CREATE TABLE game_paths (
    game_id text NOT NULL REFERENCES games (id) ON DELETE CASCADE,
    node_id text NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    path    text NOT NULL,
    PRIMARY KEY (game_id, node_id)
);

-- TODO(slice-runtime): active_bindings, sync_log, manifest tables.
