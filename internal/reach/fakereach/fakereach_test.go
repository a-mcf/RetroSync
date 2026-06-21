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
