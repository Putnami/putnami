package engine

import (
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func runJobFixture(cmd, name string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		JobDef:  &extension.JobDefinition{Name: name, CommandName: cmd},
	}
}

func TestRunForwardedExitCode_ForwardsNonZero(t *testing.T) {
	t.Parallel()
	job := runJobFixture("run", "run~run")
	// JSONL numbers decode as float64; the workload exited 3.
	results := map[string]*jobs.JobResult{
		job.Key(): {Status: "failed", Data: map[string]any{"exit-code": float64(3)}},
	}
	code, ok := runForwardedExitCode([]*jobs.ScheduledJob{job}, results)
	if !ok || code != 3 {
		t.Fatalf("expected (3, true), got (%d, %v)", code, ok)
	}
}

func TestRunForwardedExitCode_ZeroNotForwarded(t *testing.T) {
	t.Parallel()
	job := runJobFixture("run", "run~run")
	results := map[string]*jobs.JobResult{
		job.Key(): {Status: "success", Data: map[string]any{"exit-code": float64(0)}},
	}
	if code, ok := runForwardedExitCode([]*jobs.ScheduledJob{job}, results); ok {
		t.Fatalf("expected zero code not to be forwarded, got (%d, %v)", code, ok)
	}
}

func TestRunForwardedExitCode_NoData(t *testing.T) {
	t.Parallel()
	job := runJobFixture("run", "run~run")
	// Build step failed upstream: no run result recorded.
	if _, ok := runForwardedExitCode([]*jobs.ScheduledJob{job}, map[string]*jobs.JobResult{}); ok {
		t.Fatal("expected no forwarded code when run never produced data")
	}
}

func TestRunForwardedExitCode_IgnoresNonRunCommands(t *testing.T) {
	t.Parallel()
	build := runJobFixture("build", "build~compile")
	results := map[string]*jobs.JobResult{
		build.Key(): {Status: "failed", Data: map[string]any{"exit-code": float64(9)}},
	}
	if _, ok := runForwardedExitCode([]*jobs.ScheduledJob{build}, results); ok {
		t.Fatal("expected non-run commands to be ignored")
	}
}

func TestExitCodeFromData(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want int
	}{
		{float64(7), 7},
		{int(5), 5},
		{int64(11), 11},
		{"nope", 0},
		{nil, 0},
	}
	for _, c := range cases {
		if got := exitCodeFromData(c.in); got != c.want {
			t.Errorf("exitCodeFromData(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
