//go:build unix

package store

import (
	"os"
	"path/filepath"
	"testing"
)

// A symlinked file in a declared output is captured with its target's mode,
// not the link's own 0777: that mode would reach the shared CAS blob and every
// restored copy.
func TestIngestTaskEntry_SymlinkCapturesTheTargetMode(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()
	out := dirOutput("dist", "dist")
	stage(t, staging, out, "real.js", "built")
	dir := TaskStagingPath(staging, out)
	if err := os.Symlink("real.js", filepath.Join(dir, "link.js")); err != nil {
		t.Fatal(err)
	}

	entry, err := s.IngestTaskEntry(staging, taskSpec("symlink-key", out))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	for _, f := range entry.Manifest.Files {
		if f.Mode != 0o644 {
			t.Errorf("manifest mode of %s = %o, want 0644", f.Path, f.Mode)
		}
		info, err := os.Stat(s.casBlobPath(f.Digest))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got&0o022 != 0 {
			t.Errorf("CAS blob of %s is %v, writable by group or other", f.Path, got)
		}
	}
}
