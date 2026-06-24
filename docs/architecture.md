# Architecture

## Components

```
   node A              node B               node C            node D
   (Deck:bob)          (Deck:alice)         (Anbernic:kid)    (MiSTer)
   ├─ syncthing        ├─ syncthing         ├─ syncthing       (no agent;
   └─ retrosync-       └─ retrosync-        └─ retrosync-       SSH only)
      agent (opt)         agent (opt)          agent (opt)
        │                   │                    │                │
        └───────────────────┼────────────────────┘                │
                            │                                     │
                  ┌─────────▼──────────┐                          │
                  │  server            │                          │
                  │  ├─ syncthing      │                          │
                  │  │  (backup)       │                          │
                  │  └─ retrosync      │── SFTP/SSH ──────────────┘
                  │     ├─ web UI      │
                  │     ├─ registry    │
                  │     └─ play loop   │
                  └────────────────────┘
```

retrosync the service knows how to reach each node by its **kind** + reach-config:

- **syncthing-share** — the server already has the device's save dir mounted via Syncthing. Pure local filesystem read/write.
- **ssh** — server SSHs in. Used for nodes that don't run Syncthing or where it's awkward (MiSTer's RO root, locked-down handhelds).

Adding a new device kind is "register a new reach-config strategy," not "fork the codebase." A v2 could add agents that push direct.

> **Implemented:** the **syncthing-share → localfs** reach adapter (read *and*
> write, atomic temp-then-rename). The **ssh** strategy is registered but not yet
> wired (resolves to "not supported yet"); see open-questions.md.

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

Runs on the same host as the Syncthing server so it has local read access to per-device backup snapshots.

### Syncthing (backup)

Independent. Each node send-only-shares its save dirs to the server. retrosync reads from these shares for the local-side view of comparisons. retrosync does not push back through Syncthing.

### Per-node reachability

| kind             | how retrosync reads | how retrosync writes |
|------------------|---------------------|----------------------|
| syncthing-share  | local fs            | not via Syncthing — out of band (see below) |
| ssh              | SFTP                | SFTP, atomic rename  |

For syncthing-share nodes the writeback path is the tricky one: writing into the local share would Syncthing-replicate back to the device, but that race-conflicts with the device's own writes. v1 plan: writeback via SSH/SFTP into the device when needed. The Syncthing share is read-only-ish from retrosync's perspective. (Optional v2: agent on the device that takes pushes directly.)

### MiSTer specifics

retrosync uses SSH/SFTP, root, password from a secrets file (MiSTer's RO root prevents key auth without rebuilding linux.img; see the home_infra ansible role for context).

## Why two channels (recap)

- **Backup** must be always-on and unidirectional (device → server). It must never push back to a device. If retrosync the service dies, backup keeps working.
- **Play** is small, explicit, and bidirectional. Only the syncs you've configured are touched. Conflicts are surfaced, not auto-merged.

## Deployment

RetroSync runs in **Kubernetes** as a **sidecar to Syncthing**: the `retrosync` container and the household `syncthing` container share a pod and a **shared volume** that holds the per-device Syncthing save shares. This preserves the local-side model from above — RetroSync still reads those shares as ordinary local filesystem paths (now a shared pod volume), and still writes back out of band via **SSH/SFTP** into the device, never through Syncthing.

The database is **Postgres** provisioned by **CNPG** (CloudNativePG) in the cluster; RetroSync connects via `DATABASE_URL`. See data-model.md for the `Store` interface and the pgx/podman integration tests.

The service is reverse-proxied behind the existing internal ingress. (The earlier single-binary + SQLite-on-a-host plan is superseded by this sidecar + CNPG layout.)
