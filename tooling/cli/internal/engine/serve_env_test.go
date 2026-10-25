package engine

import (
	"bytes"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const serveEnvTestSecret = "supervisor-secret-value"

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

// serveEnvPlan is one selected project's serve pipeline, a dependency's
// finite step and a non-serve task.
func serveEnvPlan() ([]*jobs.ScheduledJob, []*workspace.Project) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	library := &workspace.Project{ID: "/library", Name: "library", Path: "library"}
	planned := []*jobs.ScheduledJob{
		servePipelineJob(library, "serve~generate", true),
		servePipelineJob(app, "serve~generate", true, "/library:serve~generate"),
		servePipelineJob(app, "serve~serve", false, "/app:serve~generate"),
		pipelineJob(app, "lint", "", false),
	}
	return planned, []*workspace.Project{app}
}

func serveEnvRequest(env ...string) *Request {
	req := &Request{ServeProcessEnv: env}
	req.Global.ServeEnv = "-"
	return req
}

// serveEnvRefusal runs the check and returns the refusal it printed, or ""
// when the run is accepted. An accepted run prints nothing.
func serveEnvRefusal(t *testing.T, req *Request, planned []*jobs.ScheduledJob, steps map[string]bool) string {
	t.Helper()
	var stderr bytes.Buffer
	refused := refuseInvalidServeEnv(req, planned, steps, &stderr)
	if refused != (stderr.Len() > 0) {
		t.Fatalf("refused = %v with stderr %q: a refusal prints exactly when it refuses", refused, stderr.String())
	}
	return stderr.String()
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// The supervisor's environment reaches the terminal serve step of a selected
// project and nothing else: the finite steps of its pipeline and of its
// dependencies carry the serve workload trait too, but they are not the
// workload the supervisor runs.
func TestServeWorkloadSteps_NamesOnlyTheTerminalServeStep(t *testing.T) {
	spectest.Proves(t, "cli/supervised-serve-run", "serve-env-reaches-only-serve-steps", "the-engine-names-only-the-terminal-serve-step")
	planned, selected := serveEnvPlan()
	if got, want := sortedKeys(serveWorkloadSteps(planned, selected)), []string{"/app:serve~serve"}; !slices.Equal(got, want) {
		t.Errorf("serve-workload steps = %v, want %v", got, want)
	}
	if steps := serveWorkloadSteps(planned[3:], selected); steps != nil {
		t.Errorf("a plan without a serve step named %v, want nil", sortedKeys(steps))
	}
}

// Only the terminal, uncacheable serve step of every SELECTED project is a
// serve-workload step: the generate step of a dependency nobody asked to
// serve, a step something still depends on, and a step of a project that was
// not selected are not. A composition member is the one project its serve run
// selects, so this step is the member's workload.
func TestServeWorkloadSteps_TakesOnlyTheTerminalServeStepOfSelectedProjects(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-composition-member-is-a-serve-workload-step")
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

	steps := serveWorkloadSteps(planned, []*workspace.Project{provider, consumer})
	if got, want := sortedKeys(steps), []string{"/consumer:serve~serve", "/provider:serve~serve"}; !slices.Equal(got, want) {
		t.Errorf("serve-workload steps = %v, want %v", got, want)
	}
}

func TestServeWorkloadSteps_NonServeCommandsAreNeverOne(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	planned := []*jobs.ScheduledJob{
		pipelineJob(app, "run~exec", extension.WorkloadOnce, false),
		pipelineJob(app, "test", "", false),
		nil,
	}
	if steps := serveWorkloadSteps(planned, []*workspace.Project{app, nil}); steps != nil {
		t.Errorf("serve-workload steps = %v, want none", sortedKeys(steps))
	}
}

// The terminal step of a command whose traits declare the serve workload is a
// serve-workload step whatever its name: a daemon command that declares it
// streams until interrupted, and a serve command that declares no workload
// runs its step like any finite one.
func TestServeWorkloadSteps_FollowTheServeWorkloadNotTheName(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "workload-lifecycle", "a-serve-workload-command-of-any-name-streams-until-interrupted")
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	planned := []*jobs.ScheduledJob{
		pipelineJob(app, "daemon~start", extension.WorkloadServe, false),
		pipelineJob(app, "serve~serve", "", false),
	}
	if got := sortedKeys(serveWorkloadSteps(planned, []*workspace.Project{app})); !slices.Equal(got, []string{"/app:daemon~start"}) {
		t.Errorf("serve-workload steps = %v, want [/app:daemon~start]", got)
	}
}

