package localfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/reach"
)

func newRooted(t *testing.T) (*LocalFS, string) {
	t.Helper()
	root := t.TempDir()
	l, err := New(root)
	if err != nil {
		t.Fatalf("New(%q): %v", root, err)
	}
	return l, root
}

func TestNew_RequiresAbsolute(t *testing.T) {
	if _, err := New("relative/root"); err == nil {
		t.Fatal("New with relative root = nil err, want error")
	}
}

func TestWriteReadStat_RoundTrip(t *testing.T) {
	l, _ := newRooted(t)
	ctx := context.Background()
	data := []byte("hello save")
	mtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	if err := l.WriteAtomic(ctx, "saves/game.srm", data, mtime); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := l.Read(ctx, "saves/game.srm")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Read = %q, want %q", got, data)
	}

	fm, err := l.Stat(ctx, "saves/game.srm")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fm.Size != int64(len(data)) {
		t.Fatalf("Stat size = %d, want %d", fm.Size, len(data))
	}
	// mtime fidelity: the written mtime comes back from Stat.
	if !fm.Mtime.Equal(mtime) {
		t.Fatalf("Stat mtime = %v, want %v", fm.Mtime, mtime)
	}
}

func TestWriteAtomic_MkdirAllNestedParent(t *testing.T) {
	l, root := newRooted(t)
	ctx := context.Background()
	if err := l.WriteAtomic(ctx, "a/b/c/deep.srm", []byte("x"), time.Unix(100, 0)); err != nil {
		t.Fatalf("WriteAtomic into missing nested parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a/b/c/deep.srm")); err != nil {
		t.Fatalf("expected file created: %v", err)
	}
}

func TestWriteAtomic_NoLeftoverTempOnSuccess(t *testing.T) {
	l, root := newRooted(t)
	ctx := context.Background()
	if err := l.WriteAtomic(ctx, "dir/game.srm", []byte("data"), time.Unix(1, 0)); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	assertNoTempLeftover(t, filepath.Join(root, "dir"))
}

// TestWriteAtomic_FailureLeavesDestinationIntact forces the rename step to fail
// by making the destination a directory (you cannot rename a file over a
// non-empty directory). It asserts the prior destination content survives and no
// temp file is left behind.
func TestWriteAtomic_FailureLeavesDestinationIntactAndNoTemp(t *testing.T) {
	l, root := newRooted(t)
	ctx := context.Background()

	// Seed a good prior file via a normal write.
	if err := l.WriteAtomic(ctx, "dir/game.srm", []byte("ORIGINAL"), time.Unix(10, 0)); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// Now replace the destination file with a NON-EMPTY directory of the same
	// name so os.Rename(tmp, dest) fails (rename onto a non-empty dir errors).
	dest := filepath.Join(root, "dir/game.srm")
	if err := os.Remove(dest); err != nil {
		t.Fatalf("remove seed file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "child"), 0o755); err != nil {
		t.Fatalf("make dest dir: %v", err)
	}
	// Put a file inside the dir so it is non-empty and rename is guaranteed to
	// fail across platforms.
	if err := os.WriteFile(filepath.Join(dest, "child", "f"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("populate dest dir: %v", err)
	}

	err := l.WriteAtomic(ctx, "dir/game.srm", []byte("NEW DATA"), time.Unix(20, 0))
	if err == nil {
		t.Fatal("WriteAtomic over non-empty dir = nil err, want failure")
	}

	// The destination directory (the "prior" thing) must be untouched.
	if _, statErr := os.Stat(filepath.Join(dest, "child", "f")); statErr != nil {
		t.Fatalf("destination clobbered on failed write: %v", statErr)
	}
	// No temp file left behind in the directory.
	assertNoTempLeftover(t, filepath.Join(root, "dir"))
}

func TestStat_NotExist(t *testing.T) {
	l, _ := newRooted(t)
	_, err := l.Stat(context.Background(), "nope.srm")
	if !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("Stat missing = %v, want ErrNotExist", err)
	}
}

func TestRead_NotExist(t *testing.T) {
	l, _ := newRooted(t)
	_, err := l.Read(context.Background(), "nope.srm")
	if !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("Read missing = %v, want ErrNotExist", err)
	}
}

func TestHash_HexSha256OfFileBytes(t *testing.T) {
	l, _ := newRooted(t)
	ctx := context.Background()
	data := []byte("hello save bytes")
	if err := l.WriteAtomic(ctx, "saves/game.srm", data, time.Unix(10, 0)); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	got, err := l.Hash(ctx, "saves/game.srm")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("Hash = %q, want %q", got, want)
	}
}

func TestHash_NotExist(t *testing.T) {
	l, _ := newRooted(t)
	_, err := l.Hash(context.Background(), "nope.srm")
	if !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("Hash missing = %v, want ErrNotExist", err)
	}
}

func TestHash_TraversalRejected(t *testing.T) {
	l, _ := newRooted(t)
	ctx := context.Background()
	for _, bad := range []string{"../escape", "/etc/passwd", "a/../../b"} {
		if _, err := l.Hash(ctx, bad); err == nil {
			t.Fatalf("Hash(%q) = nil err, want rejection", bad)
		}
	}
}

