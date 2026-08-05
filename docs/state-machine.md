# State machine

> **Auto-mirror (slice 18).** The explicit primary/binding/take-over model is
> retired. Every sync **auto-mirrors**: there is no "activate", no "play session",
> no primary node, and no "done playing". The daemon watches **every** sync and
> propagates the lone changed member's save to the others. The only human action
> is **resolving a conflict**.

## Per-sync states

```
   ┌──────────────┐   one member changes   ┌──────────────┐
   │  in sync     │ ─────────────────────▶ │  mirroring   │
   │ (0 changed,  │   (the engine fans it   │  (propagate, │
   │   noop)      │ ◀──────────────────────  │  then back   │
   └──────────────┘   manifest updated      │  to in sync) │
         │                                  └──────────────┘
         │ two or more members
         │ change between polls
         ▼
   ┌──────────────┐
   │  conflict    │   resolve(winner)   ┌──────────────┐
   │ (paused,     │ ──────────────────▶ │  in sync     │
   │  await       │                     │  (winner     │
   │  human)      │                     │  fanned out) │
   └──────────────┘                     └──────────────┘
```

A sync with `conflict_at IS NULL` is mirroring normally; a non-null `conflict_at`
pauses it. There is no separate "idle" state — every sync mirrors all the time.

## The auto-mirror loop

> **Implemented.** The engine's `Poll(syncID)` (the changed-set table below) is
> driven by the `internal/daemon` ticker, which sweeps **every** sync every N
> seconds (`store.ListSyncs`) and isolates per-sync errors. A conflicted sync is
> skipped (paused) until resolved.

For each sync, every N seconds (default 15s):

1. If the sync's `conflict_at` is set → **skip** (paused, awaiting human).
2. Otherwise run **hash-backed two-tier change detection** on every member:
   - **Tier 1 — stat gate (cheap).** `Stat` (mtime+size) vs the `manifest`, via
     the microsecond `mtimeEqual` compare. If they are equal the member is
     **unchanged** and is *not hashed* — so a noop poll stays hash-free.
     - **Periodic verification (the backstop).** mtime+size cannot detect a
       change an *external* writer made without moving the mtime, and saves are a
       fixed size per game — syncthing preserves mtimes when it delivers a file,
       so it can restore an older save carrying exactly the manifest's mtime.
       Such a change would be invisible **forever**, not merely late. So a member
       whose `manifest.last_checked` is older than `verifyInterval` (5 min) is
       hashed regardless of the stat gate. Bytes that still match take the touch
       path below (reconcile, not a change), which re-stamps `last_checked` — so
       this costs one hash per member per interval and every poll in between
       stays hash-free.
   - **Tier 2 — content hash (truth), only when the stat differs.** Compute the
     member's current `sha256` and compare it to the manifest's stored `sha256`:
     - **hash equal** → a **touch** (mtime/size moved, bytes identical). NOT a
       change: reconcile the manifest's mtime/size to the current stat (keeping the
       same hash) so the next poll fast-paths it. Nothing propagates.
     - **hash differs** → a **real content change**; the member joins the changed
       set carrying its current hash.
3. Decide by the **content hashes of the changed members** (group the changed
   members by their current hash):

   | distinct hashes among changed members | action                                |
   |---------------------------------------|---------------------------------------|
   | 0 changed                             | noop                                  |
   | exactly **one** distinct hash         | the agreed-latest content (1 changed, OR several changed to identical bytes) → fan it out to every member that doesn't already hold it; update their manifest + the sync's `last_synced` |
   | **two or more** distinct hashes       | a genuine fork → set `conflict_at`, log a conflict, **stop** mirroring this sync until resolved |

4. After a successful fan-out, update `manifest` for the source and every written
   member to the new mtime/size **and the agreed content hash**. Log to `sync_log`.

There is **no primary**. When there is a single distinct hash among the changers,
any one of them is the source (they are byte-identical); everyone else receives
it. A sync with fewer than two members can never fork — it just propagates (1
changed) or no-ops (0 changed).

Why the hash matters:

- **Touches don't propagate.** Bare mtime+size false-positives on a `touch` (a
  metadata move with no byte change). The hash recognizes it and reconciles the
  manifest instead of fanning out a no-op.
- **Same-content multi-change isn't a fork.** Two devices that both end up at the
  *same* bytes (e.g. each already received your save through the backup channel,
  or an onboarding sync where two members already hold identical saves) are ONE
  distinct hash — agreed content, propagated to any lagging member, **not** a
  conflict.

Two edge cases inside the propagate branch:

- If a changed member is now **absent** (the manifest had a file but it vanished),
  the engine treats it as a **conflict** rather than propagating the deletion to
  the others. Surfacing beats destruction. (Checked before the distinct-hash
  decision so a lone deletion pauses rather than being mistaken for an agreed
  propagation.)
- A write failure during the fan-out aborts the pass **without** advancing the
  failed member's manifest (the manifest trails the write), so the next poll
  re-detects the divergence and retries (self-heal).
- **Capture before overwrite (the recovery net, slice 20).** Before the engine
  overwrites a member that currently *has* a file — in the fan-out here **and** in
  conflict resolution — it snapshots that member's current bytes into the
  server-side, content-addressed save-version store (`save_blobs` / `save_versions`;
  see data-model.md). This is a **hard gate, ordered exactly like the write
  itself**: if the capture fails, that write is **aborted** (the manifest is not
  advanced) and retried on the next poll — so nothing recoverable is ever
  destroyed un-captured. A member with no current file has nothing to capture
  (skip). This **replaces** the old device-side `.retrosync-conflict-<ts>` sibling
  backups.

