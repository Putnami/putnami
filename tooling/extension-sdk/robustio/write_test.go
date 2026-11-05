package robustio

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// leftovers returns the temporary files WriteFile left in dir.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestWriteFileReplacesTheFileWithItsMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o644, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("the file holds %q, %v; want the new bytes", got, err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644", info.Mode().Perm())
	}
}

func TestWriteFileSyncsTheDestinationDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	var synced string
	err := WriteFile(path, []byte("new"), 0o600, func(dir string) error {
		synced = dir
		return nil
	})
	if err != nil || synced != dir {
		t.Fatalf("WriteFile = %v, synced %q; want %q synced", err, synced, dir)
	}
}

func TestWriteFileReportsALandedWriteWhenTheDirectorySyncFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.json")
	refused := errors.New("the disk refused the sync")
	err := WriteFile(path, []byte("new"), 0o600, func(string) error { return refused })
	var failure *WriteError
	if !errors.As(err, &failure) || failure.Op != OpSyncDir || !failure.Landed() || !errors.Is(err, refused) {
		t.Fatalf("WriteFile = %#v, want a landed directory sync failure", err)
	}
	if err.Error() != refused.Error() {
		t.Fatalf("text %q, want the sync error's own %q", err, refused)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "new" {
		t.Fatalf("the file holds %q, %v; want the landed bytes", got, readErr)
	}
}

func TestWriteFileCreatesNoDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "doc.json")
	err := WriteFile(path, []byte("x"), 0o644, nil)
	var failure *WriteError
	if !errors.As(err, &failure) || failure.Op != OpCreate || failure.Landed() || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WriteFile = %#v, want a create failure", err)
	}
}

func TestWriteFileLeavesNoTemporaryFileWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "occupied")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "entry"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := WriteFile(path, []byte("new"), 0o644, nil)
	var failure *WriteError
	if !errors.As(err, &failure) || failure.Op != OpRename || failure.Landed() {
		t.Fatalf("WriteFile over a directory = %#v, want a rename failure", err)
	}
	if found := leftovers(t, dir); len(found) != 0 {
		t.Fatalf("temporary files left behind: %v", found)
	}
}
