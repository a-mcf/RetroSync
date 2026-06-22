package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/fakereach"
	"github.com/a-mcf/retrosync/internal/store"
	"github.com/a-mcf/retrosync/internal/store/memory"
)

// --- test harness --------------------------------------------------------

const (
	gameID = "super-metroid"
	syncID = "sm-bob"
)

// fixedClock returns a Clock that advances by a fixed step on each call, so
// timestamps are deterministic but distinguishable.
func steppingClock(start time.Time, step time.Duration) engine.Clock {
	cur := start
	return func() time.Time {
		t := cur
		cur = cur.Add(step)
		return t
	}
}

// harness wires a memory Store + a fakereach.Fake per node + an Engine.
type harness struct {
	t      *testing.T
	store  store.Store
	fakes  map[string]*fakereach.Fake
	paths  map[string]string // nodeID -> game path
	engine *engine.Engine
}

func newHarness(t *testing.T, clock engine.Clock) *harness {
	t.Helper()
	h := &harness{
		t:     t,
		store: memory.New(),
		fakes: map[string]*fakereach.Fake{},
		paths: map[string]string{},
	}
	resolve := func(n store.Node) (reach.Reach, error) {
		f, ok := h.fakes[n.ID]
		if !ok {
			t.Fatalf("no fake for node %q", n.ID)
		}
		return f, nil
	}
	h.engine = engine.New(h.store, resolve, clock)
	ctx := context.Background()
	if err := h.store.CreateGame(ctx, store.Game{ID: gameID, Display: "Super Metroid", System: "snes"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateSync(ctx, store.Sync{ID: syncID, GameID: gameID, Name: "Bob's stream"}); err != nil {
		t.Fatal(err)
	}
	return h
}

// addNode registers a node + a sync member (the engine's in-scope (node, path))
// + a backing fake. If present, seeds the node's file with content at mtime.
func (h *harness) addNode(id, path string, content []byte, mtime time.Time, present bool) {
	h.t.Helper()
	ctx := context.Background()
	if err := h.store.CreateNode(ctx, store.Node{ID: id, Display: id, Kind: store.KindGeneric, Reach: store.ReachSyncthingShare}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.SetSyncMember(ctx, store.SyncMember{SyncID: syncID, NodeID: id, Path: path}); err != nil {
		h.t.Fatal(err)
	}
	f := fakereach.New()
	if present {
		f.Put(path, content, mtime)
	}
	h.fakes[id] = f
	h.paths[id] = path
}

// addNonMemberNode registers a node + a backing fake but NO sync member, so the
// node is out of scope for the sync. Used to prove the engine only touches sync
// members.
func (h *harness) addNonMemberNode(id, path string) {
	h.t.Helper()
	ctx := context.Background()
	if err := h.store.CreateNode(ctx, store.Node{ID: id, Display: id, Kind: store.KindGeneric, Reach: store.ReachSyncthingShare}); err != nil {
		h.t.Fatal(err)
	}
	h.fakes[id] = fakereach.New()
	h.paths[id] = path
}

func (h *harness) fake(id string) *fakereach.Fake { return h.fakes[id] }

func (h *harness) binding() store.ActiveBinding {
	h.t.Helper()
	b, err := h.store.GetBinding(context.Background(), syncID)
	if err != nil {
		h.t.Fatalf("get binding: %v", err)
	}
	return b
}

func (h *harness) manifest(nodeID string) store.ManifestEntry {
	h.t.Helper()
	m, err := h.store.GetManifest(context.Background(), syncID, nodeID)
	if err != nil {
		h.t.Fatalf("get manifest %s: %v", nodeID, err)
	}
	return m
}

func (h *harness) logCount(outcome store.Outcome) int {
	h.t.Helper()
	entries, err := h.store.ListLogBySync(context.Background(), syncID, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Outcome == outcome {
			n++
		}
	}
	return n
}

// content asserts a node's current file content + mtime.
func (h *harness) assertFile(nodeID string, content []byte, mtime time.Time) {
	h.t.Helper()
	fm, err := h.fake(nodeID).Stat(context.Background(), h.paths[nodeID])
	if err != nil {
		h.t.Fatalf("stat %s: %v", nodeID, err)
	}
	if !fm.Mtime.Equal(mtime) {
		h.t.Fatalf("%s mtime = %v want %v", nodeID, fm.Mtime, mtime)
	}
	data, err := h.fake(nodeID).Read(context.Background(), h.paths[nodeID])
	if err != nil {
		h.t.Fatalf("read %s: %v", nodeID, err)
	}
	if string(data) != string(content) {
		h.t.Fatalf("%s content = %q want %q", nodeID, data, content)
	}
}

var t0 = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

func ctx() context.Context { return context.Background() }

// --- Activation ----------------------------------------------------------

func TestActivate_NoNodeHasFile_Errors(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", nil, t0, false)
	h.addNode("peer", "q.srm", nil, t0, false)

	err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false)
	if !errors.Is(err, engine.ErrNoSave) {
		t.Fatalf("want ErrNoSave, got %v", err)
	}
	// No binding should have been created.
	if _, err := h.store.GetBinding(ctx(), syncID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding should not exist: %v", err)
	}
}

func TestActivate_SingleSource_AutoPickAndFanOut(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("SAVE"), srcMtime, true)
	h.addNode("peer1", "q.srm", nil, t0, false)
	h.addNode("peer2", "r.srm", nil, t0, false)

	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}

	// Fan-out reached every peer with correct content + source mtime.
	h.assertFile("peer1", []byte("SAVE"), srcMtime)
	h.assertFile("peer2", []byte("SAVE"), srcMtime)

	// Manifest populated for source + all written peers, at source mtime/size.
	for _, id := range []string{"primary", "peer1", "peer2"} {
		m := h.manifest(id)
		if m.Mtime == nil || !m.Mtime.Equal(srcMtime) {
			t.Fatalf("%s manifest mtime = %v want %v", id, m.Mtime, srcMtime)
		}
		if m.Size == nil || *m.Size != 4 {
			t.Fatalf("%s manifest size = %v want 4", id, m.Size)
		}
	}

	// sync_log: one ok row per directional copy (2 peers).
	if got := h.logCount(store.OutcomeOK); got != 2 {
		t.Fatalf("ok log rows = %d want 2", got)
	}

	b := h.binding()
	if b.LastSynced == nil {
		t.Fatal("last_synced should be set after activation")
	}
	if b.ConflictAt != nil {
		t.Fatal("should not be in conflict after clean activation")
	}
}

