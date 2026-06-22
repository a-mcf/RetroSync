package engine

// Internal (white-box) tests for the TIER-1 stat gate of change detection.
// These live in `package engine` (not engine_test) because statDiffers /
// mtimeEqual are package-private. They pin the slice-13 regression: a file whose
// stat mtime differs from the manifest mtime ONLY in sub-microsecond nanoseconds
// — the digits Postgres timestamptz truncates — must NOT trip the stat gate,
// while a genuine stat change (mtime ≥1µs apart, or a size delta) must. The
// hash-backed tier 2 (touch vs real change) is exercised in engine_test's Poll
// tests against the fakereach content hashes.

import (
	"testing"
	"time"

	"github.com/a-mcf/retrosync/internal/reach"
	"github.com/a-mcf/retrosync/internal/store"
)

func tp(t time.Time) *time.Time { return &t }
func ip(i int64) *int64         { return &i }

func TestMtimeEqual_SubMicrosecondNanosIgnored(t *testing.T) {
	// Same wall-clock down to the microsecond; differ only in the nanosecond
	// fraction below a microsecond (…252204000 vs …252204219). Postgres stores
	// only the microsecond, so these MUST compare equal.
	base := time.Date(2026, 6, 21, 12, 0, 0, 252204000, time.UTC)
	withNanos := time.Date(2026, 6, 21, 12, 0, 0, 252204219, time.UTC)

	if !mtimeEqual(base, withNanos) {
		t.Fatalf("mtimeEqual(%v, %v) = false, want true (sub-µs nanos must be ignored)", base, withNanos)
	}
	// Order must not matter.
	if !mtimeEqual(withNanos, base) {
		t.Fatalf("mtimeEqual is not symmetric")
	}
	// A ≥1µs difference must register as not-equal.
	plus1us := base.Add(time.Microsecond)
	if mtimeEqual(base, plus1us) {
		t.Fatalf("mtimeEqual treated a 1µs difference as equal")
	}
}

func TestMtimeEqual_DifferentZonesSameInstant(t *testing.T) {
	// A manifest value read back as a different zone but the same instant must
	// compare equal (mtimeEqual normalizes to UTC).
	utc := time.Date(2026, 6, 21, 12, 0, 0, 500000000, time.UTC)
	loc := time.FixedZone("X", 3600)
	other := utc.In(loc)
	if !mtimeEqual(utc, other) {
		t.Fatalf("mtimeEqual(%v, %v) = false, want true (same instant, different zone)", utc, other)
	}
}

func TestStatDiffers_MtimePrecisionAndSize(t *testing.T) {
	manifestMtime := time.Date(2026, 6, 21, 12, 0, 0, 252204000, time.UTC) // µs-truncated (as Postgres would store)
	const manifestSize int64 = 4096

	me := store.ManifestEntry{Mtime: tp(manifestMtime), Size: ip(manifestSize)}

	tests := []struct {
		name    string
		cur     reach.FileMeta
		present bool
		want    bool
	}{
		{
			// THE REGRESSION: stat returns full-nanosecond mtime that matches the
			// manifest only down to the microsecond. Must NOT be "changed".
			name:    "sub-microsecond nanos only -> not changed",
			cur:     reach.FileMeta{Mtime: manifestMtime.Add(219 * time.Nanosecond), Size: manifestSize},
			present: true,
			want:    false,
		},
		{
			name:    "identical -> not changed",
			cur:     reach.FileMeta{Mtime: manifestMtime, Size: manifestSize},
			present: true,
			want:    false,
		},
		{
			name:    "mtime differs by >=1us -> changed",
			cur:     reach.FileMeta{Mtime: manifestMtime.Add(time.Microsecond), Size: manifestSize},
			present: true,
			want:    true,
		},
		{
			name:    "size differs -> changed",
			cur:     reach.FileMeta{Mtime: manifestMtime.Add(219 * time.Nanosecond), Size: manifestSize + 1},
			present: true,
			want:    true,
		},
		{
			name:    "absent but manifest had file -> changed (deleted)",
			cur:     reach.FileMeta{},
			present: false,
			want:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statDiffers(me, tt.cur, tt.present); got != tt.want {
				t.Fatalf("statDiffers(%+v, present=%v) = %v, want %v", tt.cur, tt.present, got, tt.want)
			}
		})
	}
}

func TestStatDiffers_AppearedAndBothAbsent(t *testing.T) {
	empty := store.ManifestEntry{} // no mtime/size recorded
	cur := reach.FileMeta{Mtime: time.Now(), Size: 10}
	if !statDiffers(empty, cur, true) {
		t.Fatalf("a present file with no manifest row must differ (appeared)")
	}
	if statDiffers(empty, reach.FileMeta{}, false) {
		t.Fatalf("both-absent must not differ")
	}
}
