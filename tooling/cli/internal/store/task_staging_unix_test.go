//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// ingestDirAndFile publishes an entry with one directory output ("dist") and
// one file output ("tool").
func ingestDirAndFile(t *testing.T, s *LocalStore, key string) *TaskEntry {
	t.Helper()
	staging := t.TempDir()
	dist := dirOutput("dist", "dist")
	tool := fileOutput("tool", "bin/tool")
	stage(t, staging, dist, "main.js", "built")
	stage(t, staging, dist, "sub/chunk.js", "chunk")
	stage(t, staging, tool, "", "#!/bin/sh\necho tool\n")
	entry, err := s.IngestTaskEntry(staging, taskSpec(key, dist, tool))
	if err != nil {
		t.Fatalf("IngestTaskEntry: %v", err)
	}
	return entry
}

// assertEmptyDir fails unless dir exists and holds nothing.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		t.Errorf("leftover in the staging root: %s", e.Name())
	}
}

// assertFile fails unless path is a regular file holding content.
func assertFile(t *testing.T, path, content string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != content {
		t.Fatalf("%s = %q, want %q", path, data, content)
	}
}

// strangersIn returns the names in dir other than allowed.
func strangersIn(dir string, allowed ...string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var strangers []string
	for _, e := range entries {
		if !slices.Contains(allowed, e.Name()) {
			strangers = append(strangers, e.Name())
		}
	}
	return strangers, nil
}

// watchSwapParents wraps pathExchange for the rest of the test. At the instant
// a directory swap starts, the staged tree is complete and still under its
// staging name, so that is when a staging path beside the destination would be
// visible. It records every directory the staged tree was swapped from and
// every stranger the destination's parent held at that instant.
func watchSwapParents(t *testing.T, allowed ...string) (stagedIn func() []string, strangers func() []string) {
	t.Helper()
	saved := pathExchange
	var dirs, seen []string
	pathExchange = func(staging, dest string) error {
		dirs = append(dirs, filepath.Dir(staging))
		names, err := strangersIn(filepath.Dir(dest), allowed...)
		if err != nil {
			t.Errorf("list the destination's parent at the swap: %v", err)
		}
		seen = append(seen, names...)
		return saved(staging, dest)
	}
	t.Cleanup(func() { pathExchange = saved })
	return func() []string { return dirs }, func() []string { return seen }
}

// TestMaterializeTaskOutput_StagingRootKeepsTheParentClean pins that a restore
// with a staging root on the destination's filesystem creates nothing in the
// destination's parent but the destination: no staging path, no replaced
// tree, at any instant. A task that walks that directory while another task's
// output is restored into it must never meet an entry that appears and
// vanishes. A watcher lists the parent concurrently through every restore of a
// directory output and a file output, on both directory swaps, and every
// directory swap is checked at the instant it starts. The staging root is left
// empty.
func TestMaterializeTaskOutput_StagingRootKeepsTheParentClean(t *testing.T) {
	forEachSwapPath(t, testStagingRootKeepsTheParentClean)
}

func testStagingRootKeepsTheParentClean(t *testing.T) {
	const restores = 200

	s := NewLocalStore(t.TempDir())
	entry := ingestDirAndFile(t, s, "staging-root-key")
	parent := t.TempDir()
	dirDest := filepath.Join(parent, "dist")
	fileDest := filepath.Join(parent, "tool")
	// Not created in advance: the restore creates the staging root.
	stagingRoot := filepath.Join(t.TempDir(), ".putnami", "cache")
	stagedIn, swapStrangers := watchSwapParents(t, "dist", "tool")

	var stop atomic.Bool
	var listings atomic.Int64
	var watcher sync.WaitGroup
	warm := make(chan struct{})
	failures := make(chan error, 1)
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		for !stop.Load() {
			names, err := strangersIn(parent, "dist", "tool")
			if listings.Add(1) == 1 {
				close(warm)
			}
			if err == nil && len(names) > 0 {
				err = fmt.Errorf("the destination's parent held %v during a restore", names)
			}
			if err != nil {
				failures <- err
				return
			}
		}
	}()
	<-warm // the watcher is listing before the first restore starts

	var restoreErr error
	for i := range restores {
		if _, err := s.MaterializeTaskOutput(entry, "dist", dirDest, stagingRoot); err != nil {
			restoreErr = fmt.Errorf("restore %d of dist: %w", i, err)
			break
		}
		if _, err := s.MaterializeTaskOutput(entry, "tool", fileDest, stagingRoot); err != nil {
			restoreErr = fmt.Errorf("restore %d of tool: %w", i, err)
			break
		}
	}
	stop.Store(true)
	watcher.Wait()
	close(failures)

	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	for err := range failures {
		t.Error(err)
	}
	if names := swapStrangers(); len(names) > 0 {
		t.Errorf("the destination's parent held %v when a directory swap started", names)
	}
	dirs := stagedIn()
	if len(dirs) == 0 {
		t.Fatal("no restore swapped over an existing directory: the swap-instant check proves nothing")
	}
	for _, dir := range dirs {
		if dir != stagingRoot {
			t.Fatalf("a directory was staged in %s, want the staging root %s", dir, stagingRoot)
		}
	}
	assertTree(t, dirDest, map[string]string{"main.js": "built", "sub/chunk.js": "chunk"})
	assertFile(t, fileDest, "#!/bin/sh\necho tool\n")
	assertEmptyDir(t, stagingRoot)
	t.Logf("%d restores; %d concurrent listings of the parent", restores, listings.Load())
}

