package scheduler

import (
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// TestParentSessionEnvIsNotInTheJobContext pins the OTHER path a value could
// reach a hash by. BuildEnvVars mirrors the JSON job-context document, and that
// document is serialized to a job-context-*.json file the task reads; its
// members are the task's declared inputs, several of which are lifted into the
// cache key as params. The parent session id is deliberately absent from both,
// which is why it is delivered as run-level process environment instead.
func TestParentSessionEnvIsNotInTheJobContext(t *testing.T) {
	ws := &workspace.Workspace{Root: t.TempDir(), Name: "ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
		JobDef:    &extension.JobDefinition{Name: "build", CommandName: "build", ExtensionName: "@test/ext"},
	}
	t.Setenv(protocolcli.ParentSessionEnv, "20260913-023611-4a5b87")

	jobCtx := BuildJobContext(ws, job, nil, nil, nil)
	for _, entry := range BuildEnvVars(jobCtx) {
		if strings.HasPrefix(entry, protocolcli.ParentSessionEnv+"=") {
			t.Fatalf("BuildEnvVars carries %s; the job-context document it mirrors is hashed", protocolcli.ParentSessionEnv)
		}
	}
}

// TestJobProcessEnvCarriesTheSessionID states the delivery contract from the
// other side: the scheduler DOES export the running session's id to every task
// subprocess, including under --no-cache.
//
// The --no-cache case is the one worth pinning. The object-cache entries beside
// it are withheld then, deliberately — a run that consults no cache must not
// hand its jobs one — and it would be an easy, invisible mistake to gate the
// session id the same way. That would make nested runs unattributable on
// exactly the uncached runs whose cost matters most.
func TestJobProcessEnvCarriesTheSessionID(t *testing.T) {
	want := protocolcli.ParentSessionEnv + "=20260913-023611-4a5b87"
	for _, noCache := range []bool{false, true} {
		scheduler := &Scheduler{cfg: SchedulerConfig{SessionID: "20260913-023611-4a5b87", NoCache: noCache}}
		if got := scheduler.jobProcessEnv(); !slices.Contains(got, want) {
			t.Errorf("jobProcessEnv() with NoCache=%v = %v, want it to contain %q", noCache, got, want)
		}
	}

	// A run that records no session (every lifecycle run) exports nothing, so a
	// task it spawns is not told it has a parent that does not exist.
	scheduler := &Scheduler{cfg: SchedulerConfig{}}
	for _, entry := range scheduler.jobProcessEnv() {
		if strings.HasPrefix(entry, protocolcli.ParentSessionEnv+"=") {
			t.Errorf("a session-less run exported %q", entry)
		}
	}
}

// TestParentSessionEnvIsNotOverridableByATask pins the PRECEDENCE.
//
// buildJobInvocation assembles cmd.Env as os.Environ(), BuildEnvVars, extraEnv,
// then the job's own Env — and os/exec keeps the LAST duplicate. A manifest that
// spelled PUTNAMI_PARENT_SESSION in its job Env would therefore have won over
// the scheduler's value, and the two ways that corrupts a ledger are opposite:
// a wrong id makes a nested run name a session that never spawned it, and an
// EMPTY one makes it look top-level and be counted as a gate of its own.
//
// The re-assertion is the same rule the runtime event protocol already states
// one line above it in runner.go: a fact about how the CLI spawns a process is
// never task configuration.
func TestParentSessionEnvIsNotOverridableByATask(t *testing.T) {
	const scheduled = "20260913-161334-a9bebb"

	for _, tc := range []struct {
		name     string
		taskEnv  map[string]string
		extraEnv []string
		want     string
	}{
		{
			name:     "a task cannot restate it",
			taskEnv:  map[string]string{protocolcli.ParentSessionEnv: "20260101-000000-000000"},
			extraEnv: []string{protocolcli.ParentSessionEnv + "=" + scheduled},
			want:     scheduled,
		},
		{
			name:     "a task cannot blank it",
			taskEnv:  map[string]string{protocolcli.ParentSessionEnv: ""},
			extraEnv: []string{protocolcli.ParentSessionEnv + "=" + scheduled},
			want:     scheduled,
		},
		{
			name:     "the scheduler's value is carried when no task states one",
			extraEnv: []string{protocolcli.ParentSessionEnv + "=" + scheduled},
			want:     scheduled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := makeExecutorTestWorkspace(t)
			job := &ScheduledJob{
				Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
				Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
				JobDef: &extension.JobDefinition{
					Name: "build", CommandName: "build", ExtensionName: "@test/ext",
					Command: "true", Env: tc.taskEnv,
				},
			}

			invocation := buildJobInvocation(ws, job, &JobCommandContext{
				WorkspaceRoot: ws.Root,
				Workspace:     JobContextWorkspace{Name: "ws"},
				Extension:     JobContextExtension{Name: "@test/ext"},
				Job:           JobContextJob{Name: "build"},
			}, tc.extraEnv, "")

			if got := envLastValue(invocation.env, protocolcli.ParentSessionEnv); got != tc.want {
				t.Errorf("%s = %q, want %q: a task overrode who spawned it",
					protocolcli.ParentSessionEnv, got, tc.want)
			}
		})
	}
}

// TestParentSessionEnvIsAbsentWhenTheRunRecordsNoSession pins the other half:
// a run with no session (every lifecycle run) exports nothing, so a task it
// spawns is top-level rather than a child of a session that does not exist.
func TestParentSessionEnvIsAbsentWhenTheRunRecordsNoSession(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
		JobDef: &extension.JobDefinition{
			Name: "build", CommandName: "build", ExtensionName: "@test/ext", Command: "true",
		},
	}

	invocation := buildJobInvocation(ws, job, &JobCommandContext{
		WorkspaceRoot: ws.Root,
		Workspace:     JobContextWorkspace{Name: "ws"},
		Extension:     JobContextExtension{Name: "@test/ext"},
		Job:           JobContextJob{Name: "build"},
	}, nil, "")

	// The ambient value this very test process may carry is the point: the CLI
	// must not re-export an inherited id as if it had scheduled it.
	for _, entry := range invocation.env[len(invocation.env)-1:] {
		if strings.HasPrefix(entry, protocolcli.ParentSessionEnv+"=") {
			t.Errorf("a run recording no session re-asserted %s as its own: %q",
				protocolcli.ParentSessionEnv, entry)
		}
	}
}