func TestActivate_MultiSource_FromPrimary(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	pMtime := t0.Add(-time.Hour)
	peerMtime := t0.Add(-2 * time.Hour)
	h.addNode("primary", "p.srm", []byte("PRIMARY"), pMtime, true)
	h.addNode("peer", "q.srm", []byte("PEER"), peerMtime, true)

	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	// from-primary => primary is the source; peer is overwritten with PRIMARY.
	h.assertFile("peer", []byte("PRIMARY"), pMtime)
}

func TestActivate_MultiSource_FromPeer(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	pMtime := t0.Add(-time.Hour)
	peerMtime := t0.Add(-2 * time.Hour)
	h.addNode("primary", "p.srm", []byte("PRIMARY"), pMtime, true)
	h.addNode("peer", "q.srm", []byte("PEER"), peerMtime, true)

	if err := h.engine.Activate(ctx(), syncID, "primary", "from-peer-peer", false); err != nil {
		t.Fatal(err)
	}
	// from-peer-peer => peer is the source; primary is overwritten with PEER.
	h.assertFile("primary", []byte("PEER"), peerMtime)
}

func TestActivate_FromPeer_MissingSource_Errors(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", []byte("PRIMARY"), t0, true)
	h.addNode("peer", "q.srm", nil, t0, false) // peer has no file

	err := h.engine.Activate(ctx(), syncID, "primary", "from-peer-peer", false)
	if !errors.Is(err, engine.ErrSourceMissing) {
		t.Fatalf("want ErrSourceMissing, got %v", err)
	}
}

func TestActivate_AlreadyActive_Rejected(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", []byte("SAVE"), t0, true)
	h.addNode("peer", "q.srm", nil, t0, false)

	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want store.ErrConflict, got %v", err)
	}
}

func TestActivate_ForceTakeover_ReplacesBinding(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	mineMtime := t0.Add(2 * time.Hour)
	h.addNode("primary", "p.srm", []byte("OLD"), srcMtime, true)
	h.addNode("other", "q.srm", []byte("OLD"), srcMtime, true)

	// First session: "primary" holds the binding, sourced from itself; the
	// fan-out gives "other" the OLD save too (both nodes are members of the sync —
	// a sync's members ARE its scope, there is no per-session scope to exclude).
	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	if b := h.binding(); b.PrimaryNode != "primary" {
		t.Fatalf("first binding primary = %q, want primary", b.PrimaryNode)
	}

	// "other" writes its own save out of band (the divergence the taker wants to
	// keep). We do NOT poll, so the conflict is not flagged before the takeover.
	h.fake("other").Mutate("q.srm", []byte("MINE"), mineMtime)

	// Without force, taking over fails.
	if err := h.engine.Activate(ctx(), syncID, "other", "from-peer-other", false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("takeover without force: want ErrConflict, got %v", err)
	}

	// With force, the existing binding is replaced (raw delete + create), and
	// the new source ("other") is fanned out — so "primary" now holds MINE.
	if err := h.engine.Activate(ctx(), syncID, "other", "from-peer-other", true); err != nil {
		t.Fatalf("force takeover: %v", err)
	}
	b := h.binding()
	if b.PrimaryNode != "other" {
		t.Fatalf("after force takeover primary = %q, want other", b.PrimaryNode)
	}
	if b.Direction != "from-peer-other" {
		t.Fatalf("after force takeover direction = %q, want from-peer-other", b.Direction)
	}
	h.assertFile("primary", []byte("MINE"), mineMtime)
}

func TestActivate_Force_NonViableSource_LeavesExistingBinding(t *testing.T) {
	// A force-takeover whose chosen source has NO file must return its error
	// (ErrSourceMissing) WITHOUT having deleted the pre-existing binding. The
	// displaced session must not be lost for a takeover that cannot proceed.
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("OLD"), srcMtime, true)
	h.addNode("other", "q.srm", nil, srcMtime, false /* no file */)

	// First session: "primary" holds the binding, sourced from itself. The
	// fan-out gives "other" the OLD save; we then REMOVE it out of band so the
	// takeover below has a non-viable source (a member with no file).
	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	h.fake("other").Remove("q.srm")
	before := h.binding()
	if before.PrimaryNode != "primary" {
		t.Fatalf("setup binding primary = %q, want primary", before.PrimaryNode)
	}

	// Force takeover sourced from "other", which has no file → ErrSourceMissing.
	err := h.engine.Activate(ctx(), syncID, "other", "from-peer-other", true)
	if !errors.Is(err, engine.ErrSourceMissing) {
		t.Fatalf("force takeover with missing source: want ErrSourceMissing, got %v", err)
	}

	// The pre-existing binding must be UNCHANGED (not dropped, not replaced).
	after := h.binding()
	if after.PrimaryNode != "primary" {
		t.Fatalf("after failed force takeover primary = %q, want primary (binding must survive)", after.PrimaryNode)
	}
	if after.Direction != before.Direction || !after.StartedAt.Equal(before.StartedAt) {
		t.Fatalf("after failed force takeover binding mutated: before=%+v after=%+v", before, after)
	}
}

