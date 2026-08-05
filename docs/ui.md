# UI

HTMX + server-rendered HTML. No build step. One CSS file. Optimized for "I'm sitting on the couch with a Deck and want it to just work."

> **Auto-mirror (slice 18).** There are **no Play / Done / Take-over buttons** and
> **no activation modal**. Every sync mirrors automatically; the dashboard is a
> **status view**. The only action on the dashboard is **Resolve conflict**
> (labelled **Choose starting save** on a sync's first conflict).

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
  - conflicted, **never synced** (`last_synced` is null) → an amber **Almost
    there — pick your starting save** banner with a **Choose starting save**
    button (opens the conflict modal). The recommended setup path — play the game
    once on each device, then Discover — guarantees the members hold different
    bytes, so the engine's first poll always flags a conflict. That is expected,
    not breakage, and the card says so.
  - conflicted, **synced before** → a red **Sync paused** banner with a **Resolve
    conflict** button (opens the conflict modal)

  Both banners open the same modal and post to the same
  `/api/syncs/{id}/resolve-conflict`; only the copy and colour differ.

There are no Play buttons: a sync needs no human action to mirror. The only
state-changing buttons are **Resolve conflict** (when the sync has forked;
labelled **Choose starting save** on a first conflict) and **Restore** (on the
history page).

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

Two variants, chosen by whether the sync has ever completed a mirror pass
(`last_synced`). Identical mechanics — same per-device rows, same winner buttons,
same `POST /api/syncs/{id}/resolve-conflict` — only the framing differs.

**First sync (`last_synced` is null)** — the last setup step, not an error:

```
  Super Metroid — choose your starting save        [×]
  ─────────────────────────────────────────────────
  Your devices hold different saves for this game.
  That's expected — playing it once on each device
  is how the save files got here. Pick the one to
  continue from:

    bob-deck:            4 minutes ago, 65 KB
    living-room-mister:  7 minutes ago, 64 KB

  [Use bob-deck]                  [Use living-room-mister]
```

**Mid-life fork (`last_synced` set)** — two devices really did diverge:

```
  Super Metroid — sync paused                      [×]
  ─────────────────────────────────────────────────
  More than one device changed since the last sync.
  retrosync won't choose for you. Pick a winner:

    bob-deck:            4 minutes ago, 65 KB
    living-room-mister:  7 minutes ago, 64 KB

  [Use bob-deck]                  [Use living-room-mister]
```

Either way the losing devices' current saves are snapshotted to the save history
before being overwritten, and the modal links there so a wrong pick is
recoverable.

### `/discover` — discovery (the on-ramp)

Admin-only. The one-click replacement for hand-building syncs. On load it runs a
**read-only** scan of every reachable device's save directory (via the engine's
`DiscoverGames`), infers a game name from each save file's name, and groups the
results: **"Save files found across your devices"**, one card per inferred game,
each listing the candidate nodes + file paths (size, mtime).

For each game a checkbox per candidate (all checked by default) lets the admin
honor the Bob-vs-Alice split — uncheck a copy that is really a different person's
save — then **Create sync** one-click-creates the sync (game label prefilled to
the inferred name, sync **name** editable, defaulting to "Main"; the optional id
lives behind an **Advanced** `<details>` and auto-slugs from game + name). On
success the admin lands on `/syncs` with the new sync showing. The page sets the
expectation up front — since each device has its own save right now, RetroSync
will ask which one to start from before it mirrors anything (the first-sync
conflict modal above).

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

