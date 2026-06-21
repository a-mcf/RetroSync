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

const gameID = "super-metroid"

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
	return h
}

// addNode registers a node + its game path + a backing fake. If present, seeds
// the node's file with content at mtime.
func (h *harness) addNode(id, path string, content []byte, mtime time.Time, present bool) {
	h.t.Helper()
	ctx := context.Background()
	if err := h.store.CreateNode(ctx, store.Node{ID: id, Display: id, Kind: store.KindGeneric, Reach: store.ReachSyncthingShare}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.SetGamePath(ctx, store.GamePath{GameID: gameID, NodeID: id, Path: path}); err != nil {
		h.t.Fatal(err)
	}
	f := fakereach.New()
	if present {
		f.Put(path, content, mtime)
	}
	h.fakes[id] = f
	h.paths[id] = path
}

func (h *harness) fake(id string) *fakereach.Fake { return h.fakes[id] }

func (h *harness) binding() store.ActiveBinding {
	h.t.Helper()
	b, err := h.store.GetBinding(context.Background(), gameID)
	if err != nil {
		h.t.Fatalf("get binding: %v", err)
	}
	return b
}

func (h *harness) manifest(nodeID string) store.ManifestEntry {
	h.t.Helper()
	m, err := h.store.GetManifest(context.Background(), gameID, nodeID)
	if err != nil {
		h.t.Fatalf("get manifest %s: %v", nodeID, err)
	}
	return m
}

func (h *harness) logCount(outcome store.Outcome) int {
	h.t.Helper()
	entries, err := h.store.ListLogByGame(context.Background(), gameID, 0)
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

	err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", false)
	if !errors.Is(err, engine.ErrNoSave) {
		t.Fatalf("want ErrNoSave, got %v", err)
	}
	// No binding should have been created.
	if _, err := h.store.GetBinding(ctx(), gameID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding should not exist: %v", err)
	}
}

func TestActivate_SingleSource_AutoPickAndFanOut(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("SAVE"), srcMtime, true)
	h.addNode("peer1", "q.srm", nil, t0, false)
	h.addNode("peer2", "r.srm", nil, t0, false)

	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", false); err != nil {
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

	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", false); err != nil {
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

	if err := h.engine.Activate(ctx(), gameID, "primary", "from-peer-peer", "all-configured", false); err != nil {
		t.Fatal(err)
	}
	// from-peer-peer => peer is the source; primary is overwritten with PEER.
	h.assertFile("primary", []byte("PEER"), peerMtime)
}

func TestActivate_FromPeer_MissingSource_Errors(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", []byte("PRIMARY"), t0, true)
	h.addNode("peer", "q.srm", nil, t0, false) // peer has no file

	err := h.engine.Activate(ctx(), gameID, "primary", "from-peer-peer", "all-configured", false)
	if !errors.Is(err, engine.ErrSourceMissing) {
		t.Fatalf("want ErrSourceMissing, got %v", err)
	}
}

func TestActivate_AlreadyActive_Rejected(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", []byte("SAVE"), t0, true)
	h.addNode("peer", "q.srm", nil, t0, false)

	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", false); err != nil {
		t.Fatal(err)
	}
	err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", false)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want store.ErrConflict, got %v", err)
	}
}

func TestActivate_ForceTakeover_ReplacesBinding(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("OLD"), srcMtime, true)
	h.addNode("other", "q.srm", []byte("MINE"), srcMtime, true)

	// First session: "primary" holds the binding, sourced from itself, scoped to
	// itself so "other" keeps its own MINE save (we take over from it next).
	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "primary", false); err != nil {
		t.Fatal(err)
	}
	if b := h.binding(); b.PrimaryNode != "primary" {
		t.Fatalf("first binding primary = %q, want primary", b.PrimaryNode)
	}

	// Without force, taking over fails.
	if err := h.engine.Activate(ctx(), gameID, "other", "from-peer-other", "all-configured", false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("takeover without force: want ErrConflict, got %v", err)
	}

	// With force, the existing binding is replaced (raw delete + create), and
	// the new source ("other") is fanned out — so "primary" now holds MINE.
	if err := h.engine.Activate(ctx(), gameID, "other", "from-peer-other", "all-configured", true); err != nil {
		t.Fatalf("force takeover: %v", err)
	}
	b := h.binding()
	if b.PrimaryNode != "other" {
		t.Fatalf("after force takeover primary = %q, want other", b.PrimaryNode)
	}
	if b.Direction != "from-peer-other" {
		t.Fatalf("after force takeover direction = %q, want from-peer-other", b.Direction)
	}
	h.assertFile("primary", []byte("MINE"), srcMtime)
}

func TestActivate_Force_NonViableSource_LeavesExistingBinding(t *testing.T) {
	// A force-takeover whose chosen source has NO file must return its error
	// (ErrSourceMissing) WITHOUT having deleted the pre-existing binding. The
	// displaced session must not be lost for a takeover that cannot proceed.
	h := newHarness(t, steppingClock(t0, time.Second))
	srcMtime := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("OLD"), srcMtime, true)
	h.addNode("other", "q.srm", nil, srcMtime, false /* no file */)

	// First session: "primary" holds the binding, scoped to itself so the
	// fan-out does not give "other" a file (we need "other" to stay empty so the
	// takeover below has a non-viable source).
	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "primary", false); err != nil {
		t.Fatal(err)
	}
	before := h.binding()
	if before.PrimaryNode != "primary" {
		t.Fatalf("setup binding primary = %q, want primary", before.PrimaryNode)
	}

	// Force takeover sourced from "other", which has no file → ErrSourceMissing.
	err := h.engine.Activate(ctx(), gameID, "other", "from-peer-other", "all-configured", true)
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
	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", true); err != nil {
		t.Fatalf("force activate on idle: %v", err)
	}
	if b := h.binding(); b.PrimaryNode != "primary" {
		t.Fatalf("binding primary = %q, want primary", b.PrimaryNode)
	}
}