func TestActivate_Force_OnIdleGame_Activates(t *testing.T) {
	// force=true on a game with no existing binding behaves like a normal
	// activate (the delete is skipped because GetBinding returns ErrNotFound).
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", []byte("SAVE"), t0.Add(-time.Hour), true)
	h.addNode("peer", "q.srm", nil, t0, false)
	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", true); err != nil {
		t.Fatalf("force activate on idle: %v", err)
	}
	if b := h.binding(); b.PrimaryNode != "primary" {
		t.Fatalf("binding primary = %q, want primary", b.PrimaryNode)
	}
}

func TestActivate_BadDirection_Rejected(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", []byte("SAVE"), t0, true)
	err := h.engine.Activate(ctx(), syncID, "primary", "sideways", false)
	if !errors.Is(err, store.ErrInvalidValue) {
		t.Fatalf("want ErrInvalidValue, got %v", err)
	}
}

// TestActivate_ScopeIsSyncMembers proves the engine's in-scope set is EXACTLY
// the sync's members (a sync's members ARE its scope — there is no peer_scope).
// A node that is NOT a member of the sync is never touched, even if it holds a
// file at the same path on the same device class.
func TestActivate_ScopeIsSyncMembers(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	src := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("SAVE"), src, true)
	h.addNode("in", "q.srm", nil, t0, false)
	// "out" is a registered node with a backing fake but NOT a member of this
	// sync, so it is out of scope. addNonMemberNode wires the fake without a
	// SyncMember row.
	h.addNonMemberNode("out", "r.srm")

	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	h.assertFile("in", []byte("SAVE"), src)
	// "out" is not a member: nothing written.
	if _, err := h.fake("out").Stat(ctx(), "r.srm"); !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("non-member node should be untouched, got %v", err)
	}
}

// TestActivate_PrimaryNotMember_Rejected_NothingMutated proves the authority
// gate: a primaryNode that is NOT a member of the sync is rejected with
// ErrPrimaryNotMember BEFORE any binding is created — even though the node is a
// registered node with a file. (The web layer only checks node OWNERSHIP; the
// engine must independently require membership so owning an unrelated node can
// never seize a sync's play authority.)
func TestActivate_PrimaryNotMember_Rejected_NothingMutated(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("member", "p.srm", []byte("SAVE"), t0.Add(-time.Hour), true)
	// "outsider" is a registered node with its own file but is NOT a member of
	// the sync. Naming it as primary must be rejected.
	h.addNonMemberNode("outsider", "o.srm")
	h.fake("outsider").Put("o.srm", []byte("OUTSIDER"), t0)

	err := h.engine.Activate(ctx(), syncID, "outsider", "from-peer-member", false)
	if !errors.Is(err, engine.ErrPrimaryNotMember) {
		t.Fatalf("want ErrPrimaryNotMember, got %v", err)
	}
	// No binding created.
	if _, err := h.store.GetBinding(ctx(), syncID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding must not exist after rejected activate: %v", err)
	}
	// The member's file was not fanned out anywhere (nothing mutated).
	if got := h.logCount(store.OutcomeOK); got != 0 {
		t.Fatalf("rejected activate logged ok rows: %d", got)
	}
}

// TestActivate_ForcePrimaryNotMember_LeavesExistingBinding proves the
// authority gate runs BEFORE the force-takeover delete: a force activate naming
// a non-member primary must be rejected WITHOUT having displaced the existing
// binding.
func TestActivate_ForcePrimaryNotMember_LeavesExistingBinding(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("member", "p.srm", []byte("SAVE"), t0.Add(-time.Hour), true)
	h.addNode("peer", "q.srm", nil, t0, false)
	h.addNonMemberNode("outsider", "o.srm")
	h.fake("outsider").Put("o.srm", []byte("OUTSIDER"), t0)

	// Establish an existing binding on a legitimate member.
	if err := h.engine.Activate(ctx(), syncID, "member", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	before := h.binding()

	// Force-takeover naming the non-member as primary → rejected, binding intact.
	err := h.engine.Activate(ctx(), syncID, "outsider", "from-peer-member", true)
	if !errors.Is(err, engine.ErrPrimaryNotMember) {
		t.Fatalf("want ErrPrimaryNotMember, got %v", err)
	}
	after := h.binding()
	if after.PrimaryNode != before.PrimaryNode || !after.StartedAt.Equal(before.StartedAt) {
		t.Fatalf("existing binding must survive a rejected force activate: before=%+v after=%+v", before, after)
	}
}

// --- Poll case table -----------------------------------------------------

// activateClean sets up primary+peer with the primary holding a save, runs a
// clean activation, and returns the harness ready for Poll cases.
func activateClean(t *testing.T) (*harness, time.Time) {
	t.Helper()
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("V1"), srcMtime, true)
	h.addNode("peer", "q.srm", nil, t0, false)
	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	return h, srcMtime
}

func TestPoll_Noop(t *testing.T) {
	h, _ := activateClean(t)
	okBefore := h.logCount(store.OutcomeOK)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if got := h.logCount(store.OutcomeOK); got != okBefore {
		t.Fatalf("noop poll added ok rows: %d -> %d", okBefore, got)
	}
	if h.binding().ConflictAt != nil {
		t.Fatal("noop should not conflict")
	}
}

func TestPoll_PrimaryChanged_FanOut(t *testing.T) {
	h, _ := activateClean(t)
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2"), newMtime)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	h.assertFile("peer", []byte("V2"), newMtime)
	if m := h.manifest("peer"); m.Mtime == nil || !m.Mtime.Equal(newMtime) {
		t.Fatalf("peer manifest not advanced: %v", m.Mtime)
	}
	if h.binding().ConflictAt != nil {
		t.Fatal("primary-only change must not conflict")
	}
}

func TestPoll_PeerChanged_Conflict(t *testing.T) {
	h, _ := activateClean(t)
	// Peer mutates out of band; primary unchanged.
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE"), t0.Add(2*time.Hour))

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.binding().ConflictAt == nil {
		t.Fatal("peer-changed must set conflict_at")
	}
	if got := h.logCount(store.OutcomeConflict); got != 1 {
		t.Fatalf("conflict log rows = %d want 1", got)
	}
	// Sync stops: peer's out-of-band content is NOT overwritten.
	h.assertFile("peer", []byte("PEER-WROTE"), t0.Add(2*time.Hour))
}