func TestTraversalRejected_ReadStatWrite(t *testing.T) {
	l, _ := newRooted(t)
	ctx := context.Background()
	for _, bad := range []string{"../escape", "/etc/passwd", "a/../../b"} {
		if _, err := l.Stat(ctx, bad); err == nil {
			t.Fatalf("Stat(%q) = nil err, want rejection", bad)
		}
		if _, err := l.Read(ctx, bad); err == nil {
			t.Fatalf("Read(%q) = nil err, want rejection", bad)
		}
		if err := l.WriteAtomic(ctx, bad, []byte("x"), time.Unix(1, 0)); err == nil {
			t.Fatalf("WriteAtomic(%q) = nil err, want rejection", bad)
		}
	}
}

// TestWriteAtomic_RejectionCreatesNothing confirms a rejected write does not
// MkdirAll or create any temp file (the safepath check runs first).
func TestWriteAtomic_RejectionCreatesNothing(t *testing.T) {
	l, root := newRooted(t)
	if err := l.WriteAtomic(context.Background(), "../evil/x", []byte("x"), time.Unix(1, 0)); err == nil {
		t.Fatal("expected rejection")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected write created entries in root: %v", entries)
	}
}

// seedTree writes a nested directory tree under root for List tests:
//
//	root/
//	  alpha.srm
//	  beta.srm
//	  saves/
//	    deep/
//	      nested.srm
//	    game.srm
func seedTree(t *testing.T, root string) {
	t.Helper()
	mustWrite := func(rel, data string) {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %q: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(data), 0o600); err != nil {
			t.Fatalf("write %q: %v", rel, err)
		}
	}
	mustWrite("beta.srm", "bb")
	mustWrite("alpha.srm", "a")
	mustWrite("saves/game.srm", "game-bytes")
	mustWrite("saves/deep/nested.srm", "x")
}

func TestList_RootSortedDirsFirst(t *testing.T) {
	l, root := newRooted(t)
	seedTree(t, root)
	ctx := context.Background()

	for _, rel := range []string{"", "."} {
		entries, err := l.List(ctx, rel)
		if err != nil {
			t.Fatalf("List(%q): %v", rel, err)
		}
		// Expect: saves/ (dir) first, then alpha.srm, beta.srm (files, alphabetical).
		if len(entries) != 3 {
			t.Fatalf("List(%q) len = %d, want 3: %+v", rel, len(entries), entries)
		}
		if !entries[0].IsDir || entries[0].Name != "saves" {
			t.Errorf("List(%q)[0] = %+v, want dir saves first", rel, entries[0])
		}
		if entries[1].Name != "alpha.srm" || entries[2].Name != "beta.srm" {
			t.Errorf("List(%q) files = %q,%q, want alpha,beta", rel, entries[1].Name, entries[2].Name)
		}
		// A file entry carries its size; a directory does not require contents.
		var alpha reach.DirEntry
		for _, e := range entries {
			if e.Name == "alpha.srm" {
				alpha = e
			}
		}
		if alpha.IsDir || alpha.Size != 1 {
			t.Errorf("alpha.srm entry = %+v, want file size 1", alpha)
		}
	}
}

func TestList_Subdir(t *testing.T) {
	l, root := newRooted(t)
	seedTree(t, root)
	entries, err := l.List(context.Background(), "saves")
	if err != nil {
		t.Fatalf("List(saves): %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List(saves) len = %d, want 2: %+v", len(entries), entries)
	}
	if !entries[0].IsDir || entries[0].Name != "deep" {
		t.Errorf("List(saves)[0] = %+v, want dir deep", entries[0])
	}
	if entries[1].IsDir || entries[1].Name != "game.srm" {
		t.Errorf("List(saves)[1] = %+v, want file game.srm", entries[1])
	}
}

func TestList_TraversalRejected(t *testing.T) {
	l, _ := newRooted(t)
	ctx := context.Background()
	for _, bad := range []string{"../escape", "/etc", "a/../../b"} {
		if _, err := l.List(ctx, bad); err == nil {
			t.Fatalf("List(%q) = nil err, want rejection", bad)
		}
	}
}

func TestList_NotExist(t *testing.T) {
	l, _ := newRooted(t)
	_, err := l.List(context.Background(), "no-such-dir")
	if !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("List(missing) = %v, want ErrNotExist", err)
	}
}

func TestList_NotADirectory(t *testing.T) {
	l, root := newRooted(t)
	seedTree(t, root)
	_, err := l.List(context.Background(), "alpha.srm")
	if err == nil {
		t.Fatal("List(file) = nil err, want not-a-directory error")
	}
	if errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("List(file) mapped to ErrNotExist; want a distinct not-a-directory error: %v", err)
	}
}

func assertNoTempLeftover(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), tempSuffix) {
			t.Fatalf("leftover temp file %q in %q", e.Name(), dir)
		}
	}
}
