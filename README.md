# retrosync

Self-hosted, opt-in save sync between any two (or more) game devices that expose a sync folder. Originally born from "MiSTer FPGA in the living room, Steam Decks scattered around the house," but the model generalizes:

- MiSTer ↔ Deck
- Deck ↔ Deck (couch co-op continuity)
- Anbernic ↔ Deck (KNULLI/Batocera handhelds)
- Anything ↔ Anything, as long as retrosync can read and write the file.

Replaces [SGM-Helper](https://github.com/joeblack2k/SGM-Helper) on the Deck side. SGM-Helper is great for "one user, one device tree per emulator," but it can't represent:

1. A flat RetroArch saves directory shared across cores (writes always nest under `<system>/`).
2. A household with multiple devices where only one person should be the live save authority for a given game at a time.

retrosync is opinionated about *what gets synced when*, and lets a human flip the switch.

## The two channels

retrosync separates **backup** from **play**.

### Backup channel — always on, never the source of truth

Each device runs a Syncthing send-only share of its save directory(s) to the server. The server keeps a per-device snapshot. This is disaster recovery, not gameplay sync. No reverse direction, no merging. If a device dies, you can fish saves out of the snapshot manually.

Syncthing handles this well already. retrosync does not own this channel — it only reads from it (so the UI can show "last backup: 12 min ago for alice's Deck").

### Play channel — per-game, exclusive, opt-in

When you sit down to play *Super Metroid*, you bind that game to your device in the retrosync UI. While bound:

- Server watches the file on the bound device and on every other configured peer.
- Newer mtime wins; the others get overwritten.
- Only one binding at a time per game.

When you stop playing, you unbind. The game falls back to backup-only.

This avoids the failure mode where every device silently writes its own copy back to a shared peer and the last-writer-wins eats someone's progress.

## Why nodes, not "Deck and MiSTer"

In the registry, every device is a **node**. A node has a kind (`mister`, `deck`, `anbernic`, `generic`), a way for retrosync to reach it (Syncthing share path on the server, or SSH/SFTP), and a path on it for each game you've registered.

A binding picks a *primary* node — the one you're actively playing on — and retrosync mirrors its file to every other node that has a path mapped for that game.

The "Deck and MiSTer" pairing falls out of this naturally; so does "Bob's Deck and Alice's Deck for couch swap."

## Family-sync motivation

Bob, Alice, and the kid each have a Deck. The MiSTer lives in the living room. Three different RetroArch directory layouts (flat, EmuDeck per-core, custom). Anyone can play, anywhere. retrosync stores per-game *and* per-node path mappings, then asks "who's the live editor right now?" before pushing anything anywhere.

## Status

Spec only. No code yet.

Specs live in [`docs/`](docs/):

- [`docs/architecture.md`](docs/architecture.md) — components, channels, deployment shape
- [`docs/data-model.md`](docs/data-model.md) — registry schema, active bindings, manifest
- [`docs/state-machine.md`](docs/state-machine.md) — activation, conflicts, deactivation
- [`docs/api.md`](docs/api.md) — REST/HTMX endpoints
- [`docs/ui.md`](docs/ui.md) — screens and flows
- [`docs/auth.md`](docs/auth.md) — per-user identity, node reachability
- [`docs/open-questions.md`](docs/open-questions.md) — undecided design points

## Non-goals

- Save format conversion (SRM/SAV/state). Devices we care about agree on raw formats for the systems we care about.
- Cloud sync to third parties. Self-hosted only.
- Replacing Syncthing for backup. retrosync coexists with it.
- Auto-detection of emulator layouts. EmuDeck symlinks-to-flat-dir defeat detection; the user maps paths explicitly per game.
