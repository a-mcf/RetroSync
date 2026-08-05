# Which save files sync well

RetroSync mirrors save files that a sync layer (syncthing today) has already
delivered to a shared directory. That makes the **shape of the save file** the
biggest factor in whether syncing is reliable — more than anything RetroSync
itself does.

## The rule

**Small files, written once when you save, sync reliably. Large files that get
rewritten while you play do not.**

| shape | example | verdict |
|---|---|---|
| per-game save | `Game (USA).srm`, `.sav`, `.mcr` | good — a few KB, written when you save |
| per-game folder | PCSX2 **folder** memory card | good — one small directory per game |
| whole-console image | PCSX2 `Mcd001.ps2` (8 MB) and similar | **avoid** |
| save states | `.state*`, `.p2s` | excluded from RetroSync by design |

A whole memory card is one large file holding *every* game's saves, and the
emulator rewrites the entire image as you play, not only when you choose "save".
A file that is constantly changing is a moving target for any sync tool, and a
copy that never lands cleanly can leave you back at an older save. Two things
about this are counter-intuitive and worth knowing: saving repeatedly "to be
safe" makes it worse, and the blast radius is every game on the card, not just
the one you were playing.

If a save goes missing, suspect this first — it presents as *"the game didn't
save"*, which sends you looking in entirely the wrong place.

## Prefer per-game saves

Use a save format that writes one small file, or one small directory, per game.

For PS2, PCSX2 supports **folder memory cards**: the card becomes a directory
with one subfolder per game, and only real save data is stored. The game still
sees a standard 8 MB card, so capacity behaves normally, and one folder card
holds many games comfortably.

To convert: pause syncing for that folder, copy the existing `.ps2` somewhere
outside the synced directory, fully close the emulator, then use its
convert-memory-card action. Check the game loads from the new card before you
remove the old image, and move that image out of the synced folder so it is not
still being transferred.

## Turn on file versioning

Whatever shape your saves are, enable your sync layer's **file versioning** for
folders containing saves (syncthing: folder → Versioning → *Simple*, keep 5–10).
It archives the previous copy whenever a file is replaced, which turns this whole
class of problem from data loss into a restore.

Check every folder that holds saves — it is easy to have versioning on some and
not others, and the gap only shows up on the day you need it.

RetroSync keeps its own capture of a file's previous contents before it
overwrites one (see [`data-model.md`](data-model.md)), but that only covers
writes **RetroSync** makes. It cannot help with a change made underneath it by
the sync layer or the emulator.

## Save states

RetroSync excludes save states (`.state*`) from discovery deliberately — they are
large, emulator- and version-specific, and not portable between devices. If your
sync layer carries the whole save folder, they are being synced whether or not
RetroSync knows about them.

Taking a save state before you exit is a cheap backstop, and worth the habit
while any whole-card image is still in your sync.
