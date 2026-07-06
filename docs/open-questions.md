# Open questions

Decisions intentionally deferred. Resolve before building, or pick a default and learn.

Items below are tagged **RESOLVED** (decided and, where noted, built) or left open.

## Storage: Postgres over SQLite — RESOLVED (built)

The store is **Postgres** (CNPG in production), behind the storage-agnostic
`internal/store.Store` interface. SQLite was the original single-file default but
Postgres fits the k8s deployment and gives us the `UNIQUE (node_id, path)`
invariant on `sync_members`, JSONB `reach_config`, and `ON DELETE CASCADE`. Both an
in-memory fake and the pgx v5 implementation run one conformance suite. See
data-model.md.

## Activation default direction

State-machine.md says default to "use the node you're binding from" when multiple nodes have a save. That's right when the binding node really is the freshest source. But when you bind from inside the UI on the couch, it's possible another node is more recent. Maybe the default should be "the node with the newer mtime, but require explicit confirmation if they're within an hour of each other"?

Decision: ship with "the binding node" default. Watch how often users override; revisit.

## Polling interval and watcher

15s poll is fine for couch-pace play. Good enough? An optional node-side agent with inotify would drop latency to sub-second. Worth it?

Decision: poll for v1. Agent is a v2 thinking-about.

## What counts as "the same save"?

Right now: same path on each side, mapped explicitly. A game that uses save *states* in addition to SRAM gets weird — RetroArch stores `.state*` next to `.srm`, and MiSTer doesn't have an equivalent. Do we sync states? Probably no — they're not portable across MiSTer/standalone — but worth being explicit in UI.

Decision: SRAM/SAV only in v1. Document explicitly that states are out of scope.

## Multi-file games

Some games have multi-file save layouts (memory cards, multiple slots). Model right now is one file per (game, node). Extend to "save bundle" later if it bites.

## Peer scope per binding

The model assumes the bound primary mirrors to *all* nodes that have a path mapping for the game. But sometimes you want narrower — "play from Bob's Deck → MiSTer only" without also touching Alice's Deck. The data model already supports per-binding scope (`peer_scope` on `active_bindings`).

Decision: default to `all-configured`. Expose scope override in an "advanced" disclosure on the activation modal. Skip the override entirely in v1 if it adds clutter.

## Syncthing as the local-side transport — RESOLVED (read path built)

For `syncthing-share` nodes, we're reading from the local Syncthing share, which means the device must have run Syncthing recently for retrosync to see updates. If the device is offline, the local share is stale, and retrosync will read stale data.

Mitigations (future — none are wired today):

- Show "last seen" per node prominently. **Note:** the backing `last_seen_at` column
  was removed (migration 0009) along with the misleading persistent status badge, so
  reintroducing this would mean re-adding last-seen tracking first.
- Refuse activation if the binding node's last_seen is too old (configurable
  threshold). Same caveat — there is no `last_seen` to gate on anymore.
- Long term: optional retrosync agent on the node that pushes save updates direct to the server, bypassing the Syncthing-as-transport limitation.

Decision: ship RetroSync as a **standalone Kubernetes deployment** that mounts the **same network volume** backing the Syncthing shares, reading them as a local filesystem (see architecture.md; an earlier sidecar-in-the-Syncthing-pod plan was dropped once the shares turned out to live on NFS, which any pod can mount). This keeps Syncthing as the local-side transport for v1: the staleness window is inherent to "device must have synced recently." The mitigations above remain open future work — last-seen tracking was dropped as misleading, so any staleness-gating feature would need to reintroduce it. The optional node-side push agent remains the v2 escape hatch.

**Built:** the read *and* write path for `syncthing-share` nodes goes through the
**localfs** reach adapter (atomic temp-file-then-rename), rooted at
`reach_config.path`. The engine writes the winning save back into the same local
share path it read from — Syncthing then replicates it out. (Writeback *into a
locked-down device over SSH* is the separate deferred item below; the localfs
write path itself is implemented.)

## Writeback for syncthing-share nodes — partially RESOLVED (ssh deferred)

