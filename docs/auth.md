# Auth

> **Implemented.** Login (argon2id), server-side sessions, a per-session CSRF
> token on every state-changing POST, and admin-gating of the registry are all
> built (`internal/web`, `internal/auth`). The `user set` CLI bootstraps the first
> admin; **every user after that is managed in the app** (`/users`, admin-only)
> and anyone can change their own password on `/settings`. Reaching nodes is
> implemented for `syncthing-share` (localfs), the only reach strategy.

## Identities

- **Users** log into the web UI with username + password (**argon2id**, OWASP
  baseline params, constant-time verify). Sessions are server-side, keyed by an
  `HttpOnly`/`Secure`/`SameSite=Lax` cookie; the session token and a paired CSRF
  token rotate on every login (session-fixation defense).
- **Nodes** are not authenticated peers — retrosync never connects *to* a device. Every device runs Syncthing and shares its save folder to the server; retrosync reads and writes those shares as local files.

There's no per-node API token because nodes don't call retrosync; retrosync calls nodes (or reads files local to itself).

## Roles

- `user` — can act on their own nodes. Can resolve a conflict on any sync that includes a node they own. Can view other syncs but not resolve their conflicts.
- `admin` — can edit the registry, add nodes, create/edit/delete syncs, and resolve any sync's conflict.

Single-tenant household — keep this minimal, two roles is enough.

**Implemented gating.** Registry mutations (`/nodes`, `/syncs` and their
members, smoke-test, `/users`) are admin-only — a non-admin gets a `403` before
the Store is touched. Resolving a conflict
(`/api/syncs/{id}/resolve-conflict`) requires the caller to **own a member node
of that sync** *or* be an admin — enforced by `userOwnsAnyMember` (admins always
pass). Viewing a sync and its conflict modal is allowed for any authenticated
user (read-only). `/settings` is the one authenticated-but-not-admin mutation:
changing your own password. See api.md for the per-route status codes.

## Managing people

`retrosync user set` bootstraps the **first** admin (and remains the recovery
path). Because it is the recovery path, an omitted `--role`/`--display` on an
existing user leaves that field as it is rather than reapplying the flag default:
`user set alice` sets a password and nothing else. Applying the `--role` default
would demote the account being recovered, and on a single-admin install the
last-admin guard would then refuse the whole write. Everything after that happens
in the app:

- **`/users`** (admin-only) — add a person, edit display name + role, reset a
  password, delete an account. See ui.md for the refusals and api.md for the
  status codes.
- **`/settings`** (any signed-in user) — change your own password, proving the
  current one first. This is the only user-management action a non-admin has.
  Admins also reach `/users` from here; it is deliberately not in the top nav.

**One hashing rule, one hashing path.** Every place a password is set — CLI,
admin create, admin reset, self-service change — calls the same
`auth.ValidatePassword` (≥ 8 characters, no composition theatre) and the same
`auth.Hash` (argon2id, identical cost parameters). Login does not apply the
length rule, so an older short password can still sign in and be changed.
`pw_hash` is never rendered in a response or written to a log.

**Never lock the household out.** The store refuses to delete or demote the last
remaining admin (`store.ErrLastAdmin`), checked *inside the write's transaction*
with a `SELECT ... FOR UPDATE` over the admin rows — a read-then-write check
outside the transaction would let two concurrent demotes both pass and leave zero
admins. Self-delete is refused outright, and deleting a person who still owns
nodes is refused with a message naming the count (`nodes.owner_user_id` has no
`ON DELETE`: cascading or orphaning devices to make a delete succeed would be
worse than saying no).

**User writes are field-scoped, never whole-row.** The store exposes
`UpdateUserProfile` (display + role) and `UpdateUserPassword` (pw_hash), and no
setter that writes all three. A single whole-row `UpdateUser` forces every caller
into a read-modify-write, and the callers touch disjoint fields: a self-service
password change that read the row a moment before an admin's demotion committed
would write the old role straight back, silently restoring a privilege that was
just revoked — with nothing in the logs to show it. Scoping each statement to the
columns its caller actually means removes the interleaving rather than narrowing
its window, and it is what lets the sole admin's password be reset without the
write colliding with the last-admin guard.

**Session invalidation.** Sessions are server-side (a token-keyed map in
`internal/web`), so they are revocable: a password reset, a role change, and a
delete all destroy every session of the affected user, and a self-service
password change destroys them all and mints a fresh session for the caller (new
session token + new CSRF token). Caveat: that map is **per process** — a restart
logs everyone out (acceptable), but with more than one replica a revocation only
reaches the replica that served the request. RetroSync runs single-replica today;
a shared/persistent session store is the follow-up if that changes.

## Reaching nodes

For each node, retrosync needs credentials *to itself*, not to the user. Stored encrypted in the registry:

- **Syncthing-share node** (Deck): `backup_root` is a path on the server's filesystem. No creds needed beyond filesystem permissions.
There are **no per-node credentials today** — the only reach strategy is a local
filesystem path, so there is nothing to authenticate to. `reach_config` carries
non-secret connection info only.

If a future strategy ever needs a credential, it goes in the host's secret store
(sops file, vault agent) behind a *reference* in `reach_config` — never a
cleartext secret in the column. The Postgres conformance suite guards that column
against credential-shaped fields precisely so this stays true.

## Pairing a new device

Power-user flow (admin, via `/nodes`):

1. Add the node entry: id, kind, display, reach-config.
2. retrosync runs a smoke test (stat the node's save root). **Implemented for
   `syncthing-share`** (a localfs stat), which is every node.
3. Smoke pass → node appears as available; it can now be added as a member of a
   sync (via `/discover` or the `/syncs` member picker).

No QR-code pairing dance. Family-scale; the admin can set a few up by hand.
