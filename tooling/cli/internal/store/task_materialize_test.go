package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ingestTree publishes a one-directory-output entry whose payload is files.
func ingestTree(t *testing.T, s *LocalStore, key string, files map[string]string) *TaskEntry {
	t.Helper()
	staging := t.TempDir()
	out := dirOutput("dist", "dist")
	for rel, content := range files {
		stage(t, staging, out, rel, content)
	}
	entry, err := s.IngestTaskEntry(staging, taskSpec(key, out))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	return entry
}

// assertTree fails unless dest is a real directory holding exactly want.
func assertTree(t *testing.T, dest string, want map[string]string) {
	t.Helper()
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("lstat %s: %v", dest, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is still a symlink after materialize", dest)
	}
	got := map[string]string{}
	err = filepath.Walk(dest, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dest, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dest, err)
	}
	if len(got) != len(want) {
		t.Fatalf("materialized tree = %v, want %v", keysOf(got), keysOf(want))
	}
	for rel, content := range want {
		if got[rel] != content {
			t.Errorf("%s = %q, want %q", rel, got[rel], content)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestMaterializeTaskOutput_ReplacesALiveSymlinkDestination pins the ENOTDIR
// gotcha: rename(2) refuses to put a directory where a symlink lives, so the
// primitive must detach the link before publishing. It also pins
// the CAS-safety half — detaching moves the LINK, never its target, so the blob
// the stale link pointed at survives untouched.
func TestMaterializeTaskOutput_ReplacesALiveSymlinkDestination(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	stale := ingestTree(t, s, "stale-key", map[string]string{"old.js": "old"})
	fresh := ingestTree(t, s, "fresh-key", map[string]string{"main.js": "new", "sub/x.js": "x"})

	dest := filepath.Join(t.TempDir(), "out")
	staleTarget := filepath.Join(stale.FilesDir, "dist")
	if err := os.Symlink(staleTarget, dest); err != nil {
		t.Fatal(err)
	}

	written, err := s.MaterializeTaskOutput(fresh, "dist", dest, "")
	if err != nil {
		t.Fatalf("MaterializeTaskOutput over a live symlink: %v", err)
	}
	if !written {
		t.Fatal("materialize reported no bytes written")
	}
	assertTree(t, dest, map[string]string{"main.js": "new", "sub/x.js": "x"})

	// The blob behind the detached link must be intact: removing it would
	// corrupt every other entry sharing those CAS bytes.
	data, err := os.ReadFile(filepath.Join(staleTarget, "old.js"))
	if err != nil {
		t.Fatalf("the detached symlink's target was destroyed: %v", err)
	}
	if string(data) != "old" {
		t.Errorf("blob content = %q, want old (the swap wrote through the link)", data)
	}
}

// TestMaterializeTaskOutput_DirectoryDestinationShapes covers the remaining
// things that can sit at a destination — nothing, a real (stale) tree, and a
// plain file — since each hits a different rename(2) error.
func TestMaterializeTaskOutput_DirectoryDestinationShapes(t *testing.T) {
	want := map[string]string{"main.js": "new", "sub/x.js": "x"}

	t.Run("absent", func(t *testing.T) {
		s := NewLocalStore(t.TempDir())
		entry := ingestTree(t, s, "absent-key", want)
		dest := filepath.Join(t.TempDir(), "nested", "out")
		if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		assertTree(t, dest, want)
	})

	t.Run("stale directory is replaced, not merged", func(t *testing.T) {
		s := NewLocalStore(t.TempDir())
		entry := ingestTree(t, s, "stale-dir-key", want)
		dest := filepath.Join(t.TempDir(), "out")
		if err := os.MkdirAll(filepath.Join(dest, "gone"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, "gone", "leftover.js"), []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		assertTree(t, dest, want) // the stale file is gone
	})

	t.Run("plain file at a directory destination", func(t *testing.T) {
		s := NewLocalStore(t.TempDir())
		entry := ingestTree(t, s, "file-dest-key", want)
		dest := filepath.Join(t.TempDir(), "out")
		if err := os.WriteFile(dest, []byte("not a tree"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		assertTree(t, dest, want)
	})
}

// TestMaterializeTaskOutput_EmptyOutputNeverTouchesItsDestination is the binding
// invariant of the explicit empty state. Command-output paths are shared between
// the steps of one command, so an empty output must leave the path exactly as it
// found it — including a sibling task's artifacts sitting there.
//
// The positive control at the end replaces the same destination from a PRESENT
// output, so the test cannot pass by materialize being a no-op in general.
func TestMaterializeTaskOutput_EmptyOutputNeverTouchesItsDestination(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	staging := t.TempDir()

	required := dirOutput("dist", "dist")
	optional := dirOutput("coverage", "coverage")
	optional.Optional = true
	stage(t, staging, required, "main.js", "built")

	entry, err := s.IngestTaskEntry(staging, taskSpec("empty-materialize-key", required, optional))
	if err != nil {
		t.Fatal(err)
	}

	// A sibling step's artifacts already live at the destination.
	dest := filepath.Join(t.TempDir(), "coverage")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dest, "sibling.info")
	if err := os.WriteFile(sibling, []byte("sibling output"), 0o644); err != nil {
		t.Fatal(err)
	}

	written, err := s.MaterializeTaskOutput(entry, "coverage", dest, "")
	if err != nil {
		t.Fatalf("materialize empty output: %v", err)
	}
	if written {
		t.Error("an empty output reported writing bytes")
	}
	data, err := os.ReadFile(sibling)
	if err != nil {
		t.Fatalf("an empty output deleted a sibling's artifacts: %v", err)
	}
	if string(data) != "sibling output" {
		t.Errorf("sibling artifact = %q, want unchanged", data)
	}
	if entries, err := os.ReadDir(dest); err != nil || len(entries) != 1 {
		t.Errorf("destination changed: %d entries (err=%v), want 1", len(entries), err)
	}

	// Positive control: the same destination IS replaced by a present output.
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
		t.Fatalf("materialize present output: %v", err)
	}
	assertTree(t, dest, map[string]string{"main.js": "built"})
}

// TestMaterializeTaskOutput_LeavesNoStagingResidue pins that the staging
// siblings the primitive creates are always reclaimed — a leaked
// .tmp-materialize-* tree would be indistinguishable from real output to the
// next capture.
func TestMaterializeTaskOutput_LeavesNoStagingResidue(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "residue-key", map[string]string{"main.js": "built"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "out")
	for range 3 {
		if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
			t.Fatalf("materialize: %v", err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "out" {
			t.Errorf("leftover staging path: %s", e.Name())
		}
	}
}

// TestMaterializeTaskOutput_RejectsUndeclaredOutputs pins that materialize is
// driven by the entry's own declared-output manifest: an id the entry does not
// record is an error, not a silent no-op that would leave a consumer with
// nothing and no diagnostic.
func TestMaterializeTaskOutput_RejectsUndeclaredOutputs(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "undeclared-key", map[string]string{"main.js": "built"})
	dest := filepath.Join(t.TempDir(), "out")

	if _, err := s.MaterializeTaskOutput(entry, "clients", dest, ""); err == nil {
		t.Error("materializing an output the entry does not declare succeeded")
	}
	if _, err := s.MaterializeTaskOutput(nil, "dist", dest, ""); err == nil {
		t.Error("materializing from a nil entry succeeded")
	}
	// Positive control.
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
		t.Errorf("declared output rejected: %v", err)
	}
}

// TestOutManager_RefusesTaskOwnedBlobs pins the retirement of the CAS-symlink
// all-hit fast path for task-owned entries (see the decision in task_entry.go).
// Linking a task-owned blob would point a shared, writable command directory at
// an immutable hardlink tree, so it fails loudly instead.
func TestOutManager_RefusesTaskOwnedBlobs(t *testing.T) {
	wsRoot := t.TempDir()
	storeRoot := filepath.Join(wsRoot, ".putnami", "store")
	s := NewLocalStore(storeRoot)
	om := NewOutManager(wsRoot, storeRoot)

	entry := ingestTree(t, s, "fastpath-key", map[string]string{"main.js": "built"})

	if err := om.Link("pkg", "build", "", entry.Address); !errors.Is(err, ErrFastPathRetired) {
		t.Errorf("Link error = %v, want ErrFastPathRetired", err)
	}
	if err := om.ReplaceLink("pkg", "build", "", entry.Address); !errors.Is(err, ErrFastPathRetired) {
		t.Errorf("ReplaceLink error = %v, want ErrFastPathRetired", err)
	}
	if _, err := os.Lstat(filepath.Join(wsRoot, ".putnami", "out", "pkg", "build")); err == nil {
		t.Error("a link was created for a task-owned blob")
	}

	// Positive control: the legacy fast path is untouched — the scheduler keeps
	// using it until B4b.
	legacyHash := "cccc000000000000000000000000000000000000000000000000000000000000"
	if err := s.Put(legacyHash, entryWith(makeSource(t, "legacy"))); err != nil {
		t.Fatal(err)
	}
	if err := om.Link("pkg", "test", "", legacyHash); err != nil {
		t.Fatalf("legacy Link broke: %v", err)
	}
	if _, err := os.Readlink(filepath.Join(wsRoot, ".putnami", "out", "pkg", "test")); err != nil {
		t.Errorf("legacy link missing: %v", err)
	}
}

// TestMaterializeTaskOutput_CededSubtreeSurvivesTheSwap pins the
// restore half of a carve-out (protocol ADR 0003). A directory output that
// cedes a subpath never captured it, so its entry has nothing to put there —
// and the destination may hold, at exactly that path, bytes another task
// produced while this one was served from cache (a remote-cache recurrence: a
// `package` describe's migration bundle deleted by a `test` generate's
// restore, with a `publish` step reading it in between). The kept
// subtree must survive the restore; everything else the old tree held must
// not, or the test would pass for a restore that merged stale entries in. It
// runs on both directory swaps (forEachSwapPath).
func TestMaterializeTaskOutput_CededSubtreeSurvivesTheSwap(t *testing.T) {
	forEachSwapPath(t, testCededSubtreeSurvivesTheSwap)
}

func testCededSubtreeSurvivesTheSwap(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "keep-key", map[string]string{"generate-result.json": "new", "sub/x.js": "x"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "gen")
	writeTree(t, dest, map[string]string{
		"generate-result.json":              "old",
		"stale/leftover.js":                 "stale",
		"migration-bundle/bundle.json":      "bundle",
		"migration-bundle/payload/0001.sql": "create table t (id uuid);",
		"schema/openapi.json":               "converged",
	})

	written, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "migration-bundle", "schema", "never-there")
	if err != nil {
		t.Fatalf("materialize keeping: %v", err)
	}
	if !written {
		t.Error("a present output reported writing nothing")
	}
	assertTree(t, dest, map[string]string{
		"generate-result.json":              "new",
		"sub/x.js":                          "x",
		"migration-bundle/bundle.json":      "bundle",
		"migration-bundle/payload/0001.sql": "create table t (id uuid);",
		"schema/openapi.json":               "converged",
	})
	assertNothingBeside(t, dest)

	// Control: the same entry without a keep list replaces the whole tree,
	// kept subtrees included, so the keep list is what saved them above.
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	assertTree(t, dest, map[string]string{"generate-result.json": "new", "sub/x.js": "x"})

	// A destination that is not a directory holds nothing to keep, and a keep
	// list must not turn the shapes the plain swap already handles into errors.
	for name, place := range map[string]func(string) error{
		"plain file": func(p string) error { return os.WriteFile(p, []byte("not a tree"), 0o644) },
		"symlink":    func(p string) error { return os.Symlink(t.TempDir(), p) },
	} {
		odd := filepath.Join(t.TempDir(), "gen")
		if err := place(odd); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MaterializeTaskOutput(entry, "dist", odd, "", "migration-bundle"); err != nil {
			t.Fatalf("%s at a kept destination: %v", name, err)
		}
		assertTree(t, odd, map[string]string{"generate-result.json": "new", "sub/x.js": "x"})
	}
}

// TestMaterializeTaskOutput_RefusesToMergeOverCapturedBytes pins the shapes a
// ceding restore must not resolve by itself: the entry holds bytes at a path
// the caller says is ceded, or a file where a ceded path's directory has to be.
// The two can only disagree when the entry was captured under another
// declaration, which the cache key rules out, so the answer is an error. It is
// found before dest is touched, so the other task's bytes stay exactly where
// they were — whether or not dest holds anything at the ceded path yet.
func TestMaterializeTaskOutput_RefusesToMergeOverCapturedBytes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry map[string]string
		live  map[string]string
		keep  string
	}{
		{"both hold the ceded path", map[string]string{"vendor/captured.js": "entry"}, map[string]string{"vendor/theirs.js": "theirs"}, "vendor"},
		{"only the entry holds it", map[string]string{"vendor/captured.js": "entry"}, map[string]string{"main.js": "old"}, "vendor"},
		{"a file on the way to it", map[string]string{"api": "a file"}, map[string]string{"api/v1/theirs.json": "theirs"}, "api/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewLocalStore(t.TempDir())
			entry := ingestTree(t, s, "keep-conflict-key", tc.entry)
			dest := filepath.Join(t.TempDir(), "gen")
			writeTree(t, dest, tc.live)
			before := snapshotInodes(t, dest)

			if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", tc.keep); !errors.Is(err, errKeptConflict) {
				t.Fatalf("error = %v, want errKeptConflict", err)
			}
			assertTree(t, dest, tc.live)
			assertSameInodes(t, dest, before)
			assertNothingBeside(t, dest)
		})
	}
}

