package ownerperm

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ownerOnlyOrFail(t *testing.T, path string) bool {
	t.Helper()
	private, err := OwnerOnly(path)
	if err != nil {
		t.Fatalf("OwnerOnly(%s): %v", path, err)
	}
	return private
}

// A file created with the usual defaults is readable by others. Restrict
// makes it and a directory owner-only on every platform.
func TestRestrictMakesAFileAndADirectoryOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "secret")
	writeFile(t, file)
	// On Windows what a new file inherits depends on the temporary directory;
	// ownerperm_windows_test.go checks the list itself.
	if runtime.GOOS != "windows" && ownerOnlyOrFail(t, file) {
		t.Fatal("precondition: a new file is already owner-only")
	}

	if err := Restrict(file, 0o600); err != nil {
		t.Fatalf("Restrict(file): %v", err)
	}
	if err := Restrict(dir, 0o700); err != nil {
		t.Fatalf("Restrict(dir): %v", err)
	}
	for _, path := range []string{file, dir} {
		if !ownerOnlyOrFail(t, path) {
			t.Errorf("%s is not owner-only after Restrict", path)
		}
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{file: 0o600, dir: 0o700} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != want {
				t.Errorf("%s mode = %04o, want %04o", path, got, want)
			}
		}
	}
}

func TestRestrictRefusesAModeThatSharesAccess(t *testing.T) {
	file := filepath.Join(t.TempDir(), "shared")
	writeFile(t, file)
	for _, mode := range []os.FileMode{0o640, 0o604, 0o710} {
		if err := Restrict(file, mode); err == nil {
			t.Errorf("Restrict accepted mode %04o", mode)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("a refused Restrict changed the mode to %04o", got)
		}
	}
}

func TestMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := Restrict(missing, 0o600); err == nil {
		t.Error("Restrict of a missing path reported no error")
	}
	if _, err := OwnerOnly(missing); err == nil {
		t.Error("OwnerOnly of a missing path reported no error")
	}
}
