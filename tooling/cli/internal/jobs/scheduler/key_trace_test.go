package scheduler

import (
	"errors"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// A traced job that started at another key than its precomputed one, or with
// none precomputed, diverged; one served at its precomputed key where the
// precompute found no entry was not addressed; one not served there where the
// precompute found its entry was not served. None is said of a job that
// matched, of one that executed where nothing was found, or of one that never
// started.
func TestTheKeyTraceNamesEachDivergence(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		entry                           KeyTraceEntry
		diverged, unaddressed, unserved bool
	}{
		{"matched", KeyTraceEntry{Precomputed: "a", PrecomputedEntry: true, Started: "a", ServedLocally: true}, false, false, false},
		{"executed where nothing was found", KeyTraceEntry{Precomputed: "a", Started: "a"}, false, false, false},
		{"started at another key", KeyTraceEntry{Precomputed: "a", PrecomputedEntry: true, Started: "b", ServedLocally: true}, true, false, false},
		{"started with no precomputed key", KeyTraceEntry{Started: "a", ServedLocally: true}, true, false, false},
		{"served where the precompute found nothing", KeyTraceEntry{Precomputed: "a", Started: "a", ServedLocally: true}, false, true, false},
		{"not served where the precompute found it", KeyTraceEntry{Precomputed: "a", PrecomputedEntry: true, Started: "a"}, false, false, true},
		{"never started", KeyTraceEntry{Precomputed: "a", PrecomputedEntry: true}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.Diverged(); got != tc.diverged {
				t.Errorf("Diverged() = %v, want %v", got, tc.diverged)
			}
			if got := tc.entry.Unaddressed(); got != tc.unaddressed {
				t.Errorf("Unaddressed() = %v, want %v", got, tc.unaddressed)
			}
			if got := tc.entry.Unserved(); got != tc.unserved {
				t.Errorf("Unserved() = %v, want %v", got, tc.unserved)
			}
		})
	}
}

// A run's trace reaches the installed KeyTrace when the run finishes, and
// a trace stopped before the run starts collects nothing from it.
func TestTheKeyTraceCollectsARunOnceInstalled(t *testing.T) {
	trace, stop := TraceKeys()
	run := &keyTrace{trace: trace, byJob: map[string]*KeyTraceEntry{}}
	job := cacheableJob("build", "/lib", "lib", "lib")
	run.started(job, "key", true)
	run.finish()
	stop()
	entries := trace.Entries()
	if len(entries) != 1 || entries[0].Job != job.Key() || entries[0].Started != "key" || !entries[0].ServedLocally {
		t.Fatalf("entries = %+v, want the one job started at key and served", entries)
	}
	if prunes := trace.Prunes(); len(prunes) != 1 {
		t.Fatalf("prunes = %+v, want the run's one", prunes)
	}
	if installedKeyTrace.Load() != nil {
		t.Fatal("stop left the trace installed")
	}
	var s Scheduler
	if s.startKeyTrace(map[string]string{"x": "y"}, 0) != nil {
		t.Fatal("a run started with no trace installed traced its keys")
	}

	// A run whose keys cannot be traced is recorded with the reason.
	trace, stop = TraceKeys()
	defer stop()
	s.startKeyTrace(nil, 0).finish()
	if prunes := trace.Prunes(); len(prunes) != 1 || prunes[0].Err == "" {
		t.Fatalf("prunes = %+v, want one run that names why it traced nothing", prunes)
	}
}

// The key trace finds a source-bound entry where the task would look it up
// if it started now: at its cache key bound to its project's current binding,
// never at another binding, never at the plain key, and not at all while no
// binding can be computed. Pruning still never counts it (above).
func TestSourceBoundEntryIsPresentOnlyAtTheCurrentBinding(t *testing.T) {
	job := sourceBoundJob()
	f := newCaptureFixture(t, job)
	storeSourceBoundEntry(t, f, job, cachekey.SourceBoundEntryKey(captureHashA, "binding-a"))

	stubSourceBinding(f.sched, "binding-a", nil, false)
	if !f.sched.entryPresentAtCurrentBinding(job, captureHashA) {
		t.Fatal("a source-bound entry at the current binding was not found")
	}
	if cachedEntryPresent(f.cache, job, captureHashA) {
		t.Fatal("a source-bound entry counted as present for invocation pruning")
	}
	if f.sched.entryPresentAtCurrentBinding(job, strings.Repeat("b", 64)) {
		t.Fatal("a source-bound entry was found at another cache key")
	}
	stubSourceBinding(f.sched, "binding-b", nil, false)
	if f.sched.entryPresentAtCurrentBinding(job, captureHashA) {
		t.Fatal("a source-bound entry was found at another binding")
	}
	stubSourceBinding(f.sched, "", errors.New("no binding"), false)
	if f.sched.entryPresentAtCurrentBinding(job, captureHashA) {
		t.Fatal("a source-bound entry was found while its binding cannot be computed")
	}

	// An entry at the plain cache key is never found for a source-bound task,
	// and is found for the same task without the policy.
	storeSourceBoundEntry(t, f, job, captureHashA)
	stubSourceBinding(f.sched, "binding-b", nil, false)
	if f.sched.entryPresentAtCurrentBinding(job, captureHashA) {
		t.Fatal("an entry at the plain cache key was found for a source-bound task")
	}
	job.JobDef.TaskCachePolicy.SourceBound = false
	if !f.sched.entryPresentAtCurrentBinding(job, captureHashA) {
		t.Fatal("the control entry without cache.sourceBound was not found")
	}
}
