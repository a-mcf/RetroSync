# RetroSync

Self-hosted, opt-in game-save sync for retro handhelds, MiSTer, Steam Decks, and
anything else that exposes a save folder. Born from "MiSTer FPGA in the living
room, Steam Decks scattered around the house," but the model generalizes to any
mix of devices:

- MiSTer ↔ Deck
- Deck ↔ Deck (couch co-op continuity)
- Anbernic ↔ Deck (KNULLI/Batocera handhelds)
- Anything ↔ Anything, as long as RetroSync can read and write the save file.

You play on whichever device is in your hands; your save follows you to the
others automatically. RetroSync never silently overwrites a save that changed
out from under it, and it keeps a server-side version history so a bad sync is
always recoverable.

## Mental model

RetroSync has one core concept: a **sync**.

> A **sync** is a named mirror group — a set of `(node, save-file)` members that
> are kept byte-identical to each other.

Everything else falls out of that:

1. **Discover** — point RetroSync at your devices and let `/discover` scan their
   save directories. It infers a game name from each save filename and groups
   "this game is on these devices," so you don't hand-build anything.
2. **Create a sync** — one click from the discovered candidates (or by hand on
   `/syncs`). A sync carries a free-text **game label** (just a display grouping,
   e.g. "Super Metroid") and a set of members — a `(node, path)` per device.
3. **Just play** — there is no "activate," no "I'm playing now" button, no
   primary device. The daemon polls every sync; when exactly one member's save
   has changed, it propagates those bytes to the other members. The save follows
   you.

### Per-person isolation

A given save file lives in **exactly one** sync (enforced by a
`UNIQUE(node, path)` invariant). So Bob's *Super Metroid* stream and Alice's
*Super Metroid* stream are two separate syncs that happen to share the same
"Super Metroid" label — their saves never cross.

### True-fork conflicts

If two devices change the *same* sync's save to **different** content between
polls (Bob played on the MiSTer, Alice played on her Deck), that is a genuine
fork. RetroSync does **not** pick a winner — it pauses the sync and asks you to
choose. (Two devices that coincidentally arrive at the *same* bytes are not a
fork; they just agree.) Save files don't merge, so RetroSync surfaces the
collision instead of guessing.

### Versioned recovery net

Before RetroSync overwrites any member's save — during a normal propagation *or*
a conflict resolution — it snapshots the about-to-be-overwritten bytes into a
server-side, content-addressed store. Every sync's `History` page lists the last
10 versions per device with a one-click **Restore**. A bad propagation or a
wrong conflict pick is never destructive.

## Safety properties

- **Never silently overwrite a changed save.** The propagate path only writes to
  members that did *not* change since the last poll. A member that changed to
  content not shared by the winner is, by construction, a second distinct hash —
  so it lands in the conflict case, where nothing is written.
- **Capture before overwrite.** Snapshotting the old bytes is a hard gate: if the
  capture fails, the write is aborted and retried next poll. Nothing recoverable
  is destroyed un-captured.
- **A touch is not a change.** Detection is two-tier — mtime+size is the cheap
  gate, sha256 is content truth. A file whose mtime moved but whose bytes are
  identical does not propagate.
- **A fork halts.** Two or more distinct hashes among the changed members pauses
  the sync until a human resolves it. No auto-merge, no last-writer-wins.

## Architecture

RetroSync is a single **Go 1.23** service backed by **Postgres**.

- **Syncthing is the transport, not RetroSync.** Each device send-only-shares its
  save directory to the server via Syncthing. RetroSync reads those shares as
  ordinary local files and, when it propagates a save, writes back into the local
  share — Syncthing then replicates it out. RetroSync is *not* itself a network
  file-mover.
- **Auto-mirror daemon.** A background ticker (`internal/daemon`) sweeps every
  sync each `RETROSYNC_POLL_INTERVAL` (default 15s) and runs one engine pass
  (`internal/engine`) per sync, with per-sync error isolation. The engine counts
  the changed members: 0 → noop, exactly 1 distinct hash → propagate to the
  others, 2+ distinct hashes → conflict + pause.
- **Web UI.** stdlib `net/http` + `html/template` + **HTMX** — server-rendered
  fragments, no SPA, no build step. The dashboard is a **status view** (it shows
  what each device holds and whether a sync is paused); the only state-changing
  controls are **Resolve conflict** and **Restore**.
- **Auth.** Username + password with **argon2id** (OWASP-baseline params,
  constant-time verify), server-side sessions (`HttpOnly`/`Secure`/`SameSite=Lax`
  cookie), and a per-session **CSRF** token on every state-changing POST.
- **Storage.** All business logic depends on a Go `Store` interface
  (`internal/store`), never on the driver. Two implementations — a thread-safe
  in-memory fake and a **pgx v5** Postgres store with hand-written parameterized
  SQL (no ORM) — run one shared conformance suite so they cannot drift.
  Migrations are embedded and applied at startup.
- **The `reach` abstraction.** Nodes are reached through a `Reach` adapter
  resolved from the node's reach strategy. Today only **`syncthing-share`** is
  wired — a localfs adapter rooted at `reach_config.path`, with atomic
  temp-file-then-rename writes. The **`ssh`/sftp** adapter is a documented future
  strategy: `ssh` nodes currently resolve to "not supported yet"
  (`reach.ErrUnsupportedReach`).

### Deployment

