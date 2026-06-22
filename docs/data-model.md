# Data model

> **Implemented.** All tables below exist as embedded migrations applied at
> startup, behind the storage-agnostic `internal/store.Store` (Postgres + in-memory
> fake, one conformance suite). The registry (users, nodes, games, syncs,
> sync_members) is edited through the admin UI; game/node/sync ids are slugs,
> auto-generated from the display/name when omitted (manual id overrides).
> Per-sync runtime state (`syncs.conflict_at` / `syncs.last_synced`), `manifest`,
> and `sync_log` are written by the engine/daemon. See the "Storage choice" note
> below.
>
> **Note (auto-mirror, slice 18):** the old `active_bindings` table (one row per
> game/sync in an explicit "play session", with a primary node + direction) was
> **dropped** (migration 0006). There is no primary, no binding, no
> activate/deactivate/take-over anymore — every sync auto-mirrors. The two pieces
> of per-sync runtime state that lived on the binding (`conflict_at`,
> `last_synced`) moved onto the `syncs` row itself.
>
> **Note:** `game_paths` (one row per `(game, node)`) was the original
> registry-of-paths table. It has been **retired** (migration 0005): a game's
> playable members now live in `sync_members` (a `(node, path)` member of a
> `sync`), which is the unit of mirroring. The section below documents the
> current `syncs` / `sync_members` shape.

Two stores: a **registry** (slow-changing, human-edited) and a **runtime state** (fast-changing, machine-edited). They live in the same Postgres database (see "Storage choice"); the conceptual split matters more than the physical.

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

Note: no `mister_path` or canonical path here. A game's playable members live in
its `syncs`' `sync_members`.

### `syncs`

A *sync* is a mirror group: a specific set of `(node, save-file)` members that
sync together. It belongs to one game, but a game may have MANY independent syncs
(e.g. two unrelated streams of the same title) so long as they do not share a
`(node, path)` member.

| field        | type | notes                                                        |
|--------------|------|--------------------------------------------------------------|
| id           | text | slug, e.g. `sm-bob`                                           |
| game_id      | text | FK → games, `ON DELETE CASCADE`                              |
| name         | text | free-form label, e.g. "Bob's stream"                         |
| conflict_at  | ts   | nullable; set when the sync forked (2+ members changed) — mirroring is paused until a human resolves it |
| last_synced  | ts   | nullable; time of the last successful mirror pass            |

`conflict_at` / `last_synced` are the per-sync runtime state (formerly on
`active_bindings`). The runtime tables (`manifest`, `sync_log`) key off `sync_id`;
the engine, daemon, and web all operate on a `sync` and its members. A sync with
`conflict_at IS NULL` is mirroring normally; a non-null `conflict_at` pauses it.

### `sync_members`

The replacement for `game_paths` (and SGM-Helper's per-system layout
assumptions). One row per `(node, save-file)` member of a sync — the thing you
want playable.

| field    | type | notes                                                          |
|----------|------|----------------------------------------------------------------|
| sync_id  | text | FK → syncs, `ON DELETE CASCADE`                                |
| node_id  | text | FK → nodes, `ON DELETE CASCADE`                                |
| path     | text | path on that node — relative to a node-defined root, see below |

- **PK `(sync_id, node_id)`** — a node appears at most once per sync.
- **`UNIQUE (node_id, path)`** across all members — a given `(device, file)` lives
  in at most one sync. This is the core invariant: it permits multi-slot (same
  node, different path → different sync) while forbidding the same file from being
  claimed by two syncs.

`path` is **relative to a node-defined save root** (kept in `reach_config` or a
separate `save_roots` field) so that:

- For `syncthing-share` nodes, retrosync resolves to `<reach_config.path>/<save_root>/<path>` on the local filesystem.
- For `ssh` nodes, retrosync resolves to `<save_root>/<path>` on the remote.

This keeps registry rows portable if a path prefix moves.

Example:

```
games:
  super-metroid:
    syncs:
      sm-bob (Bob's stream):
        bob-deck:           retroarch/saves/Super Metroid.srm
        living-room-mister: SNES/Super Metroid.sav
      sm-alice (Alice's stream):
        alice-deck:         Emulation/saves/snes9x/Super Metroid.srm
```

