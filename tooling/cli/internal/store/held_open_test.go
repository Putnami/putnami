package store

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// waitOnHeld applies a holderPolicy for the rest of the test: held says which
// errors mean another process holds the path, and budget bounds the wait. Not
// for parallel tests: it swaps a package variable.
func waitOnHeld(t *testing.T, held func(error) bool, budget time.Duration) {
	t.Helper()
	saved := heldOpenPolicy
	heldOpenPolicy = holderPolicy{held: held, budget: budget}
	t.Cleanup(func() { heldOpenPolicy = saved })
}

// heldByTest classifies the failure these tests inject (errHeldOpen) as a held
// handle, beside every error the host itself reports as one: a scanner that
// holds the staged tree on Windows is waited out as in production.
func heldByTest(err error) bool { return errors.Is(err, errHeldOpen) || hostHeldOpen(err) }

// neverHeld is the policy of a host where no error means a held handle.
func neverHeld(error) bool { return false }

// failStagedRenames replaces renameStaged for the rest of the test: the first
// n renames of a staged path fail with errHeldOpen, and later ones rename. It
// returns the number of renames asked for so far. Not for parallel tests: it
// swaps a package variable.
func failStagedRenames(t *testing.T, n int) *int {
	t.Helper()
	saved := renameStaged
	calls := 0
	renameStaged = func(staging, dest string) error {
		calls++
		if calls <= n {
			return &os.LinkError{Op: "rename", Old: staging, New: dest, Err: errHeldOpen}
		}
		return saved(staging, dest)
	}
	t.Cleanup(func() { renameStaged = saved })
	return &calls
}

// assertOnlyEntries fails unless dir holds exactly the named entries: no
// staged tree and no detached copy is left beside a destination.
func assertOnlyEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("%s holds %v, want %v", dir, got, names)
	}
}

// TestMaterializeTaskOutputWaitsOutAHeldDirectory pins F4: a directory swap
// whose renames fail because another process holds a handle in the tree keeps
// retrying, paced, after materializeSwapAttempts rounds, and publishes once
// the handle closes. A host whose policy reports no holder still gives up
// after materializeSwapAttempts rounds, with the held error and the previous
// tree intact.
func TestMaterializeTaskOutputWaitsOutAHeldDirectory(t *testing.T) {
	want := map[string]string{"main.js": "built", "sub/chunk.js": "chunk"}
	stale := map[string]string{"stale.js": "old"}
	for name, existing := range map[string]bool{"absent destination": false, "existing destination": true} {
		// A round of the two-rename swap renames the staged tree once when
		// the destination is absent, and twice when it exists (the direct
		// rename, then the rename after the detach).
		perRound := 1
		if existing {
			perRound = 2
		}
		failures := (materializeSwapAttempts + 3) * perRound

		setup := func(t *testing.T) (*LocalStore, *TaskEntry, string) {
			s := NewLocalStore(t.TempDir())
			entry := ingestTree(t, s, "held-directory-key", want)
			dest := filepath.Join(t.TempDir(), "out")
			if existing {
				writeTree(t, dest, stale)
			}
			forceTwoRenameSwap(t)
			return s, entry, dest
		}

		t.Run(name+"/paced", func(t *testing.T) {
			s, entry, dest := setup(t)
			waitOnHeld(t, heldByTest, time.Minute)
			calls := failStagedRenames(t, failures)

			if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
				t.Fatalf("materialize while a holder keeps the tree: %v", err)
			}
			if *calls <= failures {
				t.Fatalf("renames asked = %d, want more than the %d held ones", *calls, failures)
			}
			assertTree(t, dest, want)
			assertOnlyEntries(t, filepath.Dir(dest), "out")
		})

		t.Run(name+"/never held", func(t *testing.T) {
			s, entry, dest := setup(t)
			waitOnHeld(t, neverHeld, time.Minute)
			failStagedRenames(t, failures)

			_, err := s.MaterializeTaskOutput(entry, "dist", dest, "")
			if err == nil || !strings.Contains(err.Error(), "stayed contended") || !errors.Is(err, errHeldOpen) {
				t.Fatalf("materialize = %v, want a contended swap carrying the held error", err)
			}
			if existing {
				assertTree(t, dest, stale)
				assertOnlyEntries(t, filepath.Dir(dest), "out")
			} else {
				assertOnlyEntries(t, filepath.Dir(dest))
			}
		})
	}
}

