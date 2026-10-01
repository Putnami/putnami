//go:build unix

package store

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// inodeOf returns the filesystem inode number for path. The atomicity proof
// hinges on the inode changing: an in-place O_TRUNC rewrite keeps the same
// inode, whereas a temp+rename publishes a brand-new inode over dst.
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: Sys() is %T, want *syscall.Stat_t", path, info.Sys())
	}
	return st.Ino
}

// TestCopyFile_AtomicByRename pins the invariant that copyFile publishes the
// destination via rename() — a concurrent reader/exec of dst must never observe
// a half-written file. If the write were in-place (os.Create + O_TRUNC) the dst
// inode would be unchanged; proving it changed proves the atomic rename path.
func TestCopyFile_AtomicByRename(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	// Pre-existing dst with content A and a distinct mode.
	if err := os.WriteFile(dst, []byte("AAAA-old-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldIno := inodeOf(t, dst)

	// Source with new content B and the executable bit set.
	newContent := []byte("BBBB-new-content-longer")
	if err := os.WriteFile(src, newContent, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	// Rename, not in-place truncate: the dst path resolves to a new inode.
	newIno := inodeOf(t, dst)
	if newIno == oldIno {
		t.Errorf("dst inode unchanged (%d): write was in-place, not atomic rename", oldIno)
	}

	// Content and mode (incl. exec bit) match the source.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newContent) {
		t.Errorf("dst content = %q, want %q", got, newContent)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("dst mode = %v, want -rwxr-xr-x (0755)", info.Mode().Perm())
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("dst should keep the exec bit, got mode %v", info.Mode())
	}

	// No staging temp files may linger in the destination directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file in destination dir: %s", e.Name())
		}
	}
}
