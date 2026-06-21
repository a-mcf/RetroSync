# Open questions

Decisions intentionally deferred. Resolve before building, or pick a default and learn.

## Activation default direction

State-machine.md says default to "use the node you're binding from" when multiple nodes have a save. That's right when the binding node really is the freshest source. But when you bind from inside the UI on the couch, it's possible another node is more recent. Maybe the default should be "the node with the newer mtime, but require explicit confirmation if they're within an hour of each other"?

Decision: ship with "the binding node" default. Watch how often users override; revisit.

## Polling interval and watcher

15s poll is fine for couch-pace play. Good enough? An optional node-side agent with inotify would drop latency to sub-second. Worth it?

Decision: poll for v1. Agent is a v2 thinking-about.

## What counts as "the same save"?

Right now: same path on each side, mapped explicitly. A game that uses save *states* in addition to SRAM gets weird — RetroArch stores `.state*` next to `.srm`, and MiSTer doesn't have an equivalent. Do we sync states? Probably no — they're not portable across MiSTer/standalone — but worth being explicit in UI.

Decision: SRAM/SAV only in v1. Document explicitly that states are out of scope.

## Multi-file games

Some games have multi-file save layouts (memory cards, multiple slots). Model right now is one file per (game, node). Extend to "save bundle" later if it bites.

## Peer scope per binding

The model assumes the bound primary mirrors to *all* nodes that have a path mapping for the game. But sometimes you want narrower — "play from Bob's Deck → MiSTer only" without also touching Alice's Deck. The data model already supports per-binding scope (`peer_scope` on `active_bindings`).

Decision: default to `all-configured`. Expose scope override in an "advanced" disclosure on the activation modal. Skip the override entirely in v1 if it adds clutter.

## Syncthing as the local-side transport

For `syncthing-share` nodes, we're reading from the local Syncthing share, which means the device must have run Syncthing recently for retrosync to see updates. If the device is offline, the local share is stale, and retrosync will read stale data.

Mitigations:

- Show "last seen" per node prominently.
- Refuse activation if the binding node's last_seen is too old (configurable threshold).
- Long term: optional retrosync agent on the node that pushes save updates direct to the server, bypassing the Syncthing-as-transport limitation.

## Writeback for syncthing-share nodes

Today's plan: read via local Syncthing share, write back via SSH/SFTP into the device. That requires the device to also be SSH-reachable. For locked-down devices (a stock SteamOS Deck *can* be SSH'd to; a household Anbernic running KNULLI can; some can't), we'd need a fallback.

Open: do we accept "writeable nodes need SSH" as a hard requirement, or do we eventually need a node-side agent that listens for push?

## Conflict resolution UX

Right now: pick a winner. Should we offer "save both, let me sort it" — copy the loser to a `.conflict-<ts>` file before overwriting? Cheap insurance.

Decision: yes, do that. Default behavior, no toggle. Update state-machine.md.

## Identity & name slugs

`super-metroid` works. `super-mario-bros-3` works. Do we need a manual ID at all, or auto-generate from display? Auto with manual override is probably right.

## TGFX16 and other systems SGM-Helper drops

retrosync doesn't classify by content — it syncs paths the user mapped. So TGFX16 is fine here as long as the user maps the path. Worth noting in README.
