// Package reach defines the Reach port: the engine's only view of a node's
// save filesystem. The play-sync engine (internal/engine) talks exclusively
// to this interface, so it stays adapter-agnostic and fully unit-testable
// against an in-memory fake (see internal/reach/fakereach).
//
// Real adapters (local Syncthing-share filesystem, ssh/sftp) implement this
// interface in a later slice.
//
// TODO(slice-reach-adapters): provide the concrete syncthing-share (local fs)
// and ssh/sftp implementations of Reach.
package reach

import (
	"context"
	"errors"
	"time"
)

// ErrNotExist is the sentinel returned by Stat (and Read) when the requested
// path does not exist on the node. Callers compare with errors.Is, so adapters
// may wrap it with %w. The engine treats "this node has no file" as a
// first-class state (a node may legitimately not yet hold a save), so it
// matches on this sentinel rather than on any OS-specific error.
var ErrNotExist = errors.New("reach: file does not exist")

// ErrUnsupportedReach is returned by a Resolver when a node's reach strategy has
// no adapter wired yet. Callers compare with errors.Is. Today the ssh/sftp
// adapter is unimplemented, so a Resolver returns this for ssh nodes.
//
// TODO(slice-ssh): remove the ssh case from this sentinel once the ssh/sftp
// adapter (with secret-store credential handling) lands.
var ErrUnsupportedReach = errors.New("reach: unsupported reach strategy")

// FileMeta is the fast-path identity of a file: modification time and size.
// It deliberately omits content hashing; size+mtime is the cheap comparison
// the poll loop relies on (see docs/data-model.md, docs/state-machine.md).
type FileMeta struct {
	// Mtime is the file's last-modified time. Adapters should return it in a
	// stable location (UTC recommended) so manifest comparisons are reliable.
	Mtime time.Time
	// Size is the file's size in bytes.
	Size int64
}

// Reach is the engine's port onto one node's save filesystem.
//
// Path semantics: every path passed to these methods is the node's
// game_paths.path verbatim — i.e. relative to that node's save root. Resolving
// the path to an absolute on-disk (or on-remote) location is the ADAPTER's
// job, never the engine's. The engine never knows about save roots,
// reach_config, or absolute paths; it just hands the relative path through.
// This keeps registry rows portable (see docs/data-model.md).
type Reach interface {
	// Stat returns the file's metadata, or ErrNotExist (a sentinel in this
	// package) when the path is absent on the node.
	Stat(ctx context.Context, path string) (FileMeta, error)

	// Read returns the full file contents, or ErrNotExist when absent.
	Read(ctx context.Context, path string) ([]byte, error)

	// WriteAtomic writes data to path crash-safely and sets the file's mtime to
	// the given mtime. The contract (from docs/state-machine.md "Crash safety"):
	//
	//   1. Write the bytes to a temporary sibling (e.g. <path>.retrosync-tmp).
	//   2. Atomically rename the temp file over path.
	//
	// A crash before the rename leaves the previous good file intact, so the
	// next poll re-detects divergence and retries. The caller (engine) only
	// advances the manifest AFTER WriteAtomic returns nil — manifest trails the
	// actual write.
	//
	// The destination mtime is set to the SOURCE file's mtime so a fan-out copy
	// is byte-and-mtime faithful: the next poll then sees primary == peer rather
	// than a spurious "peer changed" (the just-written peer would otherwise have
	// a wall-clock mtime that differs from the source's manifest mtime).
	WriteAtomic(ctx context.Context, path string, data []byte, mtime time.Time) error
}
