// Package store defines the RetroSync domain types and the storage-agnostic
// Store interface. Business logic depends only on this package; it never
// imports pgx or any concrete database driver.
package store

import (
	"context"
	"errors"
	"time"
)

// Typed errors returned by every Store implementation. Callers compare with
// errors.Is so implementations may wrap them with %w.
var (
	// ErrNotFound is returned when a requested entity does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned on a uniqueness violation: a duplicate primary
	// key (id) or a duplicate (sync_id, node_id) manifest / (node_id, path)
	// sync_member.
	ErrConflict = errors.New("store: conflict")
	// ErrInvalidReference is returned when a write references a parent row that
	// does not exist: e.g. a node with an owner_user_id for a missing user, a
	// sync_member naming a missing sync or node, or a manifest/log naming a
	// missing sync (a foreign-key violation). Note: a sync's game is a free-text
	// label, NOT a foreign key, so creating a sync never raises this for the game.
	ErrInvalidReference = errors.New("store: invalid reference")
	// ErrInvalidValue is returned when a field fails a domain/enum constraint:
	// e.g. a bad role, kind, or reach (a CHECK violation).
	ErrInvalidValue = errors.New("store: invalid value")
	// ErrLastAdmin is returned when a write would leave the system with zero
	// admins: deleting the only admin, or demoting them to `user`. It is a
	// lockout guard, enforced INSIDE the write's transaction (see
	// UpdateUserProfile / DeleteUser) so two concurrent demotes can't both observe
	// "there is another admin" and strand the registry with none.
	ErrLastAdmin = errors.New("store: last admin")
)

// Role is a user's authorization level. See docs/auth.md.
type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

// Kind classifies a node by device type. Informational + used to pick a reach
// strategy in later slices.
type Kind string

const (
	KindDeck     Kind = "deck"
	KindMister   Kind = "mister"
	KindAnbernic Kind = "anbernic"
	KindGeneric  Kind = "generic"
)

// Reach is how RetroSync reaches a node. See docs/architecture.md.
type Reach string

const (
	ReachSyncthingShare Reach = "syncthing-share"
)

// Allowed enum value sets, mirroring the CHECK constraints in the registry
// migration. Defined here so storage-agnostic validation (e.g. the in-memory
// store) stays consistent with the schema without importing any driver.
var (
	validRoles = map[Role]bool{RoleUser: true, RoleAdmin: true}
	validKinds = map[Kind]bool{KindDeck: true, KindMister: true, KindAnbernic: true, KindGeneric: true}
	validReach = map[Reach]bool{ReachSyncthingShare: true}
)

// ValidRole reports whether r is an allowed role value.
func ValidRole(r Role) bool { return validRoles[r] }

// ValidKind reports whether k is an allowed kind value.
func ValidKind(k Kind) bool { return validKinds[k] }

// ValidReach reports whether r is an allowed reach value.
func ValidReach(r Reach) bool { return validReach[r] }

// ReachConfig carries the non-secret connection info for a node.
//
// Secrets discipline (docs/auth.md): this struct NEVER holds a cleartext
// password or private key. Do not log its contents without redaction even so,
// to keep paths out of logs.
//
// It carries exactly one field today because there is exactly one reach
// strategy. A future strategy that needs connection details adds its own
// fields here, alongside a `reach` value and an adapter — that is the whole
// cost of adding one, which was the point of the Reach port.
type ReachConfig struct {
	// Path is the server-local filesystem path of the Syncthing share.
	// Set for reach=syncthing-share.
	Path string `json:"path,omitempty"`
}

// User is a human who logs into the web UI.
type User struct {
	ID      string
	Display string
	// PwHash is an argon2 hash (see docs/auth.md). Never a cleartext password.
	PwHash string
	Role   Role
}

// Node is any device that holds save files.
type Node struct {
	ID string
	// OwnerUserID is the owning user's id; nil for shared nodes (e.g. a MiSTer).
	OwnerUserID *string
	Display     string
	Kind        Kind
	Reach       Reach
	ReachConfig ReachConfig
}

// Outcome is the result of a single directional sync copy, recorded in
// sync_log. Mirrors the CHECK constraint in the runtime migration.
type Outcome string

const (
	OutcomeOK       Outcome = "ok"
	OutcomeNoop     Outcome = "noop"
	OutcomeConflict Outcome = "conflict"
	OutcomeError    Outcome = "error"
)

