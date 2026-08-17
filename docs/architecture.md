# Architecture

## Components

```
   node A              node B               node C            node D
   (Deck:bob)          (Deck:alice)         (Anbernic:kid)    (MiSTer)
   └─ syncthing        └─ syncthing         └─ syncthing      └─ syncthing
        │                   │                    │                │
        └───────────────────┼────────────────────┴────────────────┘
                            │
                  ┌─────────▼──────────┐
                  │  server            │
                  │  ├─ syncthing      │
                  │  │  (backup)       │
                  │  └─ retrosync      │
                  │     ├─ web UI      │
                  │     ├─ registry    │
                  │     └─ auto-mirror │
                  │        poll loop   │
                  └────────────────────┘
```

retrosync the service knows how to reach each node by its **kind** + reach-config:

- **syncthing-share** — the server already has the device's save dir mounted via Syncthing. Pure local filesystem read/write. **This is the only strategy**, and every device kind uses it: a Deck, a MiSTer and an Anbernic all run Syncthing and share their save folder.

Adding a device that *cannot* run Syncthing means writing a new adapter behind the `Reach` port and adding a `reach` value — "register a new strategy," not "fork the codebase." That extension point is the design; a second strategy is not.

> **An `ssh` strategy was removed in migration 0010.** It assumed a MiSTer could
> not run Syncthing (read-only root), so RetroSync would have to SFTP in. The
> MiSTer runs Syncthing, so it never had a device and was never implemented —
> while still being offered in the `/nodes` dropdown, where choosing it produced
> a node that silently never synced.

## Roles

### `retrosync` server

Single Go (or Python) service. Owns:

- The registry (nodes, syncs and their members — each carrying a free-text game
  label, each member a node+path).
- The per-sync manifest (last-known mtime/size per member) and conflict state.
- A poll loop that, for **every** sync, compares each member's live mtime/size
  against the manifest and acts on the changed set: 0 changed is a no-op, exactly
  1 changed is the source and fans out to the others, 2+ changed flags a conflict
  and pauses the sync. There is no primary/take-over and no activate/deactivate —
  every sync mirrors automatically. **Implemented** as `internal/daemon` driving
  `internal/engine`: a background ticker sweeps every sync every
  `RETROSYNC_POLL_INTERVAL` (default 15s), one engine pass per sync, per-sync
  error isolation, idempotent. The only human action is resolving a conflict
  (picking the winning member), after which the sync resumes mirroring.
- A web UI (HTMX, no SPA framework) for browsing the registry and resolving
  conflicts. **Implemented** (`internal/web`), server-rendered fragments, no SPA.
- The HTTP surface the UI is built on. **Implemented as HTMX/POST routes** (HTML
  forms can't issue PATCH/PUT/DELETE); a small read-only JSON surface
  (`GET /api/{status,syncs,nodes}`) is the seed of a future agent-facing API. See
  api.md.

Mounts the same storage as the Syncthing server (see Deployment below) so it has local-filesystem read access to per-device backup snapshots.

### Syncthing (backup)

Independent. Each node send-only-shares its save dirs to the server. retrosync reads from these shares for the local-side view of comparisons. retrosync does not push back through Syncthing.

### Per-node reachability

| reach            | how retrosync reads | how retrosync writes |
|------------------|---------------------|----------------------|
| syncthing-share  | local fs            | local fs, atomic temp-then-rename |

**Writeback goes back through the share**, and Syncthing replicates it outward.
An earlier plan called for writing back *out of band over SSH* to avoid racing
the device's own writes; that is not what shipped and not what is wanted. The
race is handled where it belongs — in the engine, which only writes to members
that did not change since the last poll, and pauses the sync outright when more
than one did.

Writing into the share does mean RetroSync depends on Syncthing noticing the
change. That is a real coupling and it has bitten once: a fan-out that published
a byte-identical mtime *and* size was invisible to Syncthing's scanner. See the
mtime policy in `state-machine.md` for the rule that came out of it.

### MiSTer specifics

The MiSTer runs Syncthing and shares its save folder like every other device, so
it is an ordinary `syncthing-share` node — `kind: mister` is informational only.
Its read-only root once motivated an SSH-based design; that turned out to be
unnecessary (see migration 0010).

## Backup vs. mirroring (recap)

- **Backup** (Syncthing) is always-on and unidirectional (device → server). It
  must never push back to a device. If retrosync the service dies, backup keeps
  working. retrosync only *reads* from these shares.
- **Mirroring** (retrosync's auto-mirror loop) is bidirectional, but only the
  syncs you've configured are ever touched. Every configured sync mirrors
  automatically — there is no play session to start. Conflicts are surfaced, not
  auto-merged.

## Deployment

RetroSync runs in **Kubernetes** as a **standalone deployment** in its own namespace. The per-device Syncthing save shares live on a network volume (NFS); RetroSync mounts that same export as its own volume, which preserves the local-side model from above — RetroSync reads *and writes* the shares as ordinary local filesystem paths, and Syncthing replicates its writes back out to the devices. It runs with the same uid/fsGroup as the Syncthing workload so files written by either stay mutually readable and writable. (An earlier plan had RetroSync as a sidecar container in the Syncthing pod; the shared network volume makes that coupling unnecessary — the two deploy and upgrade independently.)

The mount point of that shared save volume is `RETROSYNC_SHARE_ROOT` (default `/shares`). It is the root the node-registry folder picker browses (docs/ui.md) so an admin can point-and-click a device's absolute mount path when registering a syncthing-share node, instead of hand-typing it. It is not existence-checked at startup — a missing directory surfaces as a friendly browse-time message.

The database is **Postgres** provisioned by **CNPG** (CloudNativePG) in the cluster; RetroSync connects via `DATABASE_URL`. See data-model.md for the `Store` interface and the pgx/podman integration tests.

The service is reverse-proxied behind the existing internal ingress. (The earlier single-binary + SQLite-on-a-host plan is superseded by this standalone + CNPG layout.)
