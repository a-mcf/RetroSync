# UI

HTMX + server-rendered HTML. No build step. One CSS file. Optimized for "I'm sitting on the couch with a Deck and want to start playing in 5 seconds."

## Screens

### `/` — dashboard

Top section: **active bindings**. One row per active game, showing:

- Game title and system
- Bound on: `<primary_node>` since `<time>`
- Peers: list of nodes being mirrored, with last-synced mtime
- Last sync: `<time ago>`, fan-out summary, bytes
- Buttons: `Sync now`, `Done playing`
- Conflict banner (red) if conflicted, with `Use <node>` buttons per node

Middle section: **my games** — games with a path mapping on at least one of my nodes. Search box at top. Each row:

- Title, system
- One line per node that has a path: `<node>: <mtime>` (or "no save yet")
- Button: `Play on <node>` (multiple buttons if user has multiple nodes)
  - If another node is currently the primary: button reads `Take over from <other-node>` and uses `force: true`

Bottom section: **other people's active games** — read-only awareness. "alice-deck is playing Super Metroid since 14:02." Helps avoid takeover surprises.

### Activation modal

When user clicks `Play on <node>` for a game with saves on multiple nodes:

```
  Super Metroid                                   [×]
  ─────────────────────────────────────────────────
  Multiple saves exist. Which one do you want to start from?

    ◉ bob-deck            (4 hours ago,  64 KB)   ← you
    ◯ alice-deck          (2 days ago,   64 KB)
    ◯ living-room-mister (12 hours ago, 64 KB)

  [Cancel]                            [Start playing]
```

Default radio: **the node you're binding from** (see state-machine.md for why). Modal only appears when more than one node has a file.

### Conflict modal

```
  Super Metroid — sync paused                     [×]
  ─────────────────────────────────────────────────
  More than one node changed since the last sync.
  retrosync won't choose for you. Pick a winner:

    bob-deck:            4 minutes ago, 65 KB
    living-room-mister:  7 minutes ago, 64 KB

  [Use bob-deck]                  [Use living-room-mister]
```

### `/games` — registry

Admin-ish. Add a game; under each game, create syncs; under each sync, add
members (a node + the save-file path on it). Power-user surface. Most users live
on `/`.

### `/nodes` — devices

Per-node configuration UI. See auth.md.

## Couch ergonomics

- Big tap targets. The dashboard "Play on <node>" button should be the largest thing on the page.
- Touch-friendly modal close (×) corners.
- Dark mode by default.
- Keyboard nav nice-to-have, not required.

## States the UI must surface clearly

- Backup is healthy / stale / failing per node (Syncthing API).
- A node is reachable / not.
- A game has no save on any node ("haven't played yet — when you do, it'll be picked up").
- A node's save is from a different person's last session (show whose, with timestamp).
- Sync paused due to conflict.
- Sync currently in flight (transient spinner).
