-- Recreate game_paths per the original 0001 shape (dev-only rollback). No data
-- is restored — the table comes back empty.

CREATE TABLE game_paths (
    game_id text NOT NULL REFERENCES games (id) ON DELETE CASCADE,
    node_id text NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    path    text NOT NULL,
    PRIMARY KEY (game_id, node_id)
);