Today's plan: read/write via the local Syncthing share for `syncthing-share`
nodes (built, via localfs). The remaining open piece is writing back into devices
that must be reached **over SSH/SFTP** (MiSTer's RO root, locked-down handhelds).
That requires the device to be SSH-reachable, and for some devices it isn't.

**Status:** the **ssh adapter is not built** — `ssh` nodes resolve to
"not supported yet" (`reach.ErrUnsupportedReach`) and the registry smoke-test
surfaces that. So today only `syncthing-share` (localfs) nodes are fully
operational. Open: accept "writeable SSH nodes need SSH" as a hard requirement, or
eventually ship a node-side agent that listens for push.

## Conflict resolution UX — RESOLVED (built)

Right now: pick a winner. Should we offer "save both, let me sort it" — copy the loser to a `.conflict-<ts>` file before overwriting? Cheap insurance.

Decision: yes, do that. Default behavior, no toggle.

**Built (slice 20 form):** `ResolveConflict` captures every loser's current bytes
*before* any overwrite, then fans the winner out. The original implementation
wrote a sibling `<path>.retrosync-conflict-<ts>` on the device; that was
**superseded** by the server-side, content-addressed save-version store
(`save_blobs` / `save_versions`) — the same capture-before-overwrite net now also
guards normal propagation, dedups by content hash, keeps the newest 10 per member,
and offers one-click **Restore**, without cluttering device directories or
replicating sibling files via Syncthing. The capture is a hard gate: a capture
failure aborts that write and leaves the sync re-resolvable. No toggle. See
data-model.md ("save_blobs + save_versions") and state-machine.md ("Conflict
handling" / "Restore").

## Identity & name slugs — RESOLVED (built)

`super-metroid` works. `super-mario-bros-3` works. Do we need a manual ID at all, or auto-generate from display? Auto with manual override is probably right.

**Built:** sync create auto-generates the sync id as a slug from `game + name`; a
manually-supplied id overrides. The final id is validated against the shared slug
shape (lowercase letters, digits, hyphens). Node ids use the same slug validation.

## Save-file discovery: filename-match vs. content-classify — RESOLVED (built)

How does the admin go from "a pile of save files on several devices" to a sync,
without typing a game label and a path per device? Two approaches: **classify by
content** (parse ROM headers / hash against a DB — heavy, brittle, and an explicit
non-goal: retrosync syncs the paths the user mapped, it does not identify games),
or **match by filename** (group saves whose filenames infer the same game name).

Decision: **filename-match**, shipped as the read-only `/discover` on-ramp (slice
22). The engine's `DiscoverGames` scans each directory-listing-reachable node's
save dir (ssh skipped, not errored; the walk is depth-/entry-bounded so it can't
hang), collects **save-like** files (a fixed SRAM/EEPROM/memory-card extension
set; `.state*` excluded per "What counts as the same save?" above), infers a game
name (strip extension + trailing `(...)`/`[...]` region/version tags), excludes
files already in a sync, and aggregates by inferred name across nodes. The admin
unchecks any candidate that is really a different person's save (the Bob-vs-Alice
split stays a human call — discovery never auto-creates), then one-click-creates
the sync (label prefilled to the inferred name). Read-only until that explicit
create. No content classification, no per-system layout auto-detection (that
remains a non-goal).

## TGFX16 and other systems some sync tools skip

retrosync doesn't classify by content — it syncs paths the user mapped. So TGFX16 is fine here as long as the user maps the path. Worth noting in README.

## Node reachability / backup-health are not yet wired to Syncthing — OPEN

There is **no always-on, Syncthing-derived node status** today — no status poller
exists, and the persistent `reachable` / `last_seen` surface (plus its backing
`last_seen_at` column) was removed as misleading. A future Syncthing-status poller
could read each node's last-seen / sync state and reintroduce a real persistent
status surface. (The registry smoke-test *does* probe localfs reachability live;
this open item is specifically about the always-on Syncthing-derived status, not
the on-demand smoke-test.)

## SSH adapter pending — OPEN

The `ssh` reach strategy has no adapter yet: `ssh` nodes resolve to
`reach.ErrUnsupportedReach` ("not supported yet"). Until it lands, only
`syncthing-share` (localfs) nodes are operational, and SSH writeback into
locked-down devices (see "Writeback for syncthing-share nodes" above) is blocked on
it. Building it means an ssh/sftp adapter with secret-store credential resolution
(`reach_config.host/user/secret_ref`).
