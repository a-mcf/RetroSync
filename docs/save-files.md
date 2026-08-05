# Which save files sync well

RetroSync mirrors save files that a sync layer (syncthing today) has already
delivered to a shared directory. That makes the **shape of the save file** the
single biggest factor in whether syncing is reliable — more than anything
RetroSync itself does.

This page exists because the failure it describes is not a RetroSync bug, is
easy to hit, and presents as *"the game didn't save"* — which sends you looking
in exactly the wrong place. It cost a full evening to diagnose once.

## The rule

**Small files, written once per save, sync reliably. Large files rewritten in
full while you play do not.**

| shape | example | how it behaves |
|---|---|---|
| per-game save | `Game (USA).srm`, `.sav`, `.mcr` | good — a few KB, written when you save |
| per-game folder | PCSX2 **folder** memory card | good — small files, one game's directory per save |
| whole-console image | PCSX2 `Mcd001.ps2` (8 MB), other whole-card images | **bad** — see below |
| save states | `.state*`, `.p2s` | excluded from RetroSync by design; see below |

## Why whole-card images break

A whole memory card is one large file holding *every* game's saves, and the
emulator rewrites the **entire image** each time the card's internal filesystem
changes — repeatedly during play, not just when you choose "save".

That produces a long window in which the file is both being written by the
emulator and being transferred by the sync layer. If a transfer does not
finalize cleanly, syncthing can record a change against *itself* for a file it
was merely copying. The device's newer card then conflicts with the server's
stale one, the newer copy is renamed aside as `.sync-conflict-*`, and the device
is rolled back to whatever the server last fully ingested.

From the player's chair this looks like: **you saved, you exited, you went to
load, and your save was gone** — reverting to some earlier point rather than to
nothing.

Two consequences that are worth knowing because they are counter-intuitive:

- **Saving repeatedly "to be safe" makes it worse.** Each rewrite restarts or
  interrupts the in-flight transfer, so the sync layer never captures a clean
  copy and the version it considers authoritative falls further behind your
  actual progress.
- **The blast radius is every game on the card**, not just the one you were
  playing, because they all live in the same file.

### Diagnostic signature

If you suspect this, these three things appear together within seconds:

1. an orphaned, full-size `.syncthing.<name>.tmp` beside the real file — a
   transfer that completed but never finalized;
2. a version-vector entry for the **server's own device** on a file only the
   device writes (`GET /rest/db/file?folder=<id>&file=<path>`);
3. a `.sync-conflict-*` copy created moments later.

Meanwhile every syncthing status endpoint reports healthy — `errors: 0`,
`pullErrors: 0`, `needBytes: 0`, folder `idle`. **The status plane does not
model this failure.** Do not read green as proof; go look at the files.

## The fix: per-game saves

Prefer a save format that writes one small file (or one small directory) per
game. For PS2, PCSX2 supports **folder memory cards**: the card becomes a
directory with one subfolder per game, and the host stores only the real save
data instead of a full-size image. The emulator still presents a standard 8 MB
card to the game, so capacity behaves normally.

Converting: pause the sync for that folder first, copy the existing `.ps2`
somewhere outside the synced directory, fully close the emulator, then use the
emulator's convert-memory-card action. Verify the game loads from the new card
**before** removing the old image, and move the old image out of the synced
folder afterwards so it is not still transferred.

One folder card holds many games comfortably; splitting per game is unnecessary.
If a folder card ever exceeds the emulated card's capacity, the emulator
presents only a subset of game folders, prioritising the running game — saves
are still on disk but may not all be visible in the in-game card browser. That
is a display filter, not data loss, and it looks alarmingly like the real
failure above. Check the directory before concluding anything.

## Save states

RetroSync **excludes save states** (`.state*`) from discovery deliberately —
they are large, emulator- and version-specific, and not portable between
devices.

They are also less exposed to the failure above, because a state is written once
when you press save and then left alone, so it does not present a moving target
to an in-flight transfer. Observed behaviour, not a guarantee: if your sync layer
carries the whole save folder, states are being synced whether or not RetroSync
knows about them.

A save state taken before exiting remains a cheap and effective backstop, and is
worth the habit while any large-image card is still in your sync.

## Enable file versioning

Whatever shape your saves are, turn on your sync layer's **file versioning** for
folders containing saves (in syncthing: folder → Versioning → *Simple*, keep
5–10). It archives the previous copy whenever a file is replaced, which converts
this entire class of problem from data loss into a restore.

Check every folder that holds saves. It is easy to have versioning on some and
not others, and the gap only becomes visible on the day you need it.

RetroSync keeps its own capture of a file's previous contents before it
overwrites it (see [`data-model.md`](data-model.md)), but that only covers writes
**RetroSync** makes. It cannot help with a change made underneath it by the sync
layer or the emulator.
