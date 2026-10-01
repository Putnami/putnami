package jobs

import (
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// makeJob builds a minimal ScheduledJob for DAG tests.
func makeJob(projectName, jobName string, dependsOn []string) *ScheduledJob {
	return &ScheduledJob{
		Project:   &workspace.Project{ID: "/" + projectName, Name: projectName, Path: projectName},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: jobName},
		DependsOn: dependsOn,
	}
}

// --- newDAGState ---

func TestNewDAGState_NoJobs(t *testing.T) {
	ds := newDAGState(nil)
	if len(ds.remaining) != 0 {
		t.Errorf("expected empty remaining, got %v", ds.remaining)
	}
}

func TestNewDAGState_SingleJobNoDeps(t *testing.T) {
	job := makeJob("app", "build", nil)
	ds := newDAGState([]*ScheduledJob{job})
	if ds.remaining[job.Key()] != 0 {
		t.Errorf("job with no deps should have remaining=0, got %d", ds.remaining[job.Key()])
	}
}

func TestNewDAGState_JobWithDeps(t *testing.T) {
	lib := makeJob("lib", "build", nil)
	app := makeJob("app", "build", []string{lib.Key()})
	ds := newDAGState([]*ScheduledJob{lib, app})

	if ds.remaining[lib.Key()] != 0 {
		t.Errorf("lib should have remaining=0, got %d", ds.remaining[lib.Key()])
	}
	if ds.remaining[app.Key()] != 1 {
		t.Errorf("app should have remaining=1, got %d", ds.remaining[app.Key()])
	}
	if len(ds.dependents[lib.Key()]) != 1 || ds.dependents[lib.Key()][0] != app.Key() {
		t.Errorf("lib.dependents should contain app, got %v", ds.dependents[lib.Key()])
	}
}

func TestNewDAGState_ChainedDeps(t *testing.T) {
	a := makeJob("a", "build", nil)
	b := makeJob("b", "build", []string{a.Key()})
	c := makeJob("c", "build", []string{b.Key()})
	ds := newDAGState([]*ScheduledJob{a, b, c})

	if ds.remaining[a.Key()] != 0 {
		t.Errorf("a remaining = %d, want 0", ds.remaining[a.Key()])
	}
	if ds.remaining[b.Key()] != 1 {
		t.Errorf("b remaining = %d, want 1", ds.remaining[b.Key()])
	}
	if ds.remaining[c.Key()] != 1 {
		t.Errorf("c remaining = %d, want 1", ds.remaining[c.Key()])
	}
}

func TestNewDAGState_SerializeAfterGatesReadiness(t *testing.T) {
	// Two functionally-independent writers serialized on a shared resource.
	a := makeJob("a", "build", nil)
	b := makeJob("b", "build", nil)
	b.SerializeAfter = []string{a.Key()}

	ds := newDAGState([]*ScheduledJob{a, b})
	if ds.remaining[b.Key()] != 1 {
		t.Errorf("serialize edge should gate b: remaining = %d, want 1", ds.remaining[b.Key()])
	}
	if len(ds.dependents[a.Key()]) != 1 || ds.dependents[a.Key()][0] != b.Key() {
		t.Errorf("a should have b as a serialize dependent, got %v", ds.dependents[a.Key()])
	}

	ready := ds.ready()
	if len(ready) != 1 || ready[0].Key() != a.Key() {
		t.Fatalf("only a should be ready first, got %v", ready)
	}
	newReady := ds.complete(a.Key())
	if len(newReady) != 1 || newReady[0].Key() != b.Key() {
		t.Errorf("completing a should unblock serialized b, got %v", newReady)
	}
}

func TestSchedulingPredecessors_DedupesFunctionalAndSerializeEdges(t *testing.T) {
	a := makeJob("a", "build", nil)
	b := makeJob("b", "build", []string{a.Key()})
	// The same predecessor declared both functionally and as a serialize edge.
	b.SerializeAfter = []string{a.Key()}

	preds := b.SchedulingPredecessors()
	if len(preds) != 1 || preds[0] != a.Key() {
		t.Errorf("schedulingPredecessors = %v, want [%s] deduped", preds, a.Key())
	}
	ds := newDAGState([]*ScheduledJob{a, b})
	if ds.remaining[b.Key()] != 1 {
		t.Errorf("overlapping edges must be counted once: remaining = %d, want 1", ds.remaining[b.Key()])
	}
}

// --- dagState.ready ---

func TestDAGReady_AllNoDeps(t *testing.T) {
	a := makeJob("a", "build", nil)
	b := makeJob("b", "build", nil)
	ds := newDAGState([]*ScheduledJob{a, b})
	ready := ds.ready()
	if len(ready) != 2 {
		t.Errorf("expected 2 ready jobs, got %d", len(ready))
	}
}

func TestDAGReady_BlockedJob(t *testing.T) {
	lib := makeJob("lib", "build", nil)
	app := makeJob("app", "build", []string{lib.Key()})
	ds := newDAGState([]*ScheduledJob{lib, app})
	ready := ds.ready()
	if len(ready) != 1 {
		t.Errorf("expected 1 ready job, got %d", len(ready))
	}
	if ready[0].Key() != lib.Key() {
		t.Errorf("expected lib to be ready, got %s", ready[0].Key())
	}
}

