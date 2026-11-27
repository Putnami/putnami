package machineproc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/gitread"
)

// armMemory makes w the worker's watcher and arms the memory of a run in
// root with run, for the test. A run given a check is offered under the
// decision's state taken now.
func armMemory(t *testing.T, w *Watcher, root string, run runMemory) {
	t.Helper()
	run.armed, run.root = true, root
	if run.check != nil {
		syncPoint(t, w, root)
		run.check = &memoryCheck{root: root, seq: w.Since(root, ^uint64(0)).Seq, witness: outsideWitness(root)}
	}
	memory.Lock()
	previous := memory.watcher
	memory.watcher, memory.run = w, run
	memory.Unlock()
	t.Cleanup(func() {
		memory.Lock()
		memory.watcher, memory.run = previous, runMemory{}
		memory.Unlock()
	})
}

// TestAnEngineRunIsHeldAndOfferedOnlyWithItsPlansRecord pins how the worker
// pairs what an engine run computed with the record of the plan it handed the
// scheduler (CLI ADR 0116 §6a). An engine run whose plan was answered from a
// record is held, one whose plan ran is not; a later run is offered what the
// held engine run computed only at the same plan position, before any plan of
// it ran, and while nothing the decision witnessed moved.
func TestAnEngineRunIsHeldAndOfferedOnlyWithItsPlansRecord(t *testing.T) {
	// The witnesses under the Putnami home are the test's own.
	t.Setenv("HOME", t.TempDir())
	w, root := watchedTree(t, 0)
	m := RunMemory{}

	armMemory(t, w, root, runMemory{})
	if offered := m.TakeRun(root); offered != nil {
		t.Fatalf("a run not decided answerable was offered %v", offered)
	}
	m.Take(root)
	m.Keep(root, "answered plan", true)
	m.KeepRun(root, "first engine run", false)
	m.TakeRun(root)
	m.Take(root)
	m.Keep(root, "computed plan", false)
	m.KeepRun(root, "second engine run", false)
	memory.Lock()
	kept := memory.run
	memory.Unlock()
	if len(kept.runs) != 1 || kept.runs[0] != (heldEngineRun{record: "first engine run", plan: 0}) || !kept.runsPartial || kept.planned != 2 {
		t.Fatalf("kept %+v (partial %v, planned %d), want the first engine run paired with plan 0 and the second refused",
			kept.runs, kept.runsPartial, kept.planned)
	}

	offer := func() runMemory {
		return runMemory{
			check:       &memoryCheck{},
			offered:     []any{"answered plan"},
			offeredRuns: []heldEngineRun{{record: "first engine run", plan: 0}},
		}
	}
	armMemory(t, w, root, offer())
	if offered := m.TakeRun(root); offered != "first engine run" {
		t.Fatalf("an answerable run was offered %v, want what the held engine run computed", offered)
	}
	if record := m.Take(root); record != "answered plan" {
		t.Fatalf("its plan was offered %v, want the paired record", record)
	}
	m.Keep(root, "answered plan", true)
	m.KeepRun(root, "first engine run", true)
	memory.Lock()
	kept = memory.run
	memory.Unlock()
	if len(kept.runs) != 1 || kept.runsPartial || kept.skipped != 1 || kept.planned != 0 {
		t.Fatalf("an answered engine run kept %+v (partial %v, skipped %d, planned %d), want it held again",
			kept.runs, kept.runsPartial, kept.skipped, kept.planned)
	}

	for name, arm := range map[string]func(*runMemory){
		"another plan position": func(r *runMemory) { r.offeredRuns[0].plan = 1 },
		"a plan that ran":       func(r *runMemory) { r.computed = 1 },
		"an engine run planned": func(r *runMemory) { r.planned = 1 },
		"no check":              func(r *runMemory) { r.check = nil },
	} {
		run := offer()
		arm(&run)
		armMemory(t, w, root, run)
		if offered := m.TakeRun(root); offered != nil {
			t.Errorf("%s: offered %v, want nothing", name, offered)
		}
	}

	armMemory(t, w, root, offer())
	write(t, filepath.Join(root, "c", "g.txt"), "written by a before-hook")
	if offered := m.TakeRun(root); offered != nil {
		t.Errorf("a write after the decision: offered %v, want nothing", offered)
	}
}

// TestARefusedAnswerRecordsWhy pins what the worker records of a run decided
// answerable whose plan the scheduler refused to answer from the held record,
// because an entry a task of the held run was served from moved since: the
// decision is not answered, and gives the first reason the scheduler told for
// this worktree (CLI ADR 0116 §6a).
func TestARefusedAnswerRecordsWhy(t *testing.T) {
	dir, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if _, err := Dir(os.Getenv, true); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	m := RunMemory{}

	armMemory(t, nil, root, runMemory{})
	m.Refused(t.TempDir(), "another worktree's entry moved")
	m.Refused(root, "a served task's cache entry moved: /app:test")
	m.Refused(root, "a later reason")
	m.Take(root)
	m.Keep(root, nil, false)
	afterRun(nil, MemoryDecision{Root: root, Answerable: true}, 0)
	d := ReadLastMemory()
	if d == nil || !d.Answerable || d.Answered || d.Reason != "a served task's cache entry moved: /app:test" {
		t.Fatalf("recorded %+v, want answerable, not answered, with the scheduler's first reason for this worktree", d)
	}
}