RetroSync runs in **Kubernetes** as a **standalone deployment** that mounts the
same network volume backing Syncthing's per-device save shares, so RetroSync
reads them as local filesystem paths (and runs with Syncthing's uid/fsGroup so
writes through the share stay mutually accessible). The database is
**Postgres** provisioned by **CNPG** (CloudNativePG) in the cluster; RetroSync
connects via `DATABASE_URL`. Container images are published to
`ghcr.io/a-mcf/retrosync` on version tags. (Manifests live in the deployment
repo, not here; the shape is documented in
[`docs/architecture.md`](docs/architecture.md).)

## Getting started

Everything — build, unit tests, integration tests against a real Postgres, and
the production image — runs in **podman**; the host stays clean (no Go or
Postgres installed locally). The `Makefile` is the single entrypoint:

| target              | what it does                                                        |
|---------------------|---------------------------------------------------------------------|
| `make ci`           | the full gate: fmt-check, vet, unit tests, integration tests, image build (`>> CI OK`) |
| `make build`        | build the production container image (`retrosync:dev`)              |
| `make test`         | unit tests in a container (no database)                             |
| `make test-integration` | spin up a podman Postgres, run integration-tagged tests, tear it down |
| `make vet`          | `go vet` in a container                                             |
| `make fmt-check`    | fail if anything is not gofmt-clean                                 |
| `make tidy`         | `go mod tidy` in a container                                        |
| `make clean`        | remove the test Postgres container if present                      |

`make verify` is an alias for `make ci`.

### Running it

The binary is `retrosync`. With no subcommand (or `serve`) it connects to
Postgres, runs migrations, then runs the web UI and the auto-mirror daemon
together under one signal context.

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

`user set <id> [--display NAME] [--role user|admin]` upserts a web user; run it
once with `--role admin` to bootstrap. `--role` defaults to `user`.

### Environment variables

| var                       | required          | default | meaning                              |
|---------------------------|-------------------|---------|--------------------------------------|
| `DATABASE_URL`            | yes               | —       | Postgres DSN (pgx)                   |
| `RETROSYNC_HTTP_ADDR`     | no                | `:8080` | HTTP listen address                  |
| `RETROSYNC_POLL_INTERVAL` | no                | `15s`   | per-sync sweep cadence               |
| `RETROSYNC_POLL_TIMEOUT`  | no                | `60s`   | per-sync poll timeout                |
| `RETROSYNC_SHARE_ROOT`    | no                | `/shares` | server-side mount the node-registry folder picker browses (absolute; scope it to the saves volume — pointing it at `/` lets admins browse every container file name) |
| `RETROSYNC_PASSWORD`      | `user set` only   | —       | password for `retrosync user set <id>` |

**SELinux note:** on an enforcing host (Fedora/RHEL), a save directory
bind-mounted into the container needs the `:Z` (or `:z`) volume suffix so the
container can read/write it — e.g. `-v /srv/syncthing/bob-deck:/saves/bob-deck:Z`.
The `Makefile` already uses `:Z` on its bind mounts for the same reason.

**Container write-permissions note:** the production image runs as a non-root user
(`nonroot`), so a bind-mounted save directory must be **writable by that user** or
the atomic write (temp-file-then-rename) propagation fails with `permission denied`.
Under rootless podman the simplest fix is to run the container as your own uid —
`--userns=keep-id --user "$(id -u):$(id -g)"` — or otherwise ensure the mounted save
dirs are writable by the container user. Note that the safety design holds even on
this failure: the write lands in a `*.retrosync-tmp-*` file first, so a failed write
never corrupts the real save.

## What's deferred

Registered as `TODO(...)` hook points in the code, not yet built:

- **ssh/sftp reach adapter** — `ssh` nodes resolve to `reach.ErrUnsupportedReach`;
  only `syncthing-share` (localfs) is wired. Writeback into a device that must be
  reached over SSH (MiSTer's RO root, locked-down handhelds) is blocked on this.
- **Syncthing-status poller** — there is no always-on, Syncthing-derived node
  status (reachability / backup-health). The only reachability check today is the
  on-demand smoke-test, which probes localfs reachability live and persists nothing;
  a live status poller (and any persistent reachable/last-seen surface) remains
  deferred.
- **JSON mutation API** — the shipped HTTP surface is HTMX/POST-driven; only a
  small read-only `GET /api/{status,syncs,nodes}` JSON surface exists, as the seed
  of a future agent-facing API.
- **Deploy / k8s manifests** — the standalone + shared-volume + CNPG layout is
  documented here, but the manifests live in the deployment repo.

## Documentation

Design and reference docs live in [`docs/`](docs/):

- [`docs/architecture.md`](docs/architecture.md) — components, transport, deployment shape
- [`docs/data-model.md`](docs/data-model.md) — schema (`syncs`, `sync_members`, `manifest`, `save_blobs`/`save_versions`, `sync_log`)
- [`docs/state-machine.md`](docs/state-machine.md) — auto-mirror loop, hash detection, conflicts, restore
- [`docs/api.md`](docs/api.md) — the HTTP surface (HTMX/POST + read-only JSON)
- [`docs/ui.md`](docs/ui.md) — screens and flows
- [`docs/auth.md`](docs/auth.md) — identity, sessions, CSRF, role gating
- [`docs/open-questions.md`](docs/open-questions.md) — design decisions (resolved + still-open)

## Non-goals

- Save-format conversion (SRM/SAV/state). Devices we care about agree on raw
  formats for the systems we care about.
- Cloud sync to third parties. Self-hosted only.
- Replacing Syncthing for backup. RetroSync coexists with it.
- Identifying games by content. RetroSync syncs the paths you mapped; discovery
  infers labels from *filenames*, not ROM headers. (So TGFX16 and anything else
  works as long as the save file is mapped.)
- Save states (`.state*`). Not portable across MiSTer/standalone; excluded.
