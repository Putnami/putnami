package orchestration

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/repotest"
)

// A run decides on the keys it precomputes before any task runs: prune keeps
// or drops a finalizer's producer on them, and the remote negotiation asks for
// them. A job then looks its entry up at the key it computes when it starts.
// On an unchanged workspace the two name one entry: a job started at another
// key, a job served where the precompute found no entry, and a job not served
// where the precompute found one are each a decision the precompute took on
// something the run did not do.
//
// keyExplanations names, by task, the jobs that may differ so on an unchanged
// run, each with the reason it is correct. Every other such job fails the test
// that traces it.
var keyExplanations = map[string]string{}

// traceUnchangedRun runs args on the workspace at root with the key trace
// installed, fails t unless the run exits 0, and returns what it traced.
func traceUnchangedRun(t *testing.T, root string, args ...string) keyTraceSummary {
	t.Helper()
	trace, stop := scheduler.TraceKeys()
	defer stop()
	if code, out := clitest.RunGateArgs(t, root, args...); code != 0 {
		t.Fatalf("putnami %s exit %d:\n%s", strings.Join(args, " "), code, out)
	}
	return holdPrecomputedKeys(t, trace)
}

// keyTraceSummary counts the traced jobs, and lists by task those whose
// precomputed key and start disagree, in each of the three ways.
type keyTraceSummary struct {
	runs                               int
	traced, precomputed, servedLocally int
	// sourceBound counts the source-bound jobs served at their precomputed
	// key, which a presence check that ignores the source binding misses.
	sourceBound                     int
	diverged, unaddressed, unserved map[string][]string
	entries                         []scheduler.KeyTraceEntry
}

func summarizeKeyTrace(runs int, entries []scheduler.KeyTraceEntry) keyTraceSummary {
	summary := keyTraceSummary{
		runs: runs, entries: entries,
		diverged: map[string][]string{}, unaddressed: map[string][]string{}, unserved: map[string][]string{},
	}
	for _, entry := range entries {
		summary.traced++
		if entry.Precomputed != "" {
			summary.precomputed++
		}
		if entry.ServedLocally {
			summary.servedLocally++
		}
		if entry.SourceBound && entry.ServedLocally && !entry.Diverged() {
			summary.sourceBound++
		}
		switch {
		case entry.Diverged():
			summary.diverged[entry.Task] = append(summary.diverged[entry.Task], entry.Job)
		case entry.Unaddressed():
			summary.unaddressed[entry.Task] = append(summary.unaddressed[entry.Task], entry.Job)
		case entry.Unserved():
			summary.unserved[entry.Task] = append(summary.unserved[entry.Task], entry.Job)
		}
	}
	return summary
}

type keyTraceClass struct {
	name string
	jobs map[string][]string
}

// classes are the three ways a precomputed key and a start disagree.
func (s keyTraceSummary) classes() []keyTraceClass {
	return []keyTraceClass{
		{"started at another key than its precomputed one", s.diverged},
		{"was served at its precomputed key, where the precompute found no entry", s.unaddressed},
		{"was not served at its precomputed key, where the precompute found its entry", s.unserved},
	}
}

func (s keyTraceSummary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d run(s), %d jobs traced, %d precomputed, %d served locally, %d source-bound served at their precomputed key",
		s.runs, s.traced, s.precomputed, s.servedLocally, s.sourceBound)
	for _, class := range s.classes() {
		for _, task := range slices.Sorted(maps.Keys(class.jobs)) {
			jobs := class.jobs[task]
			fmt.Fprintf(&b, "\n  %s %d: %s (first %s)", task, len(jobs), class.name, jobs[0])
		}
	}
	return b.String()
}

