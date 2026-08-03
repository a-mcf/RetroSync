package fakereach_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/reach/fakereach"
)

func ctx() context.Context { return context.Background() }

func TestStatNotExist(t *testing.T) {
	f := fakereach.New()
	_, err := f.Stat(ctx(), "missing.srm")
	if !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestStatReturnsSizeAndMtime(t *testing.T) {
	mt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := fakereach.New().Put("a.srm", []byte("hello"), mt)
	fm, err := f.Stat(ctx(), "a.srm")
	if err != nil {
		t.Fatal(err)
	}
	if fm.Size != 5 || !fm.Mtime.Equal(mt) {
		t.Fatalf("got %+v", fm)
	}
}

func TestReadNotExist(t *testing.T) {
	f := fakereach.New()
	_, err := f.Read(ctx(), "missing")
	if !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestWriteAtomicStoresMtimeFaithfully(t *testing.T) {
	mt := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	f := fakereach.New()
	if _, err := f.WriteAtomic(ctx(), "b.srm", []byte("data"), mt); err != nil {
		t.Fatal(err)
	}
	fm, err := f.Stat(ctx(), "b.srm")
	if err != nil {
		t.Fatal(err)
	}
	if !fm.Mtime.Equal(mt) {
		t.Fatalf("mtime not faithful: got %v want %v", fm.Mtime, mt)
	}
	if fm.Size != 4 {
		t.Fatalf("size got %d", fm.Size)
	}
	writes := f.Writes()
	if len(writes) != 1 || writes[0].Path != "b.srm" {
		t.Fatalf("writes = %+v", writes)
	}
}

// TestWriteAtomic_MtimeCollisionRule pins that the fake models the production
// adapter's issue-#33 rule: a published mtime is never allowed to collide with
// (or predate) the destination's current mtime, because engine tests rely on the
// fake to behave like localfs here. The returned mtime is what was published.
func TestWriteAtomic_MtimeCollisionRule(t *testing.T) {
	base := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		// seed is the destination's existing mtime; zero means no destination.
		seed     time.Time
		request  time.Time
		want     time.Time
		wantSame bool // want == request (no bump)
	}{
		{name: "fresh create keeps the requested mtime", request: base, want: base, wantSame: true},
		{name: "newer request is kept", seed: base, request: base.Add(time.Hour), want: base.Add(time.Hour), wantSame: true},
		{name: "colliding request is bumped", seed: base, request: base, want: base.Add(time.Microsecond)},
		{name: "older request is bumped", seed: base, request: base.Add(-time.Hour), want: base.Add(time.Microsecond)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fakereach.New()
			if !tc.seed.IsZero() {
				f.Put("g.srm", []byte("OLD"), tc.seed)
			}
			got, err := f.WriteAtomic(ctx(), "g.srm", []byte("NEW"), tc.request)
			if err != nil {
				t.Fatalf("WriteAtomic: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("returned mtime = %v, want %v", got, tc.want)
			}
			// The stored file, and the recorded write, both carry the PUBLISHED mtime.
			fm, err := f.Stat(ctx(), "g.srm")
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if !fm.Mtime.Equal(got) {
				t.Errorf("stored mtime = %v, but WriteAtomic returned %v", fm.Mtime, got)
			}
			writes := f.Writes()
			if len(writes) != 1 || !writes[0].Mtime.Equal(got) {
				t.Errorf("write record = %+v, want one record at %v", writes, got)
			}
			if !tc.wantSame && !fm.Mtime.After(tc.seed) {
				t.Errorf("published mtime %v is not strictly newer than the destination's previous %v", fm.Mtime, tc.seed)
			}
		})
	}
}

func TestHash_HexSha256OfStoredBytes(t *testing.T) {
	f := fakereach.New().Put("a.srm", []byte("hello save"), time.Now())
	got, err := f.Hash(ctx(), "a.srm")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	// sha256("hello save") computed independently.
	sum := sha256.Sum256([]byte("hello save"))
	want := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("Hash = %q, want %q", got, want)
	}
}

func TestHash_NotExist(t *testing.T) {
	f := fakereach.New()
	_, err := f.Hash(ctx(), "missing")
	if !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("Hash missing = %v, want ErrNotExist", err)
	}
}

func TestHash_InjectedError(t *testing.T) {
	boom := errors.New("io error")
	f := fakereach.New().Put("a.srm", []byte("x"), time.Now())
	f.FailHash("a.srm", boom)
	if _, err := f.Hash(ctx(), "a.srm"); !errors.Is(err, boom) {
		t.Fatalf("Hash with injected failure = %v, want boom", err)
	}
}

