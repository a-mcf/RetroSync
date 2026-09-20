# UI

HTMX + server-rendered HTML, no build step. **RetroSync is an admin console**,
operated from a desktop browser: register devices, wire up syncs, resolve the
occasional conflict, browse save history. It is not operated from the couch —
devices are hands-off by design, which is the entire point of auto-mirror.

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
  a node whose reach cannot be resolved is **skipped**, not errored. A node whose scan fails (missing share,
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

**A sync's members are a read-only table until you ask to change them.** Each row
is device + save file as text, with an **Edit** button; **Add a device** sits
below. Both open the same `<dialog>`, served by
`GET /syncs/{id}/member-editor` (`?node=` edits that member, no node adds one),
swapped into the page's `#modal` mount exactly like the conflict modal. The two
modes differ only in whether the device is fixed or chosen from a `<select>`, so
they are one template and one endpoint. **Stop syncing this device** lives in the
dialog's footer, quiet and behind a confirm.

Why a dialog rather than editing in the row: setting a path offers the **Browse**
picker, which is a scrolling file tree that does not fit in a table row. Trying to
put it there is what made the old member rows sprawl. The read state matters
independently — previously every path was a live `<input>` with a Save button
always present, so the page could not show a sync's configuration without
simultaneously offering to mutate it, and a path could be nudged by accident.
(The disclosure on each sync is now **"Rename or delete this sync"** — it used to
say "Edit / delete sync" while the actual editing sat outside it.)

**Display language.** The visible UI says **device** (not "node") and **save file**
(not "member"): the member editor's field is "Save file", its commit is "Save
device", the remove confirm is "Stop syncing this save file on `<device>`?", and
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
- An admin **resetting their own** password is sent to `/settings` instead: a
  reset skips proving you are still at the keyboard, which is fine for someone
  else's forgotten password and not fine for your own.

A password reset, a role change, and a delete all **sign that person out
everywhere** (sessions are server-side, so they can be revoked).

### `/settings`

Every signed-in user, admin or not. Two sections:

- **Your account** — who you are signed in as, and **change your own password**,
  which requires your **current** password (a borrowed unlocked session is not
  enough). Changing it signs you out on every other device and rotates your
  session here.
- **Administration** (admin only) — a summary of the people registry and a link
  to `/users`.

People is **linked, not inlined**. The registry is a list plus an add form plus
per-person edit/reset/delete; folding that in would rebuild the long
mixed-purpose page `/syncs` and `/nodes` were pulled apart to avoid. The point
of this page is to get those destinations out of the top navigation, which was
getting busy — not to merge two screens into one.

`/account` was this page before settings absorbed it, and now **301s here**
(preserving the query, so the post-change `?changed=1` confirmation survives).

**Appearance is the obvious next section** — a dark/light/system preference. It
is deliberately not here yet: every page hardcodes `data-theme="dark"` and
RetroSync's own CSS layer has only ever been rendered on a dark ground, so the
work is verifying badges, both conflict banners and the `color-mix` backgrounds
read correctly on light — not adding the control. The shared `head` partial
exists partly so that when it lands, `data-theme` is set in one place.

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

## Design language

**RetroSync uses [Pico CSS](https://picocss.com) v2, vendored** as
`internal/web/static/pico.min.css` and loaded before `app.css`. Pico is a plain
stylesheet — no build step, no framework — and it is semantic-first, so the
markup carries the meaning and Pico supplies the look. That matters here because
HTMX swaps fragments of this markup by hand.

The rules that follow from adopting it:

- **Write semantic HTML and let Pico style it.** `<article>` is a card, `<nav>`
  is the header, `<details>` is a disclosure, `<dialog>` is a modal, `<table>` is
  a table. Reach for a class only when Pico has no opinion.
- **`app.css` is RetroSync's layer only** — status badges, the two conflict
  banners, the save-file picker, the compact size for table row actions. If you
  are adding a rule, first check whether Pico already styles the element. The
  file is ~120 lines including comments and should stay near that.
- **Express values in Pico's custom properties** (`--pico-spacing`,
  `--pico-muted-color`, `--pico-border-radius`, …) rather than hard numbers, so
  the two layers cannot drift.
- **Desktop density.** There is no touch sizing. An earlier version of this doc
  assumed syncing would be operated by hand on the device, and sized every
  control for a thumb (`min-height: 48px` on everything); the assumption was
  wrong and the result read as blown-up on the screen actually used. If a screen
  ever genuinely needs coarse-pointer sizing, scope it to
  `@media (pointer: coarse)` rather than applying it to everything.
- **Dark by default** — `<html data-theme="dark">` on every page.
- **Page chrome is two shared partials**, `head` and `nav` (`layout.tmpl.html`).
  `head` takes the page title as its data; `nav` takes a `userView`. The
  navigation is the SAME on every page — it used to be an ad-hoc subset, so
  where you could go depended on where you were. Admin-only destinations are
  hidden from a non-admin rather than shown and refused (the server still gates
  every one of them). Add a stylesheet, or the theme attribute, in one place.
- **Destructive actions are the quietest control on the screen**, never a filled
  red button. Delete/remove is `class="secondary outline"`, lives inside the
  disclosure or dialog that owns the thing, and keeps its `hx-confirm`.
- Keyboard nav is a nice-to-have, not a requirement — but native `<dialog>`
  brings Esc and focus trapping for free, so don't hand-roll a modal and lose it.

## States the UI must surface clearly

- Backup is healthy / stale / failing per node (Syncthing API).
- A node is reachable / not.
- A sync has no save on any node ("haven't played yet — when you do, it'll be picked up").
- A node's save is from a different person's last session (show whose, with timestamp).
- Sync paused due to conflict (mid-life fork), vs. a never-synced sync awaiting
  its starting save (same mechanism, expected-setup framing).
- Sync currently in flight (transient spinner).
