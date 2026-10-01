package engine

import (
	"strconv"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// makeExitCodeJob builds a job with a distinct key, so each fixture task has its
// own entry in the result map.
func makeExitCodeJob(index int) *jobs.ScheduledJob {
	id := "/p" + strconv.Itoa(index)
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: id, Name: "@acme" + id},
		Extension: &extension.ExtensionDescription{Name: "go"},
		JobDef:    &extension.JobDefinition{Name: "build"},
	}
}

// One exit-code derivation.
//
// Two rules decided a process exit code before A2b: this file's abort/failure
// branch walked SchedulerResult.Outcome and .Success, and output/json.go's
// envelope classified its own error from its own tally. Both now read the
// canonical SessionResult. The verdicts they produce are UNCHANGED — including
// where they disagree, which is the ordering pinned below.

func exitCodeFixture(aborted bool, statuses ...string) *jobs.SessionResult {
	planned := make([]*jobs.ScheduledJob, 0, len(statuses))
	results := make(map[string]*jobs.JobResult, len(statuses))
	for i, status := range statuses {
		job := makeExitCodeJob(i)
		planned = append(planned, job)
		results[job.Key()] = &jobs.JobResult{Status: status}
	}
	outcome := jobs.SessionOutcome{}
	if aborted {
		outcome = jobs.SessionOutcome{Aborted: true, AbortedBy: jobs.AbortUser}
	}
	return jobs.ReduceRun(planned, results, outcome)
}

func TestSessionExitCode_DerivesFromTheCanonicalReduction(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "terminal-outcome", "session-exit-derives-from-the-canonical-reduction")
	for _, test := range []struct {
		name    string
		session *jobs.SessionResult
		want    int
	}{
		{"all succeeded", exitCodeFixture(false, "success", "success"), ExitSuccess},
		{"one failed", exitCodeFixture(false, "success", "failed"), ExitError},
		{"skipped work is not a failure", exitCodeFixture(false, "success", "skipped"), ExitSuccess},
		{"aborted", exitCodeFixture(true, "success"), ExitSignalReceived},
		{
			// The abort wins over the failure here, unlike in the --output=json
			// envelope (output/json.go reports the failure). Both orderings are
			// shipped v1 behavior; A2b unified only where the inputs come from.
			name:    "aborted run that also failed reports the signal",
			session: exitCodeFixture(true, "failed"),
			want:    ExitSignalReceived,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionExitCode(test.session, nil); got != test.want {
				t.Errorf("exit code = %d, want %d", got, test.want)
			}
		})
	}
}

// TestSessionExitCode_ForwardsRunWorkloadCode pins that `putnami run` still
// forwards its workload's exact code, and only ever on a failed, unaborted run:
// forwarding on a green run would invent a failure, and forwarding on an abort
// would hide the signal.
func TestSessionExitCode_ForwardsRunWorkloadCode(t *testing.T) {
	t.Parallel()
	forwarded := func() (int, bool) { return 3, true }

	if got := sessionExitCode(exitCodeFixture(false, "failed"), forwarded); got != 3 {
		t.Errorf("failed run exit code = %d, want the forwarded 3", got)
	}
	if got := sessionExitCode(exitCodeFixture(false, "success"), forwarded); got != ExitSuccess {
		t.Errorf("successful run exit code = %d, want %d", got, ExitSuccess)
	}
	if got := sessionExitCode(exitCodeFixture(true, "failed"), forwarded); got != ExitSignalReceived {
		t.Errorf("aborted run exit code = %d, want %d", got, ExitSignalReceived)
	}

	// No workload code on record (an upstream build step failed before the
	// workload ran) falls back to the generic failure code.
	none := func() (int, bool) { return 0, false }
	if got := sessionExitCode(exitCodeFixture(false, "failed"), none); got != ExitError {
		t.Errorf("unforwarded failure exit code = %d, want %d", got, ExitError)
	}
}

// TestSessionExitCode_ReusedFailureStillFails pins the STRICT half of the
// settled two-predicate ruling at the surface that consumes it. The
// --output=jsonl session summary reports this same strict verdict since an
// earlier change unified the success rule.
func TestSessionExitCode_ReusedFailureStillFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "terminal-outcome", "session-exit-derives-from-the-canonical-reduction")
	job := makeExitCodeJob(0)
	results := map[string]*jobs.JobResult{
		job.Key(): {Status: "failed", CacheHit: true, Reuse: jobs.ReuseLocalCache},
	}
	session := jobs.ReduceRun([]*jobs.ScheduledJob{job}, results, jobs.SessionOutcome{})

	if got := sessionExitCode(session, nil); got != ExitError {
		t.Errorf("exit code = %d, want %d: a reused FAILED result still fails the run", got, ExitError)
	}
	if session.Success() {
		t.Error("the unified strict predicate green-lit a reused failure")
	}
}
