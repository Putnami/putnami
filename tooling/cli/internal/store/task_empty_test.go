package store

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// pathsUnder returns every path under root, directories included, in slash
// form and sorted. assertTree lists files only, and an emptied output must
// hold no leftover directory either.
func pathsUnder(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel != "." {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(paths)
	return paths
}

// assertPaths fails unless root holds exactly the named paths.
func assertPaths(t *testing.T, root string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := pathsUnder(t, root); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s holds\n  %s\nwant\n  %s", root, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// failStaleRenames replaces renameStale for the rest of the test. fail decides,
// from the directory that holds the private directory an entry would move
// into, the error a rename returns; a nil answer renames. It returns the number
// of renames asked for per such directory. Not for parallel tests: it swaps a
// package variable.
func failStaleRenames(t *testing.T, fail func(holder string) error) map[string]int {
	t.Helper()
	saved := renameStale
	asked := map[string]int{}
	renameStale = func(from, to string) error {
		holder := filepath.Dir(filepath.Dir(to))
		asked[holder]++
		if err := fail(holder); err != nil {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
		}
		return saved(from, to)
	}
	t.Cleanup(func() { renameStale = saved })
	return asked
}

// An emptied output holds what the task does not own and nothing else. Every
// kept path keeps its inode, as does a directory on the way to one and the
// output directory itself: nothing the task does not own is moved, copied or
// rewritten. A kept path is recognized under the spelling the disk holds.
func TestEmptyTaskOutput_RemovesOwnedEntriesAndKeepsTheRest(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "executed-directory-output-starts-empty",
		"ceded-and-preserved-paths-and-the-stamp-stay-untouched")
	for name, stagingRoot := range map[string]func(t *testing.T) string{
		"staging root":      func(t *testing.T) string { return filepath.Join(t.TempDir(), "scratch") },
		"beside the output": func(*testing.T) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "gen")
			writeTree(t, dest, map[string]string{
				"routes.json":                 "stale",
				"public/static/deleted.html":  "stale",
				"public/react-islands/a.js":   "stale",
				"migration-bundle/0001.sql":   "ceded",
				"version.json":                "stamp",
				"schema/config/app.json":      "ceded",
				"nested/keep/handwritten.txt": "preserved",
				"nested/stale.txt":            "stale",
				"nested/deep/stale.txt":       "stale",
			})
			if err := os.Mkdir(filepath.Join(dest, "empty-stale-dir"), 0o755); err != nil {
				t.Fatal(err)
			}
			keep := []string{"Migration-Bundle", "version.json", "schema", "nested/keep", "absent/never-written"}
			kept := snapshotInodes(t, dest, ".", "migration-bundle", "migration-bundle/0001.sql",
				"version.json", "schema", "schema/config/app.json", "nested", "nested/keep",
				"nested/keep/handwritten.txt")
			root := stagingRoot(t)

			if err := EmptyTaskOutput(dest, root, keep...); err != nil {
				t.Fatalf("EmptyTaskOutput: %v", err)
			}

			assertPaths(t, dest,
				"migration-bundle", "migration-bundle/0001.sql",
				"version.json",
				"schema", "schema/config", "schema/config/app.json",
				"nested", "nested/keep", "nested/keep/handwritten.txt",
			)
			assertTree(t, dest, map[string]string{
				"migration-bundle/0001.sql":   "ceded",
				"version.json":                "stamp",
				"schema/config/app.json":      "ceded",
				"nested/keep/handwritten.txt": "preserved",
			})
			assertSameInodes(t, dest, kept)
			assertNothingBeside(t, dest)
			if root != "" {
				assertPaths(t, root)
			}
		})
	}
}

// With nothing kept, the output directory stays and holds nothing: a task that
// expects its output directory to exist finds it.
func TestEmptyTaskOutput_LeavesAnEmptyDirectory(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dist")
	writeTree(t, dest, map[string]string{"main.js": "old", "sub/chunk.js": "old", ".hidden": "old"})
	self := snapshotInodes(t, dest, ".")

	if err := EmptyTaskOutput(dest, ""); err != nil {
		t.Fatalf("EmptyTaskOutput: %v", err)
	}
	assertPaths(t, dest)
	assertSameInodes(t, dest, self)
	assertNothingBeside(t, dest)

	// Emptying an empty directory changes nothing.
	if err := EmptyTaskOutput(dest, ""); err != nil {
		t.Fatalf("EmptyTaskOutput of an empty directory: %v", err)
	}
	assertPaths(t, dest)
	assertSameInodes(t, dest, self)
}