## Runtime state

The per-sync runtime state (`conflict_at`, `last_synced`) lives on the `syncs`
row (see above) — there is no separate bindings table under auto-mirror.

### `sync_log` (append-only)

| field     | type      | notes                                  |
|-----------|-----------|----------------------------------------|
| id        | bigserial | PK; surrogate key, also the tiebreaker for "most recent" ordering |
| ts        | ts        |                                        |
| sync_id   | text      | FK → syncs, `ON DELETE CASCADE`        |
| from_node | text      | unconstrained text by design (see below) |
| to_node   | text      | unconstrained text by design (see below) |
| bytes     | int       |                                        |
| src_mtime | ts        |                                        |
| dst_mtime | ts        | mtime *before* overwrite               |
| outcome   | text      | `ok`, `noop`, `conflict`, `error`      |
| message   | text      | error detail                           |

Used for the UI history panel and conflict diagnostics. One row per directional copy; a single mirror pass can produce N rows when fanning the lone changed member out to multiple others.

`from_node`/`to_node` are **plain text, not foreign keys**, on purpose: the log is historical and a node may be removed later. We never want a node deletion to erase or block log history, so those columns are left unconstrained. (`sync_id`, by contrast, *is* an FK and cascades, so deleting a sync cleans up its history.)

### `manifest` (per side)

| field        | type | notes                                              |
|--------------|------|----------------------------------------------------|
| sync_id      | text |                                                    |
| node_id      | text |                                                    |
| mtime        | ts   | fast-path gate (with size)                         |
| size         | int  | fast-path gate (with mtime)                        |
| sha256       | text | content truth; recorded on every manifest write    |
| last_checked | ts   |                                                    |

PK: (sync_id, node_id). The poll loop updates this on every write, recording the
member's content `sha256` alongside mtime/size.

**Hash-backed change detection.** Detection is two-tier:

1. **mtime+size fast gate.** `Stat` vs the manifest. Equal → unchanged, *no hash
   computed* (a noop poll stays hash-free).
2. **sha256 truth, only when the stat differs.** The member's current content hash
   vs the manifest's stored `sha256`:
   - **equal** → a *touch* (mtime moved, bytes identical): reconcile the manifest's
     mtime/size, do **not** count it as changed.
   - **differs** → a real content change.

The **changed set** then drives the mirror by the **distinct content hashes** of
the changed members: 0 changed = noop; exactly **one** distinct hash = agreed
content → propagate to any member lacking it; **two or more** distinct hashes = a
genuine fork → conflict. (mtime+size alone false-positives on a touch and flags a
fork when two devices coincidentally reach the *same* bytes; the hash makes both
calls accurate without weakening any data-loss guarantee — a distinct-hash changer
is never silently overwritten.)

## Storage choice

- **Postgres** is the store, running as **CNPG** (CloudNativePG) in production. It gives us the `UNIQUE (node_id, path)` invariant on `sync_members`, JSONB for `reach_config`, `ON DELETE CASCADE` for `syncs` / `sync_members` / `manifest` / `sync_log`, and a managed/HA operator in k8s.
- All business logic depends on a Go **`Store` interface** (`internal/store`), never on the driver. There are two implementations: a thread-safe in-memory store (used by fast unit tests) and a pgx v5 Postgres store. Both run an identical **conformance suite** (`internal/store/storetest`) so they cannot drift in behavior.
- Access is via **pgx v5** with hand-written parameterized queries — no ORM, no sqlc. Migrations are embedded (`embed.FS`) and applied programmatically at startup and in tests (`internal/migrate`).
- Integration tests run against a real Postgres started with **podman** (gated by a build tag / `DATABASE_URL`); plain `go test ./...` needs no database.
- **SQLite** was the original default — single-file and simple — but Postgres/CNPG fits the k8s sidecar deployment better. **JSON files** were also considered: fine for the registry, painful for `sync_log` and the unique constraint. Skipped.
