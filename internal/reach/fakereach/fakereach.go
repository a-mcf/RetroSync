// Package fakereach is an in-memory implementation of reach.Reach for unit
// tests. It backs each node's filesystem with a map keyed by path, honors
// reach.ErrNotExist, and — like the real localfs adapter — publishes EXACTLY the
// mtime handed to WriteAtomic, so an engine test observes the engine's own mtime
// policy (engine.publishMtime) rather than an adapter's.
//
// A Fake models ONE node's save filesystem (the engine resolves one reach.Reach
// per node). Tests typically hold a Fake per node and let the engine's resolver
// hand each node its own Fake.
package fakereach

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/a-mcf/retrosync/internal/reach"
)

// File is one stored file's state.
type File struct {
	Data  []byte
	Mtime time.Time
}

// WriteRecord captures one WriteAtomic call, in order, for test assertions.
type WriteRecord struct {
	Path string
	Data []byte
	// Mtime is the mtime actually PUBLISHED (what WriteAtomic returned) — always
	// exactly the requested mtime; the adapter never adjusts it.
	Mtime time.Time
}

// Fake is an in-memory reach.Reach for one node. The zero value is not usable;
// construct with New. Safe for concurrent use.
type Fake struct {
	mu    sync.Mutex
	files map[string]File
	// writes records every successful WriteAtomic call in order.
	writes []WriteRecord
	// failWrite, when non-nil, is returned by WriteAtomic for the matching path
	// (empty key "" matches any path). Used to simulate a write failure and
	// assert that the manifest is not advanced (crash-safety intent).
	failWrite map[string]error
	// failStat / failRead / failHash behave likewise for Stat / Read / Hash; they
	// let tests inject transient I/O errors distinct from ErrNotExist.
	failStat map[string]error
	failRead map[string]error
	failHash map[string]error
}

// New returns an empty Fake.
func New() *Fake {
	return &Fake{
		files:     make(map[string]File),
		failWrite: make(map[string]error),
		failStat:  make(map[string]error),
		failRead:  make(map[string]error),
		failHash:  make(map[string]error),
	}
}

// Put seeds a file at path with the given content and mtime. It does NOT record
// a WriteRecord (it is test setup, not an engine write). Returns the Fake for
// chaining.
func (f *Fake) Put(path string, data []byte, mtime time.Time) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.files[path] = File{Data: cp, Mtime: mtime}
	return f
}

// Mutate simulates an out-of-band change to path (e.g. a peer device wrote its
// own save during a session). Like Put it does not record a WriteRecord.
func (f *Fake) Mutate(path string, data []byte, mtime time.Time) *Fake {
	return f.Put(path, data, mtime)
}

// Remove deletes path from the fake filesystem.
func (f *Fake) Remove(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, path)
}

// FailWriteAtomic makes WriteAtomic return err for path. A path of "" matches
// any path. Pass a nil err to clear.
func (f *Fake) FailWriteAtomic(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failWrite, path)
		return
	}
	f.failWrite[path] = err
}

// FailStat makes Stat return err for path ("" matches any). Pass nil to clear.
func (f *Fake) FailStat(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failStat, path)
		return
	}
	f.failStat[path] = err
}

// FailRead makes Read return err for path ("" matches any). Pass nil to clear.
func (f *Fake) FailRead(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failRead, path)
		return
	}
	f.failRead[path] = err
}

// FailHash makes Hash return err for path ("" matches any). Pass nil to clear.
func (f *Fake) FailHash(path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failHash, path)
		return
	}
	f.failHash[path] = err
}

// Writes returns a copy of the recorded WriteAtomic calls, in order.
func (f *Fake) Writes() []WriteRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]WriteRecord, len(f.writes))
	copy(out, f.writes)
	return out
}

// Get returns the current stored file at path and whether it exists. The
// returned data is a copy.
func (f *Fake) Get(path string) (File, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	file, ok := f.files[path]
	if !ok {
		return File{}, false
	}
	cp := make([]byte, len(file.Data))
	copy(cp, file.Data)
	return File{Data: cp, Mtime: file.Mtime}, true
}

func (f *Fake) injected(m map[string]error, path string) error {
	if err, ok := m[path]; ok {
		return err
	}
	if err, ok := m[""]; ok {
		return err
	}
	return nil
}