func TestActivate_BadDirection_Rejected(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	h.addNode("primary", "p.srm", []byte("SAVE"), t0, true)
	err := h.engine.Activate(ctx(), gameID, "primary", "sideways", "all-configured", false)
	if !errors.Is(err, store.ErrInvalidValue) {
		t.Fatalf("want ErrInvalidValue, got %v", err)
	}
}

func TestActivate_PeerScope_CSV(t *testing.T) {
	h := newHarness(t, steppingClock(t0, time.Second))
	src := t0.Add(-time.Hour)
	h.addNode("primary", "p.srm", []byte("SAVE"), src, true)
	h.addNode("in", "q.srm", nil, t0, false)
	h.addNode("out", "r.srm", nil, t0, false)

	// Scope excludes "out".
	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "primary,in", false); err != nil {
		t.Fatal(err)
	}
	h.assertFile("in", []byte("SAVE"), src)
	// "out" was out of scope: nothing written.
	if _, err := h.fake("out").Stat(ctx(), "r.srm"); !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("out-of-scope node should be untouched, got %v", err)
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
	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", false); err != nil {
		t.Fatal(err)
	}
	return h, srcMtime
}

func TestPoll_Noop(t *testing.T) {
	h, _ := activateClean(t)
	okBefore := h.logCount(store.OutcomeOK)
	if err := h.engine.Poll(ctx(), gameID); err != nil {
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

	if err := h.engine.Poll(ctx(), gameID); err != nil {
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

	if err := h.engine.Poll(ctx(), gameID); err != nil {
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

	if err := h.engine.Poll(ctx(), gameID); err != nil {
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
	if err := h.engine.Poll(ctx(), gameID); err != nil {
		t.Fatal(err)
	}
	conflictAt := h.binding().ConflictAt
	if conflictAt == nil {
		t.Fatal("setup: expected conflict")
	}
	// Now the primary changes; a subsequent poll must do nothing (paused).
	h.fake("primary").Mutate("p.srm", []byte("V99"), t0.Add(3*time.Hour))
	okBefore := h.logCount(store.OutcomeOK)
	if err := h.engine.Poll(ctx(), gameID); err != nil {
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

	if err := h.engine.Deactivate(ctx(), gameID); err != nil {
		t.Fatal(err)
	}
	h.assertFile("peer", []byte("FINAL"), newMtime)
	if _, err := h.store.GetBinding(ctx(), gameID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding should be deleted, got %v", err)
	}
}

func TestDeactivate_NoChange_Deletes(t *testing.T) {
	h, _ := activateClean(t)
	if err := h.engine.Deactivate(ctx(), gameID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.GetBinding(ctx(), gameID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding should be deleted, got %v", err)
	}
}

func TestDeactivate_ConflictingFinalPass_LeavesActive(t *testing.T) {
	h, _ := activateClean(t)
	// A peer mutated out of band: the final pass would conflict.
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE"), t0.Add(2*time.Hour))

	if err := h.engine.Deactivate(ctx(), gameID); err != nil {
		t.Fatal(err)
	}
	b, err := h.store.GetBinding(ctx(), gameID)
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
	if err := h.engine.Deactivate(ctx(), gameID); err != nil {
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

	if err := h.engine.Poll(ctx(), gameID); !errors.Is(err, boom) {
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
	if err := h.engine.Poll(ctx(), gameID); err != nil {
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

	err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", false)
	if !errors.Is(err, boom) {
		t.Fatalf("want fan-out write error, got %v", err)
	}
	// The binding must have been rolled back: the game is idle again, not stuck
	// half-active with a nil last_synced.
	if _, err := h.store.GetBinding(ctx(), gameID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("binding should have been rolled back to idle, got %v", err)
	}

	// Clear the injected failure and retry: a clean activation must now succeed
	// (no lingering ErrConflict from the half-active row).
	h.fake("peer").FailWriteAtomic("q.srm", nil)
	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", false); err != nil {
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
	if err := h.engine.Activate(ctx(), gameID, "primary", "from-primary", "all-configured", false); err != nil {
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

	if err := h.engine.Poll(ctx(), gameID); err != nil {
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

	if err := h.engine.Poll(ctx(), gameID); err != nil {
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

	if err := h.engine.Poll(ctx(), gameID); !errors.Is(err, boom) {
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
	if err := h.engine.Poll(ctx(), gameID); err != nil {
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

	if err := h.engine.Poll(ctx(), gameID); err != nil {
		t.Fatal(err)
	}
	if h.binding().ConflictAt == nil {
		t.Fatal("a vanished primary must flag a conflict, not delete peers")
	}
	// Peers keep their saves: no deletion was propagated.
	h.assertFile("peerA", []byte("V1"), srcMtime)
	h.assertFile("peerB", []byte("V1"), srcMtime)
}

// --- finalPass conflict message distinguishable in sync_log --------------

func TestDeactivate_ConflictMessage_MarksFinalPass(t *testing.T) {
	h, _ := activateClean(t)
	h.fake("peer").Mutate("q.srm", []byte("PEER-WROTE"), t0.Add(2*time.Hour))

	if err := h.engine.Deactivate(ctx(), gameID); err != nil {
		t.Fatal(err)
	}
	entries, err := h.store.ListLogByGame(ctx(), gameID, 0)
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