// TestMaterializeTaskOutput_CedingOutputMergesAroundKeptPaths pins the merge
// that restores a ceding output: every entry the output owns is
// replaced, including across a change of type, every entry it no longer has is
// removed, and a kept path — at the top, nested under a directory the entry
// also writes into, or nested under one the entry lacks — is never touched. Its
// inodes prove it, and so does the destination's own. It runs on both
// directory swaps (forEachSwapPath).
func TestMaterializeTaskOutput_CedingOutputMergesAroundKeptPaths(t *testing.T) {
	forEachSwapPath(t, testCedingOutputMergesAroundKeptPaths)
}

func testCedingOutputMergesAroundKeptPaths(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "merge-key", map[string]string{
		"generate-result.json": "new",
		"dir-to-file":          "now a file",
		"file-to-dir/z.js":     "z",
		"link-to-dir/w.js":     "w",
		"m/owned.js":           "owned",
		"fresh/new.js":         "fresh",
	})
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"target.txt": "must survive"})

	parent := t.TempDir()
	dest := filepath.Join(parent, "gen")
	writeTree(t, dest, map[string]string{
		"generate-result.json":         "old",
		"dir-to-file/y.js":             "y",
		"file-to-dir":                  "was a file",
		"stale.txt":                    "stale",
		"stale-dir/x.js":               "stale",
		"a/b/kept.sql":                 "kept under a pruned parent",
		"a/c/stale.js":                 "stale under a kept parent",
		"m/n/kept.txt":                 "kept under an owned parent",
		"m/old.js":                     "stale beside an owned file",
		"migration-bundle/bundle.json": "bundle",
		".describe.lock":               "",
	})
	for link, target := range map[string]string{"stale-link": outside, "link-to-dir": outside} {
		if err := os.Symlink(target, filepath.Join(dest, link)); err != nil {
			t.Fatal(err)
		}
	}
	keep := []string{"migration-bundle", ".describe.lock", "a/b", "m/n", "x/y"}
	kept := snapshotInodes(t, dest, ".", "migration-bundle", ".describe.lock", "a", "a/b", "m/n")

	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", keep...); err != nil {
		t.Fatalf("merge: %v", err)
	}
	assertTree(t, dest, map[string]string{
		"generate-result.json":         "new",
		"dir-to-file":                  "now a file",
		"file-to-dir/z.js":             "z",
		"link-to-dir/w.js":             "w",
		"m/owned.js":                   "owned",
		"fresh/new.js":                 "fresh",
		"a/b/kept.sql":                 "kept under a pruned parent",
		"m/n/kept.txt":                 "kept under an owned parent",
		"migration-bundle/bundle.json": "bundle",
		".describe.lock":               "",
	})
	assertSameInodes(t, dest, kept)
	if data, err := os.ReadFile(filepath.Join(outside, "target.txt")); err != nil || string(data) != "must survive" {
		t.Errorf("a stale symlink's target was touched: %q (err=%v)", data, err)
	}
	assertNothingBeside(t, dest)
}

