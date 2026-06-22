package engine_test

import (
	"errors"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/fakereach"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// --- SmokeTest -----------------------------------------------------------

// newSmokeEngine builds an Engine with a resolver that maps each node to a
// supplied reach.Reach (or a supplied error), so we can exercise the
// syncthing-share success/failure paths and the ssh ErrUnsupportedReach path
// without the real resolve package.
func newSmokeEngine(t *testing.T, reaches map[string]reach.Reach, resolveErrs map[string]error) (*engine.Engine, store.Store) {
	t.Helper()
	st := memory.New()
	resolve := func(n store.Node) (reach.Reach, error) {
		if err, ok := resolveErrs[n.ID]; ok {
			return nil, err
		}
		r, ok := reaches[n.ID]
		if !ok {
			t.Fatalf("no reach for node %q", n.ID)
		}
		return r, nil
	}
	return engine.New(st, resolve, func() time.Time { return time.Unix(0, 0).UTC() }), st
}

func TestSmokeTest_SyncthingShareReachable(t *testing.T) {
	f := fakereach.New()
	f.Put(".", nil, time.Unix(0, 0)) // the share root "." exists and stats cleanly.
	eng, st := newSmokeEngine(t, map[string]reach.Reach{"bob-deck": f}, nil)
	if err := st.CreateNode(ctx(), store.Node{
		ID: "bob-deck", Display: "Bob's Deck", Kind: store.KindDeck,
		Reach: store.ReachSyncthingShare, ReachConfig: store.ReachConfig{Path: "/srv/saves"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := eng.SmokeTest(ctx(), "bob-deck"); err != nil {
		t.Fatalf("smoke-test reachable node: %v", err)
	}
}

func TestSmokeTest_SyncthingShareUnreachable(t *testing.T) {
	f := fakereach.New() // "." not seeded -> Stat returns ErrNotExist.
	eng, st := newSmokeEngine(t, map[string]reach.Reach{"bob-deck": f}, nil)
	if err := st.CreateNode(ctx(), store.Node{
		ID: "bob-deck", Display: "Bob's Deck", Kind: store.KindDeck,
		Reach: store.ReachSyncthingShare, ReachConfig: store.ReachConfig{Path: "/srv/missing"},
	}); err != nil {
		t.Fatal(err)
	}
	err := eng.SmokeTest(ctx(), "bob-deck")
	if err == nil {
		t.Fatal("smoke-test of an unreachable share should error")
	}
	if !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}

func TestSmokeTest_SSHUnsupported(t *testing.T) {
	eng, st := newSmokeEngine(t, nil, map[string]error{"mister": reach.ErrUnsupportedReach})
	if err := st.CreateNode(ctx(), store.Node{
		ID: "mister", Display: "Living-room MiSTer", Kind: store.KindMister,
		Reach: store.ReachSSH, ReachConfig: store.ReachConfig{Host: "10.0.0.2", User: "root", SecretRef: "mister-1"},
	}); err != nil {
		t.Fatal(err)
	}
	err := eng.SmokeTest(ctx(), "mister")
	if !errors.Is(err, engine.ErrSmokeTestUnsupported) {
		t.Fatalf("ssh smoke-test err = %v, want ErrSmokeTestUnsupported", err)
	}
	if !errors.Is(err, reach.ErrUnsupportedReach) {
		t.Fatalf("ssh smoke-test err = %v should also wrap reach.ErrUnsupportedReach", err)
	}
}

func TestSmokeTest_MissingNode(t *testing.T) {
	eng, _ := newSmokeEngine(t, map[string]reach.Reach{}, nil)
	err := eng.SmokeTest(ctx(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing-node err = %v, want ErrNotFound", err)
	}
}

// --- BrowseNode ----------------------------------------------------------
// BrowseNode reuses the smoke-engine harness (a resolver mapping node id ->
// reach.Reach or a resolve error), so it exercises the syncthing-share, ssh, and
// missing-node paths without the real resolve package.

func TestBrowseNode_SyncthingShareListsEntries(t *testing.T) {
	mt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := fakereach.New().
		Put("saves/game.srm", []byte("bytes"), mt).
		Put("alpha.srm", []byte("a"), mt)
	eng, st := newSmokeEngine(t, map[string]reach.Reach{"bob-deck": f}, nil)
	if err := st.CreateNode(ctx(), store.Node{
		ID: "bob-deck", Display: "Bob's Deck", Kind: store.KindDeck,
		Reach: store.ReachSyncthingShare, ReachConfig: store.ReachConfig{Path: "/srv/saves"},
	}); err != nil {
		t.Fatal(err)
	}

	// Root listing: saves/ (dir) first, then alpha.srm (file).
	entries, err := eng.BrowseNode(ctx(), "bob-deck", "")
	if err != nil {
		t.Fatalf("BrowseNode root: %v", err)
	}
	if len(entries) != 2 || !entries[0].IsDir || entries[0].Name != "saves" || entries[1].Name != "alpha.srm" {
		t.Fatalf("BrowseNode root = %+v, want saves/ then alpha.srm", entries)
	}
	// The file entry carries metadata (size/mtime) but the engine never reads
	// contents — DirEntry has no content field.
	if entries[1].Size != 1 || !entries[1].Mtime.Equal(mt) {
		t.Errorf("alpha.srm meta = %+v, want size 1 + mtime %v", entries[1], mt)
	}

	// Descend into saves/.
	sub, err := eng.BrowseNode(ctx(), "bob-deck", "saves")
	if err != nil {
		t.Fatalf("BrowseNode saves: %v", err)
	}
	if len(sub) != 1 || sub[0].IsDir || sub[0].Name != "game.srm" {
		t.Fatalf("BrowseNode saves = %+v, want game.srm", sub)
	}
}

func TestBrowseNode_SSHUnsupported(t *testing.T) {
	eng, st := newSmokeEngine(t, nil, map[string]error{"mister": reach.ErrUnsupportedReach})
	if err := st.CreateNode(ctx(), store.Node{
		ID: "mister", Display: "Living-room MiSTer", Kind: store.KindMister,
		Reach: store.ReachSSH, ReachConfig: store.ReachConfig{Host: "10.0.0.2", User: "root", SecretRef: "mister-1"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := eng.BrowseNode(ctx(), "mister", "")
	if !errors.Is(err, engine.ErrBrowseUnsupported) {
		t.Fatalf("ssh browse err = %v, want ErrBrowseUnsupported", err)
	}
	if !errors.Is(err, reach.ErrUnsupportedReach) {
		t.Fatalf("ssh browse err = %v should also wrap reach.ErrUnsupportedReach", err)
	}
}

func TestBrowseNode_MissingNode(t *testing.T) {
	eng, _ := newSmokeEngine(t, map[string]reach.Reach{}, nil)
	_, err := eng.BrowseNode(ctx(), "nope", "")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing-node browse err = %v, want ErrNotFound", err)
	}
}
