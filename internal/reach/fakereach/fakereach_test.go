package fakereach_test

import (
	"context"
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
	if err := f.WriteAtomic(ctx(), "b.srm", []byte("data"), mt); err != nil {
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
	if err := f.WriteAtomic(ctx(), "a", []byte("new"), time.Now()); !errors.Is(err, boom) {
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
	if err := f.WriteAtomic(ctx(), "p", buf, time.Now()); err != nil {
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