func TestPoll_BothChanged_Conflict(t *testing.T) {
	h, _ := activateClean(t)
	h.fake("primary").Mutate("p.srm", []byte("V2"), t0.Add(time.Hour))
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE"), t0.Add(2*time.Hour))

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.binding().ConflictAt == nil {
		t.Fatal("both-changed must set conflict_at")
	}
}

func TestPoll_ConflictedBinding_Skipped(t *testing.T) {
	h, _ := activateClean(t)
	// Force into conflict.
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE"), t0.Add(2*time.Hour))
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	conflictAt := h.binding().ConflictAt
	if conflictAt == nil {
		t.Fatal("setup: expected conflict")
	}
	// Now the primary changes; a subsequent poll must do nothing (paused).
	h.fake("primary").Mutate("p.srm", []byte("V99"), t0.Add(3*time.Hour))
	okBefore := h.logCount(store.OutcomeOK)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if got := h.logCount(store.OutcomeOK); got != okBefore {
		t.Fatalf("conflicted binding should not sync: ok %d -> %d", okBefore, got)
	}
	// conflict_at unchanged; peer not overwritten.
	if got := h.binding().ConflictAt; got == nil || !got.Equal(*conflictAt) {
		t.Fatalf("conflict_at changed on skipped poll: %v -> %v", conflictAt, got)
	}
	h.assertFile("peer", []byte("PEER-WROTE"), t0.Add(2*time.Hour))
}

// --- Deactivate ----------------------------------------------------------

func TestDeactivate_CleanFinalPass_Deletes(t *testing.T) {
	h, _ := activateClean(t)
	// Primary changed since activation: final pass fans it out, then deletes.
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("FINAL"), newMtime)

	if err := h.engine.Deactivate(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	h.assertFile("peer", []byte("FINAL"), newMtime)
	if _, err := h.store.GetBinding(ctx(), syncID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding should be deleted, got %v", err)
	}
}

func TestDeactivate_NoChange_Deletes(t *testing.T) {
	h, _ := activateClean(t)
	if err := h.engine.Deactivate(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.GetBinding(ctx(), syncID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding should be deleted, got %v", err)
	}
}

func TestDeactivate_ConflictingFinalPass_LeavesActive(t *testing.T) {
	h, _ := activateClean(t)
	// A peer mutated out of band: the final pass would conflict.
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE"), t0.Add(2*time.Hour))

	if err := h.engine.Deactivate(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	b, err := h.store.GetBinding(ctx(), syncID)
	if err != nil {
		t.Fatalf("binding should still exist: %v", err)
	}
	if b.ConflictAt == nil {
		t.Fatal("conflicting final pass must flag conflict and leave the binding")
	}
}

func TestDeactivate_Idle_Idempotent(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", nil, t0, false)
	if err := h.engine.Deactivate(ctx(), syncID); err != nil {
		t.Fatalf("deactivating idle game should be a no-op, got %v", err)
	}
}

// --- Crash-safety intent -------------------------------------------------

func TestPoll_WriteFails_ManifestNotAdvanced_RetriesNextPoll(t *testing.T) {
	h, _ := activateClean(t)
	peerManifestBefore := h.manifest("peer")

	// Primary changes, but the peer's WriteAtomic will fail this pass.
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2"), newMtime)
	boom := errors.New("sftp: connection reset")
	h.fake("peer").FailWriteAtomic("q.srm", boom)

	if err := h.engine.Poll(ctx(), syncID); !errors.Is(err, boom) {
		t.Fatalf("want write error, got %v", err)
	}
	// Manifest must NOT have advanced (crash-safety: manifest trails the write).
	m := h.manifest("peer")
	if peerManifestBefore.Mtime == nil || m.Mtime == nil || !m.Mtime.Equal(*peerManifestBefore.Mtime) {
		t.Fatalf("peer manifest advanced despite failed write: before=%v after=%v", peerManifestBefore.Mtime, m.Mtime)
	}
	// An error row was logged.
	if got := h.logCount(store.OutcomeError); got != 1 {
		t.Fatalf("error log rows = %d want 1", got)
	}

	// Recover: clear the failure and re-poll. The retry now succeeds.
	h.fake("peer").FailWriteAtomic("q.srm", nil)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("retry poll: %v", err)
	}
	h.assertFile("peer", []byte("V2"), newMtime)
	if m := h.manifest("peer"); m.Mtime == nil || !m.Mtime.Equal(newMtime) {
		t.Fatalf("peer manifest not advanced after successful retry: %v", m.Mtime)
	}
}

// --- Activation rollback on fan-out failure ------------------------------

func TestActivate_FanOutFails_RollsBackToIdle_AndRetrySucceeds(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("SAVE"), srcMtime, true)
	h.addNode("peer", "q.srm", nil, t0, false)

	// The first-sync fan-out to the peer will fail.
	boom := errors.New("sftp: connection reset")
	h.fake("peer").FailWriteAtomic("q.srm", boom)

	err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false)
	if !errors.Is(err, boom) {
		t.Fatalf("want fan-out write error, got %v", err)
	}
	// The binding must have been rolled back: the game is idle again, not stuck
	// half-active with a nil last_synced.
	if _, err := h.store.GetBinding(ctx(), syncID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding should have been rolled back to idle, got %v", err)
	}

	// Clear the injected failure and retry: a clean activation must now succeed
	// (no lingering ErrConflict from the half-active row).
	h.fake("peer").FailWriteAtomic("q.srm", nil)
	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatalf("retry activate should succeed after rollback, got %v", err)
	}
	h.assertFile("peer", []byte("SAVE"), srcMtime)
	if b := h.binding(); b.LastSynced == nil || b.ConflictAt != nil {
		t.Fatalf("retry should be cleanly synced: last_synced=%v conflict_at=%v", b.LastSynced, b.ConflictAt)
	}
}

