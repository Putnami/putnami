package engine

import (
	"testing"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// runJobFixture is a job of command cmd whose resolved traits declare
// workload, or none when it is empty.
func runJobFixture(cmd, name, workload string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		JobDef:  &extension.JobDefinition{Name: name, CommandName: cmd, Traits: extension.CommandTraits{Workload: workload}},
	}
}

// exitedWith is the result of job whose workload reported code. JSONL numbers
// decode as float64.
func exitedWith(job *jobs.ScheduledJob, code float64) map[string]*jobs.JobResult {
	status := "failed"
	if code == 0 {
		status = "success"
	}
	return map[string]*jobs.JobResult{job.Key(): {Status: status, Data: map[string]any{"exit-code": code}}}
}

func TestRunForwardedExitCode_ForwardsNonZero(t *testing.T) {
	t.Parallel()
	job := runJobFixture("run", "run~run", extension.WorkloadOnce)
	code, ok := runForwardedExitCode([]*jobs.ScheduledJob{job}, exitedWith(job, 3))
	if !ok || code != 3 {
		t.Fatalf("expected (3, true), got (%d, %v)", code, ok)
	}
}

func TestRunForwardedExitCode_ZeroNotForwarded(t *testing.T) {
	t.Parallel()
	job := runJobFixture("run", "run~run", extension.WorkloadOnce)
	if code, ok := runForwardedExitCode([]*jobs.ScheduledJob{job}, exitedWith(job, 0)); ok {
		t.Fatalf("expected zero code not to be forwarded, got (%d, %v)", code, ok)
	}
}

func TestRunForwardedExitCode_NoData(t *testing.T) {
	t.Parallel()
	job := runJobFixture("run", "run~run", extension.WorkloadOnce)
	// Build step failed upstream: no run result recorded.
	if _, ok := runForwardedExitCode([]*jobs.ScheduledJob{job}, map[string]*jobs.JobResult{}); ok {
		t.Fatal("expected no forwarded code when run never produced data")
	}
}

func TestRunForwardedExitCode_IgnoresNonRunCommands(t *testing.T) {
	t.Parallel()
	build := runJobFixture("build", "build~compile", "")
	if _, ok := runForwardedExitCode([]*jobs.ScheduledJob{build}, exitedWith(build, 9)); ok {
		t.Fatal("expected non-run commands to be ignored")
	}
}

// The exit code a workload reports is forwarded when its job's resolved
// traits declare the once workload, whatever the command's name: a migrate
// command that declares it forwards 3, a run command that declares no
// workload forwards nothing, and a serve workload forwards nothing either.
func TestRunForwardedExitCode_FollowsTheOnceWorkload(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-once-workload-command-forwards-its-exit-code")
	migrate := runJobFixture("migrate", "migrate~exec", extension.WorkloadOnce)
	if code, ok := runForwardedExitCode([]*jobs.ScheduledJob{migrate}, exitedWith(migrate, 3)); !ok || code != 3 {
		t.Errorf("a once-workload migrate forwarded (%d, %v), want (3, true)", code, ok)
	}
	for _, job := range []*jobs.ScheduledJob{
		runJobFixture("run", "run~run", ""),
		runJobFixture("daemon", "daemon~start", extension.WorkloadServe),
	} {
		if code, ok := runForwardedExitCode([]*jobs.ScheduledJob{job}, exitedWith(job, 3)); ok {
			t.Errorf("%s forwarded (%d, %v), want nothing", job.Key(), code, ok)
		}
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