Admin-only. Lists all syncs (grouped by their game label for display). The page
fronts **Discover** as the easy path: a callout card ("Discover is the easy way —
play the game once on each device, then Discover finds the save files and links
them for you") links to `/discover`, and the empty list (no active search) points
there too. An empty *search* result stays a plain "No syncs match."

Manual creation is demoted to a collapsed **"Create a sync manually (advanced)"**
`<details>`. It is **one-shot**: a game label, a sync name, and one or more member
rows — each a device + save-file path (with the **Browse** picker) — submitted
together with **Save**; **"+ another device"** clones a row (progressive
enhancement; the single row still works with no JS, and the server accepts
repeated `node_id`/`path` fields).

**Button wording rule.** A button that COMMITS says *Save* (`Save`, `Save
device`, `Save changes`); a button that only adds UI is phrased as adding and is
link-styled (`+ another device`). The two were previously near-identical in
wording but opposite in effect ("+ add another device" saved nothing, "Add
device to sync" committed immediately), which is genuinely confusing in use.
`/discover` keeps **Create sync**: there you are accepting a suggested sync
rather than committing a form you filled in, and nothing on that page competes
with it. The manual form's submit greys out (CSS `:has(:invalid)`, no JS) until
the required fields are filled, but stays clickable so a click surfaces the
browser's own "fill this in" hint. Member rows are validated lexically *before* anything is created (a bad
path → 400, no sync). A member failure at the store (missing device → 422; a
`(node, path)` already in another sync → 409) leaves the created sync **in
place** with the members that succeeded — the error says so, and the admin
finishes or fixes it in the registry list. There is deliberately no compensating
delete: the create window is not serialized with the poll loop, and deleting the
sync would cascade `save_versions`, destroying any capture the engine made in
that window (an unrecoverable save loss; a visible partial sync is recoverable).
The optional **id** slug lives behind a nested
**Advanced** `<details>` (it auto-generates from game + name). Under each existing
sync, manage save files (a device + the path on it), and **rename/relabel** or
**delete** the sync (delete is refused on a conflicted sync). No game CRUD — there
is no games table. Most users live on `/`.

**Display language.** The visible UI says **device** (not "node") and **save file**
(not "member"): the member-add label is "save file", the button "Add device to
sync", the remove confirm "Stop syncing this save file on `<device>`?", and
`/nodes` reads "Registered devices" / "Add a device". These are display strings
only — the data model, form field names (`node_id`, `path`), API routes, and
`docs/data-model.md` keep the precise terms (node, member).

### `/users` — people

Admin-only. Everyone who can sign in: display name, username, role, and how many
**devices** they own (the count links to `/nodes`, because owning devices is
exactly what blocks a delete). Per person: **Edit** (display name + role),
**Reset password**, **Delete**. "Add a person" at the bottom takes a username,
optional display name, role, and password, and commits with **Save person**.

The refusals are the feature, and each says why in a sentence rather than
throwing a 500:

- The **only admin** cannot be deleted or demoted ("make someone else an admin
  first, or nobody can manage RetroSync"). Their row is badged **only admin** so
  it is visible before you click. Enforced in the store, inside the write's
  transaction — two admins demoting each other at once cannot both win.
- **You cannot delete yourself** — ask another admin.
- A person who **still owns devices** cannot be deleted; the message names the
  count and points at `/nodes` to reassign them. Devices are never
  cascade-deleted or silently orphaned to make a user delete succeed.
- An admin **resetting their own** password is sent to `/account` instead: a
  reset skips proving you are still at the keyboard, which is fine for someone
  else's forgotten password and not fine for your own.

A password reset, a role change, and a delete all **sign that person out
everywhere** (sessions are server-side, so they can be revoked).

### `/account` — your account

Every signed-in user, admin or not. Shows who you are signed in as and lets you
**change your own password**, which requires your **current** password (a
borrowed unlocked session is not enough). Changing it signs you out on every
other device and rotates your session here. Linked from the header on the
dashboard.

### `/nodes` — devices

Per-node configuration UI (admin-only). Each node lists its
id/display/kind/reach/owner and offers a **Test** button that runs an on-demand
reachability check (a transient reachable / not-reachable result, nothing
persisted), plus **Edit** and **Delete**. See auth.md.

For a **syncthing-share** node the add/edit form's **path** field (the device's
absolute save-mount path on the server) has a **Browse…** folder picker beside it,
rooted at the server's share mount (`RETROSYNC_SHARE_ROOT`, default `/shares`) —
*not* at the node, since a node's picker root is the very path being configured
(chicken-and-egg). Directory rows descend; file rows are shown (so the admin can
confirm "yes, these are Bob's saves") but are **inert** — not selectable in this
mode. A **Use this folder** button fills the path input with the current folder's
**absolute** path (the server renders it, since the browser can't know the mount
root) and collapses the panel. The manual text field stays as a fallback. A missing
share root surfaces as a friendly in-place message, not an error. This mirrors the
`/syncs` member **Browse** picker (which selects a save *file* on a specific node);
the two share one fragment, parameterized folder-select vs file-select.

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
- Sync paused due to conflict (mid-life fork), vs. a never-synced sync awaiting
  its starting save (same mechanism, expected-setup framing).
- Sync currently in flight (transient spinner).
