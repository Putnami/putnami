package machineproc

import (
	"os"
	"path/filepath"
	"testing"
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
