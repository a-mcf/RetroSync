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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

// maxListEntries is a generous per-call cap on how many directory entries a single
// List will accumulate. List backs BOTH the discovery scan and the slice-17
// picker; without a cap, a save directory with a pathological number of entries
// would let a streamed read accumulate unboundedly (os.ReadDir would do worse —
// it slurps the entire entry list before any caller-side cap can apply). When the
// cap is hit, List stops reading (it does NOT drain the rest of the directory) and
// logs a truncation at WARN; the engine/picker still get a bounded, useful listing.
// The discovery scan's own maxScanEntries (total across the whole walk) still
// applies on top — this only bounds a SINGLE directory read.
//
// It is a var (not a const) so a test can lower it to assert the bound without
// having to seed tens of thousands of files.
var maxListEntries = 10000

// listReadBatch is how many entries each f.ReadDir call requests while streaming.
// Streaming in batches (rather than one os.ReadDir slurp) is what lets List stop
// at the cap without first materializing the entire directory in memory.
const listReadBatch = 1024

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

// Hash implements reach.Reach. It resolves path through the same safepath
// containment as Read/Stat, then STREAMS the file through sha256 (open +
// io.Copy) so a large save is never loaded whole into memory, and returns the
// lowercase-hex digest. A missing file maps to reach.ErrNotExist.
func (l *LocalFS) Hash(_ context.Context, path string) (string, error) {
	abs, err := safepath.Resolve(l.root, path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("localfs: hash %q: %w", path, reach.ErrNotExist)
		}
		return "", fmt.Errorf("localfs: hash %q: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("localfs: hash %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// List implements reach.Reach. It resolves relPath through safepath (so a
// traversal/absolute/escape path is rejected exactly as Stat/Read are), then
// STREAMS the resolved directory's entries (os.Open + f.ReadDir in batches) and
// maps each to a reach.DirEntry, stopping at maxListEntries so a pathological
// directory can't spike memory before the caller's own cap (e.g. the discovery
// scan's maxScanEntries) applies.
//
// An empty relPath (or ".") names the node's save root. The returned entries are
// metadata ONLY — name, type, size, mtime — never file contents: f.ReadDir reads
// directory entries, and each entry's Info() is a stat, so no save bytes are ever
// read here. Entries are sorted directories-first, then alphabetically, for a
// deterministic picker render. A missing directory maps to reach.ErrNotExist; a
// path that exists but is not a directory is a clear error.
//
// If the directory holds more than maxListEntries entries, List stops reading at
// the cap (it does NOT drain the remainder) and logs a truncation at WARN; the
// returned listing is bounded but still useful.
func (l *LocalFS) List(_ context.Context, relPath string) ([]reach.DirEntry, error) {
	// safepath rejects an empty rel; "" and "." both mean "the node root", so
	// normalize "" to "." before resolving. Resolve(root, ".") yields the root.
	rel := relPath
	if rel == "" {
		rel = "."
	}
	abs, err := safepath.Resolve(l.root, rel)
	if err != nil {
		return nil, err
	}

	// Stat first so we can map "absent" to ErrNotExist and reject a non-directory
	// with a clear error (a ReadDir on a file returns a less obvious error).
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("localfs: list %q: %w", relPath, reach.ErrNotExist)
		}
		return nil, fmt.Errorf("localfs: list %q: %w", relPath, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("localfs: list %q: not a directory", relPath)
	}

	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("localfs: list %q: %w", relPath, reach.ErrNotExist)
		}
		return nil, fmt.Errorf("localfs: open dir %q: %w", relPath, err)
	}
	defer f.Close()

	out := make([]reach.DirEntry, 0, listReadBatch)
	truncated := false
readLoop:
	for {
		dirents, rerr := f.ReadDir(listReadBatch)
		for _, de := range dirents {
			if len(out) >= maxListEntries {
				// Cap hit: stop reading entirely (don't drain the rest of the
				// directory) so memory stays bounded. We have a useful prefix.
				truncated = true
				break readLoop
			}
			// Info() is a stat of the entry: it yields size + mtime, NOT contents.
			info, ierr := de.Info()
			if ierr != nil {
				// The entry vanished between ReadDir and Info (a transient race);
				// skip it rather than failing the whole listing.
				if os.IsNotExist(ierr) {
					continue
				}
				return nil, fmt.Errorf("localfs: stat entry %q in %q: %w", de.Name(), relPath, ierr)
			}
			out = append(out, reach.DirEntry{
				Name:  de.Name(),
				IsDir: de.IsDir(),
				Size:  info.Size(),
				Mtime: info.ModTime(),
			})
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return nil, fmt.Errorf("localfs: read dir %q: %w", relPath, rerr)
		}
	}

	if truncated {
		slog.Warn("localfs: directory listing truncated at cap",
			"dir", abs, "cap", maxListEntries)
	}

	sortDirEntries(out)
	return out, nil
}

// sortDirEntries orders entries directories-first, then alphabetically by name,
// so the picker render is deterministic regardless of filesystem order.
func sortDirEntries(entries []reach.DirEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // dirs before files
		}
		return entries[i].Name < entries[j].Name
	})
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
