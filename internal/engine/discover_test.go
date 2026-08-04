package engine_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/fakereach"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// --- inferGameName (exported via a thin test-only wrapper) ---------------
//
// inferGameName is unexported; we exercise it through the package_test boundary
// by testing the observable end-to-end aggregation, AND directly via the exported
// helper below. To keep a focused table test, engine exposes InferGameNameForTest.

func TestInferGameName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Super Metroid (USA).srm", "Super Metroid"},
		{"Super Metroid (USA) [!].srm", "Super Metroid"},
		{"Zelda [!].sav", "Zelda"},
		{"Game (USA, Europe) (Rev 1).srm", "Game"},
		{"Final Fantasy VI.srm", "Final Fantasy VI"},
		{"Chrono Trigger.sav", "Chrono Trigger"},
		{"  Spaced   Out  (USA).mcr", "Spaced Out"},
		{"Game.v2.srm", "Game.v2"},                 // internal dot survives; only last ext stripped
		{"NoExtension", "NoExtension"},             // no extension at all
		{"(USA).srm", ""},                          // whole base is a tag -> empty
		{"Mega Man [U][!].sav", "Mega Man"},        // multiple bracket tags
		{"Tag (Unbalanced.srm", "Tag (Unbalanced"}, // unbalanced -> left as-is
	}
	for _, c := range cases {
		if got := engine.InferGameNameForTest(c.in); got != c.want {
			t.Errorf("inferGameName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- DiscoverGames end-to-end (recursive scan + aggregation) -------------

// discoverHarness wires a memory store + a per-node fakereach (or an ssh node
// with no fake) + an Engine, with a resolver that returns ErrUnsupportedReach for
// ssh nodes (mirroring production resolve.ResolveReach), so the scan's skip path
// is exercised.
type discoverHarness struct {
	t      *testing.T
	store  store.Store
	fakes  map[string]*fakereach.Fake
	engine *engine.Engine
}

func newDiscoverHarness(t *testing.T) *discoverHarness {
	t.Helper()
	h := &discoverHarness{
		t:     t,
		store: memory.New(),
		fakes: map[string]*fakereach.Fake{},
	}
	resolve := func(n store.Node) (reach.Reach, error) {
		if n.Reach == store.ReachSSH {
			// Production resolve returns ErrUnsupportedReach for ssh; discovery must
			// SKIP such a node, not error.
			return nil, fmt.Errorf("resolve %q: %w", n.ID, reach.ErrUnsupportedReach)
		}
		f, ok := h.fakes[n.ID]
		if !ok {
			t.Fatalf("no fake for node %q", n.ID)
		}
		return f, nil
	}
	h.engine = engine.New(h.store, resolve, nil)
	return h
}

// node registers a syncthing-share node backed by a fresh fake and returns it for
// seeding files.
func (h *discoverHarness) node(id string) *fakereach.Fake {
	h.t.Helper()
	if err := h.store.CreateNode(context.Background(), store.Node{
		ID: id, Display: id, Kind: store.KindGeneric, Reach: store.ReachSyncthingShare,
	}); err != nil {
		h.t.Fatal(err)
	}
	f := fakereach.New()
	h.fakes[id] = f
	return f
}

// sshNode registers an ssh node (no fake; the resolver returns ErrUnsupportedReach).
func (h *discoverHarness) sshNode(id string) {
	h.t.Helper()
	if err := h.store.CreateNode(context.Background(), store.Node{
		ID: id, Display: id, Kind: store.KindMister, Reach: store.ReachSSH,
		ReachConfig: store.ReachConfig{Host: "h", User: "u", SecretRef: "s"},
	}); err != nil {
		h.t.Fatal(err)
	}
}

// claim creates a sync and a member so a (node, path) is "already synced" and
// must be excluded from discovery.
func (h *discoverHarness) claim(syncID, nodeID, path string) {
	h.t.Helper()
	ctx := context.Background()
	if err := h.store.CreateSync(ctx, store.Sync{ID: syncID, Game: "X", Name: "X"}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.SetSyncMember(ctx, store.SyncMember{SyncID: syncID, NodeID: nodeID, Path: path}); err != nil {
		h.t.Fatal(err)
	}
}

func mt(s string) time.Time {
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return tm
}

func TestDiscoverGames_AggregatesAcrossNodes(t *testing.T) {
	h := newDiscoverHarness(t)

	bob := h.node("bob-deck")
	alice := h.node("alice-deck")
	h.sshNode("mister") // must be SKIPPED, not errored

	// Same inferred game on two nodes (different region tags) -> one group, two
	// candidates.
	bob.Put("Super Metroid (USA).srm", []byte("a"), mt("2026-06-20T10:00:00Z"))
	alice.Put("Super Metroid (Europe).srm", []byte("bb"), mt("2026-06-21T10:00:00Z"))

	// Single-node game -> still returned (one candidate).
	bob.Put("Chrono Trigger.sav", []byte("ccc"), mt("2026-06-19T10:00:00Z"))

	// A save STATE next to a save -> the state is skipped, the .srm is found.
	bob.Put("Zelda.srm", []byte("d"), mt("2026-06-18T10:00:00Z"))
	bob.Put("Zelda.state", []byte("ignored"), mt("2026-06-18T10:00:00Z"))
	bob.Put("Zelda.state1", []byte("ignored"), mt("2026-06-18T10:00:00Z"))

	// A non-save file -> ignored.
	bob.Put("readme.txt", []byte("hi"), mt("2026-06-18T10:00:00Z"))

	// An already-synced file -> excluded from discovery.
	alice.Put("Metroid Prime.gci", []byte("ee"), mt("2026-06-17T10:00:00Z"))
	h.claim("mp-sync", "alice-deck", "Metroid Prime.gci")

	// A nested save -> the recursive walk finds it.
	bob.Put("subdir/Mega Man (USA).srm", []byte("f"), mt("2026-06-16T10:00:00Z"))

	games, err := h.engine.DiscoverGames(context.Background())
	if err != nil {
		t.Fatalf("DiscoverGames: %v", err)
	}

	got := map[string][]engine.DiscoveredCandidate{}
	for _, g := range games {
		got[g.Name] = g.Candidates
	}

	// Super Metroid: two candidates, sorted by node id (alice-deck before bob-deck).
	sm, ok := got["Super Metroid"]
	if !ok {
		t.Fatalf("Super Metroid not discovered; games=%v", names(games))
	}
	if len(sm) != 2 {
		t.Fatalf("Super Metroid candidates = %d, want 2: %+v", len(sm), sm)
	}
	if sm[0].NodeID != "alice-deck" || sm[1].NodeID != "bob-deck" {
		t.Errorf("Super Metroid candidates not node-sorted: %+v", sm)
	}
	if sm[0].Path != "Super Metroid (Europe).srm" || sm[0].Size != 2 {
		t.Errorf("Super Metroid alice candidate wrong: %+v", sm[0])
	}

	// Chrono Trigger: single-node, still present.
	if ct, ok := got["Chrono Trigger"]; !ok || len(ct) != 1 || ct[0].NodeID != "bob-deck" {
		t.Errorf("Chrono Trigger = %+v ok=%v, want single bob-deck candidate", got["Chrono Trigger"], ok)
	}

	// Zelda: only the .srm, the .state/.state1 are excluded.
	if z, ok := got["Zelda"]; !ok || len(z) != 1 || z[0].Path != "Zelda.srm" {
		t.Errorf("Zelda = %+v ok=%v, want single Zelda.srm candidate (states excluded)", got["Zelda"], ok)
	}

	// Mega Man found via nested directory.
	if mm, ok := got["Mega Man"]; !ok || len(mm) != 1 || mm[0].Path != "subdir/Mega Man (USA).srm" {
		t.Errorf("Mega Man = %+v ok=%v, want nested subdir candidate", got["Mega Man"], ok)
	}

	// Metroid Prime is already synced -> NOT discovered.
	if _, ok := got["Metroid Prime"]; ok {
		t.Errorf("Metroid Prime should be excluded (already in a sync); games=%v", names(games))
	}

	// readme.txt (non-save) contributed nothing.
	for _, g := range games {
		for _, c := range g.Candidates {
			if c.Path == "readme.txt" {
				t.Errorf("non-save readme.txt leaked into discovery: %+v", c)
			}
		}
	}

}

// listFailReach is a reach.Reach whose List always errors (non-ErrNotExist), to
// model a node whose scan fails (missing share, transient I/O). Only List is
// exercised by discovery; the other methods are unused stubs.
type listFailReach struct{ err error }

func (l listFailReach) Stat(context.Context, string) (reach.FileMeta, error) {
	return reach.FileMeta{}, l.err
}
func (l listFailReach) Read(context.Context, string) ([]byte, error) { return nil, l.err }
func (l listFailReach) Hash(context.Context, string) (string, error) { return "", l.err }
func (l listFailReach) List(context.Context, string) ([]reach.DirEntry, error) {
	return nil, l.err
}
func (l listFailReach) WriteAtomic(context.Context, string, []byte, time.Time) (time.Time, error) {
	return time.Time{}, l.err
}

// TestDiscoverGames_SkipsBadNode confirms a node whose scan errors is skipped
// (recorded via the test hook), not fatal to the whole discovery: the good node
// still discovers its game.
func TestDiscoverGames_SkipsBadNode(t *testing.T) {
	st := memory.New()
	ctx := context.Background()
	mkNode := func(id string) {
		if err := st.CreateNode(ctx, store.Node{
			ID: id, Display: id, Kind: store.KindGeneric, Reach: store.ReachSyncthingShare,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mkNode("good")
	mkNode("bad")

	good := fakereach.New()
	good.Put("Game (USA).srm", []byte("a"), mt("2026-06-20T10:00:00Z"))

	resolve := func(n store.Node) (reach.Reach, error) {
		if n.ID == "bad" {
			return listFailReach{err: fmt.Errorf("boom: share unreadable")}, nil
		}
		return good, nil
	}
	eng := engine.New(st, resolve, nil)

	var skipped []string
	engine.SetDiscoverSkipHookForTest(func(nodeID, reason string, err error) {
		skipped = append(skipped, nodeID)
	})
	defer engine.SetDiscoverSkipHookForTest(nil)

	games, err := eng.DiscoverGames(ctx)
	if err != nil {
		t.Fatalf("DiscoverGames errored on a bad node (should skip): %v", err)
	}
	if len(games) != 1 || games[0].Name != "Game" {
		t.Fatalf("expected only the good node's Game, got %v", names(games))
	}
	found := false
	for _, id := range skipped {
		if id == "bad" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the bad node to be skipped via the hook; skipped=%v", skipped)
	}
}

// TestDiscoverGames_SkipHookFiresForSSH asserts the per-node skip hook records the
// unsupported-reach skip (ssh) so the operator-facing log line is produced.
func TestDiscoverGames_SkipHookFiresForSSH(t *testing.T) {
	h := newDiscoverHarness(t)
	h.node("deck").Put("Game (USA).srm", []byte("a"), mt("2026-06-20T10:00:00Z"))
	h.sshNode("mister")

	var skipped []string
	engine.SetDiscoverSkipHookForTest(func(nodeID, reason string, err error) {
		skipped = append(skipped, nodeID)
	})
	defer engine.SetDiscoverSkipHookForTest(nil)

	if _, err := h.engine.DiscoverGames(context.Background()); err != nil {
		t.Fatalf("DiscoverGames: %v", err)
	}
	found := false
	for _, id := range skipped {
		if id == "mister" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ssh node 'mister' to be skipped via the hook; skipped=%v", skipped)
	}
}

// TestDiscoverGames_BoundedDepth confirms the walk stops descending past
// maxScanDepth so a pathological tree can't hang it: a file buried below the depth
// cap is not discovered.
func TestDiscoverGames_BoundedDepth(t *testing.T) {
	h := newDiscoverHarness(t)
	n := h.node("deck")

	// Depth 0 is the root. maxScanDepth is 6, so a file at d1/.../d7 (7 dirs deep)
	// sits below the cap and must NOT be found, while a shallow one is.
	n.Put("Shallow (USA).srm", []byte("a"), mt("2026-06-20T10:00:00Z"))
	n.Put("d1/d2/d3/d4/d5/d6/d7/Deep (USA).srm", []byte("b"), mt("2026-06-20T10:00:00Z"))

	games, err := h.engine.DiscoverGames(context.Background())
	if err != nil {
		t.Fatalf("DiscoverGames: %v", err)
	}
	got := map[string]bool{}
	for _, g := range games {
		got[g.Name] = true
	}
	if !got["Shallow"] {
		t.Errorf("shallow save should be discovered; games=%v", names(games))
	}
	if got["Deep"] {
		t.Errorf("save below the depth cap should NOT be discovered; games=%v", names(games))
	}
}

func names(games []engine.DiscoveredGame) []string {
	out := make([]string, 0, len(games))
	for _, g := range games {
		out = append(out, g.Name)
	}
	return out
}
