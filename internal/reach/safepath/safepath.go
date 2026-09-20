// Package safepath resolves a node-relative path against an absolute root and
// guarantees the result stays inside that root. It is the single chokepoint for
// path-traversal defense shared by every filesystem-backed Reach adapter (the
// local syncthing-share adapter today, any future transport adapter),
// so the containment policy lives in exactly one place.
//
// The threat it defends against: a game_paths.path row is registry data and
// could be malformed or hostile ("../../etc/passwd", "/etc/passwd", or a path
// that traverses a symlink out of the root). The engine hands these paths
// through verbatim (it never resolves them itself), so the adapter must refuse
// to read or write anything outside its configured root.
package safepath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafePath is the sentinel returned when a path would escape the root, is
// absolute, or is otherwise rejected. Callers compare with errors.Is.
var ErrUnsafePath = errors.New("safepath: path escapes root")

// Resolve joins a node-relative rel onto the absolute root and returns the
// cleaned absolute path, GUARANTEEING the result is contained within root.
//
// It enforces, in order:
//
//  1. root must be absolute (a programming error otherwise — adapters are
//     constructed with an absolute root).
//  2. rel must not be absolute. An absolute rel is rejected outright; we never
//     let registry data pick an arbitrary on-disk location.
//  3. After filepath.Clean, rel must not lexically escape the root via "..".
//     This catches "../x", "a/../../b", and bare "..".
//  4. Symlink defense: we resolve symlinks on the longest EXISTING ancestor of
//     the target and re-check that the real ancestor is still under the real
//     root. This stops a symlink already planted inside the root from pointing
//     the resolved location outside it (e.g. root/link -> /etc, then
//     rel="link/passwd").
//
// What is guaranteed: on a nil return, the returned path is lexically within
// root, AND no symlink on any currently-existing path component redirects the
// target's existing-prefix outside the real root.
//
// What is NOT guaranteed (limits):
//   - TOCTOU: this is a check, not a lock. A symlink could be created in a
//     not-yet-existing component between Resolve returning and the caller's
//     os.Open/os.Rename. The localfs adapter narrows this window by operating
//     on the resolved path immediately and by writing via a temp file + rename
//     within the (already-validated) destination directory; it does not, and
//     cannot portably, eliminate it. For RetroSync's single-tenant household
//     threat model (the registry is operator-controlled, not attacker-fed at
//     runtime) this is acceptable. A stronger guarantee would require openat2
//     with RESOLVE_BENEATH (Linux-only) — out of scope for this slice.
//   - It does not create any directories; it only validates. Callers MkdirAll
//     the parent themselves, then may re-validate if they want symlink coverage
//     over the freshly-created components.
func Resolve(root, rel string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("safepath: root %q is not absolute", root)
	}
	if rel == "" {
		return "", fmt.Errorf("safepath: empty relative path: %w", ErrUnsafePath)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("safepath: absolute path %q not allowed: %w", rel, ErrUnsafePath)
	}

	cleanRoot := filepath.Clean(root)
	// Join cleans the combined path, collapsing any ".." segments. We then verify
	// the cleaned result is still under cleanRoot — Join alone is not a guarantee
	// because filepath.Join("/root", "../etc") yields "/etc".
	target := filepath.Join(cleanRoot, rel)
	if !withinRoot(cleanRoot, target) {
		return "", fmt.Errorf("safepath: %q escapes root %q: %w", rel, root, ErrUnsafePath)
	}

	// Symlink defense. Resolve symlinks on the longest existing ancestor of the
	// target and confirm it is still under the real (symlink-resolved) root.
	realRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		// The root itself must exist and be resolvable. If it does not, that is a
		// configuration error, not a traversal — surface it plainly.
		return "", fmt.Errorf("safepath: resolve root %q: %w", cleanRoot, err)
	}
	realPrefix, err := evalExistingPrefix(target)
	if err != nil {
		return "", fmt.Errorf("safepath: resolve %q: %w", target, err)
	}
	if !withinRoot(realRoot, realPrefix) {
		return "", fmt.Errorf("safepath: %q resolves via symlink outside root %q: %w", rel, root, ErrUnsafePath)
	}

	return target, nil
}

// withinRoot reports whether p is root itself or lies beneath it, using cleaned
// paths and a separator-terminated prefix so "/root2" is not treated as being
// under "/root".
func withinRoot(root, p string) bool {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// evalExistingPrefix returns the symlink-resolved real path of the longest
// existing ancestor of p (which may be p itself if it exists). Non-existing
// trailing components are appended back, unresolved, onto the resolved prefix.
// This lets us validate the part of the path that actually exists on disk —
// the part that could carry a malicious symlink — without requiring the full
// target to exist (a write creates new components).
func evalExistingPrefix(p string) (string, error) {
	p = filepath.Clean(p)
	// Walk up until we find a component that exists, accumulating the missing
	// trailing components.
	var missing []string
	cur := p
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			// Re-attach the missing tail (in original order).
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without finding an existing component.
			// Nothing to resolve; return p unchanged.
			return p, nil
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}
