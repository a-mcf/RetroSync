package engine_test

// Slice-26 hardening tests:
//
//  1. Data integrity: capture and fan-out must hash the bytes they ACTUALLY
//     read, in-process, never trust a separate adapter Hash call — on a live
//     Syncthing share the file can change between a Read and a Hash, and a
//     mismatched (hash, data) pair permanently poisons the content-addressed
//     save_blobs store (ON CONFLICT (hash) DO NOTHING keeps the first blob under
//     a hash forever) and breaks RestoreVersion's manifest hash. lyingHashReach
//     models that window deterministically: its Hash returns a fixed wrong value
//     while Read serves the true bytes.
//
//  2. Serialization: the daemon's Poll sweep and web-triggered
//     ResolveConflict/RestoreVersion share one Engine; a per-sync mutex must
//     make concurrent operations on the same sync run one-after-another so two
//     racing resolves can never both fan out.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/engine"
	"github.com/a-mcf/retrosync/internal/reach"
)

// lyingHashReach wraps a Reach but returns a fixed bogus value from Hash while
// every other method (Read, Stat, WriteAtomic, ...) passes through. It models a
// file whose content changes between a filesystem Hash call and the Read that
// the engine acts on: any engine path that records/stores the adapter's hash
// instead of hashing the bytes it read will record the lie.
type lyingHashReach struct {
	reach.Reach
	lie string
}

func (l lyingHashReach) Hash(context.Context, string) (string, error) { return l.lie, nil }

// TestResolveConflict_Capture_HashesBytesRead_NotAdapterHash is the poisoning
// regression: the loser's adapter serves bytes B but reports a WRONG hash. The
// capture-before-overwrite must store B under sha256(B) — computed in-process
// from the bytes actually read — so the content-addressed store is never
// poisoned with a blob filed under a hash that is not its own.
func TestResolveConflict_Capture_HashesBytesRead_NotAdapterHash(t *testing.T) {
	h, _, _ := seedConflicted(t) // loser "peer" holds "PEER-WROTE"
	h.overrideReach("peer", lyingHashReach{Reach: h.fake("peer"), lie: "0000-not-the-real-hash"})

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	vs := h.versions("peer")
	if len(vs) != 1 {
		t.Fatalf("loser captured versions = %d, want 1", len(vs))
	}
	wantHash := sha256Hex([]byte("PEER-WROTE"))
	if vs[0].Hash != wantHash {
		t.Fatalf("captured version hash = %q, want sha256(bytes read) %q — the adapter's Hash must never be trusted for a capture", vs[0].Hash, wantHash)
	}
	if got := string(h.versionData(vs[0].Seq)); got != "PEER-WROTE" {
		t.Fatalf("captured bytes = %q, want PEER-WROTE", got)
	}
}

// TestPoll_FanOut_RecordsHashOfBytesRead_NotStaleHash asserts the propagate
// path records sha256(bytes actually read from the source) in every written
// member's manifest — and in the source's own manifest — even when the tier-2
// Hash the source's adapter reported is stale/wrong (the file moved between the
// Hash and the fan-out Read).
func TestPoll_FanOut_RecordsHashOfBytesRead_NotStaleHash(t *testing.T) {
	h, _ := seedSynced(t)
	newMtime := t0.Add(time.Hour)
	h.fake("primary").Mutate("p.srm", []byte("V2-REAL"), newMtime)
	// The source's adapter reports a stale hash; Read serves the real bytes.
	h.overrideReach("primary", lyingHashReach{Reach: h.fake("primary"), lie: "stale-stale-stale"})

	if err := h.engine.Poll(ctx(), syncID); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// The peer received the REAL bytes.
	h.assertFile("peer", []byte("V2-REAL"), newMtime)
	// Both manifests carry sha256 of the bytes actually propagated, not the lie.
	wantHash := sha256Hex([]byte("V2-REAL"))
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.SHA256 == nil || *m.SHA256 != wantHash {
			t.Fatalf("%s manifest hash = %v, want sha256(bytes actually read) %q — a pre-computed stale hash must not reach the manifest", id, m.SHA256, wantHash)
		}
	}
}

// TestResolveConflict_RecordsHashOfBytesFannedOut asserts the resolve path no
// longer performs (or trusts) a separate winner Hash call: even with a lying
// winner adapter, the winner's and every loser's manifest record sha256 of the
// winner bytes actually read and fanned out.
func TestResolveConflict_RecordsHashOfBytesFannedOut(t *testing.T) {
	h, _, _ := seedConflicted(t) // winner "primary" holds "PRIMARY"
	h.overrideReach("primary", lyingHashReach{Reach: h.fake("primary"), lie: "bogus-winner-hash"})

	if err := h.engine.ResolveConflict(ctx(), syncID, "primary"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantHash := sha256Hex([]byte("PRIMARY"))
	for _, id := range []string{"primary", "peer"} {
		m := h.manifest(id)
		if m.SHA256 == nil || *m.SHA256 != wantHash {
			t.Fatalf("%s manifest hash = %v, want sha256(fanned-out bytes) %q", id, m.SHA256, wantHash)
		}
	}
}

// TestResolveConflict_Concurrent_DifferentWinners_OneLoses asserts the per-sync
// serialization: two concurrent ResolveConflict calls with DIFFERENT winners
// cannot both fan out. The per-sync lock runs them one-after-another; the first
// clears the conflict, so the second must fail ErrNotConflicted — and the final
// state is consistent: every member holds the single successful winner's bytes
// and the manifests match them.
func TestResolveConflict_Concurrent_DifferentWinners_OneLoses(t *testing.T) {
	h, _, _ := seedConflicted(t) // primary holds "PRIMARY", peer holds "PEER-WROTE"

	winners := []string{"primary", "peer"}
	errs := make([]error, len(winners))
	var wg sync.WaitGroup
	for i, w := range winners {
		wg.Add(1)
		go func(i int, w string) {
			defer wg.Done()
			errs[i] = h.engine.ResolveConflict(ctx(), syncID, w)
		}(i, w)
	}
	wg.Wait()

	// Exactly one resolve wins; the other must lose the race ENTIRELY with
	// ErrNotConflicted (it reloaded the sync under the lock after the winner
	// cleared the conflict). Both succeeding would mean interleaved fan-outs.
	won := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if won != -1 {
				t.Fatalf("both concurrent resolves succeeded (winners %v): fan-outs interleaved", winners)
			}
			won = i
		case !errors.Is(err, engine.ErrNotConflicted):
			t.Fatalf("losing resolve (winner %q) error = %v, want ErrNotConflicted", winners[i], err)
		}
	}
	if won == -1 {
		t.Fatalf("neither concurrent resolve succeeded: %v", errs)
	}

	// Final state is consistent: every member holds the single winner's bytes and
	// the manifests carry that content's hash; the conflict is cleared.
	wantBytes := []byte("PRIMARY")
	if winners[won] == "peer" {
		wantBytes = []byte("PEER-WROTE")
	}
	wantHash := sha256Hex(wantBytes)
	for _, id := range []string{"primary", "peer"} {
		h.assertFileContent(id, wantBytes)
		m := h.manifest(id)
		if m.SHA256 == nil || *m.SHA256 != wantHash {
			t.Fatalf("%s manifest hash = %v, want %q (the single winner's)", id, m.SHA256, wantHash)
		}
	}
	if h.sync().ConflictAt != nil {
		t.Fatalf("conflict_at should be cleared after the winning resolve")
	}
	if h.sync().LastSynced == nil {
		t.Fatalf("last_synced should be set after the winning resolve")
	}
}
