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
}

// TODO(slice-runtime): add ActiveBinding, SyncLog, and Manifest types plus
// their Store methods (active_bindings, sync_log, manifest tables).