func TestRefuseInvalidServeEnv_AcceptsAServeRunAndIgnoresARunWithoutTheFlag(t *testing.T) {
	planned, selected := serveEnvPlan()
	steps := serveWorkloadSteps(planned, selected)
	if refusal := serveEnvRefusal(t, serveEnvRequest("PORT=0", "TOKEN="+serveEnvTestSecret), planned, steps); refusal != "" {
		t.Errorf("a serve run with --serve-env was refused: %s", refusal)
	}
	if refusal := serveEnvRefusal(t, &Request{}, planned[3:], nil); refusal != "" {
		t.Errorf("a run without --serve-env was refused: %s", refusal)
	}
}

func TestRefuseInvalidServeEnv_RefusesARunWithoutAServeStep(t *testing.T) {
	planned, selected := serveEnvPlan()
	lintOnly := planned[3:]
	refusal := serveEnvRefusal(t, serveEnvRequest("PORT=0"), lintOnly, serveWorkloadSteps(lintOnly, selected))
	if !strings.HasPrefix(refusal, "putnami: --serve-env needs a serve-workload step") {
		t.Errorf("refusal = %q, want the refusal of a plan without a serve step", refusal)
	}
	// An empty document still asked for a serve run.
	if serveEnvRefusal(t, serveEnvRequest(), lintOnly, nil) == "" {
		t.Error("an empty --serve-env document on a plan without a serve step was accepted")
	}
}

// A serve task that declares a variable in its own manifest env would replace
// the supervisor's value, because the job runner applies the manifest env
// last; the run is refused instead, naming the variable and never its value.
func TestRefuseInvalidServeEnv_RefusesAManifestEnvConflict(t *testing.T) {
	spectest.Proves(t, "cli/supervised-serve-run", "a-manifest-env-conflict-is-refused", "the-refusal-names-the-variable-and-the-task")
	planned, selected := serveEnvPlan()
	planned[2].JobDef.Env = map[string]string{"NODE_ENV": "development", "PORT": "4310"}
	refusal := serveEnvRefusal(t, serveEnvRequest("PORT="+serveEnvTestSecret), planned, serveWorkloadSteps(planned, selected))
	if refusal == "" {
		t.Fatal("a conflict with the serve task's manifest env was accepted")
	}
	for _, want := range []string{"PORT", "serve~serve", "/app"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal %q does not name %s", refusal, want)
		}
	}
	for _, leaked := range []string{serveEnvTestSecret, "4310"} {
		if strings.Contains(refusal, leaked) {
			t.Errorf("the refusal %q quotes a value", refusal)
		}
	}

	// A finite step's own env is not the serve process's: no conflict.
	planned, selected = serveEnvPlan()
	planned[1].JobDef.Env = map[string]string{"PORT": "4310"}
	if refusal := serveEnvRefusal(t, serveEnvRequest("PORT=0"), planned, serveWorkloadSteps(planned, selected)); refusal != "" {
		t.Errorf("a finite step's env was taken for a conflict: %s", refusal)
	}
}

func TestRefuseInvalidServeEnv_RefusesAPortableRun(t *testing.T) {
	planned, selected := serveEnvPlan()
	steps := serveWorkloadSteps(planned, selected)
	executing := serveEnvRequest("PORT=0")
	executing.Portable = &PortableExecution{}
	submitting := serveEnvRequest("PORT=0")
	submitting.runnerProvider = &extension.ResolvedProvider{}
	for name, req := range map[string]*Request{"executing": executing, "submitting": submitting} {
		if refusal := serveEnvRefusal(t, req, planned, steps); !strings.Contains(refusal, "portable run") {
			t.Errorf("%s side of a portable run: refusal = %q, want the portable refusal", name, refusal)
		}
	}
}

// A hosted runner has no supervisor on its stdin. SetForTest changes the
// whole process, so this test does not run in parallel.
func TestRefuseInvalidServeEnv_RefusesAHostedRun(t *testing.T) {
	t.Cleanup(runcredential.SetForTest("prc_engine_serve_env"))
	planned, selected := serveEnvPlan()
	if refusal := serveEnvRefusal(t, serveEnvRequest("PORT=0"), planned, serveWorkloadSteps(planned, selected)); !strings.Contains(refusal, "hosted run") {
		t.Errorf("refusal = %q, want the hosted refusal", refusal)
	}
}

// Only a jsonl run that reads --serve-env is supervised, and renders the serve
// log on stderr; a text run already renders it on its own streams.
func TestSupervisedServe_IsOnlyAJSONLRunThatReadsServeEnv(t *testing.T) {
	cases := []struct {
		serveEnv, output string
		want             bool
	}{
		{"-", "jsonl", true},
		{"-", "json", false},
		{"-", "", false},
		{"", "jsonl", false},
	}
	for _, c := range cases {
		flags := &GlobalFlags{ServeEnv: c.serveEnv, Output: c.output}
		if got := SupervisedServe(flags); got != c.want {
			t.Errorf("SupervisedServe(--serve-env=%q --output=%q) = %v, want %v", c.serveEnv, c.output, got, c.want)
		}
	}
}
