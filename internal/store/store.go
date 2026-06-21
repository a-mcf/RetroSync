// Package store defines the RetroSync domain types and the storage-agnostic
// Store interface. Business logic depends only on this package; it never
// imports pgx or any concrete database driver.
package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Typed errors returned by every Store implementation. Callers compare with
// errors.Is so implementations may wrap them with %w.
var (
	// ErrNotFound is returned when a requested entity does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned on a uniqueness violation: a duplicate primary
	// key (id) or a duplicate (game_id, node_id) game_path.
	ErrConflict = errors.New("store: conflict")
	// ErrInvalidReference is returned when a write references a parent row that
	// does not exist: e.g. a node with an owner_user_id for a missing user, or
	// a game_path naming a missing game or node (a foreign-key violation).
	ErrInvalidReference = errors.New("store: invalid reference")
	// ErrInvalidValue is returned when a field fails a domain/enum constraint:
	// e.g. a bad role, kind, or reach (a CHECK violation).
	ErrInvalidValue = errors.New("store: invalid value")
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
	ReachSSH            Reach = "ssh"
)

// Allowed enum value sets, mirroring the CHECK constraints in the registry
// migration. Defined here so storage-agnostic validation (e.g. the in-memory
// store) stays consistent with the schema without importing any driver.
var (
	validRoles = map[Role]bool{RoleUser: true, RoleAdmin: true}
	validKinds = map[Kind]bool{KindDeck: true, KindMister: true, KindAnbernic: true, KindGeneric: true}
	validReach = map[Reach]bool{ReachSyncthingShare: true, ReachSSH: true}
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
// password or private key. For ssh nodes it carries SecretRef, a pointer into
// the host's secret store (sops file, vault, etc). The actual credential is
// resolved out of band in a later slice. Do not log this struct's contents
// without redaction even so, to keep host/user/path out of logs.
type ReachConfig struct {
	// Path is the server-local filesystem path of the Syncthing share.
	// Set for reach=syncthing-share.
	Path string `json:"path,omitempty"`

	// Host is the ssh host (ip or name). Set for reach=ssh.
	Host string `json:"host,omitempty"`
	// User is the ssh login user. Set for reach=ssh.
	User string `json:"user,omitempty"`
	// SecretRef is a key into the host's secret store identifying the
	// credential for this node. It is a pointer, never the secret itself.
	// Set for reach=ssh.
	SecretRef string `json:"secret_ref,omitempty"`
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
	// LastSeenAt is the last time the node was observed; nil if never.
	LastSeenAt *time.Time
}

// Game is a title in the registry. Paths live in GamePath, not here.
type Game struct {
	ID      string
	Display string
	System  string
	Notes   string
}

// GamePath maps a (game, node) pair to a path on that node. The path is
// relative to a node-defined save root (see docs/data-model.md).
type GamePath struct {
	GameID string
	NodeID string
	Path   string
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

// ValidDirection reports whether d is an allowed active_bindings direction:
// either "from-primary" or "from-peer-<node_id>" with a non-empty node id.
//
// The runtime migration's CHECK uses LIKE 'from-peer-%', which (since % matches
// zero chars) would also admit the bare "from-peer-". We tighten here to require
// a non-empty suffix so the validity boundary matches engine.sourceNodeID, which
// rejects an empty source id. The DB CHECK remains a coarser backstop.
func ValidDirection(d string) bool {
	if d == "from-primary" {
		return true
	}
	id := strings.TrimPrefix(d, "from-peer-")
	return id != d && id != ""
}

// ActiveBinding is the runtime row that makes a game "active" (in a play
// session). A game with no ActiveBinding is idle (backup-only). The GameID is
// the primary key: at most one active binding may exist per game.
type ActiveBinding struct {
	// GameID is the bound game; the PK enforces one active session per game.
	GameID string
	// PrimaryNode is the node currently holding play authority.
	PrimaryNode string
	StartedAt   time.Time
	// Direction is "from-primary" (primary wins the first sync) or
	// "from-peer-<node_id>".
	Direction string
	// PeerScope is "all-configured" (default) or a csv of node ids.
	PeerScope string
	// ConflictAt is non-nil when a non-primary peer mutated mid-session; it
	// flags the conflict (paused) state. Nil otherwise.
	ConflictAt *time.Time
	// LastSynced is the time of the last successful sync pass; nil if none yet.
	LastSynced *time.Time
}

// LogEntry is one append-only sync_log row: a single directional copy. A sync
// pass that fans out from primary to N peers writes N entries.
type LogEntry struct {
	// ID is assigned by the store on append (bigserial). Zero on input.
	ID int64
	// TS is the time of the copy; the store defaults it to now() when zero.
	TS time.Time
	// GameID is the game this entry concerns (FK; cascades on game delete).
	GameID string
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

// ManifestEntry is the last-known file state for a (game, node) pair, updated
// by the poll loop. PK is (GameID, NodeID); cascades on game/node delete.
type ManifestEntry struct {
	GameID string
	NodeID string
	// Mtime / Size are the fast-path identity of the file; nil when unknown.
	Mtime *time.Time
	Size  *int64
	// SHA256 is computed lazily (size+mtime is the fast path); nil/"" when not
	// yet computed.
	SHA256 *string
	// LastChecked is when the poll loop last stat'd this file; nil if never.
	LastChecked *time.Time
}

// Sync is a mirror group: a specific set of (node, save-file) members that sync
// together. It belongs to one game, but a game may have MANY independent syncs
// (e.g. two unrelated streams of the same title) so long as they do not share a
// (node, path) member — see SyncMember and docs/data-model.md.
type Sync struct {
	ID     string
	GameID string
	// Name is a free-form label; defaults to "".
	Name string
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

// GameFilter narrows Games.List. The zero value matches everything.
type GameFilter struct {
	// Q is an optional case-insensitive substring matched against id and
	// display. Empty means no substring filter.
	Q string
	// System is an optional exact-match system filter. Empty means no filter.
	System string
}

// Store is the storage-agnostic persistence boundary. All methods are
// context-first and return ErrNotFound / ErrConflict where documented.
type Store interface {
	// Users.
	CreateUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, id string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	UpdateUser(ctx context.Context, u User) error
	DeleteUser(ctx context.Context, id string) error

	// Nodes.
	CreateNode(ctx context.Context, n Node) error
	GetNode(ctx context.Context, id string) (Node, error)
	ListNodes(ctx context.Context) ([]Node, error)
	UpdateNode(ctx context.Context, n Node) error
	DeleteNode(ctx context.Context, id string) error

	// Games.
	CreateGame(ctx context.Context, g Game) error
	GetGame(ctx context.Context, id string) (Game, error)
	ListGames(ctx context.Context, f GameFilter) ([]Game, error)
	UpdateGame(ctx context.Context, g Game) error
	DeleteGame(ctx context.Context, id string) error

	// GamePaths.
	SetGamePath(ctx context.Context, gp GamePath) error // upsert on (game_id, node_id)
	GetGamePath(ctx context.Context, gameID, nodeID string) (GamePath, error)
	ListGamePathsByGame(ctx context.Context, gameID string) ([]GamePath, error)
	ListGamePathsByNode(ctx context.Context, nodeID string) ([]GamePath, error)
	DeleteGamePath(ctx context.Context, gameID, nodeID string) error

	// ActiveBindings. The game_id PK enforces one active session per game.
	// CreateBinding: duplicate game_id -> ErrConflict; missing game/node ->
	// ErrInvalidReference; bad direction -> ErrInvalidValue.
	//
	// Typed-error precedence: inputs are expected to violate at most one
	// constraint. When an input violates several at once (e.g. duplicate game_id
	// AND bad direction), which typed error is returned is unspecified and may
	// differ between backends. (Applies to UpdateBinding too.)
	CreateBinding(ctx context.Context, b ActiveBinding) error
	GetBinding(ctx context.Context, gameID string) (ActiveBinding, error)
	ListBindings(ctx context.Context) ([]ActiveBinding, error)
	// UpdateBinding rewrites the mutable fields (primary_node, direction,
	// peer_scope, conflict_at, last_synced) of an existing binding. Missing ->
	// ErrNotFound; bad direction -> ErrInvalidValue.
	UpdateBinding(ctx context.Context, b ActiveBinding) error
	// DeleteBinding is idempotent: deleting an absent binding returns nil (per
	// api.md "deactivate is idempotent").
	DeleteBinding(ctx context.Context, gameID string) error

	// SyncLog (append-only).
	// AppendLog: bad outcome -> ErrInvalidValue; missing game ->
	// ErrInvalidReference. A non-zero LogEntry.TS is honored as-is; a zero TS
	// defaults to now() (UTC). The store assigns the entry's id (bigserial);
	// AppendLog does not write it back into the passed entry, so callers that
	// need the id re-read via ListLogByGame.
	AppendLog(ctx context.Context, e LogEntry) error
	// ListLogByGame returns a game's entries most-recent-first, ordered by ts
	// descending, with ties broken by id descending. Capped at limit
	// (limit <= 0 means no cap). Because ts may be caller-supplied or skewed
	// (RTC-less / drifting clocks), this ts-then-id ordering — not insertion
	// order — is the contract both implementations honor.
	ListLogByGame(ctx context.Context, gameID string, limit int) ([]LogEntry, error)

	// Manifest (per-side last-known file state).
	SetManifest(ctx context.Context, m ManifestEntry) error // upsert on (game_id, node_id)
	GetManifest(ctx context.Context, gameID, nodeID string) (ManifestEntry, error)
	ListManifestByGame(ctx context.Context, gameID string) ([]ManifestEntry, error)

	// Syncs and SyncMembers — the additive foundation of the sync data-model
	// redesign. These tables sit ALONGSIDE the game-based tables above; nothing
	// here re-points the engine, runtime tables, or web yet.
	//
	// TODO(slice-sync-cutover): the engine, runtime tables (active_bindings,
	// manifest, sync_log), and web all key off game_id today. Later slices move
	// them onto sync_id, using Sync/SyncMember as the unit of mirroring.

	// CreateSync inserts a sync. Duplicate id -> ErrConflict; missing game ->
	// ErrInvalidReference.
	CreateSync(ctx context.Context, sy Sync) error
	GetSync(ctx context.Context, id string) (Sync, error)
	ListSyncsByGame(ctx context.Context, gameID string) ([]Sync, error)
	// UpdateSync rewrites the mutable fields (game_id, name) of an existing sync.
	// Missing -> ErrNotFound; missing game -> ErrInvalidReference.
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
}
