package localfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

	if _, err := l.WriteAtomic(ctx, "saves/game.srm", data, mtime); err != nil {
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

// TestWriteAtomic_PublishesExactlyTheRequestedMtime pins that the adapter is
// DUMB about mtimes (slice 38): it stamps precisely what it is handed, whatever
// the destination already carries — equal, newer, older, or absent.
//
// Slice 34 put an mtime-collision rule in here, where it silently applied to
// every write forever; an adapter cannot tell a one-time human-initiated write
// from the ten-thousandth routine mirror. That policy now lives in the engine's
// fan-out, gated on user intent (engine.publishMtime), so this layer must be
// verifiably free of it — including for the case slice 34 bumped (an equal
// mtime), which is why that case is here asserting NO adjustment.
//
// Every case asserts the RETURNED mtime is exactly what ended up on disk: the
// engine records the return value in the manifest, so a return that did not
// match the file would make the manifest describe a file that does not exist.
func TestWriteAtomic_PublishesExactlyTheRequestedMtime(t *testing.T) {
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	tests := []struct {
		name string
		// seedMtime is the pre-existing destination's mtime; zero means "no
		// destination, create fresh".
		seedMtime time.Time
		srcMtime  time.Time
	}{
		{
			name:     "fresh create",
			srcMtime: base,
		},
		{
			name:      "destination exists with an older mtime",
			seedMtime: base,
			srcMtime:  base.Add(time.Hour),
		},
		{
			name:      "destination exists with the SAME mtime (no bump here anymore)",
			seedMtime: base,
			srcMtime:  base,
		},
		{
			name:      "destination exists with a newer mtime (moving backwards is allowed)",
			seedMtime: base,
			srcMtime:  base.Add(-time.Hour),
		},
		{
			name:      "sub-microsecond difference is published verbatim",
			seedMtime: base,
			srcMtime:  base.Add(time.Nanosecond),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, root := newRooted(t)
			ctx := context.Background()
			const rel = "saves/game.srm"
			abs := filepath.Join(root, rel)

			if !tc.seedMtime.IsZero() {
				if _, err := l.WriteAtomic(ctx, rel, []byte("OLD"), tc.seedMtime); err != nil {
					t.Fatalf("seed write: %v", err)
				}
				fi, err := os.Stat(abs)
				if err != nil {
					t.Fatalf("stat seed: %v", err)
				}
				if !fi.ModTime().Equal(tc.seedMtime) {
					t.Fatalf("seed mtime = %v, want %v", fi.ModTime(), tc.seedMtime)
				}
			}

			got, err := l.WriteAtomic(ctx, rel, []byte("NEW"), tc.srcMtime)
			if err != nil {
				t.Fatalf("WriteAtomic: %v", err)
			}
			if !got.Equal(tc.srcMtime) {
				t.Errorf("returned mtime = %v, want the requested %v (the adapter must not adjust it)", got, tc.srcMtime)
			}
			// The returned mtime must be what is actually ON DISK — the engine files
			// its content hash under it.
			fi, err := os.Stat(abs)
			if err != nil {
				t.Fatalf("stat published: %v", err)
			}
			if !fi.ModTime().Equal(tc.srcMtime) {
				t.Errorf("on-disk mtime = %v, want the requested %v", fi.ModTime(), tc.srcMtime)
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				t.Fatalf("read published: %v", err)
			}
			if string(data) != "NEW" {
				t.Errorf("published content = %q, want NEW", data)
			}
		})
	}
}

func TestWriteAtomic_MkdirAllNestedParent(t *testing.T) {
	l, root := newRooted(t)
	ctx := context.Background()
	if _, err := l.WriteAtomic(ctx, "a/b/c/deep.srm", []byte("x"), time.Unix(100, 0)); err != nil {
		t.Fatalf("WriteAtomic into missing nested parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a/b/c/deep.srm")); err != nil {
		t.Fatalf("expected file created: %v", err)
	}
}

// TestWriteAtomic_ParentPreexistingAndCreated pins the two parent situations
// the durable-write path must handle: the destination's parent directory
// already exists, and it (including nested components) must be created. Both
// must succeed, and the written bytes must read back.
func TestWriteAtomic_ParentPreexistingAndCreated(t *testing.T) {
	cases := []struct {
		name string
		prep func(t *testing.T, root string)
		rel  string
	}{
		{
			name: "parent pre-exists",
			prep: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "have"), 0o755); err != nil {
					t.Fatalf("prep mkdir: %v", err)
				}
			},
			rel: "have/game.srm",
		},
		{
			name: "parent created",
			prep: func(t *testing.T, root string) {},
			rel:  "made/deep/game.srm",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, root := newRooted(t)
			tc.prep(t, root)
			ctx := context.Background()
			data := []byte("payload-" + tc.name)
			if _, err := l.WriteAtomic(ctx, tc.rel, data, time.Unix(42, 0)); err != nil {
				t.Fatalf("WriteAtomic(%q): %v", tc.rel, err)
			}
			got, err := l.Read(ctx, tc.rel)
			if err != nil {
				t.Fatalf("Read(%q): %v", tc.rel, err)
			}
			if string(got) != string(data) {
				t.Fatalf("Read = %q, want %q", got, data)
			}
		})
	}
}

