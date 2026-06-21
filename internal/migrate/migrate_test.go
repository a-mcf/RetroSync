package migrate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestEmbeddedMigrationsMatchRepoRoot guards against drift between the
// canonical migrations/ directory at the repo root and the embedded copy under
// internal/migrate/sql/. They must stay byte-identical.
func TestEmbeddedMigrationsMatchRepoRoot(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file path")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	rootMigrations := filepath.Join(repoRoot, "migrations")

	entries, err := os.ReadDir(rootMigrations)
	if err != nil {
		t.Fatalf("read repo migrations dir: %v", err)
	}
	var sqlCount int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		sqlCount++
		want, err := os.ReadFile(filepath.Join(rootMigrations, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		got, err := migrationsFS.ReadFile(migrationsDir + "/" + e.Name())
		if err != nil {
			t.Fatalf("embedded copy of %s missing: %v", e.Name(), err)
		}
		if string(got) != string(want) {
			t.Errorf("embedded sql/%s differs from migrations/%s (drift)", e.Name(), e.Name())
		}
	}
	if sqlCount == 0 {
		t.Fatal("no .sql files found in repo migrations dir")
	}

	// Reverse direction: every embedded .sql must have a repo-root twin, so an
	// embedded-only migration cannot slip through undetected.
	embedded, err := migrationsFS.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read embedded migrations dir: %v", err)
	}
	for _, e := range embedded {
		if e.IsDir() || filepath.Ext(e.Name()) != ".sql" {
			continue
		}
		if _, err := os.Stat(filepath.Join(rootMigrations, e.Name())); err != nil {
			t.Errorf("embedded sql/%s has no matching migrations/%s (embedded-only)", e.Name(), e.Name())
		}
	}
}

func TestParseName(t *testing.T) {
	tests := []struct {
		in       string
		wantVer  int
		wantName string
		wantDir  string
		wantOK   bool
	}{
		{"0001_registry.up.sql", 1, "registry", "up", true},
		{"0001_registry.down.sql", 1, "registry", "down", true},
		{"0012_add_indexes.up.sql", 12, "add_indexes", "up", true},
		{"README.md", 0, "", "", false},
		{"0001_registry.sql", 0, "", "", false},
		{"noversion_foo.up.sql", 0, "", "", false},
	}
	for _, tt := range tests {
		v, name, dir, ok := parseName(tt.in)
		if ok != tt.wantOK || v != tt.wantVer || name != tt.wantName || dir != tt.wantDir {
			t.Errorf("parseName(%q) = (%d,%q,%q,%v), want (%d,%q,%q,%v)",
				tt.in, v, name, dir, ok, tt.wantVer, tt.wantName, tt.wantDir, tt.wantOK)
		}
	}
}

func TestLoadOrdersByVersion(t *testing.T) {
	migs, err := load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("expected at least one migration")
	}
	for i := 1; i < len(migs); i++ {
		if migs[i-1].version >= migs[i].version {
			t.Errorf("migrations out of order at %d", i)
		}
	}
	if migs[0].up == "" || migs[0].down == "" {
		t.Error("first migration missing up or down body")
	}
}
