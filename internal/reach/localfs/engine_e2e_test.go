package localfs_test

// End-to-end: the slice-3 engine, the in-memory store, and the REAL localfs
// adapter wired through the production resolver, driving an actual Activate +
// Poll fan-out and asserting the bytes and mtime land on the peer's real file on
// disk. This proves the engine works against real filesystems, not just the
// fakereach. It needs no database, so it runs under plain `make test`.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/resolve"
	"github.com/a-mcf/retrosync/internal/reach/safepath"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

const (
	gameID = "super-metroid"
	syncID = "sm-bob"
)

func steppingClock(start time.Time, step time.Duration) engine.Clock {
	cur := start
	return func() time.Time {
		t := cur
		cur = cur.Add(step)
		return t
	}
}

// TestEngineFanOut_RealLocalFS sets up two "nodes" as two temp-dir roots, seeds
// a save on the primary, activates (first-sync fan-out from primary), and
// asserts the peer's real file on disk now holds the same bytes and mtime.
func TestEngineFanOut_RealLocalFS(t *testing.T) {
	ctx := context.Background()

	primaryRoot := t.TempDir()
	peerRoot := t.TempDir()

	// Seed the primary's real save file. Relative path under the node root.
	const relPath = "retroarch/saves/Super Metroid.srm"
	saveData := []byte("primary save bytes v1")
	saveMtime := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	primaryAbs := filepath.Join(primaryRoot, relPath)
	if err := os.MkdirAll(filepath.Dir(primaryAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primaryAbs, saveData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(primaryAbs, saveMtime, saveMtime); err != nil {
		t.Fatal(err)
	}

	st := memory.New()
	if err := st.CreateGame(ctx, store.Game{ID: gameID, Display: "Super Metroid", System: "snes"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSync(ctx, store.Sync{ID: syncID, GameID: gameID, Name: "Bob's stream"}); err != nil {
		t.Fatal(err)
	}
	// Two syncthing-share nodes, each rooted at its own temp dir via reach_config.
	mustNode(t, st, "primary", primaryRoot)
	mustNode(t, st, "peer", peerRoot)
	mustPath(t, st, "primary", relPath)
	mustPath(t, st, "peer", relPath)

	clock := steppingClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Second)
	eng := engine.New(st, resolve.ResolveReach, clock)

	if err := eng.Activate(ctx, syncID, "primary", "from-primary", false); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// Assert the peer's REAL file received the bytes and the source mtime.
	peerAbs := filepath.Join(peerRoot, relPath)
	gotData, err := os.ReadFile(peerAbs)
	if err != nil {
		t.Fatalf("read peer file after activate: %v", err)
	}
	if string(gotData) != string(saveData) {
		t.Fatalf("peer data = %q, want %q", gotData, saveData)
	}
	fi, err := os.Stat(peerAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(saveMtime) {
		t.Fatalf("peer mtime = %v, want %v (source mtime)", fi.ModTime(), saveMtime)
	}

	// Now mutate the primary's real file and Poll: the change must fan out again.
	saveData2 := []byte("primary save bytes v2 (longer)")
	saveMtime2 := time.Date(2024, 3, 4, 6, 0, 0, 0, time.UTC)
	if err := os.WriteFile(primaryAbs, saveData2, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(primaryAbs, saveMtime2, saveMtime2); err != nil {
		t.Fatal(err)
	}

	if err := eng.Poll(ctx, syncID); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	gotData2, err := os.ReadFile(peerAbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotData2) != string(saveData2) {
		t.Fatalf("after poll peer data = %q, want %q", gotData2, saveData2)
	}
	fi2, err := os.Stat(peerAbs)
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().Equal(saveMtime2) {
		t.Fatalf("after poll peer mtime = %v, want %v", fi2.ModTime(), saveMtime2)
	}

	// A second Poll with no changes must be a noop: peer file unchanged, no error.
	if err := eng.Poll(ctx, syncID); err != nil {
		t.Fatalf("idempotent Poll: %v", err)
	}
	gotData3, err := os.ReadFile(peerAbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotData3) != string(saveData2) {
		t.Fatalf("noop poll changed peer data to %q", gotData3)
	}

	// Sanity: the engine genuinely went through reach.ErrNotExist semantics for
	// the peer at activate time (the peer started empty). Re-resolve and confirm
	// the resolver yields a working reach for the registered node.
	node, err := st.GetNode(ctx, "peer")
	if err != nil {
		t.Fatal(err)
	}
	r, err := resolve.ResolveReach(node)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stat(ctx, relPath); err != nil {
		t.Fatalf("resolved peer reach Stat: %v", err)
	}
	_ = reach.ErrNotExist // documents the sentinel exercised above
}

// TestEngineBrowseNode_RealLocalFS drives engine.BrowseNode through the real
// resolver + localfs adapter over a temp-dir node root, asserting it lists real
// directory entries (metadata only) and that a traversal path is rejected and
// surfaces as engine.ErrBrowseUnsafePath (the web layer's 400 trigger).
func TestEngineBrowseNode_RealLocalFS(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// Seed a small tree on disk.
	for _, f := range []struct{ rel, data string }{
		{"alpha.srm", "a"},
		{"saves/game.srm", "bytes"},
	} {
		abs := filepath.Join(root, f.rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(f.data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st := memory.New()
	mustNode(t, st, "bob-deck", root)
	eng := engine.New(st, resolve.ResolveReach, steppingClock(time.Unix(0, 0).UTC(), time.Second))

	// Root listing: saves/ (dir) first, then alpha.srm (file) — metadata only.
	entries, err := eng.BrowseNode(ctx, "bob-deck", "")
	if err != nil {
		t.Fatalf("BrowseNode root: %v", err)
	}
	if len(entries) != 2 || !entries[0].IsDir || entries[0].Name != "saves" || entries[1].Name != "alpha.srm" {
		t.Fatalf("BrowseNode root = %+v, want saves/ then alpha.srm", entries)
	}
	if entries[1].Size != 1 {
		t.Errorf("alpha.srm size = %d, want 1", entries[1].Size)
	}

	// Traversal is rejected by safepath inside the adapter and surfaces as the
	// engine's ErrBrowseUnsafePath sentinel (and still wraps safepath.ErrUnsafePath).
	for _, bad := range []string{"../escape", "/etc", "a/../../b"} {
		_, err := eng.BrowseNode(ctx, "bob-deck", bad)
		if !errors.Is(err, engine.ErrBrowseUnsafePath) {
			t.Fatalf("BrowseNode(%q) err = %v, want ErrBrowseUnsafePath", bad, err)
		}
		if !errors.Is(err, safepath.ErrUnsafePath) {
			t.Fatalf("BrowseNode(%q) err = %v should also wrap safepath.ErrUnsafePath", bad, err)
		}
	}
}

func mustNode(t *testing.T, st store.Store, id, root string) {
	t.Helper()
	err := st.CreateNode(context.Background(), store.Node{
		ID:          id,
		Display:     id,
		Kind:        store.KindGeneric,
		Reach:       store.ReachSyncthingShare,
		ReachConfig: store.ReachConfig{Path: root},
	})
	if err != nil {
		t.Fatalf("CreateNode %q: %v", id, err)
	}
}

// mustPath adds a sync member (node, path) — the engine's in-scope (node, file)
// pair, the sync-scoped successor of a game_path.
func mustPath(t *testing.T, st store.Store, nodeID, path string) {
	t.Helper()
	if err := st.SetSyncMember(context.Background(), store.SyncMember{SyncID: syncID, NodeID: nodeID, Path: path}); err != nil {
		t.Fatalf("SetSyncMember %q: %v", nodeID, err)
	}
}