func TestPutDoesNotRecordWrite(t *testing.T) {
	f := fakereach.New().Put("x", []byte("y"), time.Now())
	if len(f.Writes()) != 0 {
		t.Fatalf("Put should not record a write")
	}
}

func TestFailWriteLeavesPriorFile(t *testing.T) {
	mt := time.Unix(100, 0).UTC()
	f := fakereach.New().Put("a", []byte("old"), mt)
	boom := errors.New("disk full")
	f.FailWriteAtomic("a", boom)
	if _, err := f.WriteAtomic(ctx(), "a", []byte("new"), time.Now()); !errors.Is(err, boom) {
		t.Fatalf("want injected error, got %v", err)
	}
	got, _ := f.Get("a")
	if string(got.Data) != "old" {
		t.Fatalf("prior file mutated on failed write: %q", got.Data)
	}
	if len(f.Writes()) != 0 {
		t.Fatalf("failed write should not be recorded")
	}
}

func TestDataIsCopied(t *testing.T) {
	buf := []byte("abc")
	f := fakereach.New()
	if _, err := f.WriteAtomic(ctx(), "p", buf, time.Now()); err != nil {
		t.Fatal(err)
	}
	buf[0] = 'X'
	got, _ := f.Get("p")
	if string(got.Data) != "abc" {
		t.Fatalf("stored data aliased caller buffer: %q", got.Data)
	}
}

func TestList_DerivesTreeFromFlatKeys(t *testing.T) {
	mt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := fakereach.New().
		Put("beta.srm", []byte("bb"), mt).
		Put("alpha.srm", []byte("a"), mt).
		Put("saves/game.srm", []byte("game-bytes"), mt).
		Put("saves/deep/nested.srm", []byte("x"), mt)

	// Root: saves/ (dir) first, then alpha.srm, beta.srm (files, alphabetical).
	for _, rel := range []string{"", "."} {
		entries, err := f.List(ctx(), rel)
		if err != nil {
			t.Fatalf("List(%q): %v", rel, err)
		}
		if len(entries) != 3 {
			t.Fatalf("List(%q) len = %d, want 3: %+v", rel, len(entries), entries)
		}
		if !entries[0].IsDir || entries[0].Name != "saves" {
			t.Errorf("List(%q)[0] = %+v, want dir saves", rel, entries[0])
		}
		if entries[1].Name != "alpha.srm" || entries[2].Name != "beta.srm" {
			t.Errorf("List(%q) files = %q,%q, want alpha,beta", rel, entries[1].Name, entries[2].Name)
		}
		if entries[1].Size != 1 || entries[2].Size != 2 {
			t.Errorf("List(%q) file sizes = %d,%d, want 1,2", rel, entries[1].Size, entries[2].Size)
		}
	}

	// Subdir: saves/ has deep/ (dir) then game.srm (file).
	sub, err := f.List(ctx(), "saves")
	if err != nil {
		t.Fatalf("List(saves): %v", err)
	}
	if len(sub) != 2 || !sub[0].IsDir || sub[0].Name != "deep" || sub[1].Name != "game.srm" {
		t.Fatalf("List(saves) = %+v, want deep/ then game.srm", sub)
	}
}

func TestList_MissingDir(t *testing.T) {
	f := fakereach.New().Put("a.srm", nil, time.Now())
	if _, err := f.List(ctx(), "nope"); !errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("List(missing) = %v, want ErrNotExist", err)
	}
}

func TestList_NotADirectory(t *testing.T) {
	f := fakereach.New().Put("a.srm", []byte("x"), time.Now())
	_, err := f.List(ctx(), "a.srm")
	if err == nil {
		t.Fatal("List(file) = nil err, want not-a-directory")
	}
	if errors.Is(err, reach.ErrNotExist) {
		t.Fatalf("List(file) mapped to ErrNotExist; want a distinct error: %v", err)
	}
}

func TestList_EmptyFakeRootIsEmptyNotMissing(t *testing.T) {
	f := fakereach.New()
	entries, err := f.List(ctx(), "")
	if err != nil {
		t.Fatalf("List(root) on empty fake = %v, want nil (empty listing)", err)
	}
	if len(entries) != 0 {
		t.Fatalf("List(root) on empty fake = %+v, want empty", entries)
	}
}