// TestMaterializeTaskOutputOutlastsAPeerThatKeepsPublishing pins what the
// held-handle budget measures in a directory swap. Windows answers a rename
// onto a directory that another materializer just refilled with the error a
// held handle gives. A peer that keeps publishing new trees at dest is making
// progress, so the swap keeps waiting for as long as it does, even past the
// budget, and publishes once it stops. A holder that never lets go and leaves
// dest unchanged still ends the wait at the budget, and the failure names the
// path, how long it stayed contended, and how many trees other publishers put
// there.
func TestMaterializeTaskOutputOutlastsAPeerThatKeepsPublishing(t *testing.T) {
	const budget = 50 * time.Millisecond
	want := map[string]string{"main.js": "built", "sub/chunk.js": "chunk"}
	setup := func(t *testing.T) (*LocalStore, *TaskEntry, string) {
		s := NewLocalStore(t.TempDir())
		entry := ingestTree(t, s, "peer-publishing-key", want)
		dest := filepath.Join(t.TempDir(), "out")
		writeTree(t, dest, map[string]string{"stale.js": "old"})
		forceTwoRenameSwap(t)
		waitOnHeld(t, heldByTest, budget)
		return s, entry, dest
	}

	t.Run("a peer publishing past the budget", func(t *testing.T) {
		s, entry, dest := setup(t)
		// Each staged rename during the peer's run finds a new tree at dest
		// and fails as Windows fails it. Replaced trees stay alive in retired,
		// so a new tree never reuses the identity of the one it replaced.
		retired := t.TempDir()
		saved := renameStaged
		var peerStart time.Time
		published := 0
		renameStaged = func(staging, to string) error {
			if peerStart.IsZero() {
				peerStart = time.Now()
			}
			if time.Since(peerStart) >= 3*budget {
				return saved(staging, to)
			}
			published++
			if err := os.Rename(to, filepath.Join(retired, strconv.Itoa(published))); err != nil && !os.IsNotExist(err) {
				t.Fatalf("peer: retire dest: %v", err)
			}
			writeTree(t, to, map[string]string{"peer.js": strconv.Itoa(published)})
			return &os.LinkError{Op: "rename", Old: staging, New: to, Err: errHeldOpen}
		}
		t.Cleanup(func() { renameStaged = saved })

		if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
			t.Fatalf("materialize while a peer kept publishing for 3 budgets: %v", err)
		}
		assertTree(t, dest, want)
		assertOnlyEntries(t, filepath.Dir(dest), "out")
	})

	t.Run("a holder that never lets go", func(t *testing.T) {
		s, entry, dest := setup(t)
		failStagedRenames(t, math.MaxInt)

		_, err := s.MaterializeTaskOutput(entry, "dist", dest, "")
		if err == nil || !errors.Is(err, errHeldOpen) {
			t.Fatalf("materialize = %v, want the held error once the budget is spent", err)
		}
		for _, part := range []string{dest + " stayed contended for ", " rounds", "other publishers replaced it 0 times"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("materialize = %q, want it to contain %q", err, part)
			}
		}
		assertTree(t, dest, map[string]string{"stale.js": "old"})
		assertOnlyEntries(t, filepath.Dir(dest), "out")
	})
}

// TestMaterializeTaskOutputWaitsOutAHeldFile pins F4 for a file output: the
// rename that replaces the file keeps retrying, paced, while the policy
// reports a holder, and fails at once when it does not.
func TestMaterializeTaskOutputWaitsOutAHeldFile(t *testing.T) {
	const content = "#!/bin/sh\necho tool\n"
	const failures = 4
	setup := func(t *testing.T) (*LocalStore, *TaskEntry, string) {
		s := NewLocalStore(t.TempDir())
		staging := t.TempDir()
		tool := fileOutput("tool", "bin/tool")
		stage(t, staging, tool, "", content)
		entry, err := s.IngestTaskEntry(staging, taskSpec("held-file-key", tool))
		if err != nil {
			t.Fatalf("IngestTaskEntry: %v", err)
		}
		dest := filepath.Join(t.TempDir(), "tool")
		if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		return s, entry, dest
	}

	t.Run("paced", func(t *testing.T) {
		s, entry, dest := setup(t)
		waitOnHeld(t, heldByTest, time.Minute)
		calls := failStagedRenames(t, failures)

		if _, err := s.MaterializeTaskOutput(entry, "tool", dest, ""); err != nil {
			t.Fatalf("materialize while a holder keeps the file: %v", err)
		}
		if *calls != failures+1 {
			t.Errorf("renames asked = %d, want %d", *calls, failures+1)
		}
		if data, err := os.ReadFile(dest); err != nil || string(data) != content {
			t.Errorf("published file = %q, %v; want %q", data, err, content)
		}
		assertOnlyEntries(t, filepath.Dir(dest), "tool")
	})

	t.Run("never held", func(t *testing.T) {
		s, entry, dest := setup(t)
		waitOnHeld(t, neverHeld, time.Minute)
		failStagedRenames(t, failures)

		_, err := s.MaterializeTaskOutput(entry, "tool", dest, "")
		if err == nil || !strings.Contains(err.Error(), "publish output file") || !errors.Is(err, errHeldOpen) {
			t.Fatalf("materialize = %v, want the held error of the file publish", err)
		}
		if data, err := os.ReadFile(dest); err != nil || string(data) != "old" {
			t.Errorf("file after a failed publish = %q, %v; want the previous bytes", data, err)
		}
		assertOnlyEntries(t, filepath.Dir(dest), "tool")
	})
}

// TestHolderWaitGivesUpWhenTheBudgetIsSpent pins the bound of the wait: a
// holder that never closes costs at most the budget, and an error that does
// not mean a held handle is never retried.
func TestHolderWaitGivesUpWhenTheBudgetIsSpent(t *testing.T) {
	const budget = 50 * time.Millisecond
	waitOnHeld(t, heldByTest, budget)

	var wait holderWait
	start := time.Now()
	retries := 0
	for wait.retry(errHeldOpen) {
		retries++
	}
	if elapsed := time.Since(start); elapsed > 10*budget {
		t.Errorf("the wait took %v, want about %v", elapsed, budget)
	}
	if retries == 0 {
		t.Error("the wait never retried a held error within its budget")
	}

	var other holderWait
	if other.retry(errors.New("unrelated")) || other.retry(nil) {
		t.Error("the wait retried an error that does not mean a held handle")
	}
}