// An absent output is left absent, and nothing is created for it: not the
// output, not a staging root, not a directory beside it.
func TestEmptyTaskOutput_AbsentOutputStaysAbsent(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "gen")
	stagingRoot := filepath.Join(t.TempDir(), "scratch")

	if err := EmptyTaskOutput(dest, stagingRoot, "migration-bundle"); err != nil {
		t.Fatalf("EmptyTaskOutput: %v", err)
	}
	assertPaths(t, parent)
	if _, err := os.Lstat(stagingRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging root was created for an absent output: %v", err)
	}
}

// A symlink or a file where the directory output belongs holds nothing the
// task owns as a directory. The entry itself is removed and a link is never
// followed: what it points at keeps every file.
func TestEmptyTaskOutput_RemovesALinkOrAFileWithoutFollowingIt(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		target := t.TempDir()
		writeTree(t, target, map[string]string{"blob.js": "shared bytes", "sub/blob.js": "shared bytes"})
		dest := filepath.Join(t.TempDir(), "build")
		if err := os.Symlink(target, dest); err != nil {
			t.Fatal(err)
		}

		if err := EmptyTaskOutput(dest, "", "sub"); err != nil {
			t.Fatalf("EmptyTaskOutput: %v", err)
		}
		if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the link is still there: %v", err)
		}
		assertTree(t, target, map[string]string{"blob.js": "shared bytes", "sub/blob.js": "shared bytes"})
		assertPaths(t, filepath.Dir(dest))
	})

	t.Run("file", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "build")
		if err := os.WriteFile(dest, []byte("not a tree"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := EmptyTaskOutput(dest, ""); err != nil {
			t.Fatalf("EmptyTaskOutput: %v", err)
		}
		assertPaths(t, filepath.Dir(dest))
	})
}

// A rename that fails for a reason no fallback answers is returned, wrapped so
// the caller reads which output could not be emptied. The entries it could not
// move are still there, which is why the caller must not run or capture.
func TestEmptyTaskOutput_ReturnsTheFailureAndLeavesTheEntries(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "executed-directory-output-starts-empty",
		"a-failure-to-empty-fails-the-task-before-it-runs")
	dest := filepath.Join(t.TempDir(), "gen")
	writeTree(t, dest, map[string]string{"stale.html": "stale", "kept/a.txt": "kept"})
	denied := errors.New("permission denied by the test")
	failStaleRenames(t, func(string) error { return denied })

	err := EmptyTaskOutput(dest, "", "kept")
	if err == nil || !errors.Is(err, denied) {
		t.Fatalf("EmptyTaskOutput = %v, want the rename's failure", err)
	}
	for _, want := range []string{"empty output directory", "remove stale", "stale.html"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	assertTree(t, dest, map[string]string{"stale.html": "stale", "kept/a.txt": "kept"})
	assertNothingBeside(t, dest)
}

// A rename that fails because another process holds a handle inside the entry
// is waited out, as a restore waits it out, and the output ends up empty. A
// host whose policy reports no holder returns the failure at once.
func TestEmptyTaskOutput_WaitsOutAHeldEntry(t *testing.T) {
	setup := func(t *testing.T) (string, *int) {
		dest := filepath.Join(t.TempDir(), "gen")
		writeTree(t, dest, map[string]string{"held/page.html": "stale", "kept/a.txt": "kept"})
		failures := 0
		failStaleRenames(t, func(string) error {
			if failures < 4 {
				failures++
				return errHeldOpen
			}
			return nil
		})
		return dest, &failures
	}

	t.Run("waited out", func(t *testing.T) {
		dest, failures := setup(t)
		waitOnHeld(t, heldByTest, time.Minute)

		if err := EmptyTaskOutput(dest, "", "kept"); err != nil {
			t.Fatalf("EmptyTaskOutput while a holder keeps an entry: %v", err)
		}
		if *failures != 4 {
			t.Fatalf("held renames = %d, want 4", *failures)
		}
		assertPaths(t, dest, "kept", "kept/a.txt")
		assertNothingBeside(t, dest)
	})

	t.Run("never held", func(t *testing.T) {
		dest, failures := setup(t)
		waitOnHeld(t, neverHeld, time.Minute)

		err := EmptyTaskOutput(dest, "", "kept")
		if err == nil || !errors.Is(err, errHeldOpen) {
			t.Fatalf("EmptyTaskOutput = %v, want the held error", err)
		}
		if *failures != 1 {
			t.Fatalf("renames asked = %d, want one and no retry", *failures)
		}
		assertTree(t, dest, map[string]string{"held/page.html": "stale", "kept/a.txt": "kept"})
		assertNothingBeside(t, dest)
	})
}