// TestMaterializeTaskOutput_BesideDestinationStagingIsVisible is the control
// of the swap-instant check above: without a staging root, the staged tree
// sits in the destination's parent when the swap starts, so the check does see
// a staging path when there is one.
func TestMaterializeTaskOutput_BesideDestinationStagingIsVisible(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestDirAndFile(t, s, "beside-control-key")
	dest := filepath.Join(t.TempDir(), "dist")
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
		t.Fatal(err)
	}
	stagedIn, strangers := watchSwapParents(t, "dist")
	if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
		t.Fatal(err)
	}
	if dirs := stagedIn(); len(dirs) != 1 || dirs[0] != filepath.Dir(dest) {
		t.Fatalf("staged in %v, want only the destination's parent", dirs)
	}
	seen := strangers()
	if len(seen) == 0 {
		t.Fatal("a restore staged beside its destination left no trace at the swap instant")
	}
	for _, name := range seen {
		if !strings.Contains(name, ".tmp-materialize-") {
			t.Errorf("unexpected entry beside the destination: %s", name)
		}
	}
	assertNothingBeside(t, dest)
}

// TestMaterializeTaskOutput_StagingRootKeepsCededSubtree is the ceded-subtree
// restore (testCededSubtreeSurvivesTheSwap) with a staging root: the merge
// swaps each owned entry in from the staging root and prunes stale entries
// into it, the ceded subtree keeps every inode, nothing appears beside the
// destination, and the staging root is left empty.
func TestMaterializeTaskOutput_StagingRootKeepsCededSubtree(t *testing.T) {
	forEachSwapPath(t, func(t *testing.T) {
		s := NewLocalStore(t.TempDir())
		entry := ingestTree(t, s, "staging-root-keep-key", map[string]string{"generate-result.json": "new", "sub/x.js": "x"})
		dest := filepath.Join(t.TempDir(), "gen")
		stagingRoot := filepath.Join(t.TempDir(), "cache")
		writeTree(t, dest, map[string]string{
			"generate-result.json":              "old",
			"sub/x.js":                          "old x",
			"stale/leftover.js":                 "stale",
			"migration-bundle/bundle.json":      "bundle",
			"migration-bundle/payload/0001.sql": "create table t (id uuid);",
		})
		ceded := filepath.Join(dest, "migration-bundle")
		untouched := snapshotInodes(t, ceded)

		written, err := s.MaterializeTaskOutput(entry, "dist", dest, stagingRoot, "migration-bundle")
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
		})
		assertSameInodes(t, ceded, untouched)
		assertNothingBeside(t, dest)
		assertEmptyDir(t, stagingRoot)
	})
}

// TestMaterializeTaskOutput_StagingRootInsideDestinationStagesBeside pins
// that a staging root inside the destination is not used: the staged tree
// could not be renamed over a directory that contains it. The restore stages
// beside the destination instead and does not create the staging root.
func TestMaterializeTaskOutput_StagingRootInsideDestinationStagesBeside(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry := ingestTree(t, s, "staging-root-inside-key", map[string]string{"main.js": "built"})
	dest := filepath.Join(t.TempDir(), "ws")
	writeTree(t, dest, map[string]string{"old.js": "old"})
	for _, stagingRoot := range []string{dest, filepath.Join(dest, ".putnami", "cache")} {
		if _, err := s.MaterializeTaskOutput(entry, "dist", dest, stagingRoot); err != nil {
			t.Fatalf("staging root %s inside the destination: %v", stagingRoot, err)
		}
		assertTree(t, dest, map[string]string{"main.js": "built"})
		assertNothingBeside(t, dest)
		if _, err := os.Lstat(filepath.Join(dest, ".putnami")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the restore created a staging root inside its destination: %v", err)
		}
	}
}