// TestMaterializeTaskOutput_CedingMergeFoldsCase pins that the merge
// recognizes a kept path under a spelling that differs from the declaration's
// only by case, at the top and nested, the way the capture skipped it
// (isExcludedOutputPath folds case). The capture side is why this matters on a
// case-sensitive disk too: the entry never holds the ceded bytes, so a merge
// that did not fold would prune the other task's subtree as stale. It runs on
// both directory swaps (forEachSwapPath).
func TestMaterializeTaskOutput_CedingMergeFoldsCase(t *testing.T) {
	forEachSwapPath(t, testCedingMergeFoldsCase)
}

func testCedingMergeFoldsCase(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "fold-key", map[string]string{"generate-result.json": "new", "sub/x.js": "x", "m/owned.js": "owned"})
	dest := filepath.Join(t.TempDir(), "gen")
	writeTree(t, dest, map[string]string{
		"generate-result.json":         "old",
		"sub/x.js":                     "old x",
		"migration-bundle/bundle.json": "bundle",
		"m/n/kept.txt":                 "kept under an owned parent",
		"m/old.js":                     "stale beside an owned file",
	})
	kept := snapshotInodes(t, dest, ".", "migration-bundle", "migration-bundle/bundle.json", "m/n", "m/n/kept.txt")

	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "Migration-Bundle", "M/N"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	assertTree(t, dest, map[string]string{
		"generate-result.json":         "new",
		"sub/x.js":                     "x",
		"m/owned.js":                   "owned",
		"migration-bundle/bundle.json": "bundle",
		"m/n/kept.txt":                 "kept under an owned parent",
	})
	assertSameInodes(t, dest, kept)
	assertNothingBeside(t, dest)
}