// --- Multi-peer (N>1) coverage on the data-loss-critical path ------------

// activateCleanMulti sets up a primary + two peers with the primary holding the
// only save, runs a clean activation, and returns the harness. The primary's
// save is V1 at srcMtime; both peers start empty and receive V1 on activation.
func activateCleanMulti(t *testing.T) (*harness, time.Time) {
	t.Helper()
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("V1"), srcMtime, true)
	h.addNode("peerA", "a.srm", nil, t0, false)
	h.addNode("peerB", "b.srm", nil, t0, false)
	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	return h, srcMtime
}

func TestActivate_MultiPeer_FansOutToBoth(t *testing.T) {
	h, srcMtime := activateCleanMulti(t)
	// Both peers got the source content + source mtime.
	h.assertFile("peerA", []byte("V1"), srcMtime)
	h.assertFile("peerB", []byte("V1"), srcMtime)
	// One ok log row per peer.
	if got := h.logCount(store.OutcomeOK); got != 2 {
		t.Fatalf("ok log rows = %d want 2", got)
	}
	for _, id := range []string{"primary", "peerA", "peerB"} {
		if m := h.manifest(id); m.Mtime == nil || !m.Mtime.Equal(srcMtime) {
			t.Fatalf("%s manifest mtime = %v want %v", id, m.Mtime, srcMtime)
		}
	}
}

func TestPoll_MultiPeer_PrimaryChanged_FansOutToBoth(t *testing.T) {
	h, _ := activateCleanMulti(t)
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2"), newMtime)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	// Fan-out reached BOTH peers.
	h.assertFile("peerA", []byte("V2"), newMtime)
	h.assertFile("peerB", []byte("V2"), newMtime)
	if h.binding().ConflictAt != nil {
		t.Fatal("primary-only change must not conflict")
	}
}

func TestPoll_MultiPeer_OnePeerMutates_ConflictAndOtherPeerUntouched(t *testing.T) {
	h, srcMtime := activateCleanMulti(t)
	// peerA mutates out of band; primary and peerB unchanged.
	peerAMtime := t0.Add(2 * time.Hour)
	h.fake("peerA").Mutate("a.srm", []byte("A-WROTE"), peerAMtime)

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.binding().ConflictAt == nil {
		t.Fatal("a mutating peer must flag conflict")
	}
	// The mutating peer is NOT overwritten...
	h.assertFile("peerA", []byte("A-WROTE"), peerAMtime)
	// ...and crucially the OTHER peer is NOT touched either: the conflict pauses
	// the whole pass, it does not fan the primary out to the innocent peer. This
	// is the family-sync gotcha — peerB must still hold the original V1/srcMtime.
	h.assertFile("peerB", []byte("V1"), srcMtime)
}

func TestPoll_MultiPeer_PartialFanOut_SelfHeals(t *testing.T) {
	h, _ := activateCleanMulti(t)
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2"), newMtime)

	// The fan-out writes peers in node-id order (peerA before peerB). Fail the
	// SECOND peer so peerA's manifest advances but peerB's does not.
	boom := errors.New("sftp: connection reset")
	h.fake("peerB").FailWriteAtomic("b.srm", boom)

	if err := h.engine.Poll(ctx(), syncID); !errors.Is(err, boom) {
		t.Fatalf("want write error on peerB, got %v", err)
	}
	// peerA advanced (content + manifest), peerB did not.
	h.assertFile("peerA", []byte("V2"), newMtime)
	if m := h.manifest("peerA"); m.Mtime == nil || !m.Mtime.Equal(newMtime) {
		t.Fatalf("peerA manifest should have advanced: %v", m.Mtime)
	}
	if m := h.manifest("peerB"); m.Mtime == nil || m.Mtime.Equal(newMtime) {
		t.Fatalf("peerB manifest should NOT have advanced: %v", m.Mtime)
	}

	// Next poll self-heals: clear the failure and re-poll. peerB now converges
	// and the system reaches a consistent state with no conflict.
	h.fake("peerB").FailWriteAtomic("b.srm", nil)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("self-heal poll: %v", err)
	}
	h.assertFile("peerA", []byte("V2"), newMtime)
	h.assertFile("peerB", []byte("V2"), newMtime)
	if m := h.manifest("peerB"); m.Mtime == nil || !m.Mtime.Equal(newMtime) {
		t.Fatalf("peerB manifest not converged after self-heal: %v", m.Mtime)
	}
	if h.binding().ConflictAt != nil {
		t.Fatal("self-healing fan-out must not flag a conflict")
	}
}

// --- Primary vanished => conflict (no deletion propagated) ---------------

func TestPoll_PrimaryVanished_Conflict_PeersUntouched(t *testing.T) {
	h, srcMtime := activateCleanMulti(t)
	// The primary's file disappears (e.g. the device wiped its save).
	h.fake("primary").Remove("p.srm")

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.binding().ConflictAt == nil {
		t.Fatal("a vanished primary must flag a conflict, not delete peers")
	}
	// Peers keep their saves: no deletion was propagated.
	h.assertFile("peerA", []byte("V1"), srcMtime)
	h.assertFile("peerB", []byte("V1"), srcMtime)
}

// --- Conflict resolution -------------------------------------------------

// backupSuffix mirrors engine.backupSuffix for test assertions: the
// filesystem-safe sibling-backup suffix for a resolution at t.
func backupSuffix(t time.Time) string {
	return ".retrosync-conflict-" + t.UTC().Format("20060102T150405.000000000Z")
}

