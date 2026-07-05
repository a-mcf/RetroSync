# UI

HTMX + server-rendered HTML. No build step. One CSS file. Optimized for "I'm sitting on the couch with a Deck and want it to just work."

> **Auto-mirror (slice 18).** There are **no Play / Done / Take-over buttons** and
> **no activation modal**. Every sync mirrors automatically; the dashboard is a
> **status view**. The only action on the dashboard is **Resolve conflict**.

## Screens

### `/` — dashboard

**My syncs** — a status view. A regular user sees every sync that has a member
on a node they own; an **admin sees every sync** (nodes default to no owner, and
conflict resolution must stay reachable from the dashboard regardless). Search
box at top. Each card:

- Game label and sync name
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

**Nodes** — a read-only list of each node's display name and id. The node
controls (**Test**, **Edit**, **Delete**) live on `/nodes`, not here. There is no
persistent reachability badge or last-seen timestamp.

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

### `/discover` — discovery (the on-ramp)

Admin-only. The one-click replacement for hand-building syncs. On load it runs a
**read-only** scan of every reachable device's save directory (via the engine's
`DiscoverGames`), infers a game name from each save file's name, and groups the
results: **"Save files found across your devices"**, one card per inferred game,
each listing the candidate nodes + file paths (size, mtime).

For each game a checkbox per candidate (all checked by default) lets the admin
honor the Bob-vs-Alice split — uncheck a copy that is really a different person's
save — then **Create sync** one-click-creates the sync (game label prefilled to
the inferred name, sync **name** editable, defaulting to "Main"; id auto-slugs
from game + name). On success the admin lands on `/syncs` with the new sync
showing.

Scan rules (all in the engine, read-only):

- Only **directory-listing-reachable** nodes are scanned (syncthing-share today);
  ssh nodes are **skipped**, not errored. A node whose scan fails (missing share,
  I/O) is skipped too — one bad node never fails discovery.
- The per-node walk is **bounded** (max depth, max total entries) so a deep or
  pathological tree can't hang it.
- Only **save-like** files (a fixed set of SRAM/EEPROM/memory-card extensions)
  are collected; **save states** (`.state*`) are excluded per the v1 non-goal.
- Files **already in a sync** (a `(node, path)` already a `sync_member`) are
  excluded — discovery only surfaces *un-synced* saves.
- The inferred name strips the extension and trailing region/version tags
  (`Super Metroid (USA) [!].srm` → `Super Metroid`); candidates with the same
  inferred name (case-insensitive) across nodes aggregate into one card.

Adding a node that does **not yet** hold the save (a fresh target device that
should *receive* the game on first sync) stays the existing `/syncs` member-add
via the picker — discovery groups *existing* files only.

### `/syncs` — registry

Admin-only. Lists all syncs (grouped by their game label for display). A
**+ New sync** form creates one (a game label + a sync name; the id is
auto-generated from game + name). Under each sync, manage members (a node + the
save-file path on it, with the **Browse** picker), and **rename/relabel** or
**delete** the sync (delete is refused on a conflicted sync). Power-user surface;
no game CRUD — there is no games table. Most users live on `/`.

### `/nodes` — devices

Per-node configuration UI (admin-only). Each node lists its
id/display/kind/reach/owner and offers a **Test** button that runs an on-demand
reachability check (a transient reachable / not-reachable result, nothing
persisted), plus **Edit** and **Delete**. See auth.md.

## Couch ergonomics

- Big tap targets. The dashboard's primary action — **Resolve conflict** when a
  sync is paused — should be the largest, most obvious thing on a conflicted card.
- Touch-friendly modal close (×) corners.
- Dark mode by default.
- Keyboard nav nice-to-have, not required.

## States the UI must surface clearly

- Backup is healthy / stale / failing per node (Syncthing API).
- A node is reachable / not.
- A sync has no save on any node ("haven't played yet — when you do, it'll be picked up").
- A node's save is from a different person's last session (show whose, with timestamp).
- Sync paused due to conflict.
- Sync currently in flight (transient spinner).
