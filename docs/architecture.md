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

## Roles

### `retrosync` server

Single Go (or Python) service. Owns:

- The registry (games, nodes, per-node game paths).
- The active-bindings table.
- A poll loop that, for each active binding, compares mtime+hash across all nodes with a path for the game and pushes the newer copy.
- A web UI (HTMX, no SPA framework) for browsing, binding, unbinding.
- A small REST surface that the UI is built on; agents (if any future) can also call it.

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
- **Play** is small, explicit, and bidirectional. Only the games you've bound are touched. Conflicts are surfaced, not auto-merged.

## Deployment

Single binary + SQLite file (or a directory of JSON; see data-model.md). Reverse-proxied behind the existing internal ingress. Ansible role to install on the same host that runs the household Syncthing.