// TestMaterializeTaskOutput_CedingRestoreKeepsLateWrites pins the concurrent
// writer's half of the merge behavior. The ceded subtree's owner is not
// ordered against the ceding task across commands, so it may write into the
// subtree while the restore runs — here, in the middle of it, while an owned
// directory is being replaced. Every write survives and every kept inode stays the one the owner
// holds, including the run-scoped describe lock, because the restore never
// moves a kept path. It runs on both directory swaps (forEachSwapPath).
func TestMaterializeTaskOutput_CedingRestoreKeepsLateWrites(t *testing.T) {
	forEachSwapPath(t, testCedingRestoreKeepsLateWrites)
}

func testCedingRestoreKeepsLateWrites(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "late-write-key", map[string]string{"generate-result.json": "new", "sub/x.js": "x"})
	dest := filepath.Join(t.TempDir(), "gen")
	writeTree(t, dest, map[string]string{
		"generate-result.json":         "old",
		"sub/x.js":                     "old x",
		"migration-bundle/bundle.json": "bundle",
		"migrations.json":              "v1",
		".describe.lock":               "",
	})
	lock, err := os.Open(filepath.Join(dest, ".describe.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	kept := snapshotInodes(t, dest, ".", "migration-bundle", "migration-bundle/bundle.json", ".describe.lock")

	wrote := false
	saved := pathExchange
	pathExchange = func(a, b string) error {
		if !wrote {
			wrote = true
			// The owner, mid-restore: a new file in the kept directory, and a
			// kept file replaced the atomic way.
			writeTree(t, dest, map[string]string{"migration-bundle/0002.sql": "late", ".migrations.json.next": "v2"})
			if err := os.Rename(filepath.Join(dest, ".migrations.json.next"), filepath.Join(dest, "migrations.json")); err != nil {
				t.Error(err)
			}
		}
		return saved(a, b)
	}
	t.Cleanup(func() { pathExchange = saved })

	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "migration-bundle", "migrations.json", ".describe.lock"); err != nil {
		t.Fatalf("materialize keeping: %v", err)
	}
	if !wrote {
		t.Fatal("the restore never replaced an existing owned directory, so the late write never happened")
	}
	assertTree(t, dest, map[string]string{
		"generate-result.json":         "new",
		"sub/x.js":                     "x",
		"migration-bundle/bundle.json": "bundle",
		"migration-bundle/0002.sql":    "late",
		"migrations.json":              "v2",
		".describe.lock":               "",
	})
	assertSameInodes(t, dest, kept)
}

