# API

> **Implementation status (v1).** The shipped HTTP surface is **HTMX/POST-driven**:
> the UI is server-rendered HTML fragments, and every mutation is a `POST` (HTML
> forms cannot issue `PATCH`/`PUT`/`DELETE`, and HTMX submits forms). The routes
> below are the *actual* routes registered in `internal/web/server.go`. A JSON API
> with proper REST verbs (`PATCH`/`PUT`/`DELETE`) is **deferred** until a non-HTMX
> client exists to justify it; the read-only `GET /api/{status,games,nodes}`
> endpoints already emit JSON for that future.

All non-public routes require an authenticated session (see auth.md). Auth and
authorization are enforced by middleware before any handler runs:

- **Unauthenticated** request → `303` redirect to `/login` for an HTML route, or
  `401` for an `/api/*` route.
- **CSRF** — every state-changing `POST` is wrapped in a CSRF check. The token is a
  **per-session** synchronizer token (minted at login, rotated on each login,
  compared in constant time). It travels in the `X-CSRF-Token` header (HTMX) or a
  `csrf_token` form field. A missing/invalid token (or no session) → `403`.
- **Admin-gating** — all **registry** mutations (`/nodes`, `/games`, sync + member
  edits, smoke-test) are admin-only; a non-admin → `403` ("admins only").
- **Member-owner/admin-gating** — `resolve-conflict` (the only play-side action
  under auto-mirror) requires the caller to **own at least one of the sync's
  member nodes** *or* be an admin; otherwise `403`. There is no primary anymore,
  so authority comes from owning a node that is part of the sync.

## Auth

### `GET /login` / `POST /login`

Login form and authenticate. `POST /login` is intentionally outside the
auth/CSRF wrappers. Bad credentials → `401` (same message for unknown user and
wrong password — no enumeration); success sets the session cookie and `303`s to
`/`.

### `POST /logout`

Destroys the session, clears the cookie, `303` to `/login`. Idempotent.

### `GET /healthz`

Liveness, no auth.

## Dashboard

### `GET /` (auth required)

The home dashboard: the "My syncs" status view (each sync's members + mtimes +
state) and node reachability. Under auto-mirror there are no Play/Done controls —
the only action is **Resolve conflict** (and only when a sync is conflicted).

## Play-sync actions

Under auto-mirror there is exactly **one** play-side action: resolving a conflict.
There is no activate / deactivate / take-over (every sync auto-mirrors). It drives
the engine via the narrow `Actioner` interface; member-owner/admin-gated.

### `POST /api/syncs/{id}/resolve-conflict`

Form field: `winner_node_id`. **Captures** every other member's current bytes to
the server-side save-version store (the recovery net — see `/syncs/{id}/history`),
then fans the winner out, marks the sync synced, and clears the conflict. The
capture is a hard gate before each overwrite (a capture failure aborts that write
and leaves the sync re-resolvable). The single most destructive action; auth is
checked **first**:

- Unknown sync → `409` ("no such sync to resolve" — does not leak existence).
- Caller owns no member node (and is not admin) → `403`. Empty `winner_node_id`
  → `400`.
- Engine `ErrNotConflicted` → `409` (already resolved); winner not a member
  (`ErrNoPath`) → `400`; winner has no save (`ErrSourceMissing`) → `422`.

### `GET /syncs/{id}/conflict`

Read-only HTML fragment: the conflict modal, showing every member's live state
(mtime, size, presence) so the human can pick a winner. Viewable by any
authenticated user (not owner-gated — viewing is not mutating). Unknown sync →
`404`.

### `GET /syncs/{id}/history`

Read-only HTML page: the **save history** for the sync — every member with its
captured save versions (newest-first by `seq`: captured_at "ago", reason, size),
each offering a **Restore** button. Viewable by any authenticated user (not
owner-gated, like the conflict modal). Unknown sync → `404`.

### `POST /api/syncs/{id}/versions/{seq}/restore`

Makes the captured version `seq` authoritative again: writes its bytes back to its
`(sync, node)` member and propagates to the rest of the sync (capture-before-
overwrite on the others), clearing any conflict and marking synced. Destructive;
auth is checked **first** with the same rule as resolve-conflict
(member-owner-or-admin) + CSRF:

- Unknown sync → `404`. Caller owns no member node (and is not admin) → `403`.
  Non-numeric `seq` → `400`.
- Engine `store.ErrNotFound` (version pruned/gone) → `410` Gone; captured member
  no longer in the sync (`ErrNoPath`) → `409`. Success refreshes the dashboard.

## Registry — Nodes (admin-only)

### `GET /nodes`

The node registry admin page (and, for an `HX-Request`, the list fragment).

### `POST /api/nodes`

Create. Form fields: `id`, `owner_user_id?`, `display`, `kind`, `reach`, plus
reach-config fields (`path` for `syncthing-share`; `host`/`user`/`secret_ref` for
`ssh`). Errors: duplicate id → `409`; bad owner FK → `422`; bad kind/reach or bad
reach-config shape (caught in form validation) → `400`.

### `POST /api/nodes/{id}`

Edit (rewrites mutable fields; the path id is authoritative). Unknown node → `404`,
otherwise same mapping as create.

### `POST /api/nodes/{id}/delete`

