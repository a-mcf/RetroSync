package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/store"
)

func TestResolveReach_SyncthingShare(t *testing.T) {
	root := t.TempDir()
	node := store.Node{
		ID:          "bob-deck",
		Reach:       store.ReachSyncthingShare,
		ReachConfig: store.ReachConfig{Path: root},
	}
	r, err := ResolveReach(node)
	if err != nil {
		t.Fatalf("ResolveReach: %v", err)
	}
	if r == nil {
		t.Fatal("ResolveReach returned nil Reach")
	}

	// It is a working localfs rooted at reach_config.path: a round-trip lands the
	// file under root.
	ctx := context.Background()
	mtime := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	if _, err := r.WriteAtomic(ctx, "saves/g.srm", []byte("payload"), mtime); err != nil {
		t.Fatalf("WriteAtomic via resolved reach: %v", err)
	}
	fm, err := r.Stat(ctx, "saves/g.srm")
	if err != nil {
		t.Fatalf("Stat via resolved reach: %v", err)
	}
	if fm.Size != int64(len("payload")) || !fm.Mtime.Equal(mtime) {
		t.Fatalf("resolved reach round-trip mismatch: size=%d mtime=%v", fm.Size, fm.Mtime)
	}
}

func TestResolveReach_SyncthingShare_EmptyPath(t *testing.T) {
	node := store.Node{ID: "x", Reach: store.ReachSyncthingShare}
	if _, err := ResolveReach(node); err == nil {
		t.Fatal("empty reach_config.path = nil err, want error")
	}
}

// TestResolveReach_Unknown pins the contract that replaced the ssh case (0010).
// An unrecognized reach is a data fault — the CHECK constraint and
// store.ValidReach both prevent one being written — but it reports with the
// ErrUnsupportedReach sentinel rather than a bare error, because bulk callers
// (discovery, the poll loop) match on it to skip that ONE node and keep going.
// A bare error there would fail the whole pass over one bad row.
func TestResolveReach_Unknown(t *testing.T) {
	node := store.Node{ID: "weird", Reach: store.Reach("carrier-pigeon")}
	_, err := ResolveReach(node)
	if err == nil {
		t.Fatal("unknown reach = nil err, want error")
	}
	if !errors.Is(err, reach.ErrUnsupportedReach) {
		t.Fatalf("unknown reach err = %v, want ErrUnsupportedReach so bulk callers skip it", err)
	}
	// The offending value belongs in the message; an operator has to be able to
	// tell WHICH node and which value from the log alone.
	if !strings.Contains(err.Error(), "carrier-pigeon") || !strings.Contains(err.Error(), "weird") {
		t.Errorf("error should name the node and the bad value, got: %v", err)
	}
}