// writeTree creates files (slash-form paths relative to root) with content.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshotInodes records the identity of the named paths under root, or of
// every path under root when none are named.
func snapshotInodes(t *testing.T, root string, rels ...string) map[string]os.FileInfo {
	t.Helper()
	if len(rels) == 0 {
		err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			rels = append(rels, filepath.ToSlash(rel))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	infos := make(map[string]os.FileInfo, len(rels))
	for _, rel := range rels {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		infos[rel] = info
	}
	return infos
}

// assertSameInodes fails unless every recorded path still names the same inode.
func assertSameInodes(t *testing.T, root string, before map[string]os.FileInfo) {
	t.Helper()
	for _, rel := range sortedKeys(before) {
		now, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s is gone: %v", rel, err)
			continue
		}
		if !os.SameFile(before[rel], now) {
			t.Errorf("%s names another inode: the restore moved or replaced it", rel)
		}
	}
}

func sortedKeys(m map[string]os.FileInfo) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertNothingBeside fails when anything but dest sits in dest's parent: a
// staging tree, a replaced tree or a pruned entry that outlived the restore.
func assertNothingBeside(t *testing.T, dest string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(dest) {
			t.Errorf("leftover path beside the destination: %s", e.Name())
		}
	}
}

// forEachSwapPath runs test once per directory swap. "exchange" is the one-step
// path; on a temp filesystem that cannot exchange, the code falls back by
// itself and the subtest covers the fallback a second time, which is still a
// correct statement about what ships there. "two-rename fallback" forces the
// fallback on any filesystem.
func forEachSwapPath(t *testing.T, test func(t *testing.T)) {
	t.Helper()
	t.Run("exchange", test)
	t.Run("two-rename fallback", func(t *testing.T) {
		forceTwoRenameSwap(t)
		test(t)
	})
}