Delete. Unknown node → `404`. Under auto-mirror a node delete simply cascades its
`sync_members` + `manifest` rows (a removed member just stops mirroring); there is
no active-session FK to block it.

### `POST /api/nodes/{id}/smoke-test`

Probe reachability (engine `SmokeTest` → localfs stat of the save root). Returns an
HTML result fragment: "reachable" (and bumps `last_seen_at`) on success, or the
error surfaced to the admin. For an `ssh` node the adapter is not wired yet, so the
fragment says "not supported yet (ssh adapter pending)". Unknown node → `404`.

## Registry — Games (admin-only)

### `GET /games`

The game registry admin page (or the list fragment for an `HX-Request`). Query
params: `q` (substring over id/display), `system`.

### `POST /api/games`

Create. Form fields: `id?`, `display`, `system`, `notes?`. When `id` is omitted it
is auto-generated as a slug from `display` (manual id overrides). Validation
failure (bad/empty fields, invalid slug) → `400`; duplicate id → `409`.

### `POST /api/games/{id}`

Edit (path id authoritative). Unknown game → `404`, else same mapping as create.

### `POST /api/games/{id}/delete`

Delete. The game's `syncs` (and their `sync_members` / `manifest` / `sync_log`)
cascade; a **conflicted** sync on the game blocks it → `409` ("a sync of this game
is in conflict — resolve it first"), so an unresolved fork is never silently
discarded. Unknown game → `404`.

## Registry — Syncs (admin-only)

A *sync* is the unit of mirroring: a set of `(node, save-file path)` members that
sync together. A game may have many independent syncs. These routes edit the
registry (distinct from the play-side `/api/syncs/{id}/resolve-conflict` route,
which drives the engine and is member-owner/admin-gated).

### `POST /api/syncs`

Create. Form fields: `game_id` (required), `name` (required), `id?`. When `id` is
omitted it is auto-generated as a slug from `game_id` + `name`; a manual id
overrides. Bad/empty fields or an invalid derived slug → `400`; missing game (FK)
→ `422`; duplicate id → `409`.

### `POST /api/syncs/{id}` (rename)

Rename. Form field: `name` (required). The id and game are immutable here. Empty
name → `400`; unknown sync → `404`.

### `POST /api/syncs/{id}/delete`

Delete. Refused with `409` if the sync is **conflicted** (don't silently discard
an unresolved fork; checked against `conflict_at` before delete). Otherwise
deletes; `sync_members` / `manifest` / `sync_log` cascade. Unknown sync → `404`.

### `POST /api/syncs/{id}/members/{node_id}`

Add or update a sync member. Form field: `path`. **This is the authoritative
route** — the client-side HTMX path-rewrite that splices the chosen `node_id`
into the URL is progressive enhancement only. Empty path → `400`; missing sync or
node (FK) → `422`; the `(node, path)` already a member of ANOTHER sync (the global
`UNIQUE (node_id, path)` invariant) → `409` ("that save file is already in another
sync"). The file picker / save-file discovery is deferred (slice 17); for now the
path is typed by hand.

### `POST /api/syncs/{id}/members/{node_id}/delete`

Remove a member. Under auto-mirror **any** member is removable — there is no
active primary to protect; a removed member's file simply stops mirroring.
Unknown member → `404`.

## Read-only JSON

These three emit JSON today (the seed of a future agent-facing API). Auth required.

### `GET /api/status`

```json
{
  "syncs": [
    { "sync_id": "sm-bob", "game_id": "super-metroid", "conflict": false,
      "conflict_at": null, "last_synced": "2026-06-21T11:30:00Z" }
  ],
  "nodes": [
    { "id": "bob-deck", "reachable": true, "last_seen": "..." }
  ]
}
```

Every sync is reported (there is no "active" subset under auto-mirror).
`conflict` is `true` when `conflict_at` is set; `last_synced` is the time of the
last successful mirror pass (or `null`).

> `reachable` / backup-health are **not yet wired to Syncthing** — they are
> cosmetic until a status poller lands (see open-questions.md).

### `GET /api/games`

List games with their syncs (JSON). Each sync carries its members (node + path +
last-known manifest mtime) and its auto-mirror `state`:

```json
[
  {
    "id": "super-metroid",
    "display": "Super Metroid",
    "system": "snes",
    "syncs": [
      {
        "id": "sm-bob",
        "name": "Bob's stream",
        "state": { "conflict": false, "conflict_at": null, "last_synced": "2026-06-21T11:30:00Z" },
        "members": [
          { "node_id": "bob-deck", "path": "sm.srm", "mtime": "2026-06-21T11:30:00Z" },
          { "node_id": "carol-deck", "path": "sm.srm", "mtime": null }
        ]
      }
    ]
  }
]
```

`state.conflict` is `true` when the sync is forked; `state.last_synced` is the
last successful mirror pass (or `null`). Each member's `mtime` is always present
(a value or `null`).

### `GET /api/nodes`

List nodes (JSON).

## Deferred

- A JSON mutation API with `PATCH`/`PUT`/`DELETE` verbs (the v1 surface is
  POST-only because it is HTMX/form-driven).
- `GET /api/games/{id}/log` (a JSON history endpoint) — `sync_log` is written and
  read by the engine, but no dedicated history endpoint is exposed yet.
