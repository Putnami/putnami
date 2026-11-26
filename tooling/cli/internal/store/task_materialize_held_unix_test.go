//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A restore over a destination that already holds the entry's output leaves
// it untouched: the same inodes, so the file index answers its digests on the
// next run. Any difference makes the restore write the whole output.
func TestARestoreLeavesADestinationThatHoldsTheOutput(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestDirAndFile(t, s, "held-key")
	parent := t.TempDir()
	dist, tool := filepath.Join(parent, "dist"), filepath.Join(parent, "tool")
	restore := func() {
		t.Helper()
		if _, err := s.MaterializeTaskOutput(entry, "dist", dist, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MaterializeTaskOutput(entry, "tool", tool, ""); err != nil {
			t.Fatal(err)
		}
	}
	restore()
	held := snapshotInodes(t, dist)
	heldTool := snapshotInodes(t, parent, "tool")
	restore()
	assertSameInodes(t, dist, held)
	assertSameInodes(t, parent, heldTool)
	assertTree(t, dist, map[string]string{"main.js": "built", "sub/chunk.js": "chunk"})
	assertFile(t, tool, "#!/bin/sh\necho tool\n")

	changes := map[string]func(){
		"a file of the same size with its time set back": func() {
			path := filepath.Join(dist, "main.js")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("BUILT"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
		"an extra file": func() { writeTree(t, dist, map[string]string{"stray.js": "stray"}) },
		"a missing file": func() {
			if err := os.Remove(filepath.Join(dist, "sub", "chunk.js")); err != nil {
				t.Fatal(err)
			}
		},
		"an extra empty directory": func() {
			if err := os.Mkdir(filepath.Join(dist, "empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"another file mode": func() {
			if err := os.Chmod(filepath.Join(dist, "main.js"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"another directory mode": func() {
			if err := os.Chmod(filepath.Join(dist, "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a setgid bit a restore does not write": func() {
			if err := os.Chmod(filepath.Join(dist, "sub"), 0o755|os.ModeSetgid); err != nil {
				t.Fatal(err)
			}
		},
		"a link in place of a file": func() {
			path := filepath.Join(dist, "main.js")
			target := filepath.Join(t.TempDir(), "main.js")
			if err := os.WriteFile(target, []byte("built"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			restore()
			change()
			before := snapshotInodes(t, parent, "dist")
			restore()
			assertTree(t, dist, map[string]string{"main.js": "built", "sub/chunk.js": "chunk"})
			after, err := os.Lstat(dist)
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(before["dist"], after) {
				t.Fatalf("after %s the restore left the destination in place", name)
			}
		})
	}

	t.Run("a file output rewritten in the same second", func(t *testing.T) {
		restore()
		info, err := os.Stat(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tool, []byte("#!/bin/sh\necho TOOL\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(tool, info.ModTime(), info.ModTime().Add(time.Nanosecond)); err != nil {
			t.Fatal(err)
		}
		restore()
		assertFile(t, tool, "#!/bin/sh\necho tool\n")
	})
}

// A kept subtree is not compared: what it holds is left alone either way.
func TestARestoreThatHoldsItsOutputIgnoresTheKeptSubtree(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "held-keep-key", map[string]string{"sub/x.js": "x"})
	dest := filepath.Join(t.TempDir(), "gen")
	writeTree(t, dest, map[string]string{"sub/x.js": "x", "migration-bundle/bundle.json": "bundle"})
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "migration-bundle"); err != nil {
		t.Fatal(err)
	}
	held := snapshotInodes(t, dest)
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "migration-bundle"); err != nil {
		t.Fatal(err)
	}
	assertSameInodes(t, dest, held)
	assertTree(t, dest, map[string]string{"sub/x.js": "x", "migration-bundle/bundle.json": "bundle"})
}

// A file output whose payload the entry lost is restored, and so fails, even
// when its destination holds the bytes.
func TestARestoreOfATornFileOutputIsRefused(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestDirAndFile(t, s, "held-torn-key")
	tool := filepath.Join(t.TempDir(), "tool")
	if _, err := s.MaterializeTaskOutput(entry, "tool", tool, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(entry.FilesDir, "tool")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MaterializeTaskOutput(entry, "tool", tool, ""); err == nil {
		t.Fatal("a restore of a file output without its payload succeeded")
	}
}

// An entry that holds bytes at a path its output cedes is refused by the
// restore, even when the destination holds the same bytes.
func TestARestoreOfCededBytesIsRefusedOverAHeldDestination(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "held-ceded-key", map[string]string{"sub/x.js": "x", "bundle/b.json": "b"})
	dest := filepath.Join(t.TempDir(), "gen")
	writeTree(t, dest, map[string]string{"sub/x.js": "x", "bundle/b.json": "b"})
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "bundle"); err == nil {
		t.Fatal("a restore of an entry holding bytes at a ceded path succeeded")
	}
}