// TestMaterializeTaskOutput_CrossMountSwapRestagesBeside pins the fallback
// for a staging root that shares a device with the destination but not a
// mount (two mounts of one filesystem): the rename from the staging root fails
// with EXDEV, swapIn returns it at once instead of retrying into a later error,
// and the restore is published again from beside the destination. Any other
// rename failure is returned without restaging. A test cannot mount, so the
// rename out of the staging root is made to fail the way the kernel fails it.
// The destinations do not exist yet: that is the case where swapIn's retry loop
// would otherwise end on the exchange's ENOENT.
func TestMaterializeTaskOutput_CrossMountSwapRestagesBeside(t *testing.T) {
	for name, tc := range map[string]struct {
		errno   syscall.Errno
		restage bool
	}{
		"EXDEV restages beside":  {errno: syscall.EXDEV, restage: true},
		"EACCES is not restaged": {errno: syscall.EACCES, restage: false},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewLocalStore(t.TempDir())
			entry := ingestDirAndFile(t, s, "cross-mount-key")
			parent := t.TempDir()
			stagingRoot := filepath.Join(t.TempDir(), "cache")

			saved := renameStaged
			var fromRoot, beside int
			renameStaged = func(staging, dest string) error {
				if filepath.Dir(staging) == stagingRoot {
					fromRoot++
					return &os.LinkError{Op: "rename", Old: staging, New: dest, Err: tc.errno}
				}
				beside++
				return saved(staging, dest)
			}
			t.Cleanup(func() { renameStaged = saved })

			for _, id := range []string{"dist", "tool"} {
				_, err := s.MaterializeTaskOutput(entry, id, filepath.Join(parent, id), stagingRoot)
				if tc.restage && err != nil {
					t.Fatalf("restore %s: %v", id, err)
				}
				if !tc.restage && err == nil {
					t.Fatalf("restore %s succeeded, want the rename's %v", id, tc.errno)
				}
			}
			wantBeside := 0
			if tc.restage {
				wantBeside = 2
				assertTree(t, filepath.Join(parent, "dist"), map[string]string{"main.js": "built", "sub/chunk.js": "chunk"})
				assertFile(t, filepath.Join(parent, "tool"), "#!/bin/sh\necho tool\n")
			}
			if fromRoot == 0 || beside != wantBeside {
				t.Errorf("renames from the staging root = %d, beside = %d; want some and %d", fromRoot, beside, wantBeside)
			}
			assertEmptyDir(t, stagingRoot)
			if names, err := strangersIn(parent, "dist", "tool"); err != nil || len(names) > 0 {
				t.Errorf("leftover beside the destinations: %v (err=%v)", names, err)
			}
		})
	}
}

// TestSameFilesystem pins the device comparison that selects the staging
// root: two directories of one temp filesystem compare equal, a directory on
// another filesystem or a path that cannot be inspected never does, and a
// staging root on another filesystem falls back to the destination's parent.
func TestSameFilesystem(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if !sameFilesystem(a, b) {
		t.Errorf("sameFilesystem(%s, %s) = false, want true", a, b)
	}
	if sameFilesystem(a, filepath.Join(b, "missing")) {
		t.Error("a missing path compared equal")
	}
	// /dev is devfs on every unix host this runs on, never the temp filesystem.
	if sameFilesystem(a, "/dev") {
		t.Errorf("sameFilesystem(%s, /dev) = true, want false", a)
	}
	// A staging root on another filesystem is not used: the restore stages
	// beside its destination.
	if got, dest := stagingDirFor(filepath.Join(a, "out"), "/dev"), a; got != dest {
		t.Errorf("stagingDirFor with a root on another filesystem = %s, want %s", got, dest)
	}
	if !crossDevice(fmt.Errorf("wrapped: %w", &os.LinkError{Op: "rename", Err: syscall.EXDEV})) {
		t.Error("a wrapped EXDEV is not reported as cross-device")
	}
	if crossDevice(nil) || crossDevice(syscall.ENOENT) {
		t.Error("an error other than EXDEV is reported as cross-device")
	}
}