// activateConflicted sets up primary + peer, activates cleanly, then mutates the
// peer out-of-band and polls to flag the conflict. Returns the harness, the
// primary's (winner-candidate) save mtime, and the time the conflict was
// flagged. After this the binding is conflicted (paused) and ready to resolve.
//   - primary "p.srm": "PRIMARY" at primaryMtime (the eventual winner content)
//   - peer    "q.srm": "PEER-WROTE" at peerMtime (the loser's divergent content)
func activateConflicted(t *testing.T) (h *harness, primaryMtime, peerMtime time.Time) {
	t.Helper()
	h = newHarness(t, steppingClock(t0, time.Second))
	primaryMtime = t0.Add(-time.Hour)
	peerMtime = t0.Add(2 * time.Hour)
	h.addNode("primary", "p.srm", []byte("PRIMARY"), primaryMtime, true)
	h.addNode("peer", "q.srm", nil, t0, false)
	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	// After activation the peer holds PRIMARY@primaryMtime; mutate it out of band.
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE"), peerMtime)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.binding().ConflictAt == nil {
		t.Fatal("setup: expected the binding to be conflicted")
	}
	return h, primaryMtime, peerMtime
}

func TestResolveConflict_PrimaryWins_BacksUpLoserAndFansOut(t *testing.T) {
	h, primaryMtime, peerMtime := activateConflicted(t)

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The <ts> in the backup path is the RESOLUTION time, which the engine also
	// records as last_synced on the (now-cleared) binding.
	resolvedAt := h.binding().LastSynced
	if resolvedAt == nil {
		t.Fatal("last_synced should be set after resolution")
	}
	// The loser's ORIGINAL file was backed up to <path>.retrosync-conflict-<ts>
	// on the loser's node, with the loser's original bytes + mtime.
	suffix := backupSuffix(*resolvedAt)
	backupPath := "q.srm" + suffix
	bf, ok := h.fake("peer").Get(backupPath)
	if !ok {
		t.Fatalf("loser backup %q not found; paths=%v", backupPath, h.fake("peer").Paths())
	}
	if string(bf.Data) != "PEER-WROTE" {
		t.Fatalf("backup content = %q want PEER-WROTE", bf.Data)
	}
	if !bf.Mtime.Equal(peerMtime) {
		t.Fatalf("backup mtime = %v want %v (loser's original)", bf.Mtime, peerMtime)
	}

	// Every peer now holds the WINNER's bytes + mtime.
	h.assertFile("peer", []byte("PRIMARY"), primaryMtime)

	// Manifest updated for winner + every written node to the winner's mtime/size.
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.Mtime == nil || !m.Mtime.Equal(primaryMtime) {
			t.Fatalf("%s manifest mtime = %v want %v", id, m.Mtime, primaryMtime)
		}
		if m.Size == nil || *m.Size != int64(len("PRIMARY")) {
			t.Fatalf("%s manifest size = %v want %d", id, m.Size, len("PRIMARY"))
		}
	}

	// conflict_at cleared (last_synced asserted above).
	if b := h.binding(); b.ConflictAt != nil {
		t.Fatalf("conflict_at should be cleared, got %v", b.ConflictAt)
	}

	// sync_log has the backup row (message names the backup path) and the fan-out
	// ok row. There is 1 backup + 1 fan-out copy here.
	entries, err := h.store.ListLogBySync(ctx(), syncID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var backups, fanouts int
	for _, e := range entries {
		if e.Outcome != store.OutcomeOK {
			continue
		}
		if strings.Contains(e.Message, "conflict-resolve backup") {
			backups++
			if !strings.Contains(e.Message, backupPath) {
				t.Fatalf("backup log message should name the backup path, got %q", e.Message)
			}
		} else {
			fanouts++
		}
	}
	if backups != 1 {
		t.Fatalf("backup log rows = %d want 1", backups)
	}
	if fanouts < 1 {
		t.Fatalf("expected at least one fan-out ok row, got %d", fanouts)
	}
}

func TestResolveConflict_NotConflicted_Errors_NothingMutated(t *testing.T) {
	h, srcMtime := activateClean(t) // clean, no conflict
	okBefore := h.logCount(store.OutcomeOK)
	writesBefore := len(h.fake("peer").Writes()) // activation already wrote once

	err := h.engine.ResolveConflict(ctx(), syncID, "primary")
	if !errors.Is(err, engine.ErrNotConflicted) {
		t.Fatalf("want ErrNotConflicted, got %v", err)
	}
	// Nothing mutated: no new writes/logs, no backup files, binding untouched.
	if got := h.logCount(store.OutcomeOK); got != okBefore {
		t.Fatalf("ResolveConflict on a clean binding logged rows: %d -> %d", okBefore, got)
	}
	if got := len(h.fake("peer").Writes()); got != writesBefore {
		t.Fatalf("no new writes expected: %d -> %d", writesBefore, got)
	}
	h.assertFile("peer", []byte("V1"), srcMtime)
}

func TestResolveConflict_WinnerNotInScope_Errors_NothingMutated(t *testing.T) {
	h, _, _ := activateConflicted(t)
	conflictBefore := h.binding().ConflictAt
	okBefore := h.logCount(store.OutcomeOK)

	err := h.engine.ResolveConflict(ctx(), syncID, "ghost-node")
	if !errors.Is(err, engine.ErrNoPath) {
		t.Fatalf("want ErrNoPath, got %v", err)
	}
	if got := h.binding().ConflictAt; got == nil || !got.Equal(*conflictBefore) {
		t.Fatalf("conflict_at must remain set/unchanged, got %v", got)
	}
	if got := h.logCount(store.OutcomeOK); got != okBefore {
		t.Fatalf("nothing should have been logged: %d -> %d", okBefore, got)
	}
}

func TestResolveConflict_WinnerHasNoFile_Errors_NothingMutated(t *testing.T) {
	h, _, peerMtime := activateConflicted(t)
	conflictBefore := h.binding().ConflictAt
	writesBefore := len(h.fake("peer").Writes()) // activation wrote once
	// Remove the primary's file so the chosen winner has nothing to fan out.
	h.fake("primary").Remove("p.srm")

	err := h.engine.ResolveConflict(ctx(), syncID, "primary")
	if !errors.Is(err, engine.ErrSourceMissing) {
		t.Fatalf("want ErrSourceMissing, got %v", err)
	}
	// Still conflicted, peer untouched (no backup, no overwrite).
	if got := h.binding().ConflictAt; got == nil || !got.Equal(*conflictBefore) {
		t.Fatalf("conflict_at must remain set, got %v", got)
	}
	h.assertFile("peer", []byte("PEER-WROTE"), peerMtime)
	if got := len(h.fake("peer").Writes()); got != writesBefore {
		t.Fatalf("no new writes (incl. backup) expected when the winner has no file: %d -> %d", writesBefore, got)
	}
}

