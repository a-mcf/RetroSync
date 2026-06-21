# API

Endpoints serve both the HTMX UI (returning HTML fragments) and JSON for any future agent. Content-negotiation by `Accept:` header. The shapes below are JSON; the UI gets HTML rendered from the same handlers.

All endpoints require an authenticated session (see auth.md). The user's identity scopes which nodes they can act on (a user can only bind games to nodes they own; admin can bind anyone's; shared nodes like a household MiSTer are usable by all).

## Registry

### `GET /api/games`

List games. Query params: `q` (substring), `system`.

```json
[
  {
    "id": "super-metroid",
    "display": "Super Metroid",
    "system": "snes",
    "active": { "primary_node": "bob-deck", "since": "..." } | null,
    "paths": [
      { "node_id": "bob-deck",           "path": "...", "mtime": "..." },
      { "node_id": "alice-deck",         "path": "...", "mtime": "..." },
      { "node_id": "living-room-mister", "path": "...", "mtime": "..." }
    ]
  }
]
```

### `POST /api/games`

Create a game. Body: `{ id, display, system, notes? }`.

### `PATCH /api/games/{id}` / `DELETE /api/games/{id}`

Edit / remove. Delete is forbidden if the game is currently active.

### `PUT /api/games/{id}/paths/{node_id}`

Add or update a node-path mapping. Body: `{ path }`.

### `DELETE /api/games/{id}/paths/{node_id}`

Remove. Forbidden if this node is the active primary for this game.

## Nodes

### `GET /api/nodes`

### `POST /api/nodes`

Body: `{ id, owner_user_id?, display, kind, reach, reach_config }`.

### `PATCH /api/nodes/{id}` / `DELETE /api/nodes/{id}`

## Bindings

### `POST /api/games/{id}/activate`

Body: `{ primary_node, direction?, peer_scope?, force?: bool }`.

- `direction` defaults server-side to "use the primary's current file" when it exists. UI passes through whatever the user chose in the modal.
- `peer_scope` defaults to `all-configured` (every node with a path mapping for this game). Can be a list of node ids to narrow.
- `force` overrides an existing binding. Returns 409 without it.

Returns the new binding.

### `POST /api/games/{id}/deactivate`

No body. Idempotent (deactivating an idle game returns 200, no-op).

### `POST /api/games/{id}/resolve-conflict`

Body: `{ winner_node_id }`. Triggers a one-shot fan-out copy from the named node to all other peers in scope, and clears the conflict flag.

## Status

### `GET /api/status`

```json
{
  "active": [
    { "game_id": "...", "primary_node": "...", "since": "...", "conflict": false }
  ],
  "nodes": [
    { "id": "bob-deck",           "reachable": true, "last_seen": "..." },
    { "id": "living-room-mister", "reachable": true, "rtt_ms": 4 }
  ]
}
```

### `GET /api/games/{id}/log`

Recent `sync_log` entries for a game.

## HTMX endpoints

Mirror of the JSON endpoints but returning HTML fragments. Naming convention: same path, `Accept: text/html`. UI never embeds JSON; everything is server-rendered partials.