// forceTwoRenameSwap makes every exchange fail the way a host without one does,
// and fails the test if nothing ever asked for one — a fallback subtest that
// never reached the seam would prove nothing about the fallback. Not for
// parallel tests: it swaps a package variable.
func forceTwoRenameSwap(t *testing.T) {
	t.Helper()
	saved := pathExchange
	asked := 0
	pathExchange = func(a, b string) error {
		asked++
		return &os.LinkError{Op: "exchange", Old: a, New: b, Err: errors.ErrUnsupported}
	}
	t.Cleanup(func() {
		pathExchange = saved
		if asked == 0 {
			t.Error("the forced fallback was never asked for an exchange: the test did not reach a directory swap")
		}
	})
}

// exchangeSupported reports whether the temp filesystem can exchange two
// directories in one step.
func exchangeSupported(t *testing.T) error {
	t.Helper()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return exchangePaths(a, b)
}

// walkTreeByPath walks root the way the migration-bundle reader does —
// os.DirFS and fs.WalkDir, lexically, reading every file — and fails unless it
// finds exactly want. Every step resolves by path again, so a directory that is
// absent for an instant is an ENOENT, and a subtree that is absent is a partial
// tree.
func walkTreeByPath(root string, want map[string]string) error {
	got := map[string]string{}
	fsys := os.DirFS(root)
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		got[p] = string(data)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", root, err)
	}
	return sameTree(root, got, want)
}

// lookUpOwnedEntries reads files directly beneath the stable ceding root and
// checks owned directory names in that root. Even stat of a replaced directory
// can race reclamation after it resolves the old inode; listing the unchanged
// parent observes the namespace without acquiring a retired directory.
func lookUpOwnedEntries(root string, want map[string]string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list stable root %s: %w", root, err)
	}
	for _, rel := range keysOf(want) {
		name, _, nested := strings.Cut(rel, "/")
		p := filepath.Join(root, name)
		if nested {
			if !slices.ContainsFunc(entries, func(entry os.DirEntry) bool { return entry.Name() == name }) {
				return fmt.Errorf("owned directory %s is absent from its stable parent", p)
			}
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("look up under %s: %w", root, err)
		}
		if string(data) != want[rel] {
			return fmt.Errorf("read %s = %q, want %q", p, data, want[rel])
		}
	}
	return nil
}

func sameTree(root string, got, want map[string]string) error {
	if len(got) != len(want) {
		return fmt.Errorf("read %s found %v, want %v", root, keysOf(got), keysOf(want))
	}
	for rel, content := range want {
		if got[rel] != content {
			return fmt.Errorf("read %s: %s = %q, want %q", root, rel, got[rel], content)
		}
	}
	return nil
}

