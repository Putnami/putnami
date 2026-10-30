package shared

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A rewrite keeps the file's permission bits: the temp file that replaces it
// takes the existing mode, not a fresh 0644.
func TestAtomicWriteFileKeepsTheExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no owner-only permission bits")
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(path, []byte(`{"env":{}}`+"\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode after the rewrite = %o, want 0600", got)
	}
	if data, _ := os.ReadFile(path); string(data) != `{"env":{}}`+"\n" {
		t.Errorf("content after the rewrite = %q", data)
	}
}

// A file that did not exist is created 0644, whatever the umask.
func TestAtomicWriteFileCreatesANewFileWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no group or other permission bits")
	}
	path := filepath.Join(t.TempDir(), "new", "contracts.json")
	if err := AtomicWriteFile(path, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode of a new file = %o, want 0644", got)
	}
}
