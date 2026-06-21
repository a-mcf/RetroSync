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
- **Admin-gating** — all **registry** mutations (`/nodes`, `/games`, path mappings,
  smoke-test) are admin-only; a non-admin → `403` ("admins only").
- **Owner/admin-gating** — `activate` / `deactivate` / `resolve-conflict` require
  the caller to **own the relevant node** (the primary, or the session's primary)
  *or* be an admin; otherwise `403`.

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

The home dashboard: active sessions, conflicts, and per-game Play/Done controls.

## Play-sync actions

These drive the engine via the narrow `Actioner` interface; owner/admin-gated.

### `POST /api/games/{id}/activate`

Form fields: `primary_node` (required), `direction?`, `peer_scope?`, `force?`.

- `direction` selects the first-sync source: `from-primary` (default — "use the
  primary's save") or `from-peer-<node_id>`.
- `peer_scope` defaults to `all-configured` (every node with a path mapping for the
  game); may be a csv of node ids to narrow.
- `force` overrides an existing binding. Without it, an already-bound game →
  **`409`**, rendered as the force-takeover modal fragment (re-POSTs with
  `force=true`).
- Missing `primary_node` → `400`. Node not found → `404`. Not owner → `403`.

### `GET /games/{id}/activate`

Read-only HTML fragment: the activation "use my save" modal. No CSRF (no state
change). Unknown game → `404`.

### `POST /api/games/{id}/deactivate`

No body. **Idempotent** — deactivating an idle game is a no-op (`200`), not an
error. If a binding exists, the caller must own its primary node (else `403`).

### `POST /api/games/{id}/resolve-conflict`

Form field: `winner_node_id`. Backs up every other in-scope node's current file to
a sibling `<path>.retrosync-conflict-<ts>` (nanosecond timestamp), then fans the
winner out and clears the conflict. The single most destructive action; auth is
checked **first**:

- No active session → `409` ("no active session to resolve").
- Not owner → `403`. Empty `winner_node_id` → `400`.
- Engine `ErrNotConflicted` → `409`; winner not in scope (`ErrNoPath`) → `400`;
  winner has no save (`ErrSourceMissing`) → `422`.

### `GET /games/{id}/conflict`

Read-only HTML fragment: the conflict modal, showing every in-scope node's live
state (mtime, size, presence) so the human can pick a winner. Viewable by any
authenticated user (not owner-gated — viewing is not mutating). Unknown game →
`404`.

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

Delete. Unknown node → `404`; node in use by an active session (FK guard) → `409`.

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

Delete. `game_paths` / `manifest` / `sync_log` cascade; only an active binding
blocks it → `409` ("being played right now"). Unknown game → `404`.

### `POST /api/games/{id}/paths/{node_id}`

Add or update a (game, node) path mapping. Form field: `path`. **This is the
authoritative route** — the client-side HTMX path-rewrite that splices the chosen
`node_id` into the URL is progressive enhancement only. Empty path → `400`; missing
game or node (FK) → `422`.

### `POST /api/games/{id}/paths/{node_id}/delete`

Remove a path mapping. Refused with `409` if that node is the active primary for
the game (checked against the binding before delete). Unknown mapping → `404`.

## Read-only JSON

These three emit JSON today (the seed of a future agent-facing API). Auth required.

### `GET /api/status`

```json
{
  "active": [
    { "game_id": "...", "primary_node": "...", "since": "...", "conflict": false }
  ],
  "nodes": [
    { "id": "bob-deck", "reachable": true, "last_seen": "..." }
  ]
}
```

> `reachable` / backup-health are **not yet wired to Syncthing** — they are
> cosmetic until a status poller lands (see open-questions.md).

### `GET /api/games`

List games with their path mappings and active state (JSON).

### `GET /api/nodes`

List nodes (JSON).

## Deferred

- A JSON mutation API with `PATCH`/`PUT`/`DELETE` verbs (the v1 surface is
  POST-only because it is HTMX/form-driven).
- `GET /api/games/{id}/log` (a JSON history endpoint) — `sync_log` is written and
  read by the engine, but no dedicated history endpoint is exposed yet.
