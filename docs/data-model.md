# Data model

Two stores: a **registry** (slow-changing, human-edited) and a **runtime state** (fast-changing, machine-edited). They can live in the same SQLite DB; the conceptual split matters more than the physical.

## Registry

### `users`

| field    | type   | notes                                        |
|----------|--------|----------------------------------------------|
| id       | text   | slug, e.g. `bob`                             |
| display  | text   | "Bob"                                        |
| pw_hash  | text   | argon2 — see auth.md                         |
| role     | text   | `user` or `admin`                            |

### `nodes`

A node is any device that holds save files. Decks, MiSTers, Anbernics, the household NAS — all the same shape.

| field           | type   | notes                                                   |
|-----------------|--------|---------------------------------------------------------|
| id              | text   | slug, e.g. `bob-deck`, `living-room-mister`             |
| owner_user_id   | text   | FK → users; nullable for shared nodes (the MiSTer)      |
| display         | text   | "Bob's Deck"                                            |
| kind            | text   | `deck`, `mister`, `anbernic`, `generic`                 |
| reach           | text   | `syncthing-share` or `ssh`                              |
| reach_config    | json   | shape depends on `reach` (see below)                    |
| last_seen_at    | ts     | from syncthing API or own ping                          |

#### reach_config shapes

- `syncthing-share`: `{ "path": "/srv/syncthing/bob-deck-saves" }`
- `ssh`: `{ "host": "172.16.7.12", "user": "root", "secret_ref": "mister-1" }` — `secret_ref` is a key into the host's secret store (sops file, vault, etc).

### `games`

| field    | type | notes                                |
|----------|------|--------------------------------------|
| id       | text | slug, e.g. `super-metroid`           |
| display  | text | "Super Metroid"                      |
| system   | text | `snes`, `n64`, etc. — informational  |
| notes    | text | free-form                            |

Note: no `mister_path` or canonical path here. Paths live in `game_paths`.

### `game_paths`

The replacement for SGM-Helper's per-system layout assumptions. One row per (game, node) pair you want playable.

| field        | type | notes                                                                    |
|--------------|------|--------------------------------------------------------------------------|
| game_id      | text | FK → games                                                               |
| node_id      | text | FK → nodes                                                               |
| path         | text | path on that node — relative to a node-defined root, see below           |

PK: (game_id, node_id).

`path` is **relative to a node-defined save root** (kept in `reach_config` or a separate `save_roots` field) so that:

- For `syncthing-share` nodes, retrosync resolves to `<reach_config.path>/<save_root>/<path>` on the local filesystem.
- For `ssh` nodes, retrosync resolves to `<save_root>/<path>` on the remote.

This keeps registry rows portable if a path prefix moves.

Example:

```
games:
  super-metroid:
    paths:
      bob-deck:           retroarch/saves/Super Metroid.srm
      alice-deck:         Emulation/saves/snes9x/Super Metroid.srm
      living-room-mister: SNES/Super Metroid.sav
```

## Runtime state

### `active_bindings`

| field        | type | notes                                                              |
|--------------|------|--------------------------------------------------------------------|
| game_id      | text | PK; one row max per game (enforced unique)                         |
| primary_node | text | the node currently holding play authority                          |
| started_at   | ts   |                                                                    |
| direction    | text | `from-primary` (initial: primary wins on first sync) or `from-peer-<node_id>` |
| peer_scope   | text | `all-configured` (default) or csv of node ids                      |
| conflict_at  | ts   | nullable; set when a non-primary peer mutates mid-session          |
| last_synced  | ts   | last successful pass                                               |

Unique on `game_id` enforces "only one active session per game at a time."

### `sync_log` (append-only)

| field     | type | notes                                  |
|-----------|------|----------------------------------------|
| ts        | ts   |                                        |
| game_id   | text |                                        |
| from_node | text |                                        |
| to_node   | text |                                        |
| bytes     | int  |                                        |
| src_mtime | ts   |                                        |
| dst_mtime | ts   | mtime *before* overwrite               |
| outcome   | text | `ok`, `noop`, `conflict`, `error`      |
| message   | text | error detail                           |

Used for the UI history panel and conflict diagnostics. One row per directional copy; a single sync pass can produce N rows when fanning out from primary to multiple peers.

### `manifest` (per side)

| field        | type | notes                                              |
|--------------|------|----------------------------------------------------|
| game_id      | text |                                                    |
| node_id      | text |                                                    |
| mtime        | ts   |                                                    |
| size         | int  |                                                    |
| sha256       | text | computed lazily; size+mtime is the fast path       |
| last_checked | ts   |                                                    |

PK: (game_id, node_id). The poll loop updates this; conflict detection compares last-known mtime per node with current.

## Storage choice

- **SQLite** is the default. Simple, single-file, supports the unique constraint on active_bindings cleanly.
- **JSON files** were considered. Fine for the registry, painful for sync_log and the unique constraint. Skip.
