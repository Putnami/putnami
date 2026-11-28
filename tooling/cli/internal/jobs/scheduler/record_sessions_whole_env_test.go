package scheduler

import (
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// TestARunWhoseSessionsTravelTellsTheRunsItSpawns pins how a run nested in a
// bound portable execution learns that its session travels to a machine whose
// store does not hold its entries (CLI ADR 0137): the scheduler of such a run
// exports RecordSessionsWholeEnv into every task subprocess, --no-cache
// included, a task cannot blank it, and the job context, whose members are
// hashed, never carries it. A run whose sessions stay exports nothing.
func TestARunWhoseSessionsTravelTellsTheRunsItSpawns(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "cached-task-by-reference", "a-session-whose-readers-cannot-resolve-writes-the-records")
	want := RecordSessionsWholeEnv + "=1"
	for _, noCache := range []bool{false, true} {
		s := &Scheduler{cfg: SchedulerConfig{RecordSessionsWhole: true, NoCache: noCache}}
		if got := s.jobProcessEnv(); !slices.Contains(got, want) {
			t.Errorf("jobProcessEnv() with NoCache=%v = %v, want it to contain %q", noCache, got, want)
		}
	}
	for _, entry := range (&Scheduler{cfg: SchedulerConfig{SessionID: "20261128-120000-abc123"}}).jobProcessEnv() {
		if strings.HasPrefix(entry, RecordSessionsWholeEnv+"=") {
			t.Errorf("a run whose sessions stay exported %q", entry)
		}
	}

	ws := makeExecutorTestWorkspace(t)
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
		JobDef: &extension.JobDefinition{
			Name: "build", CommandName: "build", ExtensionName: "@test/ext",
			Command: "true", Env: map[string]string{RecordSessionsWholeEnv: ""},
		},
	}
	jobCtx := &JobCommandContext{
		WorkspaceRoot: ws.Root,
		Workspace:     JobContextWorkspace{Name: "ws"},
		Extension:     JobContextExtension{Name: "@test/ext"},
		Job:           JobContextJob{Name: "build"},
	}
	invocation := buildJobInvocation(ws, job, jobCtx, []string{want}, "")
	if got := envLastValue(invocation.env, RecordSessionsWholeEnv); got != "1" {
		t.Errorf("%s = %q, want 1: a task blanked that its run's sessions travel", RecordSessionsWholeEnv, got)
	}

	t.Setenv(RecordSessionsWholeEnv, "1")
	for _, entry := range BuildEnvVars(BuildJobContext(ws, job, nil, nil, nil)) {
		if strings.HasPrefix(entry, RecordSessionsWholeEnv+"=") {
			t.Fatalf("BuildEnvVars carries %s; the job-context document it mirrors is hashed", RecordSessionsWholeEnv)
		}
	}
}