// holdPrecomputedKeys reads what trace collected and fails t on a run that
// traced nothing or could not precompute its keys, and on every job whose
// precomputed key and start disagree, unless keyExplanations names its task.
func holdPrecomputedKeys(t *testing.T, trace *scheduler.KeyTrace) keyTraceSummary {
	t.Helper()
	prunes := trace.Prunes()
	summary := summarizeKeyTrace(len(prunes), trace.Entries())
	t.Logf("key trace: %s", summary)
	if len(prunes) == 0 {
		t.Fatalf("no run was traced: %s", summary)
	}
	for _, prune := range prunes {
		t.Logf("relations: %d of %d lazy", prune.Lazy, prune.Relations)
		if prune.Err != "" {
			t.Errorf("a traced run precomputed no key: %s", prune.Err)
		}
	}
	for _, class := range summary.classes() {
		for _, task := range slices.Sorted(maps.Keys(class.jobs)) {
			if _, explained := keyExplanations[task]; explained {
				continue
			}
			t.Errorf("%d %s jobs: each %s, for example %s", len(class.jobs[task]), task, class.name, class.jobs[task][0])
		}
	}
	return summary
}

// TestAnUnchangedRunStartsAtItsPrecomputedKeys holds, on a workspace whose
// jobs start nothing, that every job of an unchanged run is served at the key
// the run precomputed for it, where the precompute found its entry. The
// source-bound variant finds each entry at its project's current binding.
func TestAnUnchangedRunStartsAtItsPrecomputedKeys(t *testing.T) {
	clitest.RequireShell(t)
	clitest.UnsetHostRunEnvironment(t)
	for _, tc := range []struct {
		name        string
		fixture     func(t *testing.T) string
		sourceBound int
	}{
		{"plain", func(t *testing.T) string { return clitest.CountsFixture(t, 5, false) }, 0},
		{"source-bound", func(t *testing.T) string { return clitest.SourceBoundCountsFixture(t, 5) }, 15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hometest.Temp(t)
			root := tc.fixture(t)
			clitest.RunCounts(t, root, "lint,test,build", "--all")
			summary := traceUnchangedRun(t, root, "lint,test,build", "--all")
			if summary.runs != 1 || summary.traced != 15 || summary.precomputed != 15 || summary.servedLocally != 15 ||
				summary.sourceBound != tc.sourceBound {
				t.Errorf("trace = %s; want one run of 15 jobs, each precomputed and served locally, %d of them source-bound",
					summary, tc.sourceBound)
			}
		})
	}
}

// TestThisRepositoryStartsAtItsPrecomputedKeys holds the same on this
// repository's own gate. It runs the gate twice, in this repository with its
// own store, so it runs only when PUTNAMI_KEY_TRACE_REPOSITORY is set: the
// measurement of an unchanged run's precomputed keys. PUTNAMI_KEY_TRACE_ARGS
// adds arguments to both runs, such as --fix; PUTNAMI_KEY_TRACE_OUT names a
// file the traced entries are written to.
func TestThisRepositoryStartsAtItsPrecomputedKeys(t *testing.T) {
	repotest.Require(t)
	if os.Getenv("PUTNAMI_KEY_TRACE_REPOSITORY") == "" {
		t.Skip("set PUTNAMI_KEY_TRACE_REPOSITORY=1 to run this repository's gate twice and trace its keys")
	}
	// A task that fails here, on a machine that lacks one of its tools, fails
	// both runs alike, and cancels none of its dependents: the keys of the
	// others are still traced.
	args := append([]string{"lint,test,build,validate", "--all", "--continue-on-error"},
		strings.Fields(os.Getenv("PUTNAMI_KEY_TRACE_ARGS"))...)
	if code, out := clitest.RunGateArgs(t, repoRoot, args...); code != 0 {
		t.Logf("putnami %s exit %d:\n%s", strings.Join(args, " "), code, lastLines(out, 20))
	}
	trace, stop := scheduler.TraceKeys()
	defer stop()
	if code, out := clitest.RunGateArgs(t, repoRoot, args...); code != 0 {
		t.Logf("putnami %s exit %d:\n%s", strings.Join(args, " "), code, lastLines(out, 20))
	}
	summary := holdPrecomputedKeys(t, trace)
	if out := os.Getenv("PUTNAMI_KEY_TRACE_OUT"); out != "" {
		data, err := json.MarshalIndent(summary.entries, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// lastLines is the last n lines of out.
func lastLines(out string, n int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
