# State machine

## Per-game states

```
   ┌──────────┐    bind(primary, direction)  ┌──────────┐
   │ idle     │ ───────────────────────────▶ │ active   │
   │ (backup  │                              │ (play    │
   │  only)   │ ◀──────────────────────────── │  sync)   │
   └──────────┘   unbind()                   └──────────┘
                                                   │
                                                   │ poll loop sees
                                                   │ a non-primary
                                                   │ node mutate
                                                   ▼
                                              ┌──────────┐
                                              │ conflict │
                                              │ (paused, │
                                              │  await   │
                                              │  human)  │
                                              └──────────┘
```

A game with no row in `active_bindings` is **idle**. A row makes it **active**. Conflict is a flag on the active row, not a separate row.

## Activation flow

User opens the UI, finds *Super Metroid*, clicks "Play on my Deck."

1. Server checks `active_bindings.game_id`. If a row exists with a *different* primary → return 409, show "Currently bound to alice-deck since 14:02. Force takeover?" Force = delete the existing row, create new.
2. Server stats every member node of the sync (each `sync_members` row).
3. None of them have the file → bail; user needs to create one first.
4. Only one node has the file → log it; auto-pick that as the source for the first sync pass; no prompt.
5. Multiple nodes have the file → prompt "Which save do you want to start from?" Default: **the node you're binding from** ("Use my save"), since the human just sat down with that device and knows what's on it.
6. Server creates `active_bindings` row, kicks off an immediate fan-out copy from the chosen source to all other peers in scope.

The "use my save" default is the most important decision in this flow. SGM-Helper's silent "newer mtime wins" is exactly what eats people's saves when a peer's clock is wrong (MiSTer has no RTC; Anbernics often drift).

## Active poll loop

> **Implemented.** The engine's `Poll` (the case table below) is driven by the
> `internal/daemon` ticker, which sweeps every active binding every N seconds and
> isolates per-game errors. A conflicted binding is skipped (paused) until resolved.

For each active binding, every N seconds (default 15s):

1. Stat every node in scope (primary + peers). Compare against `manifest`.
2. Cases:

   | primary changed | any peer changed | action                                            |
   |-----------------|------------------|---------------------------------------------------|
   | no              | no               | noop                                              |
   | yes             | no               | fan-out: primary → all peers                      |
   | no              | one peer         | promote? no — **conflict** (peer wrote out-of-band) |
   | yes             | yes              | **conflict**                                      |

3. After a successful fan-out, update `manifest` for primary and all written peers to the new mtime/size. Log to `sync_log`.

The "peer wrote out-of-band" case is the family-sync gotcha: while Bob is bound to the MiSTer for *Super Metroid*, Alice should not also be playing it on her Deck. If she does and her save is in the peer scope, that's a conflict and we pause rather than letting Bob's later save quietly overwrite hers.

## Conflict handling

When a non-primary peer mutates during an active session, or both primary and peer mutate between polls:

- Set `conflict_at` on the active binding.
- Stop syncing this game until the user resolves it.
- UI shows every node's current state (mtime, size) and a "use this one" button per node.
- "Use this one" copies that node's file to all others in scope and clears the conflict.
- Before any overwrite, write the loser's existing file to `<path>.retrosync-conflict-<ts>` on the same node (cheap insurance — see open-questions.md).

There is intentionally no auto-merge. Save files don't merge.

> **Implemented** (`engine.ResolveConflict`, owner/admin-gated + CSRF-protected
> `POST /api/games/{id}/resolve-conflict`). The `<ts>` is a UTC, filesystem-safe,
> **nanosecond-precision** stamp (`20060102T150405.000000000Z`), so two resolves in
> the same second can't collide. Every loser-with-a-file is backed up *before* any
> overwrite; the conflict flag clears only after the full fan-out succeeds, so a
> partial failure leaves the game re-resolvable.

## Deactivation

User clicks "Done playing." Server:

1. Runs one final sync pass (primary → peers; cannot conflict because we just synced moments ago — but if it does, leave the binding active and surface the conflict).
2. Deletes `active_bindings` row.
3. Game returns to idle (backup-only).

## Crash safety

- Poll loop is idempotent. Restart at any time; it re-stats and resumes.
- A crash mid-copy can leave a partial file. Always copy to `<dest>.retrosync-tmp` and rename atomically.
- `manifest` is only updated *after* the rename. So a crash mid-copy leaves the manifest pointing at the previous good state, and the next poll will re-detect divergence and retry.

## Stale-peer visibility

Between sessions, the saves on each node are whatever the last bound primary wrote. If Bob plays Tuesday on the MiSTer and unbinds, Wednesday Alice opens the UI to play *Super Metroid* on her Deck and sees:

- Game: Super Metroid
- living-room-mister: 18h ago (Bob's session)
- bob-deck: 18h ago (matches MiSTer; was a peer in Bob's session)
- alice-deck: 4d ago (not a peer in Bob's session)

The UI shows every mtime *before* binding so Alice knows that activating "from my Deck" will overwrite the most recent state. This is by design — surface, don't hide.
