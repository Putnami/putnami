//go:build unix

package dbtestenv

import (
	"os"
	"path/filepath"
	"testing"
)

// A state root that is a symbolic link, or one others can write, is refused.
func TestPrepareNativeRootRefusesAnUntrustedRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := prepareNativeRoot(link); err == nil {
		t.Error("a symbolic link was accepted as the state root")
	}
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := prepareNativeRoot(open); err == nil {
		t.Error("a world-writable directory was accepted as the state root")
	}
	if err := prepareNativeRoot(filepath.Join(base, "fresh")); err != nil {
		t.Errorf("a fresh state root was refused: %v", err)
	}
}

// A password file is created anew and never written through a planted link.
func TestWriteExclusiveDoesNotFollowALink(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "x.pw")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusive(path, "secret\n", -1, -1); err != nil {
		t.Fatalf("writeExclusive: %v", err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep" {
		t.Errorf("the link target was written: %q", data)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("path is %v (%v), want a fresh 0600 file", info, err)
	}
}
