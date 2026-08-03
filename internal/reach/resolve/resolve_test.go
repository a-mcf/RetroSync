package resolve

import (
	"context"
	"errors"
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

func TestResolveReach_SSH_Unsupported(t *testing.T) {
	node := store.Node{
		ID:          "mister",
		Reach:       store.ReachSSH,
		ReachConfig: store.ReachConfig{Host: "10.0.0.1", User: "root", SecretRef: "mister-1"},
	}
	_, err := ResolveReach(node)
	if !errors.Is(err, reach.ErrUnsupportedReach) {
		t.Fatalf("ssh node err = %v, want ErrUnsupportedReach", err)
	}
}

func TestResolveReach_Unknown(t *testing.T) {
	node := store.Node{ID: "weird", Reach: store.Reach("carrier-pigeon")}
	_, err := ResolveReach(node)
	if err == nil {
		t.Fatal("unknown reach = nil err, want error")
	}
	if errors.Is(err, reach.ErrUnsupportedReach) {
		t.Fatal("unknown reach should not be ErrUnsupportedReach (that is for ssh)")
	}
}
