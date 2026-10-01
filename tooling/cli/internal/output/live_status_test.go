package output

import (
	"testing"

	"go.putnami.dev/tooling/cli/internal/jobs"
)

// The live view's status vocabulary.
//
// The live renderer used to declare its own status strings and its own
// worst-status precedence. Both now derive from jobs.TaskStatus / jobs.ReuseKind,
// so a rename on the canonical side reaches the live view instead of silently
// forking. These tests pin the two things that derivation must not change: the
// spellings, and the precedence order a pipeline uses to adopt the worst status
// its steps reached.

// TestLiveStatuses_AreTheCanonicalVocabulary pins that each terminal status the
// live view renders is the canonical value, not a copy that happens to match
// today. A settled command's status comes straight from jobs.JobResult.Outcome,
// so a divergence here would make settled() and the summary tallies silently
// miss states.
func TestLiveStatuses_AreTheCanonicalVocabulary(t *testing.T) {
	for _, test := range []struct {
		name string
		got  string
		want string
	}{
		{"success", statusSuccess, string(jobs.TaskStatusSuccess)},
		{"failed", statusFailed, string(jobs.TaskStatusFailed)},
		{"skipped", statusSkipped, string(jobs.TaskStatusSkipped)},
		{"canceled", statusCanceled, string(jobs.TaskStatusCanceled)},
		{"cached", statusCached, jobs.ReuseLocalCache.Outcome()},
		{"remote cache also renders as cached", statusCached, jobs.ReuseRemoteCache.Outcome()},
		{"coalesced", statusCoalesced, jobs.ReuseCoalesced.Outcome()},
	} {
		if test.got != test.want {
			t.Errorf("%s: live view says %q, canonical model says %q", test.name, test.got, test.want)
		}
	}

	// Every terminal status must settle a command; a status the live view can
	// reach but never settles would leave a project running forever.
	for _, status := range statusPrecedence {
		if !(&commandState{status: status}).settled() {
			t.Errorf("status %q is in the precedence order but does not settle a command", status)
		}
	}
	if (&commandState{status: statusRunning}).settled() {
		t.Error("a running command must not count as settled")
	}
}

// TestStatusRank_Precedence pins the exact ordering A2b had to preserve while
// replacing the hand-written switch: failed > aborted > success > coalesced >
// cached > skipped > unsettled. It is written as pairwise comparisons rather
// than as literal ranks, because the ranks are an implementation detail and the
// ORDER is the contract a pipeline's worst-status adoption depends on.
func TestStatusRank_Precedence(t *testing.T) {
	descending := []string{
		statusFailed,
		statusAborted,
		statusSuccess,
		statusCoalesced,
		statusCached,
		statusSkipped,
		statusRunning,
	}
	for i := 0; i < len(descending)-1; i++ {
		high, low := descending[i], descending[i+1]
		if statusRank(high) <= statusRank(low) {
			t.Errorf("statusRank(%q) = %d must outrank statusRank(%q) = %d",
				high, statusRank(high), low, statusRank(low))
		}
	}

	// Running and unstarted are equally unsettled: neither may displace a
	// terminal status a sibling step already reached.
	if statusRank(statusRunning) != 0 || statusRank("") != 0 || statusRank(statusQueued) != 0 {
		t.Error("an unsettled status must rank 0 so it cannot become a command's worst step")
	}

	// The precedence table is what statusRank reads; a status missing from it
	// would silently rank 0 and be treated as unsettled.
	for _, status := range []string{
		statusSuccess, statusFailed, statusSkipped, statusCached, statusCoalesced, statusAborted,
	} {
		if statusRank(status) == 0 {
			t.Errorf("terminal status %q is missing from statusPrecedence", status)
		}
	}
}

// TestLiveTally_BucketsBothSummaryVocabularies pins the one bucketing rule the
// Projects block and the Tasks block now share. The two blocks feed it different
// vocabularies — projectStatus yields done/blocked, a settled command yields
// success/cached/coalesced — and both must land in the buckets their pre-A2b
// switches used.
func TestLiveTally_BucketsBothSummaryVocabularies(t *testing.T) {
	var project liveTally
	for _, status := range []string{
		statusDone, statusDone, statusFailed, statusSkipped, statusAborted, statusBlocked,
		statusRunning, statusQueued,
	} {
		project.add(status)
	}
	if project.passed != 2 || project.failed != 1 || project.skipped != 1 ||
		project.aborted != 1 || project.blocked != 1 {
		t.Errorf("project tally = %+v", project)
	}

	var command liveTally
	for _, status := range []string{
		statusSuccess, statusCached, statusCoalesced, statusFailed, statusSkipped, statusAborted,
		statusRunning, "",
	} {
		command.add(status)
	}
	// Reuse presents as passed: a cached or coalesced command produced a result,
	// which is what the Tasks block reports.
	if command.passed != 3 || command.failed != 1 || command.skipped != 1 || command.aborted != 1 {
		t.Errorf("command tally = %+v", command)
	}
	if command.blocked != 0 {
		t.Error("a command can never be blocked; only a project row can")
	}
}