// TestPeekRunOffersWhatTakeRunWouldWithoutTakingIt pins how an engine run
// reads the version snapshot the held run read before its hooks (CLI ADR 0116
// §6a): it is offered what TakeRun would offer, under the same check of what
// the decision witnessed, and the engine run still takes its position after.
func TestPeekRunOffersWhatTakeRunWouldWithoutTakingIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, root := watchedTree(t, 0)
	m := RunMemory{}
	offer := func() runMemory {
		return runMemory{
			check:       &memoryCheck{},
			offered:     []any{"answered plan"},
			offeredRuns: []heldEngineRun{{record: "first engine run", plan: 0}},
		}
	}
	armMemory(t, w, root, offer())
	for range 2 {
		if offered := m.PeekRun(root); offered != "first engine run" {
			t.Fatalf("peeked %v, want what the held engine run computed", offered)
		}
	}
	if offered := m.TakeRun(root); offered != "first engine run" {
		t.Fatalf("took %v after the peeks, want what the held engine run computed", offered)
	}
	if offered := m.PeekRun(root); offered != nil {
		t.Fatalf("peeked %v past the held engine runs, want nothing", offered)
	}

	armMemory(t, w, root, offer())
	write(t, filepath.Join(root, "c", "g.txt"), "written by a first-use bootstrap")
	if offered := m.PeekRun(root); offered != nil {
		t.Errorf("a write after the decision: peeked %v, want nothing", offered)
	}
	armMemory(t, w, root, runMemory{})
	if offered := m.PeekRun(root); offered != nil {
		t.Errorf("a run not decided answerable: peeked %v, want nothing", offered)
	}
}