// TestMaterializeTaskOutput_ReadersNeverSeeACededGap is the acceptance test.
// While a ceding output is restored again and again, a reader walks the
// ceded subpath the way the migration-bundle reader does and must always find
// it complete, and every inode in that subtree must be the same after the loop
// as before: deterministic proof that no restore touched it, on both swaps.
// Where the exchange is in use, a second reader looks up the owned entries
// directly beneath the stable root and must never miss one; on the two-rename
// fallback an owned directory is briefly absent by design. Nested owned
// contents are checked after every restore, when their tree is no longer being
// replaced. Ceded contents are checked concurrently on both swap paths.
//
// It is bounded by the number of restores, never by wall-clock time. Against
// the whole-tree swap with a rename carry-over that this replaced, the ceded
// walker failed within the first few restores with the same ENOENT.
func TestMaterializeTaskOutput_ReadersNeverSeeACededGap(t *testing.T) {
	t.Run("exchange", func(t *testing.T) {
		if err := exchangeSupported(t); err != nil {
			t.Skipf("the temp filesystem cannot exchange two directories in one step (%v); "+
				"the two-rename subtest still pins the ceded subtree", err)
		}
		saved := pathExchange
		pathExchange = func(a, b string) error {
			err := saved(a, b)
			if err != nil {
				t.Errorf("exchange unexpectedly fell back: %v", err)
			}
			return err
		}
		t.Cleanup(func() { pathExchange = saved })
		testReadersNeverSeeACededGap(t, true)
	})
	t.Run("two-rename fallback", func(t *testing.T) {
		forceTwoRenameSwap(t)
		testReadersNeverSeeACededGap(t, false)
	})
}

func testReadersNeverSeeACededGap(t *testing.T, lookUpOwned bool) {
	const restores = 300

	s := NewLocalStore(t.TempDir())
	owned := map[string]string{"generate-result.json": "new", "sub/x.js": "x"}
	entry := ingestTree(t, s, "reader-gap-key", owned)
	bundle := map[string]string{
		"bundle.json":          "bundle",
		"payload/sql/0001.sql": "create table t (id uuid);",
		"payload/sql/platform_iam/platform-iam-bindings": "grant",
	}
	dest := filepath.Join(t.TempDir(), "gen")
	ceded := filepath.Join(dest, "migration-bundle")
	writeTree(t, ceded, bundle)
	whole := map[string]string{}
	for rel, content := range owned {
		whole[rel] = content
	}
	for rel, content := range bundle {
		whole["migration-bundle/"+rel] = content
	}
	// From here on every restore replaces the owned entries with identical
	// bytes, so any difference a reader sees is the restore's, never the
	// content's.
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "migration-bundle"); err != nil {
		t.Fatalf("seed restore: %v", err)
	}
	untouched := snapshotInodes(t, ceded)

	readers := []func() error{func() error { return walkTreeByPath(ceded, bundle) }}
	if lookUpOwned {
		readers = append(readers, func() error { return lookUpOwnedEntries(dest, owned) })
	}
	var stop atomic.Bool
	var running, warm sync.WaitGroup
	failures := make(chan error, len(readers))
	passes := make([]int, len(readers))
	for i, reader := range readers {
		running.Add(1)
		warm.Add(1)
		go func() {
			defer running.Done()
			for {
				err := reader()
				if passes[i]++; passes[i] == 1 {
					warm.Done()
				}
				if err != nil {
					failures <- err
					return
				}
				if stop.Load() {
					return
				}
			}
		}()
	}
	warm.Wait() // every reader is reading before the first restore starts

	var restoreErr error
	for i := range restores {
		if _, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "migration-bundle"); err != nil {
			restoreErr = fmt.Errorf("restore %d: %w", i, err)
			break
		}
		if err := walkTreeByPath(dest, whole); err != nil {
			restoreErr = fmt.Errorf("completed restore %d: %w", i, err)
			break
		}
	}
	stop.Store(true)
	running.Wait()
	close(failures)

	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	for err := range failures {
		t.Errorf("a reader saw the restore: %v", err)
	}
	assertSameInodes(t, ceded, untouched)
	t.Logf("%d restores; reader passes %v", restores, passes)
}
