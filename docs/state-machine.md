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
- Before any overwrite, write each loser's existing file to
  `<path>.retrosync-conflict-<ts>` on the same node (cheap insurance).

There is intentionally no auto-merge. Save files don't merge.

> **Implemented** (`engine.ResolveConflict`, owner-of-a-member-node-or-admin-gated
> + CSRF-protected `POST /api/syncs/{id}/resolve-conflict`). The `<ts>` is a UTC,
> filesystem-safe, **nanosecond-precision** stamp
> (`20060102T150405.000000000Z`), so two resolves in the same second can't
> collide. Every loser-with-a-file is backed up *before* any overwrite; the
> sync is marked synced and the conflict flag cleared only after the full fan-out
> succeeds, so a partial failure leaves the sync re-resolvable.

## Crash safety

- The poll loop is idempotent. Restart at any time; it re-stats and resumes.
- A crash mid-copy can leave a partial file. Always copy to
  `<dest>.retrosync-tmp` and rename atomically.
- `manifest` is only updated *after* the rename. So a crash mid-copy leaves the
  manifest pointing at the previous good state, and the next poll re-detects
  divergence and retries.
- On resolve, `last_synced` is marked **before** `conflict_at` is cleared, so a
  crash between the two leaves the sync conflicted (re-resolvable) rather than
  un-paused-but-unsynced.

## Member-state visibility

The dashboard "My syncs" section is a **status view**: for each sync the user has
a member node in, it shows every member with its last-known manifest mtime, and
the sync's state — "in sync, last synced N ago", or a red **conflict** banner
with a **Resolve** button when `conflict_at` is set. There are no Play buttons.
This surfaces what each device holds without hiding it.
