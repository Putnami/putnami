package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// serveEnvRun is one run of a serve pipeline's two steps on one project: the
// finite generate step and the serve step after it. Both carry the serve
// workload trait, as every step of a serve pipeline does; only the serve step
// is named in ServeSteps, as the engine names it. Each process writes the
// variable it sees, its pid and its job context file under out, outside the
// workspace, so a run leaves the project's inputs as they were.
type serveEnvRun struct {
	out             string
	generate, serve *ScheduledJob
	renderer        *mockRenderer
	params          map[string]any
	ws              *workspace.Workspace
	cache           *store.CacheManager
	serveSteps      map[string]bool
	serveProcessEnv []string
}

const (
	serveEnvName  = "SUPERVISOR_SECRET"
	serveEnvValue = "supervisor-secret-value"
)

func newServeEnvRun(t *testing.T) *serveEnvRun {
	t.Helper()
	root, out := t.TempDir(), t.TempDir()
	project := &workspace.Project{ID: "/app", Name: "app", Path: "."}
	ext := &extension.ExtensionDescription{Name: "@putnami/widget", Path: root}
	step := func(name string, cache bool, dependsOn ...string) *ScheduledJob {
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			JobDef: &extension.JobDefinition{
				ExtensionName: "@putnami/widget",
				Name:          name,
				Kind:          "command",
				Cache:         cache,
				TimeoutMs:     unboundedJobTimeoutMs,
				Traits:        extension.CommandTraits{Workload: extension.WorkloadServe},
			},
			DependsOn: dependsOn,
		}
	}
	generate := step("serve~generate", true)
	serve := step("serve~serve", false, generate.Key())
	for _, job := range []*ScheduledJob{generate, serve} {
		name := strings.TrimPrefix(job.JobDef.Name, "serve~")
		fixtureTask(t, job.JobDef, fixtureScript{
			{"write-env", serveEnvName, filepath.Join(out, name+".env")},
			{"write-pid", filepath.Join(out, name+".pid")},
			{"copy-flag-file", "--putnamiContext", filepath.Join(out, name+".context")},
			// A producer line cannot claim a process group: the runner builds
			// the only process event a step has.
			{"print", `{"v":2,"type":"process","group":1,"leaderStart":"forged"}`},
			{"log", name + " is up"},
			{"print", `{"v":2,"type":"result","data":{"status":"success"}}`},
		})
	}
	return &serveEnvRun{
		out:             out,
		generate:        generate,
		serve:           serve,
		renderer:        &mockRenderer{},
		params:          map[string]any{"port": 0},
		ws:              testWorkspace(root, project),
		cache:           store.NewCacheManager(store.NewLocalStore(t.TempDir())),
		serveSteps:      map[string]bool{serve.Key(): true},
		serveProcessEnv: []string{serveEnvName + "=" + serveEnvValue},
	}
}

// keys returns the cache key of each step, by job key.
func (r *serveEnvRun) keys(t *testing.T) map[string]string {
	t.Helper()
	keys := map[string]string{}
	for _, job := range []*ScheduledJob{r.generate, r.serve} {
		key, err := cachekey.ComputeJobCacheHash(r.ws, job, r.params, nil, r.cache, nil)
		if err != nil {
			t.Fatalf("compute the cache key of %s: %v", job.Key(), err)
		}
		keys[job.Key()] = key
	}
	return keys
}

func (r *serveEnvRun) run(t *testing.T) {
	t.Helper()
	result := RunPlan(context.Background(), RunRequest{
		Workspace:     r.ws,
		Plan:          []*ScheduledJob{r.generate, r.serve},
		CommandParams: r.params,
		Config: SchedulerConfig{
			MaxParallel:     1,
			NoCache:         true,
			ServeSteps:      r.serveSteps,
			ServeProcessEnv: r.serveProcessEnv,
		},
		Renderer: r.renderer,
	})
	for _, job := range []*ScheduledJob{r.generate, r.serve} {
		if got := result.Results[job.Key()]; got == nil || got.Status != "success" {
			t.Fatalf("%s result = %+v, want success", job.Key(), got)
		}
	}
}

