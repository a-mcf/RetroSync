# UI

HTMX + server-rendered HTML. No build step. One CSS file. Optimized for "I'm sitting on the couch with a Deck and want it to just work."

> **Auto-mirror (slice 18).** There are **no Play / Done / Take-over buttons** and
> **no activation modal**. Every sync mirrors automatically; the dashboard is a
> **status view**. The only action on the dashboard is **Resolve conflict**.

## Screens

### `/` — dashboard

**My syncs** — a status view of every sync the user has a member node in. Search
box at top. Each card:

- Game title and system, sync name
- One line per member node: `<node>: <mtime>` (or "no save yet"); the user's own
  nodes are marked "(yours)"
- A **History** link (opens the sync's save-history page)
- The sync's **state**:
  - in sync → `in sync` badge + "last synced `<time ago>`"
  - conflicted → a red **Sync paused** banner with a **Resolve conflict** button
    (opens the conflict modal)

There are no Play buttons: a sync needs no human action to mirror. The only
state-changing buttons are **Resolve conflict** (when the sync has forked) and
**Restore** (on the history page).

**Nodes** — per-node reachability status (reachable / not seen, last-seen time).

### `/syncs/{id}/history` — save history (the recovery net)

Every sync card links here. The page lists each member node and its captured save
versions, newest-first — each row shows when it was captured ("`<time ago>`"), why
(`propagate` / `conflict-resolve` / `restore`), and its size, with a **Restore**
button. Restore makes that snapshot the current save everywhere (a one-click undo
for a bad sync). RetroSync captures a snapshot of a member's bytes **before** it
overwrites them (normal propagation or conflict resolve), keeping the newest 10
per member. Viewable by any authenticated user; **Restore** is gated on
owner-of-a-member-node-or-admin + CSRF. This central store replaces the old
device-side `.retrosync-conflict-<ts>` sibling backups.

```
  Save history — Super Metroid — Bob's stream
  ─────────────────────────────────────────────────
  bob-deck
    4 minutes ago · conflict-resolve · 65 KB   [Restore]
    2 hours ago   · propagate        · 64 KB   [Restore]
```

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
