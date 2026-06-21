package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolve_Valid(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		rel  string
		want string
	}{
		{"simple file", "save.srm", filepath.Join(root, "save.srm")},
		{"nested", "retroarch/saves/Super Metroid.srm", filepath.Join(root, "retroarch/saves/Super Metroid.srm")},
		{"dot prefix", "./a/b", filepath.Join(root, "a/b")},
		{"interior dotdot stays inside", "a/b/../c", filepath.Join(root, "a/c")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(root, tc.rel)
			if err != nil {
				t.Fatalf("Resolve(%q, %q) unexpected err: %v", root, tc.rel, err)
			}
			if got != tc.want {
				t.Fatalf("Resolve(%q, %q) = %q, want %q", root, tc.rel, got, tc.want)
			}
		})
	}
}

func TestResolve_Rejects(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		rel  string
	}{
		{"absolute", "/etc/passwd"},
		{"parent traversal", "../outside"},
		{"bare dotdot", ".."},
		{"sneaky a/../../b", "a/../../b"},
		{"deep escape", "x/y/../../../../../../etc/passwd"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(root, tc.rel)
			if err == nil {
				t.Fatalf("Resolve(%q, %q) = nil err, want rejection", root, tc.rel)
			}
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("Resolve(%q, %q) err = %v, want ErrUnsafePath", root, tc.rel, err)
			}
		})
	}
}

func TestResolve_NonAbsoluteRoot(t *testing.T) {
	_, err := Resolve("relative/root", "x")
	if err == nil {
		t.Fatal("Resolve with relative root = nil err, want error")
	}
	if errors.Is(err, ErrUnsafePath) {
		t.Fatal("relative-root error should be a config error, not ErrUnsafePath")
	}
}

// TestResolve_SymlinkEscape plants a symlink inside the root that points outside
// it, then asserts a path traversing that symlink is rejected even though it is
// lexically inside the root.
func TestResolve_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// root/escape -> outside
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	// Lexically "escape/secret" is inside root, but it resolves through the
	// symlink to outside the root and must be rejected.
	_, err := Resolve(root, "escape/secret")
	if err == nil {
		t.Fatal("Resolve through escaping symlink = nil err, want rejection")
	}
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink escape err = %v, want ErrUnsafePath", err)
	}
}

// TestResolve_SymlinkInside confirms a symlink that stays inside the root is
// allowed (we reject escape, not all symlinks).
func TestResolve_SymlinkInside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root, "alias/save.srm"); err != nil {
		t.Fatalf("Resolve through inside-root symlink unexpected err: %v", err)
	}
}

// TestResolve_NonExistingTailOK confirms a path whose trailing components do not
// yet exist (the write case) still validates against its existing prefix.
func TestResolve_NonExistingTailOK(t *testing.T) {
	root := t.TempDir()
	got, err := Resolve(root, "new/deep/save.srm")
	if err != nil {
		t.Fatalf("unexpected err for non-existing tail: %v", err)
	}
	if want := filepath.Join(root, "new/deep/save.srm"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