var validOutcomes = map[Outcome]bool{
	OutcomeOK: true, OutcomeNoop: true, OutcomeConflict: true, OutcomeError: true,
}

// ValidOutcome reports whether o is an allowed sync_log outcome value.
func ValidOutcome(o Outcome) bool { return validOutcomes[o] }

// LogEntry is one append-only sync_log row: a single directional copy. A sync
// pass that fans out from primary to N peers writes N entries.
type LogEntry struct {
	// ID is assigned by the store on append (bigserial). Zero on input.
	ID int64
	// TS is the time of the copy; the store defaults it to now() when zero.
	TS time.Time
	// SyncID is the sync this entry concerns (FK; cascades on sync delete).
	SyncID string
	// FromNode/ToNode are historical node ids, unconstrained text (NOT FKs) so
	// a later node removal neither erases nor blocks log history. May be empty.
	FromNode string
	ToNode   string
	// Bytes copied; nil when not applicable (e.g. a noop).
	Bytes *int64
	// SrcMtime / DstMtime: source mtime and the destination's mtime *before*
	// overwrite. Nil when not applicable.
	SrcMtime *time.Time
	DstMtime *time.Time
	Outcome  Outcome
	// Message carries error/diagnostic detail; empty by default.
	Message string
}

// ManifestEntry is the last-known file state for a (sync, node) pair, updated
// by the poll loop. PK is (SyncID, NodeID); cascades on sync/node delete.
type ManifestEntry struct {
	SyncID string
	NodeID string
	// Mtime / Size are the fast-path identity of the file; nil when unknown.
	Mtime *time.Time
	Size  *int64
	// SHA256 is computed lazily (size+mtime is the fast path); nil/"" when not
	// yet computed.
	SHA256 *string
	// LastChecked is when this manifest entry was last WRITTEN (a content
	// change, touch-reconcile, or propagation write) — a noop poll does not
	// update it. Nil if never written.
	LastChecked *time.Time
}

// Sync is a mirror group: a specific set of (node, save-file) members that sync
// together. It is the atomic entity of the data model. "Game" is a free-text
// LABEL on the sync (no separate games table, no FK) — a display grouping that a
// later slice will infer from save filenames. Many syncs may carry the same game
// label so long as they do not share a (node, path) member — see SyncMember and
// docs/data-model.md.
type Sync struct {
	ID string
	// Game is a free-text label (e.g. "Super Metroid"); defaults to "". It is NOT
	// a foreign key — any value is allowed.
	Game string
	// Name is a free-form label; defaults to "".
	Name string
	// ConflictAt is non-nil when the sync forked — two or more members changed
	// between polls — and auto-mirroring is paused until a human resolves it.
	// Nil means the sync is mirroring normally. This is the per-sync runtime
	// flag that replaced active_bindings.conflict_at (slice-18 auto-mirror).
	ConflictAt *time.Time
	// LastSynced is the time of the last successful mirror pass (a fan-out from
	// the single changed member to the others), or a conflict resolution; nil if
	// the sync has never synced. Replaced active_bindings.last_synced.
	LastSynced *time.Time
}

// SaveVersionRetention is the count-based retention cap: the newest N save
// versions are kept per (sync_id, node_id), ordered by the monotonic seq (NOT a
// timestamp — a bad clock must never be able to misorder/evict). Older versions
// are pruned on each PutSaveVersion, and any blob no surviving version
// references is garbage-collected.
const SaveVersionRetention = 10

// SaveVersion is one captured pre-overwrite snapshot of a member's save, indexed
// in save_versions and pointing at a content-addressed save_blobs row. It is the
// recovery net: before RetroSync overwrites any member's save (propagation or
// conflict resolve) it snapshots the about-to-be-overwritten bytes here, so a
// bad propagation/resolve is always recoverable.
type SaveVersion struct {
	// Seq is the monotonic retention key (bigserial), assigned by the store. It
	// orders versions for retention and newest-first listing; a bad clock can
	// never misorder it.
	Seq int64
	// SyncID / NodeID identify the member this snapshot was captured from.
	SyncID string
	NodeID string
	// Hash is the lowercase-hex sha256 of the captured bytes (the save_blobs PK).
	Hash string
	// CapturedAt is display-only ("ago" in the history UI); it is NEVER used for
	// ordering or retention.
	CapturedAt time.Time
	// Size is the captured blob's size in bytes (carried from save_blobs).
	Size int64
	// Reason is a short tag for why the snapshot was taken, e.g. "propagate" or
	// "conflict-resolve".
	Reason string
}

