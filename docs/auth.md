# Auth

> **Implemented.** Login (argon2id), server-side sessions, a per-session CSRF
> token on every state-changing POST, and admin-gating of the registry are all
> built (`internal/web`, `internal/auth`). The `user set` CLI bootstraps the first
> admin. Reaching nodes is implemented for `syncthing-share` (localfs) only; the
> ssh path and the smoke-test for ssh nodes are deferred (open-questions.md).

## Identities

- **Users** log into the web UI with username + password (**argon2id**, OWASP
  baseline params, constant-time verify). Sessions are server-side, keyed by an
  `HttpOnly`/`Secure`/`SameSite=Lax` cookie; the session token and a paired CSRF
  token rotate on every login (session-fixation defense).
- **Nodes** are not authenticated peers — retrosync reaches into them. For a Deck it reads the Syncthing share locally on the server. For a MiSTer it SSHs in. For an Anbernic running KNULLI/Batocera, similar SSH/SFTP.

There's no per-node API token because nodes don't call retrosync; retrosync calls nodes (or reads files local to itself).

## Roles

- `user` — can act on their own nodes. Can resolve a conflict on any sync that includes a node they own. Can view other syncs but not resolve their conflicts.
- `admin` — can edit the registry, add nodes, create/edit/delete syncs, and resolve any sync's conflict.

Single-tenant household — keep this minimal, two roles is enough.

**Implemented gating.** Registry mutations (`/nodes`, `/syncs` and their
members, smoke-test) are admin-only — a non-admin gets a `403` before the Store
is touched. Resolving a conflict (`/api/syncs/{id}/resolve-conflict`) requires the
caller to **own a member node of that sync** *or* be an admin — enforced by
`userOwnsAnyMember` (admins always pass). Viewing a sync and its conflict modal is
allowed for any authenticated user (read-only). See api.md for the per-route
status codes.

## Reaching nodes

For each node, retrosync needs credentials *to itself*, not to the user. Stored encrypted in the registry:

- **Syncthing-share node** (Deck): `backup_root` is a path on the server's filesystem. No creds needed beyond filesystem permissions.
- **SSH/SFTP node** (MiSTer, Anbernic): username + password (or key). Read in from a file on disk that's mode 0600 and owned by the retrosync user. Don't store cleartext in SQLite — keep it in a sidecar `secrets.yaml` referenced by node id, or use the host's existing secret store (sops-encrypted file, vault agent, etc).

The MiSTer-specific note: root fs is read-only there; SSH key auth requires rebuilding linux.img. Until then, password auth + the secret store is the path.

## Pairing a new device

Power-user flow (admin, via `/nodes`):

1. Add the node entry: id, kind, display, reach-config.
2. retrosync runs a smoke test (stat the node's save root). **Implemented for
   `syncthing-share`** (a localfs stat); for `ssh` nodes the smoke-test reports
   "not supported yet (ssh adapter pending)" until the ssh adapter lands.
3. Smoke pass → node appears as available; it can now be added as a member of a
   sync (via `/discover` or the `/syncs` member picker).

No QR-code pairing dance. Family-scale; the admin can set a few up by hand.