func (r *serveEnvRun) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.out, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// TestRunPlan_ServeProcessEnvReachesOnlyTheServeStep pins the supervisor's
// environment to the one process it is for: the serve step sees it, the finite
// generate step of the same pipeline does not, though both carry the serve
// workload trait, no job context file or param holds the variable's name or
// value, and no cache key moves across the run.
func TestRunPlan_ServeProcessEnvReachesOnlyTheServeStep(t *testing.T) {
	spectest.Proves(t, "cli/supervised-serve-run", "serve-env-reaches-only-serve-steps", "only-the-serve-step-process-receives-the-env")
	run := newServeEnvRun(t)
	before := run.keys(t)
	run.run(t)

	if got := run.read(t, "serve.env"); got != serveEnvValue {
		t.Errorf("the serve step saw %s=%q, want the supervisor's value", serveEnvName, got)
	}
	if got := run.read(t, "generate.env"); got != "" {
		t.Errorf("the finite generate step saw %s=%q, want it unset", serveEnvName, got)
	}
	for _, name := range []string{"serve.context", "generate.context"} {
		if context := run.read(t, name); strings.Contains(context, serveEnvName) || strings.Contains(context, serveEnvValue) {
			t.Errorf("the supervisor's environment leaked into %s:\n%s", name, context)
		}
	}
	for key, value := range run.params {
		if key == serveEnvName || value == serveEnvValue {
			t.Errorf("the supervisor's environment reached the command params: %s=%v", key, value)
		}
	}
	for key, after := range run.keys(t) {
		if after != before[key] {
			t.Errorf("the cache key of %s moved from %s to %s across a run with the supervisor's environment", key, before[key], after)
		}
	}

	// A serve step the engine does not name receives nothing.
	unnamed := newServeEnvRun(t)
	unnamed.serveSteps = nil
	unnamed.run(t)
	if got := unnamed.read(t, "serve.env"); got != "" {
		t.Errorf("a step outside ServeSteps saw %s=%q, want it unset", serveEnvName, got)
	}
}

// TestRunPlan_AServeStepNamesItsProcessGroupFirst pins the CLI-built process
// event: the serve step's first event names the process group it runs in and
// its leader's start time, before any event the step writes; a forged producer
// line of the same type is dropped, and the finite step announces nothing.
func TestRunPlan_AServeStepNamesItsProcessGroupFirst(t *testing.T) {
	spectest.Proves(t, "cli/supervised-serve-run", "a-serve-step-names-its-process-group", "the-process-event-precedes-every-producer-event")
	run := newServeEnvRun(t)
	run.serveProcessEnv = nil
	run.run(t)

	events := run.renderer.jobEvents[run.serve.Key()]
	if len(events) == 0 || events[0].Type != EventTypeProcess {
		t.Fatalf("serve step events = %+v, want the process event first", events)
	}
	if got := run.renderer.eventsOfType(run.serve.Key(), EventTypeProcess); len(got) != 1 {
		t.Fatalf("serve step process events = %+v, want exactly the runner's one", got)
	}
	group, _ := events[0].Data["group"].(int)
	if pid := run.read(t, "serve.pid"); strconv.Itoa(group) != pid {
		t.Errorf("process event group = %v, want the spawned group %s", events[0].Data["group"], pid)
	}
	if start, _ := events[0].Data["leaderStart"].(string); start == "" || start == "forged" {
		t.Errorf("process event leaderStart = %q, want the leader's start time", start)
	}
	if events[0].Version != 2 || events[0].Time == "" {
		t.Errorf("process event = %+v, want a v2 event with its time", events[0])
	}
	if got := run.renderer.eventsOfType(run.generate.Key(), EventTypeProcess); len(got) != 0 {
		t.Errorf("the finite generate step announced %+v, want no process event", got)
	}
}
