package output

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// replayedFailedResult is one failed result served from the failure cache,
// carrying exactly what a fresh failure carries plus its replay provenance.
func replayedFailedResult(replayed bool) *jobs.JobResult {
	result := &jobs.JobResult{
		Status:   "failed",
		ExitCode: 3,
		Error:    &jobs.JobError{Message: "main_test.go:12 assertion failed"},
		Events: []jobs.RawJobEvent{
			{Type: jobs.EventTypeLog, Level: "error", Message: "1 failing test"},
		},
	}
	if replayed {
		result.ReplayedFailure = &jobs.ReplayedFailure{
			FirstFailedAt: time.Now().Add(-12 * time.Minute),
			Attempts:      3,
		}
	}
	return result
}

// TestReplayedFailureIsAnnotatedAndNeverQuieter is contract point 5: the text
// renderer says the verdict was replayed, says how old the failure is and how
// many times it has been observed, prints the ORIGINAL failure output, and
// names the escape hatch — and a fresh failure still prints exactly the same
// detail lines without any of that.
func TestReplayedFailureIsAnnotatedAndNeverQuieter(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache",
		"a-replayed-failure-reports-its-provenance-and-original-output")

	render := func(result *jobs.JobResult) string {
		var out, errOut bytes.Buffer
		r := NewTextRenderer(&out, &errOut, TextRendererConfig{})
		job := makeLiveTestJob("app", "test")
		r.JobStart(job)
		r.JobComplete(job, result)
		return errOut.String()
	}

	fresh := render(replayedFailedResult(false))
	replayed := render(replayedFailedResult(true))

	for _, want := range []string{"assertion failed", "1 failing test"} {
		if !strings.Contains(fresh, want) {
			t.Fatalf("the fresh failure did not print %q, so the comparison is meaningless:\n%s", want, fresh)
		}
		if !strings.Contains(replayed, want) {
			t.Errorf("the replayed failure is quieter than a fresh one: %q missing from\n%s", want, replayed)
		}
	}
	for _, want := range []string{"replayed", "inputs unchanged since", "12m ago", "attempt 3", "--retry-failed"} {
		if !strings.Contains(replayed, want) {
			t.Errorf("the replay annotation is missing %q:\n%s", want, replayed)
		}
	}
	if strings.Contains(fresh, "replayed") || strings.Contains(fresh, "--retry-failed") {
		t.Errorf("a fresh failure was annotated as replayed:\n%s", fresh)
	}
}

// TestReplayedFailureIsAnnotatedInTheLiveSummary pins the same annotation on
// the default interactive renderer's end-of-run Failures block.
func TestReplayedFailureIsAnnotatedInTheLiveSummary(t *testing.T) {
	job := makeLiveTestJob("app", "test")
	r, errOut := newTestRenderer(t, 160, job)

	result := replayedFailedResult(true)
	r.JobStart(job)
	r.JobComplete(job, result)
	r.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})

	out := errOut.String()
	for _, want := range []string{"Failures:", "replayed", "attempt 3", "--retry-failed", "assertion failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("live failure summary is missing %q:\n%s", want, out)
		}
	}
}

// TestFreshFailureSummaryCarriesNoReplayHint is the control: the hint appears
// only when a failure was actually replayed.
func TestFreshFailureSummaryCarriesNoReplayHint(t *testing.T) {
	job := makeLiveTestJob("app", "test")
	r, errOut := newTestRenderer(t, 160, job)

	result := replayedFailedResult(false)
	r.JobStart(job)
	r.JobComplete(job, result)
	r.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})

	if out := errOut.String(); strings.Contains(out, "--retry-failed") {
		t.Errorf("a run with no replayed failure advertised --retry-failed:\n%s", out)
	}
}

// TestReplayedFailureKeepsTheFailedOutcome pins the model decision behind the
// annotation: a replay is NOT reuse, so it never lands in the cached bucket of
// the session summary.
func TestReplayedFailureKeepsTheFailedOutcome(t *testing.T) {
	result := replayedFailedResult(true)
	if result.Outcome() != "failed" || result.CacheHit {
		t.Fatalf("outcome = %q cacheHit = %v, want a plain failure", result.Outcome(), result.CacheHit)
	}

	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{})
	r.Start(nil)
	r.Finish(map[string]*jobs.JobResult{"app:test": result}, jobs.SessionOutcome{})
	if summary := errOut.String(); strings.Contains(summary, "cached") {
		t.Errorf("a replayed failure was summarized as cached:\n%s", summary)
	}
}

// TestFailureAgeReadsInOneCoarseUnit pins the age format the annotation uses.
func TestFailureAgeReadsInOneCoarseUnit(t *testing.T) {
	cases := map[time.Duration]string{
		3 * time.Second:  "3s",
		90 * time.Second: "1m",
		3 * time.Hour:    "3h",
		50 * time.Hour:   "2d",
		-1 * time.Second: "0s",
	}
	for age, want := range cases {
		if got := formatFailureAge(age); got != want {
			t.Errorf("formatFailureAge(%s) = %q, want %q", age, got, want)
		}
	}
}
