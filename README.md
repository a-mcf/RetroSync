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

Implemented and working end-to-end. Built across 11 vertical slices, all merged.

retrosync is a **Go** service backed by **Postgres**. Everything — build, unit
tests, integration tests against a real Postgres, and the production image — runs
in **podman**; the host stays clean (no Go or Postgres installed locally). The
single verification command is `make ci` (fmt, vet, unit, integration, image
build); `make build` builds the production image, `make test-integration` runs the
Postgres-backed integration suite.

### What works

- **Registry** — admin-gated CRUD over games, nodes, and per-(game, node) path
  mappings (`/games`, `/nodes` pages and their `POST /api/...` mutations).
- **Auth** — username + password login with **argon2id** hashing and per-session
  cookie sessions; a per-session CSRF token guards every state-changing POST.
- **Dashboard** — the read-only home view plus the play-sync actions:
  **Play on \<node\>** / **Done playing**, the activation "use my save" modal, and
  **force-takeover** of a game another node holds.
- **Conflict detection + resolution** — the poll loop flags a conflict when a
  non-primary peer mutates (or primary+peer diverge) and pauses that game; the
  conflict modal shows every node's live state and a per-node "use this one"
  button. Resolution backs up each loser's current file to a sibling
  `<path>.retrosync-conflict-<ts>` (nanosecond, filesystem-safe timestamp) before
  fanning the winner out.
- **Poll daemon** — a background ticker that sweeps the active bindings every
  `RETROSYNC_POLL_INTERVAL` and runs one engine pass per bound game; errors are
  isolated per game and the sweep is idempotent (crash-safe).
- **localfs reach adapter** — `syncthing-share` nodes are read/written as ordinary
  local filesystem paths (rooted at `reach_config.path`), via atomic
  temp-file-then-rename writes.

### Running it

The binary is `retrosync`; with no subcommand (or `serve`) it connects to
Postgres, runs migrations, then runs the web UI and the poll daemon together under
one signal context.

```sh
# 1. Build the production image (distroless, static binary) in podman.
make build                       # -> retrosync:dev

# 2. Run Postgres (any Postgres 16 works; here, podman).
podman run -d --name retrosync-pg \
  -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=retrosync \
  -p 5432:5432 docker.io/library/postgres:16

# 3. Run the server.
podman run --rm --network host \
  -e DATABASE_URL='postgres://postgres:secret@127.0.0.1:5432/retrosync?sslmode=disable' \
  retrosync:dev serve            # listens on :8080 by default

# 4. Bootstrap the first admin. The password comes from RETROSYNC_PASSWORD
#    (NOT argv — argv leaks via `ps`); it can also be piped on stdin.
podman run --rm --network host \
  -e DATABASE_URL='postgres://postgres:secret@127.0.0.1:5432/retrosync?sslmode=disable' \
  -e RETROSYNC_PASSWORD='choose-a-strong-password' \
  retrosync:dev user set alice --role admin
```

**Environment variables** (`cmd/retrosync/config.go`):

| var                       | required | default | meaning                                    |
|---------------------------|----------|---------|--------------------------------------------|
| `DATABASE_URL`            | yes      | —       | Postgres DSN (pgx)                         |
| `RETROSYNC_HTTP_ADDR`     | no       | `:8080` | HTTP listen address                        |
| `RETROSYNC_POLL_INTERVAL` | no       | `15s`   | active-binding sweep cadence               |
| `RETROSYNC_POLL_TIMEOUT`  | no       | `60s`   | per-game poll timeout                      |
| `RETROSYNC_PASSWORD`      | `user set` only | — | password for `retrosync user set <id>` |

**SELinux note:** on an enforcing host (Fedora/RHEL), a save directory
bind-mounted into the container needs the `:Z` (or `:z`) volume suffix so the
container can read/write it — e.g. `-v /srv/syncthing/bob-deck:/saves/bob-deck:Z`.
Without it, the container's reads/writes are denied. (The `Makefile` already uses
`:Z` on its bind mounts for the same reason.)

### Implemented vs deferred

Deferred (registered as `TODO(slice-...)` hook points in the code, not yet built):

- **ssh/sftp reach adapter** — `ssh` nodes resolve to "not supported yet"; only
  `syncthing-share` (localfs) is wired. This also means writeback into a
  Syncthing-share device (which the design routes over SSH) is not yet possible.
- **Syncthing-status poller** — node `reachable` / backup-health are not yet read
  from Syncthing, so those fields are cosmetic until a status poller lands.
- **Pushover notifier** — no outbound notifications yet.
- **Containerized browser e2e** — handlers are tested directly; no headless-browser
  end-to-end suite.
- **Deploy / k8s manifests** — the CNPG + Syncthing-sidecar layout is documented
  (architecture.md) but no manifests ship in this repo.

Specs live in [`docs/`](docs/):

- [`docs/architecture.md`](docs/architecture.md) — components, channels, deployment shape
- [`docs/data-model.md`](docs/data-model.md) — registry schema, active bindings, manifest
- [`docs/state-machine.md`](docs/state-machine.md) — activation, conflicts, deactivation
- [`docs/api.md`](docs/api.md) — the HTTP surface (HTMX/POST-driven)
- [`docs/ui.md`](docs/ui.md) — screens and flows
- [`docs/auth.md`](docs/auth.md) — per-user identity, node reachability
- [`docs/open-questions.md`](docs/open-questions.md) — design points (resolved + still-open)

## Non-goals

- Save format conversion (SRM/SAV/state). Devices we care about agree on raw formats for the systems we care about.
- Cloud sync to third parties. Self-hosted only.
- Replacing Syncthing for backup. retrosync coexists with it.
- Auto-detection of emulator layouts. EmuDeck symlinks-to-flat-dir defeat detection; the user maps paths explicitly per game.
