package output

import (
	"bytes"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// restoredDriftFailure is a cache hit whose declared output drifted from the
// checkout it landed in (jobs/task_drift.go): the entry held the successful
// run, the verdict is this run's.
func restoredDriftFailure() *jobs.JobResult {
	result := &jobs.JobResult{
		Status: "failed",
		Error:  &jobs.JobError{Code: "generated-output-drift", Message: "generated output \"client\" at app/clients/go differs"},
		Events: []jobs.RawJobEvent{{
			Type: jobs.EventTypeDiagnostic,
			Data: map[string]any{"severity": "error", "code": "generated-output-drift",
				"message": "generated output \"client\" at app/clients/go differs", "file": "app/clients/go"},
		}},
	}
	result.MarkReuse(jobs.ReuseLocalCache)
	return result
}

// TestARestoredFailureRendersAsAFailure: a reused result that failed is a
// failure of this run, in the row, in its diagnostics and in the headline —
// never the bare "cached" its provenance alone would print. Nothing else in the
// run reports it, so a renderer that hid it behind the cache label would leave
// an exit code 1 with no visible cause.
func TestARestoredFailureRendersAsAFailure(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "generated-output-drift", "a-restored-entry-is-judged-against-the-checkout-it-lands-in")
	job := makeLiveTestJob("app", "build")
	result := restoredDriftFailure()

	for _, verbose := range []bool{false, true} {
		var out, errOut bytes.Buffer
		r := NewTextRenderer(&out, &errOut, TextRendererConfig{Verbose: verbose})
		r.JobStart(job)
		r.JobComplete(job, result)
		r.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
		text := errOut.String()
		for _, want := range []string{"restored from cache", "app/clients/go differs", "1/1 failed"} {
			if !strings.Contains(text, want) {
				t.Errorf("verbose=%t: the restored failure is missing %q:\n%s", verbose, want, text)
			}
		}
		if strings.Contains(text, "  1 succeeded") {
			t.Errorf("verbose=%t: the headline counted the run as succeeded:\n%s", verbose, text)
		}
	}

	live, errOut := newTestRenderer(t, 160, job)
	live.JobStart(job)
	live.JobComplete(job, result)
	live.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
	text := errOut.String()
	for _, want := range []string{"Failures:", "1 failed", "app/clients/go differs"} {
		if !strings.Contains(text, want) {
			t.Errorf("the live summary is missing %q:\n%s", want, text)
		}
	}
}