The "two or more distinct hashes = conflict" case is the family-sync gotcha: if
Bob writes *Super Metroid* on the MiSTer and Alice writes **different** bytes on
her Deck between two polls, retrosync will **not** silently let one overwrite the
other — it pauses and asks. **Data-loss safety:** a member that changed to content
NOT shared by the chosen propagation is, by construction, a second distinct hash
— so it lands in the conflict case where *nothing is written*. The propagate path
only ever overwrites members that did not change since the manifest (and so hold
neither a competing change); a distinct-hash changer is never silently
overwritten.

## Conflict handling

When two or more members mutate between polls:

- Set `conflict_at` on the **sync**.
- Stop mirroring this sync until the user resolves it (subsequent polls skip it).
- UI shows every member's current live state (mtime, size) and a "use this one"
  button per member that holds a file.
- "Use this one" copies that member's file to all others and clears the conflict.
- Before any overwrite, each loser's existing bytes are **captured to the
  server-side save-version store** (reason `conflict-resolve`) — the recovery net,
  which replaces the old device-side `<path>.retrosync-conflict-<ts>` sibling
  backups.

There is intentionally no auto-merge. Save files don't merge.

> **Implemented** (`engine.ResolveConflict`, owner-of-a-member-node-or-admin-gated
> + CSRF-protected `POST /api/syncs/{id}/resolve-conflict`). Every loser-with-a-file
> is **captured to the central save-version store before any overwrite** — a hard
> gate: a capture failure aborts that write and leaves the sync re-resolvable, so
> nothing recoverable is destroyed un-captured. The sync is marked synced and the
> conflict flag cleared only after the full fan-out succeeds, so a partial failure
> leaves the sync re-resolvable. The data-loss guarantee is now "captured to the
> server store before overwrite" instead of "sibling file on device".

### Restore

A captured version can be made authoritative again: **restore** loads a version's
bytes, writes them back to that `(sync, node)` member, and **propagates** them to
the rest of the sync (with capture-before-overwrite on the others), clearing any
`conflict_at` and marking the sync synced. Effectively "this old save is now the
current save everywhere" — a one-click undo for a bad propagation/resolve.

> **Implemented** (`engine.RestoreVersion`, owner-of-a-member-node-or-admin-gated
> + CSRF-protected `POST /api/syncs/{id}/versions/{seq}/restore`). A pruned/gone
> seq returns `ErrNotFound` (a friendly 410 in the UI).

## Crash safety

- The poll loop is idempotent. Restart at any time; it re-stats and resumes.
- A crash mid-copy can leave a partial file. Always copy to
  `<dest>.retrosync-tmp` and rename atomically.
- `manifest` is only updated *after* the rename. So a crash mid-copy leaves the
  manifest pointing at the previous good state, and the next poll re-detects
  divergence and retries.
- **RetroSync alters a save's mtime only when a human made an explicit choice.**
  A fan-out asks the adapter to stamp the SOURCE file's mtime, and the adapter
  stamps *exactly* what it is handed — always. The one deviation is decided by the
  **engine**, which alone knows why it is writing:
  - **User-initiated writes** — a sync's **first** fan-out (`last_synced == nil`:
    the person just set this sync up), a **conflict resolution** (they picked the
    winner), and a **version restore** (they picked the version to bring back;
    both the direct write to the restore target and the fan-out to the other
    members) — publish `destination mtime + 1µs` when the source mtime is
    *exactly* equal to what the destination already carries. A same-size file
    republished under its own indexed mtime is invisible to *every* mtime+size
    gate (syncthing's scanner and the poll's tier 1 alike), so without the nudge
    the human's choice would change the bytes while every layer above believed
    nothing happened. Resolution is the case that needs it most: winner and loser
    can carry identical mtimes — that is how they became invisible to each other.
    A restore is the same shape: it publishes the resolution time, and any member
    whose file already carries it would silently ignore the restored bytes.
  - **Routine auto-mirror** writes never touch file metadata. An exact collision
    there is *logged as a WARNING* and written with the source mtime anyway: in
    steady state it means the source's content moved while its mtime did not,
    which is an anomaly worth a human seeing, not something to paper over.
    Periodic verification is the backstop.
  - Only **exact** equality qualifies. An mtime that merely moves *backwards* is
    still a difference, and both gates test for difference, not ordering.
  - **Every** deviation is logged with the sync, the node and both mtimes. Silent
    metadata rewriting in a data path is unreviewable after the fact.

  `WriteAtomic` **returns** the mtime it published and the manifest records that
  (never a follow-up stat, which could pick up an external writer's mtime and file
  it under our hash).
- On resolve, `last_synced` is marked **before** `conflict_at` is cleared, so a
  crash between the two leaves the sync conflicted (re-resolvable) rather than
  un-paused-but-unsynced.

## Member-state visibility

The dashboard "My syncs" section is a **status view**: for each sync the user has
a member node in, it shows every member with its last-known manifest mtime, and
the sync's state — "in sync, last synced N ago", or a red **conflict** banner
with a **Resolve** button when `conflict_at` is set. There are no Play buttons.
This surfaces what each device holds without hiding it.