func TestResolveConflict_NodeWithoutFile_NoBackup_StillReceivesWinner(t *testing.T) {
	// Three nodes: primary (winner) + peerA (has a divergent file) + peerB (NO
	// file). peerB must get NO backup but must still RECEIVE the winner's file.
	h := newHarness(t, steppingClock(t0, time.Second))
	primaryMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("PRIMARY"), primaryMtime, true)
	h.addNode("peerA", "a.srm", nil, t0, false)
	h.addNode("peerB", "b.srm", nil, t0, false)
	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}
	// peerA mutates out of band (becomes a loser with a file); peerB is then wiped
	// so it has NO file at resolution time.
	peerAMtime := t0.Add(2 * time.Hour)
	h.fake("peerA").Mutate("a.srm", []byte("A-WROTE"), peerAMtime)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.binding().ConflictAt == nil {
		t.Fatal("setup: expected conflict")
	}
	h.fake("peerB").Remove("b.srm")

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	resolvedAt := h.binding().LastSynced
	if resolvedAt == nil {
		t.Fatal("last_synced should be set after resolution")
	}
	suffix := backupSuffix(*resolvedAt)
	// peerA (had a file) got a backup with its original bytes.
	bf, ok := h.fake("peerA").Get("a.srm" + suffix)
	if !ok {
		t.Fatalf("peerA backup missing; paths=%v", h.fake("peerA").Paths())
	}
	if string(bf.Data) != "A-WROTE" || !bf.Mtime.Equal(peerAMtime) {
		t.Fatalf("peerA backup = %q@%v want A-WROTE@%v", bf.Data, bf.Mtime, peerAMtime)
	}
	// peerB (no file) got NO backup...
	if _, ok := h.fake("peerB").Get("b.srm" + suffix); ok {
		t.Fatal("peerB had no file; it must NOT have a backup")
	}
	// ...but STILL received the winner's file.
	h.assertFile("peerB", []byte("PRIMARY"), primaryMtime)
	h.assertFile("peerA", []byte("PRIMARY"), primaryMtime)
}

func TestResolveConflict_FanOutFails_LeavesConflictSet_Reresolvable(t *testing.T) {
	// A WriteAtomic failure during fan-out must leave conflict_at STILL set and
	// return the error, so the resolution can be retried.
	h, primaryMtime, peerMtime := activateConflicted(t)
	conflictBefore := h.binding().ConflictAt

	boom := errors.New("sftp: connection reset")
	// Fail only the fan-out WRITE to the peer's real path (not the backup path),
	// so the backup succeeds but the overwrite fails.
	h.fake("peer").FailWriteAtomic("q.srm", boom)

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); !errors.Is(err, boom) {
		t.Fatalf("want fan-out write error, got %v", err)
	}
	// Conflict must remain set (re-resolvable).
	if got := h.binding().ConflictAt; got == nil || !got.Equal(*conflictBefore) {
		t.Fatalf("conflict_at must remain set after a partial fan-out, got %v", got)
	}
	// The backup of the loser's ORIGINAL file was written before the failed
	// overwrite (insurance held). The fan-out failed so last_synced was not set;
	// locate the backup by its conflict-backup prefix.
	var backupPath string
	for _, p := range h.fake("peer").Paths() {
		if strings.HasPrefix(p, "q.srm.retrosync-conflict-") {
			backupPath = p
		}
	}
	if backupPath == "" {
		t.Fatalf("loser backup must exist even on a failed fan-out; paths=%v", h.fake("peer").Paths())
	}
	bf, _ := h.fake("peer").Get(backupPath)
	if string(bf.Data) != "PEER-WROTE" || !bf.Mtime.Equal(peerMtime) {
		t.Fatalf("backup = %q@%v want PEER-WROTE@%v", bf.Data, bf.Mtime, peerMtime)
	}

	// Recover: clear the failure and re-resolve. It now succeeds.
	h.fake("peer").FailWriteAtomic("q.srm", nil)
	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("re-resolve after recovery: %v", err)
	}
	h.assertFile("peer", []byte("PRIMARY"), primaryMtime)
	if h.binding().ConflictAt != nil {
		t.Fatal("conflict_at should be cleared after a successful re-resolution")
	}
}

// subSecondClock returns a Clock whose successive values share the SAME
// wall-clock second but differ by 1ns per call. It exists to prove the
// backup-suffix collision fix: two resolves in the same second must compute
// DIFFERENT backup paths. With the old whole-second suffix every value here
// formats identically (collision); with sub-second precision they differ.
func subSecondClock(start time.Time) engine.Clock {
	cur := start
	return func() time.Time {
		t := cur
		cur = cur.Add(time.Nanosecond)
		return t
	}
}

