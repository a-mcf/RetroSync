// Package localfs implements reach.Reach against the local filesystem. It backs
// "syncthing-share" nodes, whose save directories the server already has mounted
// locally (a shared pod volume in the k8s deployment; see docs/architecture.md).
//
// Every path the engine hands in is a node-relative game_paths.path; this
// adapter resolves it against its configured absolute root through
// internal/reach/safepath, which guarantees containment (traversal + symlink
// defense). The engine never sees absolute paths.
//
// Note on the writeback model: docs/architecture.md flags that writing into a
// Syncthing share races with the device's own writes, so the real production
// writeback for these nodes is intended to go out of band (ssh). This adapter
// still implements WriteAtomic faithfully — it is exercised by the engine's
// fan-out and is the correct primitive for any local-fs destination (e.g. the
// server's own node, tests). The race policy is a Resolver/wiring concern, not
// this adapter's.
package localfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/safepath"
)

// LocalFS is a reach.Reach rooted at an absolute directory. Construct with New.
type LocalFS struct {
	root string
}

// tempSuffix is appended (with a random component) to the destination filename
// for the temp file used by WriteAtomic. Kept distinctive so a leftover from a
// crash is recognizable.
const tempSuffix = ".retrosync-tmp"

// New returns a LocalFS rooted at root. root must be absolute; a relative root
// is a programming/config error (the Resolver passes reach_config.path, which
// is operator-provided and expected absolute).
func New(root string) (*LocalFS, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("localfs: root %q must be absolute", root)
	}
	return &LocalFS{root: filepath.Clean(root)}, nil
}

// Stat implements reach.Reach. It maps a not-exist OS error to reach.ErrNotExist
// so the engine can treat "no file here yet" as a first-class state.
func (l *LocalFS) Stat(_ context.Context, path string) (reach.FileMeta, error) {
	abs, err := safepath.Resolve(l.root, path)
	if err != nil {
		return reach.FileMeta{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return reach.FileMeta{}, fmt.Errorf("localfs: stat %q: %w", path, reach.ErrNotExist)
		}
		return reach.FileMeta{}, fmt.Errorf("localfs: stat %q: %w", path, err)
	}
	return reach.FileMeta{Mtime: fi.ModTime(), Size: fi.Size()}, nil
}

// Read implements reach.Reach.
func (l *LocalFS) Read(_ context.Context, path string) ([]byte, error) {
	abs, err := safepath.Resolve(l.root, path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("localfs: read %q: %w", path, reach.ErrNotExist)
		}
		return nil, fmt.Errorf("localfs: read %q: %w", path, err)
	}
	return data, nil
}

// WriteAtomic implements reach.Reach honoring the crash-safety contract: write
// to a temp file in the SAME directory as the destination, fsync+close it,
// os.Rename it over the destination (atomic on one filesystem), then os.Chtimes
// the destination to the requested mtime. The destination's parent directory is
// created (within root) if missing.
//
// On ANY failure after the temp file is created, the temp file is removed so no
// leftover is left behind, and the existing destination (if any) is untouched —
// the rename is the only step that replaces it, and it either fully succeeds or
// leaves the prior file intact.
func (l *LocalFS) WriteAtomic(_ context.Context, path string, data []byte, mtime time.Time) error {
	abs, err := safepath.Resolve(l.root, path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)

	// Create the destination's parent within root if missing. MkdirAll is a no-op
	// when the dir already exists.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("localfs: mkdir %q: %w", dir, err)
	}

	// Temp file in the same directory so the rename is same-filesystem (and thus
	// atomic). CreateTemp picks a unique name so concurrent writers don't collide.
	tmp, err := os.CreateTemp(dir, filepath.Base(abs)+tempSuffix+"-*")
	if err != nil {
		return fmt.Errorf("localfs: create temp in %q: %w", dir, err)
	}
	tmpName := tmp.Name()

	// From here, any error path must remove the temp file. cleanup is idempotent.
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("localfs: write temp %q: %w", tmpName, err)
	}
	// fsync so the bytes are durable before the rename publishes the file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("localfs: sync temp %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("localfs: close temp %q: %w", tmpName, err)
	}

	// Set mtime on the temp file BEFORE the rename so the published file already
	// carries the source mtime; this avoids a brief window where the destination
	// exists with a wrong mtime. (Chtimes after rename would also work; doing it
	// first keeps the destination correct at the instant it appears.)
	if err := os.Chtimes(tmpName, mtime, mtime); err != nil {
		cleanup()
		return fmt.Errorf("localfs: chtimes temp %q: %w", tmpName, err)
	}

	// Atomic publish. On failure the prior destination is untouched.
	if err := os.Rename(tmpName, abs); err != nil {
		cleanup()
		return fmt.Errorf("localfs: rename %q -> %q: %w", tmpName, abs, err)
	}
	return nil
}

// compile-time assertion that LocalFS satisfies the port.
var _ reach.Reach = (*LocalFS)(nil)
