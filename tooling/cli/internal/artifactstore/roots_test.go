package artifactstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// publishBlob writes a CLI blob under <root>/<tree>/<name>/putnami, last used at
// the given time, and returns its entry dir.
func publishBlob(t *testing.T, s *Store, tree, name string, lastUsed time.Time) string {
	t.Helper()
	dir := filepath.Join(s.root, tree, name)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cliBinaryName), make([]byte, 64), 0o755); err != nil {
		t.Fatal(err)
	}
	stampUsed(dir, lastUsed)
	return dir
}

// linkWorkspace creates a workspace whose .putnami/bin/<rel> links to target.
func linkWorkspace(t *testing.T, ws string, links map[string]string) {
	t.Helper()
	for rel, target := range links {
		link := filepath.Join(ws, ".putnami", "bin", rel)
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestGC_NeverEvictsWhatAWorkspaceLinks is the user-facing contract: the CLI and
// extensions a workspace links to survive both the idle pass and the budget pass,
// however long they sat unused, while unlinked blobs are reclaimed.
func TestGC_NeverEvictsWhatAWorkspaceLinks(t *testing.T) {
	s := New(t.TempDir())
	old := time.Now().Add(-90 * 24 * time.Hour)

	usedCLI := publishBlob(t, s, cliSourceDirName, "used", old)
	staleCLI := publishBlob(t, s, cliSourceDirName, "stale", old)
	ext := admitDir(t, s, "ext", 64)
	stampUsed(s.Path(ext), old)

	ws := t.TempDir()
	linkWorkspace(t, ws, map[string]string{
		"putnami":           filepath.Join(usedCLI, cliBinaryName),
		"extensions/go-ext": s.Path(ext),
	})
	if err := s.RegisterWorkspace(ws); err != nil {
		t.Fatal(err)
	}

	res, err := s.GC(GCOptions{MaxBytes: 1, Grace: time.Hour, MaxIdle: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Live != 2 {
		t.Errorf("Live = %d, want 2", res.Live)
	}
	if !exists(usedCLI) {
		t.Error("the CLI a workspace links to must survive GC")
	}
	if !s.Has(ext) {
		t.Error("the extension a workspace links to must survive GC")
	}
	if exists(staleCLI) {
		t.Error("an unlinked idle from-source CLI should be reclaimed")
	}
}

// TestGC_BudgetReclaimsCLISourceBlobs covers the leak itself: from-source CLIs
// count against the budget and are evicted oldest-first.
func TestGC_BudgetReclaimsCLISourceBlobs(t *testing.T) {
	s := New(t.TempDir())
	now := time.Now()
	older := publishBlob(t, s, cliSourceDirName, "older", now.Add(-3*time.Hour))
	newer := publishBlob(t, s, cliSourceDirName, "newer", now.Add(-2*time.Hour))

	res, err := s.GC(GCOptions{MaxBytes: 100, Grace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scanned != 2 || res.EvictedBudget != 1 {
		t.Errorf("got %+v, want 2 scanned and 1 evicted for budget", res)
	}
	if exists(older) || !exists(newer) {
		t.Error("budget eviction must drop the oldest from-source CLI first")
	}
}

// TestGC_PrunesRootOfDeletedWorkspace checks that a removed workspace stops
// pinning entries: its root goes, and what it alone kept ages out normally.
func TestGC_PrunesRootOfDeletedWorkspace(t *testing.T) {
	s := New(t.TempDir())
	blob := publishBlob(t, s, cliSourceDirName, "orphan", time.Now().Add(-48*time.Hour))
	ws := filepath.Join(t.TempDir(), "ws")
	linkWorkspace(t, ws, map[string]string{"putnami": filepath.Join(blob, cliBinaryName)})
	if err := s.RegisterWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}

	res, err := s.GC(GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, MaxIdle: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Live != 0 || res.EvictedIdle != 1 || exists(blob) {
		t.Errorf("a deleted workspace must not pin its CLI, got %+v", res)
	}
	if roots, _ := os.ReadDir(filepath.Join(s.root, rootsDirName)); len(roots) != 0 {
		t.Errorf("the deleted workspace's root should be pruned, found %d", len(roots))
	}
}

func TestRegisterWorkspace_IsIdempotent(t *testing.T) {
	s := New(t.TempDir())
	ws := t.TempDir()
	for range 2 {
		if err := s.RegisterWorkspace(ws); err != nil {
			t.Fatal(err)
		}
	}
	roots, err := os.ReadDir(filepath.Join(s.root, rootsDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("want one root, got %d", len(roots))
	}
	target, err := os.Readlink(filepath.Join(s.root, rootsDirName, roots[0].Name()))
	if err != nil || target != ws {
		t.Errorf("root points at %q (%v), want %q", target, err, ws)
	}
}

// TestGC_IgnoresLinksOutsideTheStore covers relative per-worktree links
// (layout.LinkArtifact): they pin nothing in the shared store.
func TestGC_IgnoresLinksOutsideTheStore(t *testing.T) {
	s := New(t.TempDir())
	ws := t.TempDir()
	linkWorkspace(t, ws, map[string]string{"extensions/local": "../artifacts/extensions/local@1.0.0"})
	if err := s.RegisterWorkspace(ws); err != nil {
		t.Fatal(err)
	}
	if live := s.liveEntries(); len(live) != 0 {
		t.Errorf("a link outside the store must pin nothing, got %v", live)
	}
}

func TestClean_RemovesCLISourceBlobs(t *testing.T) {
	s := New(t.TempDir())
	blob := publishBlob(t, s, cliSourceDirName, "any", time.Now().Add(-48*time.Hour))
	dirs, _, err := s.Clean(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if dirs != 1 || exists(blob) {
		t.Errorf("Clean should remove the from-source CLI, removed %d", dirs)
	}
}

func TestTouchExecutable(t *testing.T) {
	s := New(t.TempDir())
	blob := publishBlob(t, s, cliSourceDirName, "self", time.Now().Add(-48*time.Hour))

	s.TouchExecutable(filepath.Join(blob, cliBinaryName))
	if last, _ := readUsed(blob); time.Since(last) > time.Minute {
		t.Errorf("the running binary's entry should be stamped now, got %v", last)
	}

	outside := filepath.Join(t.TempDir(), "putnami")
	s.TouchExecutable(outside) // must not create anything outside the store
	if exists(filepath.Join(filepath.Dir(outside), lastUsedFile)) {
		t.Error("a binary outside the store must not be stamped")
	}
}