func TestDAGReady_Empty(t *testing.T) {
	ds := newDAGState(nil)
	ready := ds.ready()
	if len(ready) != 0 {
		t.Errorf("empty DAG should have 0 ready jobs, got %d", len(ready))
	}
}

// --- dagState.complete ---

func TestDAGComplete_UnblocksDependent(t *testing.T) {
	lib := makeJob("lib", "build", nil)
	app := makeJob("app", "build", []string{lib.Key()})
	ds := newDAGState([]*ScheduledJob{lib, app})

	newReady := ds.complete(lib.Key())
	if len(newReady) != 1 {
		t.Fatalf("expected 1 newly ready job, got %d", len(newReady))
	}
	if newReady[0].Key() != app.Key() {
		t.Errorf("expected app to become ready, got %s", newReady[0].Key())
	}
}

func TestDAGComplete_ChainUnblocking(t *testing.T) {
	// a → b → c: completing a unlocks b but not c yet
	a := makeJob("a", "build", nil)
	b := makeJob("b", "build", []string{a.Key()})
	c := makeJob("c", "build", []string{b.Key()})
	ds := newDAGState([]*ScheduledJob{a, b, c})

	newReady := ds.complete(a.Key())
	if len(newReady) != 1 || newReady[0].Key() != b.Key() {
		t.Errorf("completing a should unblock b only, got %v", newReady)
	}

	newReady = ds.complete(b.Key())
	if len(newReady) != 1 || newReady[0].Key() != c.Key() {
		t.Errorf("completing b should unblock c, got %v", newReady)
	}
}

func TestDAGComplete_MultipleDepsRequired(t *testing.T) {
	// both a and b must complete before c is ready
	a := makeJob("a", "build", nil)
	b := makeJob("b", "build", nil)
	c := makeJob("c", "build", []string{a.Key(), b.Key()})
	ds := newDAGState([]*ScheduledJob{a, b, c})

	newReady := ds.complete(a.Key())
	if len(newReady) != 0 {
		t.Errorf("c should not be ready after only a completes, got %v", newReady)
	}

	newReady = ds.complete(b.Key())
	if len(newReady) != 1 || newReady[0].Key() != c.Key() {
		t.Errorf("c should be ready after both a and b complete, got %v", newReady)
	}
}

func TestDAGComplete_NoNewReady(t *testing.T) {
	a := makeJob("a", "build", nil)
	ds := newDAGState([]*ScheduledJob{a})
	newReady := ds.complete(a.Key())
	if len(newReady) != 0 {
		t.Errorf("completing leaf job should return no new ready jobs, got %v", newReady)
	}
}

// --- Scheduler.shouldSkip ---

func makeScheduler(continueOnError bool) *Scheduler {
	return &Scheduler{
		cfg: SchedulerConfig{ContinueOnError: continueOnError},
	}
}

func TestShouldSkip_NoDeps(t *testing.T) {
	s := makeScheduler(false)
	job := makeJob("app", "build", nil)
	results := map[string]*JobResult{}
	var mu sync.Mutex
	if s.shouldSkip(job, results, &mu) {
		t.Error("job with no deps should not be skipped")
	}
}

func TestShouldSkip_DepFailed(t *testing.T) {
	s := makeScheduler(false)
	lib := makeJob("lib", "build", nil)
	app := makeJob("app", "build", []string{lib.Key()})
	results := map[string]*JobResult{
		lib.Key(): {Status: "failed"},
	}
	var mu sync.Mutex
	if !s.shouldSkip(app, results, &mu) {
		t.Error("job whose dependency failed should be skipped")
	}
}

func TestShouldSkip_DepSucceeded(t *testing.T) {
	s := makeScheduler(false)
	lib := makeJob("lib", "build", nil)
	app := makeJob("app", "build", []string{lib.Key()})
	results := map[string]*JobResult{
		lib.Key(): {Status: "success"},
	}
	var mu sync.Mutex
	if s.shouldSkip(app, results, &mu) {
		t.Error("job with successful dependency should not be skipped")
	}
}

func TestShouldSkip_ContinueOnError(t *testing.T) {
	// When ContinueOnError=true, shouldSkip always returns false
	s := makeScheduler(true)
	lib := makeJob("lib", "build", nil)
	app := makeJob("app", "build", []string{lib.Key()})
	results := map[string]*JobResult{
		lib.Key(): {Status: "failed"},
	}
	var mu sync.Mutex
	if s.shouldSkip(app, results, &mu) {
		t.Error("shouldSkip should return false when ContinueOnError=true")
	}
}

func TestValidatePlanDAG_UnresolvedDependency(t *testing.T) {
	app := makeJob("app", "build", []string{"/missing:build"})

	err := validatePlanDAG([]*ScheduledJob{app})
	if err == nil {
		t.Fatal("expected unresolved dependency error")
	}
	for _, want := range []string{"unresolved job dependencies", "/app:build depends on missing /missing:build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in error, got %v", want, err)
		}
	}
}

func TestValidatePlanDAG_Cycle(t *testing.T) {
	a := makeJob("a", "build", []string{"/b:build"})
	b := makeJob("b", "build", []string{"/a:build"})

	err := validatePlanDAG([]*ScheduledJob{a, b})
	if err == nil {
		t.Fatal("expected cycle error")
	}
	for _, want := range []string{"cannot be scheduled", "/a:build waits for /b:build", "/b:build waits for /a:build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in error, got %v", want, err)
		}
	}
}