// TestADecisionSaysAStepWasHeldOnlyWhenEveryEngineRunTookIt pins the
// decision's versionsHeld: every engine run of the run that reached the step
// took it from the held run, and at least one did.
func TestADecisionSaysAStepWasHeldOnlyWhenEveryEngineRunTookIt(t *testing.T) {
	dir, err := os.MkdirTemp("", "pm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if _, err := Dir(os.Getenv, true); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	m := RunMemory{}
	for name, c := range map[string]struct {
		held, computed int
		want           bool
	}{
		"every engine run took it": {2, 0, true},
		"one computed it":          {1, 1, false},
		"none reached it":          {0, 0, false},
		"all computed it":          {0, 1, false},
	} {
		armMemory(t, nil, root, runMemory{})
		m.Reused(t.TempDir(), "versions", false)
		for range c.held {
			m.Reused(root, "versions", true)
		}
		for range c.computed {
			m.Reused(root, "versions", false)
		}
		afterRun(nil, MemoryDecision{Root: root, Answerable: true}, 0)
		if d := ReadLastMemory(); d == nil || d.VersionsHeld != c.want {
			t.Errorf("%s: recorded %+v, want versionsHeld %v", name, d, c.want)
		}
	}
}

// TestTheWitnessTakesGitsConfigurationAndWhatItNames pins what the decision
// witnesses of Git beyond HEAD, the refs and the index, which a held version
// snapshot depends on (CLI ADR 0116 §6a): the global configuration, a file it
// includes, the excludes file it names, the default global ignore file, the
// repository's own exclude file and the directory of its packs.
func TestTheWitnessTakesGitsConfigurationAndWhatItNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Git reads an empty GIT_CONFIG_GLOBAL as no global configuration.
	for _, name := range []string{"XDG_CONFIG_HOME", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	if out, err := exec.Command(gitread.Executable(), "-C", root, "init", "-q").CombinedOutput(); err != nil { //nolint:gosec // fixed arguments
		t.Fatalf("git init: %v\n%s", err, out)
	}
	write(t, filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile = ~/excludes\n[include]\n\tpath = extra.inc\n")
	write(t, filepath.Join(home, "excludes"), "")
	if err := os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := outsideWitness(root)
	for _, change := range []struct {
		name, path string
		write      func(path string)
	}{
		{"the excludes file the global configuration names", filepath.Join(home, "excludes"), func(p string) { write(t, p, "*.log\n") }},
		{"a file the global configuration includes", filepath.Join(home, "extra.inc"), func(p string) { write(t, p, "[core]\n\tabbrev = 12\n") }},
		{"the global configuration", filepath.Join(home, ".gitconfig"), func(p string) {
			write(t, p, "[core]\n\texcludesFile = ~/excludes\n\tabbrev = 9\n[include]\n\tpath = extra.inc\n")
		}},
		{"the default global ignore file", filepath.Join(home, ".config", "git", "ignore"), func(p string) {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, p, "*.tmp\n")
		}},
		{"the repository's exclude file", filepath.Join(root, ".git", "info", "exclude"), func(p string) { write(t, p, "scratch/\n") }},
		{"the directory of the packs", filepath.Join(root, ".git", "objects", "pack"), func(p string) { write(t, filepath.Join(p, "pack-x.keep"), "") }},
	} {
		change.write(change.path)
		after := outsideWitness(root)
		if moved := movedWitness(before, after); moved != change.path {
			t.Errorf("%s: moved %q, want %q", change.name, moved, change.path)
		}
		before = after
	}
	if moved := movedWitness(before, outsideWitness(root)); moved != "" {
		t.Errorf("nothing changed: moved %q", moved)
	}
}

// TestAHeldDispatchIsOfferedOnlyBeforeTheRunComputedAnything pins how the
// worker offers App.Run what the held run's App.Run loaded before it
// dispatched (CLI ADR 0116 §6a): to a run decided answerable, before any
// engine run or plan of it, and while nothing the decision witnessed moved. A
// file App.Run read that no witness covers and that moved withdraws every
// offer of the run, and the decision says why.
func TestAHeldDispatchIsOfferedOnlyBeforeTheRunComputedAnything(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, root := watchedTree(t, 0)
	offer := func() runMemory {
		return runMemory{
			check:           &memoryCheck{},
			offered:         []any{"answered plan"},
			offeredRuns:     []heldEngineRun{{record: "first engine run", plan: 0}},
			offeredDispatch: "dispatch",
		}
	}
	armMemory(t, w, root, offer())
	if offered := TakeDispatch(t.TempDir()); offered != nil {
		t.Fatalf("another worktree was offered %v", offered)
	}
	if offered := TakeDispatch(root); offered != "dispatch" {
		t.Fatalf("offered %v, want what the held run's App.Run loaded", offered)
	}
	KeepDispatch(root, "dispatch", true)
	memory.Lock()
	kept := memory.run
	memory.Unlock()
	if kept.dispatch != "dispatch" || !kept.dispatchHeld {
		t.Fatalf("kept %v (held %v), want the dispatch taken", kept.dispatch, kept.dispatchHeld)
	}

	for name, arm := range map[string]func(*runMemory){
		"a run not decided answerable": func(r *runMemory) { r.check = nil },
		"an engine run already taken":  func(r *runMemory) { r.runsTaken = 1 },
		"a plan already taken":         func(r *runMemory) { r.taken = 1 },
		"nothing held":                 func(r *runMemory) { r.offeredDispatch = nil },
	} {
		run := offer()
		arm(&run)
		armMemory(t, w, root, run)
		if offered := TakeDispatch(root); offered != nil {
			t.Errorf("%s: offered %v, want nothing", name, offered)
		}
	}
	armMemory(t, w, root, offer())
	write(t, filepath.Join(root, "c", "g.txt"), "written before the dispatch")
	if offered := TakeDispatch(root); offered != nil {
		t.Errorf("a write after the decision: offered %v, want nothing", offered)
	}

	armMemory(t, w, root, offer())
	DispatchMoved(root, "an extension's manifest moved: /x/putnami.extension.json")
	m := RunMemory{}
	if TakeDispatch(root) != nil || m.PeekRun(root) != nil || m.TakeRun(root) != nil || m.Take(root) != nil {
		t.Error("a moved manifest left something offered")
	}
	memory.Lock()
	refused := memory.run.refused
	memory.Unlock()
	if refused != "an extension's manifest moved: /x/putnami.extension.json" {
		t.Errorf("recorded %q, want the moved manifest", refused)
	}
}

// TestTheWitnessTakesThePutnamiHomesConfiguration pins that the decision
// witnesses the global configuration every load of the workspace's
// configuration merges, which a held dispatch skips reading.
func TestTheWitnessTakesThePutnamiHomesConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	before := outsideWitness(root)
	path := filepath.Join(home, ".putnami", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, path, `{"options":{}}`)
	if moved := movedWitness(before, outsideWitness(root)); moved != path && moved != filepath.Dir(path) {
		t.Errorf("moved %q, want the global configuration", moved)
	}
	before = outsideWitness(root)
	write(t, path, `{"options":{"*":{"verbose":true}}}`)
	if moved := movedWitness(before, outsideWitness(root)); moved != path {
		t.Errorf("an edit: moved %q, want %q", moved, path)
	}
}

// TestAWorktreeGitDoesNotManageIsAskedAgain pins the worker's memo of a
// worktree's Git directories: one Git does not manage is asked again, so a
// repository created in it after the worker first saw it is witnessed.
func TestAWorktreeGitDoesNotManageIsAskedAgain(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	t.Cleanup(func() {
		memory.Lock()
		delete(memory.gitDirs, root)
		memory.Unlock()
	})
	if dirs := gitDirs(root); len(dirs) != 0 {
		t.Fatalf("a directory Git does not manage has Git directories %v", dirs)
	}
	if out, err := exec.Command(gitread.Executable(), "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(resolved, ".git")
	dirs := gitDirs(root)
	for _, dir := range dirs {
		if dir == want || dir == filepath.Join(root, ".git") {
			return
		}
	}
	t.Errorf("after git init the Git directories are %v, want %s", dirs, want)
}