// The restore keeps its error text: the prune it shares with the emptying
// reports a stale entry it cannot move under the restore's own prefix.
func TestMaterializeTaskOutput_PruneFailureKeepsTheRestoreMessage(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "prune-message-key", map[string]string{"main.js": "new"})
	dest := filepath.Join(t.TempDir(), "gen")
	writeTree(t, dest, map[string]string{"stale.js": "old", "migration-bundle/bundle.json": "bundle"})
	denied := errors.New("permission denied by the test")
	failStaleRenames(t, func(string) error { return denied })

	_, err := s.MaterializeTaskOutput(entry, "dist", dest, "", "migration-bundle")
	if err == nil || !errors.Is(err, denied) {
		t.Fatalf("MaterializeTaskOutput = %v, want the rename's failure", err)
	}
	if !strings.Contains(err.Error(), "publish output directory: remove stale ") {
		t.Errorf("error %q lost the restore's prefix", err)
	}
	if strings.Count(err.Error(), "publish output directory") != 1 {
		t.Errorf("error %q names the restore twice", err)
	}
}

// An empty list of emptied outputs writes zero bytes, so a task with no such
// output keeps its v9 address. A list inserts exactly "emptiedOutputs\0" and
// its ids, sorted, after the source-state block and before the task, and
// nothing else moves. No key that carries the marker equals a key computed
// without it, so an entry stored by an engine that emptied nothing is never
// found under a key this engine computes for a task it empties.
func TestCacheKey_EmptiedOutputsAddOnlyTheirMarker(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "executed-directory-output-starts-empty",
		"an-entry-stored-without-emptying-is-never-served")
	cm := NewCacheManager(nil)

	for name, none := range map[string][]string{"nil": nil, "empty": {}} {
		key := pinnedFormatKey()
		key.EmptiedOutputs = none
		got, err := key.ComputeHashUsing(cm)
		if err != nil {
			t.Fatal(err)
		}
		if got != pinnedPOSIXKeyHash {
			t.Fatalf("a %s list of emptied outputs moved the key: got %s, want %s", name, got, pinnedPOSIXKeyHash)
		}
	}

	emptied := pinnedFormatKey()
	emptied.EmptiedOutputs = []string{"gen", "dist"}
	got, err := emptied.ComputeHashUsing(cm)
	if err != nil {
		t.Fatal(err)
	}
	if want := pinnedFormatPreimageDigest("emptiedOutputs", "dist", "gen"); got != want {
		t.Fatalf("the key is not the preimage plus the sorted emptiedOutputs marker: got %s, want %s", got, want)
	}
	if got == pinnedPOSIXKeyHash {
		t.Fatal("a key with emptied outputs equals the key without them")
	}
	if strings.Join(emptied.EmptiedOutputs, ",") != "gen,dist" {
		t.Fatalf("hashing reordered the caller's list: %v", emptied.EmptiedOutputs)
	}

	reordered := pinnedFormatKey()
	reordered.EmptiedOutputs = []string{"dist", "gen"}
	if other, err := reordered.ComputeHashUsing(cm); err != nil || other != got {
		t.Fatalf("the order of the ids moved the key: %s != %s (err=%v)", other, got, err)
	}

	one := pinnedFormatKey()
	one.EmptiedOutputs = []string{"gen"}
	if other, err := one.ComputeHashUsing(cm); err != nil || other == got {
		t.Fatalf("a different set of emptied outputs kept the key: %s (err=%v)", other, err)
	}

	all := pinnedFormatKey()
	all.OSClass = OSClassWindows
	all.SourceState = SourceStateUnmanaged
	all.EmptiedOutputs = []string{"gen"}
	got, err = all.ComputeHashUsing(cm)
	if err != nil {
		t.Fatal(err)
	}
	want := pinnedFormatPreimageDigest("osClass", "windows", "sourceState", "unmanaged", "emptiedOutputs", "gen")
	if got != want {
		t.Fatalf("the emptiedOutputs marker is not written after the source state: got %s, want %s", got, want)
	}
}