// TestWriteAtomic_ReadOnlyParentFails asserts a write into an unwritable parent
// directory surfaces an error rather than silently succeeding. Skipped as root,
// where permission bits do not block writes (CAP_DAC_OVERRIDE) — e.g. the
// containerized `make test` run.
func TestWriteAtomic_ReadOnlyParentFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not block writes")
	}
	l, root := newRooted(t)
	dir := filepath.Join(root, "ro")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Restore perms so t.TempDir cleanup can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, err := l.WriteAtomic(context.Background(), "ro/game.srm", []byte("x"), time.Unix(1, 0))
	if err == nil {
		t.Fatal("WriteAtomic into read-only parent = nil err, want failure")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "game.srm")); !os.IsNotExist(statErr) {
		t.Fatalf("destination unexpectedly present after failed write: %v", statErr)
	}
}

// TestWriteAtomic_PublishesSaveFileMode: a published save must be 0644, not the
// 0600 os.CreateTemp hands out (os.Rename carries the temp file's mode to the
// destination). The REPLACE case is the regression that matters: a save already
// sitting at 0600 — every file written before this fix — must come back 0644,
// so the fix self-heals rather than preserving the damaged mode. A 0600 save can
// be unreadable to an emulator or syncthing daemon running as another user, and
// syncthing silently stops noticing files it cannot read.
func TestWriteAtomic_PublishesSaveFileMode(t *testing.T) {
	l, root := newRooted(t)
	ctx := context.Background()

	tests := []struct {
		name string
		// seedMode is applied to a pre-existing destination; 0 means "no
		// destination, create fresh".
		seedMode os.FileMode
	}{
		{name: "fresh create", seedMode: 0},
		{name: "replaces 0600 left by an older write", seedMode: 0o600},
		{name: "replaces 0644 written by the emulator", seedMode: 0o644},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rel := "dir/" + strings.ReplaceAll(tc.name, " ", "_") + ".srm"
			abs := filepath.Join(root, rel)
			if tc.seedMode != 0 {
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(abs, []byte("OLD"), tc.seedMode); err != nil {
					t.Fatalf("seed: %v", err)
				}
				// os.WriteFile applies umask; force the exact mode we are testing.
				if err := os.Chmod(abs, tc.seedMode); err != nil {
					t.Fatalf("seed chmod: %v", err)
				}
			}

			if _, err := l.WriteAtomic(ctx, rel, []byte("NEW"), time.Unix(20, 0)); err != nil {
				t.Fatalf("WriteAtomic: %v", err)
			}

			fi, err := os.Stat(abs)
			if err != nil {
				t.Fatalf("stat published file: %v", err)
			}
			if got := fi.Mode().Perm(); got != 0o644 {
				t.Errorf("published mode = %04o, want 0644", got)
			}
			got, err := os.ReadFile(abs)
			if err != nil {
				t.Fatalf("read published file: %v", err)
			}
			if string(got) != "NEW" {
				t.Errorf("published content = %q, want %q", got, "NEW")
			}
		})
	}
}

func TestWriteAtomic_NoLeftoverTempOnSuccess(t *testing.T) {
	l, root := newRooted(t)
	ctx := context.Background()
	if _, err := l.WriteAtomic(ctx, "dir/game.srm", []byte("data"), time.Unix(1, 0)); err != nil {
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
	if _, err := l.WriteAtomic(ctx, "dir/game.srm", []byte("ORIGINAL"), time.Unix(10, 0)); err != nil {
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

	_, err := l.WriteAtomic(ctx, "dir/game.srm", []byte("NEW DATA"), time.Unix(20, 0))
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
	if _, err := l.WriteAtomic(ctx, "saves/game.srm", data, time.Unix(10, 0)); err != nil {
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
		if _, err := l.WriteAtomic(ctx, bad, []byte("x"), time.Unix(1, 0)); err == nil {
			t.Fatalf("WriteAtomic(%q) = nil err, want rejection", bad)
		}
	}
}

// TestWriteAtomic_RejectionCreatesNothing confirms a rejected write does not
// MkdirAll or create any temp file (the safepath check runs first).
func TestWriteAtomic_RejectionCreatesNothing(t *testing.T) {
	l, root := newRooted(t)
	if _, err := l.WriteAtomic(context.Background(), "../evil/x", []byte("x"), time.Unix(1, 0)); err == nil {
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

// TestList_TruncatesAtCap verifies the memory-DoS bound: a directory seeded with
// more entries than maxListEntries returns EXACTLY the cap (not more) and does not
// error. The cap is lowered via the package-var seam so we don't have to seed tens
// of thousands of files.
func TestList_TruncatesAtCap(t *testing.T) {
	orig := maxListEntries
	maxListEntries = 5
	t.Cleanup(func() { maxListEntries = orig })

	l, root := newRooted(t)
	// Seed well over the cap so the streamed read must stop early.
	const seeded = 5 * 1024 // > listReadBatch, so the cap is hit mid-batch
	for i := 0; i < seeded; i++ {
		name := filepath.Join(root, fmtName(i))
		if err := os.WriteFile(name, []byte{0}, 0o644); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	entries, err := l.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List over-cap dir: %v", err)
	}
	if len(entries) != maxListEntries {
		t.Fatalf("List len = %d, want exactly cap %d", len(entries), maxListEntries)
	}
}

// TestList_SmallDirUnchanged confirms a normal (under-cap) directory still returns
// its full listing with the cap in force.
func TestList_SmallDirUnchanged(t *testing.T) {
	l, root := newRooted(t)
	seedTree(t, root)
	entries, err := l.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List small dir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("List small dir len = %d, want 3 (full listing): %+v", len(entries), entries)
	}
}

// fmtName produces a stable, unique filename for the cap test's seed loop.
func fmtName(i int) string {
	return "save-" + strconv.Itoa(i) + ".srm"
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
