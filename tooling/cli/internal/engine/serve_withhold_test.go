package engine

import (
	"slices"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// pipelineJob is a planned step whose command's resolved traits declare
// workload, or none when it is empty.
func pipelineJob(project *workspace.Project, name, workload string, cache bool, dependsOn ...string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/fixture"},
		JobDef:    &extension.JobDefinition{Name: name, Cache: cache, Traits: extension.CommandTraits{Workload: workload}},
		DependsOn: dependsOn,
	}
}

// servePipelineJob is a step of a command that declares the serve workload.
func servePipelineJob(project *workspace.Project, name string, cache bool, dependsOn ...string) *jobs.ScheduledJob {
	return pipelineJob(project, name, extension.WorkloadServe, cache, dependsOn...)
}

func jobKeys(planned []*jobs.ScheduledJob) []string {
	keys := make([]string, 0, len(planned))
	for _, job := range planned {
		keys = append(keys, job.Key())
	}
	return keys
}

// A composition must start each member's workload itself, so the engine hands
// back exactly the terminal, uncacheable serve step of every SELECTED project
// and keeps every finite step — including the generate step of a dependency
// nobody asked to serve — in the scheduled run.
func TestWithholdServeSteps_TakesOnlyTheTerminalServeStepOfSelectedProjects(t *testing.T) {
	provider := &workspace.Project{ID: "/provider", Name: "provider", Path: "provider"}
	consumer := &workspace.Project{ID: "/consumer", Name: "consumer", Path: "consumer"}
	library := &workspace.Project{ID: "/library", Name: "library", Path: "library"}

	planned := []*jobs.ScheduledJob{
		servePipelineJob(library, "serve~generate", true),
		servePipelineJob(provider, "serve~generate", true, "/library:serve~generate"),
		servePipelineJob(provider, "serve~describe", true, "/provider:serve~generate"),
		servePipelineJob(provider, "serve~serve", false, "/provider:serve~describe"),
		servePipelineJob(consumer, "serve~generate", true),
		servePipelineJob(consumer, "serve~serve", false, "/consumer:serve~generate"),
		// An uncacheable serve-command step something still depends on is not
		// terminal, and a library's own serve step was not selected.
		servePipelineJob(consumer, "serve~prepare", false),
		servePipelineJob(consumer, "serve~after", true, "/consumer:serve~prepare"),
		servePipelineJob(library, "serve~serve", false),
	}

	kept, withheld := withholdServeSteps(planned, []*workspace.Project{provider, consumer})

	if got, want := jobKeys(withheld), []string{"/provider:serve~serve", "/consumer:serve~serve"}; !slices.Equal(got, want) {
		t.Errorf("withheld = %v, want %v", got, want)
	}
	want := []string{
		"/library:serve~generate", "/provider:serve~generate", "/provider:serve~describe",
		"/consumer:serve~generate", "/consumer:serve~prepare", "/consumer:serve~after", "/library:serve~serve",
	}
	if got := jobKeys(kept); !slices.Equal(got, want) {
		t.Errorf("kept = %v, want %v", got, want)
	}
	for _, job := range kept {
		for _, predecessor := range job.SchedulingPredecessors() {
			if slices.Contains(jobKeys(withheld), predecessor) {
				t.Errorf("kept job %s depends on withheld step %s", job.Key(), predecessor)
			}
		}
	}
}

func TestWithholdServeSteps_NonServeCommandsAreNeverWithheld(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	planned := []*jobs.ScheduledJob{
		pipelineJob(app, "run~exec", extension.WorkloadOnce, false),
		pipelineJob(app, "test", "", false),
		nil,
	}
	kept, withheld := withholdServeSteps(planned, []*workspace.Project{app, nil})
	if len(withheld) != 0 {
		t.Errorf("withheld = %v, want none", jobKeys(withheld))
	}
	if len(kept) != len(planned) {
		t.Errorf("kept %d of %d jobs", len(kept), len(planned))
	}
}

// A composition withholds the terminal step of a command whose traits declare
// the serve workload, whatever its name: a daemon command that declares it
// gives its step to the supervisor, and a serve command that declares no
// workload runs its step like any finite one.
func TestWithholdServeSteps_FollowTheServeWorkloadNotTheName(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-serve-workload-command-of-any-name-streams-until-interrupted")
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	planned := []*jobs.ScheduledJob{
		pipelineJob(app, "daemon~start", extension.WorkloadServe, false),
		pipelineJob(app, "serve~serve", "", false),
	}
	kept, withheld := withholdServeSteps(planned, []*workspace.Project{app})
	if got := jobKeys(withheld); !slices.Equal(got, []string{"/app:daemon~start"}) {
		t.Errorf("withheld = %v, want [/app:daemon~start]", got)
	}
	if got := jobKeys(kept); !slices.Equal(got, []string{"/app:serve~serve"}) {
		t.Errorf("kept = %v, want [/app:serve~serve]", got)
	}
}

// A run streams its workload when one of its commands declares a workload in
// the loaded extensions, whatever its name: the serve workload is promoted to
// watch, the once workload streams once. A serve or run command that declares
// none streams nothing and is not promoted.
func TestWorkloadModes_FollowTheDeclaredWorkloadNotTheName(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-serve-workload-command-of-any-name-streams-until-interrupted")
	command := func(name, workload string) *extension.JobDefinition {
		return &extension.JobDefinition{Name: name, Kind: "command", Traits: extension.CommandTraits{Workload: workload}}
	}
	extensions := []*extension.ExtensionDescription{{Name: "@acme/lang", Jobs: map[string]*extension.JobDefinition{
		"daemon":  command("daemon", extension.WorkloadServe),
		"migrate": command("migrate", extension.WorkloadOnce),
		"serve":   command("serve", ""),
		"run":     command("run", ""),
	}}}
	for _, tc := range []struct {
		commands    []string
		serve, once bool
	}{
		{commands: []string{"daemon"}, serve: true},
		{commands: []string{"build", "daemon"}, serve: true},
		{commands: []string{"migrate"}, once: true},
		{commands: []string{"serve"}},
		{commands: []string{"run"}},
	} {
		serve, once := workloadModes(extensions, tc.commands)
		if serve != tc.serve || once != tc.once {
			t.Errorf("workloadModes(%v) = (%v, %v), want (%v, %v)", tc.commands, serve, once, tc.serve, tc.once)
		}
		if got := forcesServeWatch(&Request{Commands: tc.commands}, serve); got != tc.serve {
			t.Errorf("%v promoted to watch: %v, want %v", tc.commands, got, tc.serve)
		}
	}
	if serve, once := workloadModes(nil, []string{"serve", "run"}); serve || once {
		t.Errorf("without a loaded extension, serve and run stream: (%v, %v)", serve, once)
	}
}

// Serve watches by default, and --no-watch, which reaches the run
// as the serve flag `watch: false`, keeps it out of the restart loop.
func TestForcesServeWatch_NoWatchKeepsServeOutOfTheLoop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		watch any // the run's `watch` param; nil leaves it out
		serve bool
		want  bool
	}{
		{name: "serve by default", serve: true, want: true},
		{name: "serve --watch=true", watch: true, serve: true, want: true},
		{name: "serve --no-watch", watch: false, serve: true, want: false},
		{name: "not serve", serve: false, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{}
			if tc.watch != nil {
				params["watch"] = tc.watch
			}
			if got := forcesServeWatch(&Request{CommandParams: params}, tc.serve); got != tc.want {
				t.Errorf("forcesServeWatch = %v, want %v", got, tc.want)
			}
		})
	}
}
