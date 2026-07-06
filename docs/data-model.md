# Data model

> **Implemented.** All tables below exist as embedded migrations applied at
> startup, behind the storage-agnostic `internal/store.Store` (Postgres + in-memory
> fake, one conformance suite). The registry (users, nodes, syncs,
> sync_members) is edited through the admin UI; node/sync ids are slugs,
> auto-generated from the display/name when omitted (manual id overrides).
> Per-sync runtime state (`syncs.conflict_at` / `syncs.last_synced`), `manifest`,
> and `sync_log` are written by the engine/daemon. See the "Storage choice" note
> below.
>
> **Note (flatten games, slice 21):** the `games` table was **dropped** (migration
> 0008). The `sync` is now the atomic entity; "game" is a free-text **LABEL** on the
> sync (`syncs.game`) — a display grouping, **not** a stored entity. There is no
> games registry, no `Game` type, and no game CRUD.
>
> **Note (discovery, slice 22):** the game label is now **inferred from save
> filenames** by the read-only `/discover` on-ramp (engine `DiscoverGames`): it
> scans each reachable node's save dir, infers a game name per save file (strip
> extension + trailing region/version tags), excludes files already in a sync, and
> aggregates "this game is on these nodes." The admin one-click-creates a sync from
> the candidates (the label prefilled to the inferred name). This is read-only until
> the explicit create; it does **not** add a stored entity — it just fills the
> existing `syncs` / `sync_members` rows. Filename-match (vs. content-classify) is
> the resolved approach (see open-questions.md).
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

#### reach_config shapes

- `syncthing-share`: `{ "path": "/srv/syncthing/bob-deck-saves" }`
- `ssh`: `{ "host": "172.16.7.12", "user": "root", "secret_ref": "mister-1" }` — `secret_ref` is a key into the host's secret store (sops file, vault, etc).

### `syncs`

A *sync* is a mirror group: a specific set of `(node, save-file)` members that
sync together. It carries a free-text **game label** (a display grouping); many
independent syncs may share the same label (e.g. two unrelated streams of the same
title) so long as they do not share a `(node, path)` member. The label is **not** a
foreign key — any text is allowed (the `/discover` on-ramp, slice 22, infers it
from save filenames and prefills it on one-click sync creation).

| field        | type | notes                                                        |
|--------------|------|--------------------------------------------------------------|
| id           | text | slug, e.g. `sm-bob`                                           |
| game         | text | free-text label (display grouping); NOT a foreign key; defaults to `''` |
| name         | text | free-form label, e.g. "Bob's stream"                         |
| conflict_at  | ts   | nullable; set when the sync forked (2+ members changed) — mirroring is paused until a human resolves it |
| last_synced  | ts   | nullable; time of the last successful mirror pass            |

`conflict_at` / `last_synced` are the per-sync runtime state (formerly on
`active_bindings`). The runtime tables (`manifest`, `sync_log`) key off `sync_id`;
the engine, daemon, and web all operate on a `sync` and its members. A sync with
`conflict_at IS NULL` is mirroring normally; a non-null `conflict_at` pauses it.

### `sync_members`

The replacement for `game_paths` (and any tool-imposed per-system layout
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
syncs:
  sm-bob (game: "Super Metroid", Bob's stream):
    bob-deck:           retroarch/saves/Super Metroid.srm
    living-room-mister: SNES/Super Metroid.sav
  sm-alice (game: "Super Metroid", Alice's stream):
    alice-deck:         Emulation/saves/snes9x/Super Metroid.srm
```

(The `game:` value is just a label on each sync — two syncs sharing it is a display
grouping, not a relationship to any stored entity.)

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
| last_checked | ts   | set when the manifest entry is written, not on every poll |

PK: (sync_id, node_id). The poll loop writes this entry when a member's state
changes (a content change, a touch-reconcile, or a propagation write), recording
the member's content `sha256` alongside mtime/size; `last_checked` is stamped on
those writes — a noop poll does not touch the row.

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

### `save_blobs` + `save_versions` (the recovery net — slice 20)

Before RetroSync overwrites **any** member's save — during a normal propagation
*or* a conflict resolution — it snapshots the about-to-be-overwritten bytes into a
server-side, content-addressed version store, so a bad propagation/resolve is
always recoverable. This **replaces** the old device-side
`.retrosync-conflict-<ts>` sibling backups (which cluttered device directories and
replicated via Syncthing) with a clean central store. Restore picks a version → it
becomes the authoritative save and propagates to every member.

`save_blobs` — the content-addressed blob store. Identical content is stored once
(dedup by the `hash` PK). Saves are tiny, so `bytea` in Postgres is fine.

| field | type   | notes                                          |
|-------|--------|------------------------------------------------|
| hash  | text   | PK — lowercase-hex sha256 of `data`            |
| data  | bytea  | the raw save bytes                             |
| size  | bigint | byte length of `data`                          |

`save_versions` — the per-member capture index.

| field       | type        | notes                                                              |
|-------------|-------------|--------------------------------------------------------------------|
| seq         | bigserial   | PK — the **monotonic retention key** (see below)                   |
| sync_id     | text        | FK → syncs, `ON DELETE CASCADE`                                     |
| node_id     | text        | FK → nodes, `ON DELETE CASCADE`                                     |
| hash        | text        | FK → save_blobs                                                     |
| captured_at | timestamptz | **display-only** ("ago" in the UI); never used for ordering        |
| reason      | text        | why captured: `propagate`, `conflict-resolve`, `restore`           |

Index `(sync_id, node_id, seq DESC)` backs both newest-first listing and pruning.

**Retention is count-based** (default **10**, a const), per `(sync_id, node_id)`,
ordered by the monotonic **`seq`** — **not** `captured_at`. Ordering by `seq`
rather than a timestamp is deliberate: a device with a bad/RTC-less clock must
never be able to misorder or evict a good snapshot. On every capture the store:

1. **upserts the blob by hash** (dedup — identical content is stored once);
2. **appends a `save_versions` row** *unless* the most recent version for this
   `(sync, node)` already has this exact hash (no churn on a no-op/identical
   re-save);
3. **prunes** to the newest 10 versions per `(sync, node)` by `seq DESC`; and
4. **garbage-collects** any `save_blobs` row no surviving version references.

Cascade: deleting a sync or node removes its versions (FK `ON DELETE CASCADE`),
and the now-orphaned blobs are GC'd. Both store implementations (memory + Postgres)
mirror this retention/dedup/GC behavior and run the same conformance suite.

## Storage choice

- **Postgres** is the store, running as **CNPG** (CloudNativePG) in production. It gives us the `UNIQUE (node_id, path)` invariant on `sync_members`, JSONB for `reach_config`, `ON DELETE CASCADE` for `syncs` / `sync_members` / `manifest` / `sync_log`, and a managed/HA operator in k8s.
- All business logic depends on a Go **`Store` interface** (`internal/store`), never on the driver. There are two implementations: a thread-safe in-memory store (used by fast unit tests) and a pgx v5 Postgres store. Both run an identical **conformance suite** (`internal/store/storetest`) so they cannot drift in behavior.
- Access is via **pgx v5** with hand-written parameterized queries — no ORM, no sqlc. Migrations are embedded (`embed.FS`) and applied programmatically at startup and in tests (`internal/migrate`).
- Integration tests run against a real Postgres started with **podman** (gated by a build tag / `DATABASE_URL`); plain `go test ./...` needs no database.
- **SQLite** was the original default — single-file and simple — but Postgres/CNPG fits the k8s deployment better. **JSON files** were also considered: fine for the registry, painful for `sync_log` and the unique constraint. Skipped.
