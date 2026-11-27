package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// AbsentBeforeTreeReads answers true only for a destination that is absent
// and whose closest existing directory has not changed since the run began to
// read the tree: a removal since then, a present destination and a link above
// it all answer false (ADR 0119).
func TestAbsentBeforeTreeReads(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("a platform without change times answers false for every destination")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(dir, "removed.html")
	if err := os.WriteFile(removed, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	cm := NewCacheManager(nil)
	cm.SetTreeReadsSince(time.Now())
	time.Sleep(50 * time.Millisecond)

	if !cm.AbsentBeforeTreeReads(filepath.Join(dir, "never", "index.html")) {
		t.Error("a destination absent before the reads began answers false")
	}
	if cm.AbsentBeforeTreeReads(removed) {
		t.Error("a present destination answers true")
	}
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	if cm.AbsentBeforeTreeReads(removed) {
		t.Error("a destination removed after the reads began answers true")
	}
	if cm.AbsentBeforeTreeReads(filepath.Join(link, "index.html")) {
		t.Error("a destination below a link answers true")
	}
}