// Stat implements reach.Reach.
func (f *Fake) Stat(_ context.Context, path string) (reach.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.injected(f.failStat, path); err != nil {
		return reach.FileMeta{}, err
	}
	file, ok := f.files[path]
	if !ok {
		return reach.FileMeta{}, fmt.Errorf("stat %q: %w", path, reach.ErrNotExist)
	}
	return reach.FileMeta{Mtime: file.Mtime, Size: int64(len(file.Data))}, nil
}

// Read implements reach.Reach.
func (f *Fake) Read(_ context.Context, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.injected(f.failRead, path); err != nil {
		return nil, err
	}
	file, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("read %q: %w", path, reach.ErrNotExist)
	}
	cp := make([]byte, len(file.Data))
	copy(cp, file.Data)
	return cp, nil
}

// Hash implements reach.Reach: the lowercase-hex sha256 over the stored bytes,
// or reach.ErrNotExist when absent. It honors an injected failHash so tests can
// simulate a transient hash I/O error distinct from ErrNotExist.
func (f *Fake) Hash(_ context.Context, path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.injected(f.failHash, path); err != nil {
		return "", err
	}
	file, ok := f.files[path]
	if !ok {
		return "", fmt.Errorf("hash %q: %w", path, reach.ErrNotExist)
	}
	sum := sha256.Sum256(file.Data)
	return hex.EncodeToString(sum[:]), nil
}

// List implements reach.Reach over the flat in-memory file map. The map is keyed
// by full node-relative path; List derives the directory tree from those keys by
// looking at each stored path that lives under relDir and emitting either the
// file itself (when it is a direct child) or the intermediate directory (when it
// is deeper). It returns metadata only — never file contents — sorted
// directories-first then alphabetically, mirroring the localfs adapter.
//
// An empty relPath (or ".") names the node root. A relPath that names no file
// AND has no descendants maps to reach.ErrNotExist (the directory does not
// exist); a relPath that names an existing FILE is a clear not-a-directory error.
func (f *Fake) List(_ context.Context, relPath string) ([]reach.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Normalize the requested directory to a clean prefix. "" and "." are the root.
	dir := path.Clean(relPath)
	if dir == "." || relPath == "" {
		dir = ""
	}

	// A relPath that names a stored FILE is not a directory.
	if _, ok := f.files[dir]; ok && dir != "" {
		return nil, fmt.Errorf("fakereach: list %q: not a directory", relPath)
	}

	// prefix is what a child path must start with to be under dir.
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}

	// Collapse the flat key set into the immediate children of dir. files maps a
	// direct child file name -> its meta; dirs is the set of immediate subdir
	// names (any deeper descendant contributes its first segment as a dir).
	files := make(map[string]reach.DirEntry)
	dirs := make(map[string]struct{})
	found := dir == "" // the root always "exists" even when empty
	for full, file := range f.files {
		if dir != "" && full == dir {
			continue
		}
		if prefix != "" && !strings.HasPrefix(full, prefix) {
			continue
		}
		found = true
		rest := strings.TrimPrefix(full, prefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			dirs[rest[:i]] = struct{}{}
		} else {
			files[rest] = reach.DirEntry{
				Name:  rest,
				IsDir: false,
				Size:  int64(len(file.Data)),
				Mtime: file.Mtime,
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("fakereach: list %q: %w", relPath, reach.ErrNotExist)
	}

	out := make([]reach.DirEntry, 0, len(files)+len(dirs))
	for name := range dirs {
		out = append(out, reach.DirEntry{Name: name, IsDir: true})
	}
	for _, de := range files {
		out = append(out, de)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// WriteAtomic implements reach.Reach. It honors any injected failure BEFORE
// mutating state, so a simulated failure leaves the prior file untouched
// (modeling the temp+rename crash-safety guarantee). On success it stores the
// data with the PUBLISHED mtime, records the call, and returns that mtime.
//
// Like localfs, the fake stamps EXACTLY the requested mtime — even when that
// collides with what the destination already carries. Deciding when a colliding
// mtime may be nudged is the engine's policy (engine.publishMtime, slice 38); a
// fake that quietly applied its own rule would hide the engine's from every test
// that uses it.
func (f *Fake) WriteAtomic(_ context.Context, path string, data []byte, publish time.Time) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.injected(f.failWrite, path); err != nil {
		return time.Time{}, err
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.files[path] = File{Data: cp, Mtime: publish}
	f.writes = append(f.writes, WriteRecord{Path: path, Data: cp, Mtime: publish})
	return publish, nil
}

// Paths returns the sorted set of currently-present paths (test convenience).
func (f *Fake) Paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.files))
	for p := range f.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
