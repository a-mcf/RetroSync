package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/safepath"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// --- BrowseServer --------------------------------------------------------
// BrowseServer lists the SERVER's share root (the syncthing NFS mount), rooted
// via WithShareRoot at a t.TempDir. It uses a real localfs adapter so the safepath
// containment (traversal + absolute-path rejection) is exercised end-to-end,
// unlike the fakereach-backed BrowseNode tests.

// nilResolve is a resolver that is never called by BrowseServer (BrowseServer does
// not resolve a node — it is rooted at the server share, not a node).
func nilResolve(store.Node) (reach.Reach, error) { return nil, nil }

func TestBrowseServer_ListsShareRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bob-deck-saves"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bob-deck-saves", "sm.srm"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := engine.New(memory.New(), nilResolve, nil, engine.WithShareRoot(root))

	// Root listing: the dir (bob-deck-saves) sorts before the file (readme.txt).
	entries, err := eng.BrowseServer(ctx(), "")
	if err != nil {
		t.Fatalf("BrowseServer root: %v", err)
	}
	if len(entries) != 2 || !entries[0].IsDir || entries[0].Name != "bob-deck-saves" || entries[1].Name != "readme.txt" {
		t.Fatalf("BrowseServer root = %+v, want bob-deck-saves/ then readme.txt", entries)
	}

	// Descend into bob-deck-saves/ — "" and "." both name the root, so a real
	// sub-path is the only way to reach the file.
	sub, err := eng.BrowseServer(ctx(), "bob-deck-saves")
	if err != nil {
		t.Fatalf("BrowseServer subdir: %v", err)
	}
	if len(sub) != 1 || sub[0].IsDir || sub[0].Name != "sm.srm" {
		t.Fatalf("BrowseServer subdir = %+v, want sm.srm", sub)
	}
}

func TestBrowseServer_DotIsRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.srm"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := engine.New(memory.New(), nilResolve, nil, engine.WithShareRoot(root))
	entries, err := eng.BrowseServer(ctx(), ".")
	if err != nil {
		t.Fatalf(`BrowseServer ".": %v`, err)
	}
	if len(entries) != 1 || entries[0].Name != "a.srm" {
		t.Fatalf(`BrowseServer "." = %+v, want a.srm`, entries)
	}
}

func TestBrowseServer_TraversalRejected(t *testing.T) {
	eng := engine.New(memory.New(), nilResolve, nil, engine.WithShareRoot(t.TempDir()))
	for _, bad := range []string{"../etc", "../../etc/passwd", "/etc/passwd"} {
		_, err := eng.BrowseServer(ctx(), bad)
		if !errors.Is(err, engine.ErrBrowseUnsafePath) {
			t.Fatalf("BrowseServer(%q) err = %v, want ErrBrowseUnsafePath", bad, err)
		}
		// The underlying safepath rejection stays in the chain.
		if !errors.Is(err, safepath.ErrUnsafePath) {
			t.Fatalf("BrowseServer(%q) err = %v should also wrap safepath.ErrUnsafePath", bad, err)
		}
	}
}

func TestBrowseServer_NoShareRootUnsupported(t *testing.T) {
	// An engine built without WithShareRoot has the capability off.
	eng := engine.New(memory.New(), nilResolve, nil)
	_, err := eng.BrowseServer(ctx(), "")
	if !errors.Is(err, engine.ErrBrowseUnsupported) {
		t.Fatalf("no-share-root BrowseServer err = %v, want ErrBrowseUnsupported", err)
	}
}

func TestBrowseServer_MissingDir(t *testing.T) {
	// A configured-but-nonexistent share root surfaces as a browse-time error (not
	// ErrBrowseUnsafePath, not ErrBrowseUnsupported) so the web layer renders a
	// friendly in-place message rather than a 400/unsupported note.
	eng := engine.New(memory.New(), nilResolve, nil, engine.WithShareRoot(filepath.Join(t.TempDir(), "nope")))
	_, err := eng.BrowseServer(ctx(), "")
	if err == nil {
		t.Fatal("BrowseServer of a missing share root should error")
	}
	if errors.Is(err, engine.ErrBrowseUnsafePath) || errors.Is(err, engine.ErrBrowseUnsupported) {
		t.Fatalf("missing-dir err = %v, want a plain browse error", err)
	}
}