func TestResolveConflict_SameSecondResolves_DistinctBackupPaths_NoClobber(t *testing.T) {
	// Two resolutions of the SAME binding within one wall-clock second (a
	// double-click, or a retry after a partial fan-out) must NOT compute the same
	// backup path: the second WriteAtomic would otherwise clobber the first
	// loser's only preserved snapshot. The clock returns sub-second-distinct times
	// in the same second, so the two backups must land at DIFFERENT paths.
	//
	// This test would FAIL against the old whole-second suffix format: both
	// resolutions would format to the identical <ts>, the second backup would
	// overwrite the first, and only one snapshot would survive.
	start := time.Date(2026, 6, 21, 12, 0, 0, 1, time.UTC) // ...:00.000000001Z
	h := newHarness(t, subSecondClock(start))
	primaryMtime := t0.Add(-time.Hour)
	peerMtime1 := t0.Add(2 * time.Hour)
	h.addNode("primary", "p.srm", []byte("PRIMARY"), primaryMtime, true)
	h.addNode("peer", "q.srm", nil, t0, false)
	if err := h.engine.Activate(ctx(), syncID, "primary", "from-primary", false); err != nil {
		t.Fatal(err)
	}

	// First conflict: peer diverges, poll flags it, then resolve (primary wins).
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE-1"), peerMtime1)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.binding().ConflictAt == nil {
		t.Fatal("setup: expected first conflict")
	}
	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	firstResolvedAt := *h.binding().LastSynced
	firstBackup := "q.srm" + backupSuffix(firstResolvedAt)
	if _, ok := h.fake("peer").Get(firstBackup); !ok {
		t.Fatalf("first backup %q missing; paths=%v", firstBackup, h.fake("peer").Paths())
	}

	// Second conflict on the SAME binding, resolved later in the SAME second.
	peerMtime2 := t0.Add(3 * time.Hour)
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE-2"), peerMtime2)
	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	if h.binding().ConflictAt == nil {
		t.Fatal("setup: expected second conflict")
	}
	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	secondResolvedAt := *h.binding().LastSynced
	secondBackup := "q.srm" + backupSuffix(secondResolvedAt)

	// Both resolutions happened in the same wall-clock second...
	if firstResolvedAt.Truncate(time.Second) != secondResolvedAt.Truncate(time.Second) {
		t.Fatalf("test premise broken: resolutions not in the same second: %v vs %v", firstResolvedAt, secondResolvedAt)
	}
	// ...yet the backup paths must be DISTINCT (the fix).
	if firstBackup == secondBackup {
		t.Fatalf("same-second resolves produced the SAME backup path %q: the second clobbers the first loser's only snapshot", firstBackup)
	}

	// Both snapshots must coexist with their original divergent bytes intact.
	bf1, ok := h.fake("peer").Get(firstBackup)
	if !ok {
		t.Fatalf("first backup %q was clobbered; paths=%v", firstBackup, h.fake("peer").Paths())
	}
	if string(bf1.Data) != "PEER-WROTE-1" || !bf1.Mtime.Equal(peerMtime1) {
		t.Fatalf("first backup = %q@%v want PEER-WROTE-1@%v", bf1.Data, bf1.Mtime, peerMtime1)
	}
	bf2, ok := h.fake("peer").Get(secondBackup)
	if !ok {
		t.Fatalf("second backup %q missing; paths=%v", secondBackup, h.fake("peer").Paths())
	}
	if string(bf2.Data) != "PEER-WROTE-2" || !bf2.Mtime.Equal(peerMtime2) {
		t.Fatalf("second backup = %q@%v want PEER-WROTE-2@%v", bf2.Data, bf2.Mtime, peerMtime2)
	}
}

// --- NodeStates ----------------------------------------------------------

func TestNodeStates_PerNodePresentMtimeSize_SortedByNodeID(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	pMtime := t0.Add(-time.Hour)
	aMtime := t0.Add(2 * time.Hour)
	// Register out of node-id order to prove the result is sorted.
	h.addNode("primary", "p.srm", []byte("PRIMARY"), pMtime, true)
	h.addNode("alpha", "a.srm", []byte("ALPHALONG"), aMtime, true)
	h.addNode("zeta", "z.srm", nil, t0, false) // absent

	states, err := h.engine.NodeStates(ctx(), syncID)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"alpha", "primary", "zeta"}
	if len(states) != len(wantIDs) {
		t.Fatalf("got %d states want %d: %+v", len(states), len(wantIDs), states)
	}
	for i, want := range wantIDs {
		if states[i].NodeID != want {
			t.Fatalf("states[%d].NodeID = %q want %q (sorted)", i, states[i].NodeID, want)
		}
	}
	byNode := map[string]engine.NodeState{}
	for _, s := range states {
		byNode[s.NodeID] = s
	}
	if s := byNode["alpha"]; !s.Present || !s.Mtime.Equal(aMtime) || s.Size != int64(len("ALPHALONG")) {
		t.Fatalf("alpha = %+v want present @%v size %d", s, aMtime, len("ALPHALONG"))
	}
	if s := byNode["primary"]; !s.Present || !s.Mtime.Equal(pMtime) || s.Size != int64(len("PRIMARY")) {
		t.Fatalf("primary = %+v want present @%v size %d", s, pMtime, len("PRIMARY"))
	}
	// Absent node: Present=false, zero mtime/size.
	if s := byNode["zeta"]; s.Present || !s.Mtime.IsZero() || s.Size != 0 {
		t.Fatalf("zeta = %+v want absent (Present=false, zero mtime/size)", s)
	}
}

func TestNodeStates_StatError_IsReturned(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", []byte("PRIMARY"), t0, true)
	boom := errors.New("sftp: host down")
	h.fake("primary").FailStat("p.srm", boom)

	if _, err := h.engine.NodeStates(ctx(), syncID); !errors.Is(err, boom) {
		t.Fatalf("a non-ErrNotExist Stat error must be returned, got %v", err)
	}
}

// --- finalPass conflict message distinguishable in sync_log --------------

func TestDeactivate_ConflictMessage_MarksFinalPass(t *testing.T) {
	h, _ := activateClean(t)
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE"), t0.Add(2*time.Hour))

	if err := h.engine.Deactivate(ctx(), syncID); err != nil {
		t.Fatal(err)
	}
	entries, err := h.store.ListLogBySync(ctx(), syncID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Outcome == store.OutcomeConflict {
			found = true
			if !strings.Contains(e.Message, "final") {
				t.Fatalf("deactivation conflict message should mark the final pass, got %q", e.Message)
			}
		}
	}
	if !found {
		t.Fatal("expected a conflict log row from the deactivation final pass")
	}
}

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