// SyncMember is one (node, file) member of a Sync. The store enforces two
// uniqueness rules: PK (SyncID, NodeID) — a node appears at most once per sync —
// and a global UNIQUE (NodeID, Path): a given (device, file) lives in at most
// one sync. The second is the core invariant of the sync data model; it permits
// multi-slot (same node, different path -> different sync) while forbidding the
// same file from being claimed by two syncs.
type SyncMember struct {
	SyncID string
	NodeID string
	Path   string
}

// Store is the storage-agnostic persistence boundary. All methods are
// context-first and return ErrNotFound / ErrConflict where documented.
type Store interface {
	// Users.
	CreateUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, id string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	// The user updates are deliberately FIELD-SCOPED rather than one whole-row
	// setter. A whole-row Update forces every caller into a read-modify-write, and
	// those callers touch disjoint fields: the admin edit form writes display+role,
	// the password paths write pw_hash. With a whole-row write, a password change
	// that read the row before a concurrent demotion committed would write the old
	// role back and silently un-demote the user — a privilege the operator
	// explicitly revoked, restored by an unrelated write. Scoping the statement to
	// the columns the caller actually means removes the interleaving entirely,
	// rather than narrowing its window.

	// UpdateUserProfile rewrites a user's display and role, LEAVING pw_hash
	// untouched. Missing -> ErrNotFound; a bad role -> ErrInvalidValue. Demoting
	// the ONLY admin (role leaving "admin" when no other admin exists) ->
	// ErrLastAdmin: the write is refused so the system can never be left with
	// nobody who can administer it. The check runs inside the same transaction as
	// the update (and takes a lock over the admin rows), so concurrent demotes
	// serialize instead of racing to zero admins.
	UpdateUserProfile(ctx context.Context, id string, display string, role Role) error

	// UpdateUserPassword replaces a user's pw_hash, LEAVING display and role
	// untouched. Missing -> ErrNotFound. There is no last-admin guard because a
	// password change cannot alter the admin count — which is the point: resetting
	// the sole admin's password must never be entangled with the lockout guard.
	UpdateUserPassword(ctx context.Context, id string, pwHash string) error
	// DeleteUser removes a user. Missing -> ErrNotFound. Deleting the ONLY admin
	// -> ErrLastAdmin (same in-transaction guard as UpdateUserProfile). A user who still
	// owns nodes -> ErrInvalidReference: nodes.owner_user_id REFERENCES users (id)
	// with NO on-delete action, deliberately — orphaning or cascading away a
	// person's devices would be worse than refusing. Reassign or delete the nodes
	// first.
	DeleteUser(ctx context.Context, id string) error

	// Nodes.
	CreateNode(ctx context.Context, n Node) error
	GetNode(ctx context.Context, id string) (Node, error)
	ListNodes(ctx context.Context) ([]Node, error)
	UpdateNode(ctx context.Context, n Node) error
	DeleteNode(ctx context.Context, id string) error

	// SyncLog (append-only).
	// AppendLog: bad outcome -> ErrInvalidValue; missing sync ->
	// ErrInvalidReference. A non-zero LogEntry.TS is honored as-is; a zero TS
	// defaults to now() (UTC). The store assigns the entry's id (bigserial);
	// AppendLog does not write it back into the passed entry, so callers that
	// need the id re-read via ListLogBySync.
	AppendLog(ctx context.Context, e LogEntry) error
	// ListLogBySync returns a sync's entries most-recent-first, ordered by ts
	// descending, with ties broken by id descending. Capped at limit
	// (limit <= 0 means no cap). Because ts may be caller-supplied or skewed
	// (RTC-less / drifting clocks), this ts-then-id ordering — not insertion
	// order — is the contract both implementations honor.
	ListLogBySync(ctx context.Context, syncID string, limit int) ([]LogEntry, error)

	// Manifest (per-side last-known file state).
	SetManifest(ctx context.Context, m ManifestEntry) error // upsert on (sync_id, node_id)
	GetManifest(ctx context.Context, syncID, nodeID string) (ManifestEntry, error)
	ListManifestBySync(ctx context.Context, syncID string) ([]ManifestEntry, error)

	// Syncs and SyncMembers — the unit of mirroring. The runtime tables
	// (manifest, sync_log) above key off sync_id; the per-sync runtime state
	// (conflict_at, last_synced) lives on the sync row itself. The engine,
	// daemon, and web all operate on a Sync and its SyncMembers.

	// CreateSync inserts a sync. Duplicate id -> ErrConflict. The Game label is
	// free text (no FK), so any value — including "" — is accepted. A
	// freshly-created sync has nil ConflictAt/LastSynced.
	CreateSync(ctx context.Context, sy Sync) error
	GetSync(ctx context.Context, id string) (Sync, error)
	// ListSyncs returns ALL syncs, ordered by id. The daemon sweeps every sync
	// each poll (there is no "active" subset anymore — every sync auto-mirrors),
	// so it lists them all and polls each.
	ListSyncs(ctx context.Context) ([]Sync, error)
	// SetSyncConflict sets (or, with at==nil, clears) the sync's conflict_at
	// flag. Setting it pauses auto-mirroring for that sync until a human
	// resolves; clearing it (conflict resolution) resumes mirroring. Missing
	// sync -> ErrNotFound. It does NOT touch last_synced.
	SetSyncConflict(ctx context.Context, syncID string, at *time.Time) error
	// MarkSyncSynced sets the sync's last_synced to t (a successful mirror pass).
	// It does NOT touch conflict_at. Missing sync -> ErrNotFound.
	MarkSyncSynced(ctx context.Context, syncID string, t time.Time) error
	// UpdateSync rewrites the mutable fields (game label, name) of an existing
	// sync. Missing -> ErrNotFound. The game label is free text (no FK).
	UpdateSync(ctx context.Context, sy Sync) error
	// DeleteSync removes a sync; missing -> ErrNotFound. Its members cascade.
	DeleteSync(ctx context.Context, id string) error

	// SetSyncMember upserts on (sync_id, node_id). Missing sync/node ->
	// ErrInvalidReference. A (node_id, path) already claimed by a DIFFERENT sync
	// -> ErrConflict (the global UNIQUE (node_id, path) invariant). Re-setting an
	// existing (sync_id, node_id) to a new path is an in-place update.
	SetSyncMember(ctx context.Context, m SyncMember) error
	GetSyncMember(ctx context.Context, syncID, nodeID string) (SyncMember, error)
	ListSyncMembers(ctx context.Context, syncID string) ([]SyncMember, error)
	ListSyncMembersByNode(ctx context.Context, nodeID string) ([]SyncMember, error)
	DeleteSyncMember(ctx context.Context, syncID, nodeID string) error

	// SaveVersions — the server-side, content-addressed recovery net. Before any
	// overwrite of a member's save, the engine captures the about-to-be-overwritten
	// bytes here, so a bad propagation/resolve is always recoverable.

	// PutSaveVersion captures data as a new version for (syncID, nodeID) with the
	// given content hash (lowercase-hex sha256) and reason. It:
	//   1. upserts the blob by hash (dedup — identical content is stored once);
	//   2. appends a save_versions row (assigning a monotonic seq) UNLESS the most
	//      recent version for this (sync,node) already has this exact hash, in which
	//      case it is a no-op (no churn on identical re-saves);
	//   3. prunes to keep only the newest SaveVersionRetention versions per
	//      (sync,node) ordered by seq DESC, deleting older rows;
	//   4. garbage-collects any save_blobs row no save_versions row references.
	// Missing sync/node -> ErrInvalidReference.
	PutSaveVersion(ctx context.Context, syncID, nodeID, hash string, data []byte, reason string) error
	// ListSaveVersions returns the versions for (syncID, nodeID) newest-first by
	// seq, metadata only (no bytes). Capped at limit (limit <= 0 means no cap).
	ListSaveVersions(ctx context.Context, syncID, nodeID string, limit int) ([]SaveVersion, error)
	// GetSaveVersionData returns a version and its blob bytes, for restore. The
	// seq MUST belong to syncID: the lookup is bound to (sync_id, seq) so a seq
	// owned by a DIFFERENT sync is ErrNotFound, not someone else's data. This
	// store-enforced binding is the authz floor for restore — the web layer
	// authorizes the caller against a sync's members, then passes that same
	// syncID here, so a guessed cross-sync seq can never be loaded (and thus never
	// restored). A seq that no longer exists (e.g. pruned) -> ErrNotFound.
	GetSaveVersionData(ctx context.Context, syncID string, seq int64) (SaveVersion, []byte, error)
}
