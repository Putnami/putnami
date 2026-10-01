package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/extension"
	modeljobs "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	extensionproto "go.putnami.dev/protocol/extension"
	protocoljob "go.putnami.dev/protocol/job"
	runner "go.putnami.dev/protocol/runner"
	internalextension "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

func portableJob(project *workspace.Project, name string, dependsOn ...string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@fixture/gate", Version: "1.0.0"},
		JobDef: &extension.JobDefinition{
			Name: name, CommandName: strings.Split(name, "~")[0], TimeoutMs: 60_000, Cache: true,
			ContractDigest: "tc1:" + strings.Repeat("a", 64),
			Reads:          []extension.ResourceRef{{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}},
		},
		DependsOn: dependsOn,
	}
}

func portablePlan() []*jobs.ScheduledJob {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	planned := []*jobs.ScheduledJob{portableJob(app, "build", "/lib:build"), portableJob(lib, "build"), portableJob(app, "test~unit", "/app:build")}
	modeljobs.AttachIdentities(planned)
	return planned
}

func TestExpectedPlanProjectsIdentitiesEdgesAndContracts(t *testing.T) {
	t.Parallel()
	plan, err := expectedPlan(portablePlan())
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.ValidatePlan(plan); err != nil {
		t.Fatalf("projected plan is not admissible: %v", err)
	}
	keys := make([]string, 0, len(plan.Tasks))
	for _, task := range plan.Tasks {
		keys = append(keys, task.Identity.Key)
	}
	if strings.Join(keys, ",") != "/app:build,/app:test~unit,/lib:build" {
		t.Fatalf("plan order = %v", keys)
	}
	build := plan.Tasks[0]
	if strings.Join(build.DependsOn, ",") != "/lib:build" || !build.Cacheable || build.DeadlineMs != 60_000 || build.ContractDigest != "tc1:"+strings.Repeat("a", 64) {
		t.Fatalf("projected task = %+v", build)
	}
	if len(build.Resources.Reads) != 1 || build.Resources.Reads[0].ID != extension.ResourceIDSources || build.Resources.CPUWeight != 1 {
		t.Fatalf("projected resources = %+v", build.Resources)
	}
	if err := validateExpectedPlan(plan, portablePlan()); err != nil {
		t.Fatalf("an identical re-plan was refused: %v", err)
	}
}

func TestValidateExpectedPlanRefusesEveryDivergence(t *testing.T) {
	t.Parallel()
	expected, err := expectedPlan(portablePlan())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func([]*jobs.ScheduledJob) []*jobs.ScheduledJob{
		"missing task": func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob { return p[:2] },
		"extra task":   func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob { return append(p, portableJob(p[0].Project, "lint")) },
		"edge dropped": func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob { p[0].DependsOn = nil; return p },
		"serialize edge": func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob {
			p[0].SerializeAfter = []string{"/lib:build"}
			return p
		},
		"contract": func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob {
			p[1].JobDef.ContractDigest = "tc1:" + strings.Repeat("b", 64)
			return p
		},
		"deadline":  func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob { p[1].JobDef.TimeoutMs = 1; return p },
		"cacheable": func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob { p[1].JobDef.Cache = false; return p },
		"resource":  func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob { p[1].JobDef.Writes = p[1].JobDef.Reads; return p },
		"extension": func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob { p[2].Extension.Name = "@other/gate"; return p },
		"provider version": func(p []*jobs.ScheduledJob) []*jobs.ScheduledJob {
			p[2].Extension = &extension.ExtensionDescription{Name: "@fixture/gate", Version: "2.0.0"}
			return p
		},
	}
	for name, mutate := range cases {
		planned := mutate(portablePlan())
		modeljobs.AttachIdentities(planned)
		err := validateExpectedPlan(expected, planned)
		if err == nil || !strings.Contains(err.Error(), "differs from the expected plan") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// hostAbs is the slash path p as an absolute path on this platform. On Windows
// a path without a volume is relative, so "/elsewhere" alone would not name a
// directory outside the workspace.
func hostAbs(p string) string {
	abs, err := filepath.Abs(filepath.FromSlash(p))
	if err != nil {
		return p
	}
	return abs
}

func TestRejectUnsupportedRemoteShape(t *testing.T) {
	t.Parallel()
	planned := portablePlan()
	base := func() *Request {
		return &Request{WorkspaceRoot: hostAbs("/ws"), Commands: []string{"build"}}
	}
	if err := rejectUnsupportedRemoteShape(base(), planned); err != nil {
		t.Fatalf("finite verification refused: %v", err)
	}
	cases := map[string]func(*Request, []*jobs.ScheduledJob){
		"watch":   func(r *Request, _ []*jobs.ScheduledJob) { r.Global.Watch = true },
		"serve":   func(r *Request, _ []*jobs.ScheduledJob) { r.Commands = []string{"build", "serve"} },
		"run":     func(r *Request, _ []*jobs.ScheduledJob) { r.Commands = []string{"run"} },
		"qualify": func(r *Request, _ []*jobs.ScheduledJob) { r.Commands = []string{"qualify"} },
		"format":  func(r *Request, _ []*jobs.ScheduledJob) { r.Commands = []string{"format"} },
		"dry-run": func(r *Request, _ []*jobs.ScheduledJob) { r.ExecutesUnderDryRun = true },
		"side effects": func(_ *Request, p []*jobs.ScheduledJob) {
			p[0].JobDef.Traits.SideEffects = extensionproto.SideEffectsRegistry
		},
		"external cwd":     func(_ *Request, p []*jobs.ScheduledJob) { p[0].JobDef.Cwd = hostAbs("/elsewhere") },
		"declared effects": func(_ *Request, p []*jobs.ScheduledJob) { declareTaskEffect(p[0], extension.EffectRegistry) },
	}
	for name, mutate := range cases {
		req, planned := base(), portablePlan()
		mutate(req, planned)
		if err := rejectUnsupportedRemoteShape(req, planned); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A manifest task's declared effect is refused even when its command's
	// traits declare none: the submitter never sets invocation.publication.
	declared := portablePlan()
	declareTaskEffect(declared[0], extension.EffectCloud)
	if err := rejectUnsupportedRemoteShape(base(), declared); err == nil || !strings.Contains(err.Error(), "registry or cloud effects") {
		t.Fatalf("a task declaring a cloud effect was not refused as publication: %v", err)
	}
	inside := portablePlan()
	inside[0].JobDef.Cwd = hostAbs("/ws/app")
	if err := rejectUnsupportedRemoteShape(base(), inside); err != nil {
		t.Fatalf("a cwd inside the workspace was refused: %v", err)
	}
}

func TestPortableRequestFreezesInvocationSelectionAndEnvironment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "putnami.lock.json"), []byte(`{"version":3,"cli":{"source":"workspace"},"toolchains":{"zz":{"version":"9.9.9","integrities":{},"source":"x"}},"extensions":{},"templates":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	req := &Request{
		WorkspaceRoot: root, Commands: []string{"lint", "test"},
		CommandParams: map[string]any{"coverage": "true", "fix": false, "retries": 2},
		Global: GlobalFlags{NoCache: true, NoCacheExplicit: true, ContinueOnErr: true, Retry: 1, CacheTrust: "ci", Output: "json", EnvProfile: "dev",
			ResourceBudgets: map[string]int{"db": 1}, MaxParallelMode: "auto", Providers: []string{"publish", "install", "publish"}},
		selection:         &protocoljob.Selection{Mode: protocoljob.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/lib", "/app"}},
		selectionEvidence: selectionEvidence{requestedMode: protocoljob.SelectionModeProjects, diagnostics: []string{"note"}},
		noCacheProjects:   map[string]bool{"/app": true},
		versions:          jobs.RunVersions{"": &jobs.JobContextVersion{Base: "1.0.0", Full: "1.0.0-x", SHA: "abc", Branch: "main"}},
	}
	extensions := []*extension.ExtensionDescription{{Name: "@b/ext", Version: "2"}, {Name: "@a/ext", Version: "1"}}
	request, err := portableRequest(req, nil, &internalextension.DiscoveryResult{Extensions: extensions}, portablePlan(), []string{"app/conf/local.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(request.Source.Bound, ",") != "app/conf/local.txt" {
		t.Fatalf("the admitted bound inputs did not reach the request: %+v", request.Source.Bound)
	}
	if got := request.Invocation.Params["retries"]; got.Type != runner.ParamTypeInt || got.Value != 2 {
		t.Fatalf("typed parameter lost its Go type: %+v", got)
	}
	if got := request.Invocation.Params["fix"]; got.Type != runner.ParamTypeBool || got.Value != false {
		t.Fatalf("bool parameter = %+v", got)
	}
	flags := request.Invocation.Flags
	if !flags.NoCache || !flags.NoCacheExplicit || !flags.ContinueOnError || flags.Retry != 1 || flags.CacheTrust != "ci" || flags.Output != "json" || flags.Profile != "dev" || flags.ResourceBudgets["db"] != 1 || flags.MaxParallelMode != "auto" {
		t.Fatalf("flags lost semantics: %+v", flags)
	}
	if strings.Join(request.Selection.Projects, ",") != "/app,/lib" || request.Selection.Mode != runner.SelectionModeProjects || !request.Selection.Scoped || strings.Join(request.Selection.NoCacheProjects, ",") != "/app" || strings.Join(request.Selection.Diagnostics, ",") != "note" {
		t.Fatalf("selection was not frozen faithfully: %+v", request.Selection)
	}
	if request.Environment.CLI.Source != runner.CLISourceWorkspace || len(request.Environment.Toolchains) != 1 || request.Environment.Toolchains[0].Version != "9.9.9" {
		t.Fatalf("environment pin = %+v", request.Environment)
	}
	if len(request.Environment.Extensions) != 2 || request.Environment.Extensions[0].Name != "@a/ext" {
		t.Fatalf("extensions not sorted: %+v", request.Environment.Extensions)
	}
	if len(request.Source.Versions) != 1 || request.Source.Versions[0].Full != "1.0.0-x" {
		t.Fatalf("versions = %+v", request.Source.Versions)
	}
	if strings.Join(request.Invocation.Providers, ",") != "install,publish" || request.Invocation.Publication != nil {
		t.Fatalf("invocation providers = %v, publication = %+v", request.Invocation.Providers, request.Invocation.Publication)
	}
	if request.Control.Caller != runner.CallerCLI || len(request.Control.IdempotencyKey) != 32 || !strings.HasSuffix(request.Control.Deadline, "Z") {
		t.Fatalf("control = %+v", request.Control)
	}
	// Everything but the source identity is admissible; the provider client
	// binds that from the snapshot it captures.
	request.Source.Digest, request.Source.IndexDigest = runner.BlobDigest(nil), strings.Repeat("0", 64)
	if err := runner.ValidateExecutionRequest(request); err != nil {
		t.Fatalf("projected request is not admissible: %v", err)
	}
	versions := portableRunVersions(request.Source.Versions)
	if versions[""] == nil || versions[""].Full != "1.0.0-x" || versions[""].Branch != "main" {
		t.Fatalf("executing-side versions = %+v", versions[""])
	}
	req.CommandParams["bad"] = map[string]any{}
	if _, err := portableRequest(req, nil, &internalextension.DiscoveryResult{Extensions: extensions}, portablePlan(), nil); err == nil {
		t.Fatal("a non-portable parameter type was projected")
	}
}

// declareTaskEffect makes job execute a manifest task that declares effect,
// with no side effects in its command's traits.
func declareTaskEffect(job *jobs.ScheduledJob, effect string) {
	job.Step = &extension.PipelineStep{Task: "push"}
	job.Extension.Tasks = map[string]extension.TaskDefinition{
		"push": {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{Effects: []string{effect}}},
	}
}

// TestValidatePortablePublicationClassifiesByDeclaredEffects pins the
// executing side's publication check (jobs.ValidatePortablePublication, which
// the portable seam runs after validateExpectedPlan): it classifies a task by its traits and
// manifest effects, not by its command name, and admits it only under a
// publication block whose barrier tasks it waits for.
func TestValidatePortablePublicationClassifiesByDeclaredEffects(t *testing.T) {
	t.Parallel()
	shipping := func(dependsOn ...string) ([]*jobs.ScheduledJob, runner.ExecutionRequest) {
		planned := portablePlan()
		ship := portableJob(planned[0].Project, "ship", dependsOn...)
		ship.JobDef.Traits.SideEffects = extensionproto.SideEffectsCloud
		planned = append(planned, ship)
		modeljobs.AttachIdentities(planned)
		plan, err := expectedPlan(planned)
		if err != nil {
			t.Fatal(err)
		}
		return planned, runner.ExecutionRequest{Invocation: runner.InvocationBlock{Commands: []string{"build", "ship", "test"}}, Plan: plan}
	}

	plain := portablePlan()
	plan, err := expectedPlan(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.ValidatePortablePublication(runner.ExecutionRequest{Invocation: runner.InvocationBlock{Commands: []string{"build", "test"}}, Plan: plan}, plain); err != nil {
		t.Fatalf("a plan without effects needs no publication block: %v", err)
	}

	planned, request := shipping("/app:build", "/app:test~unit")
	if runner.ValidatePublication(request.Invocation, request.Plan, runner.IsPublicationTask) != nil {
		t.Fatal("the fixture must be invisible to the command-name classifier")
	}
	if err := jobs.ValidatePortablePublication(request, planned); err == nil || !strings.Contains(err.Error(), "no invocation.publication") {
		t.Fatalf("a cloud task ran without a publication block: %v", err)
	}
	request.Invocation.Publication = &runner.PublicationBlock{Barrier: []string{"build", "test"}}
	if err := jobs.ValidatePortablePublication(request, planned); err != nil {
		t.Fatalf("a cloud task behind its barrier was refused: %v", err)
	}

	planned, request = shipping("/app:build")
	request.Invocation.Publication = &runner.PublicationBlock{Barrier: []string{"build", "test"}}
	if err := jobs.ValidatePortablePublication(request, planned); err == nil || !strings.Contains(err.Error(), "does not wait for barrier task /app:test~unit") {
		t.Fatalf("a cloud task that skips a barrier task was admitted: %v", err)
	}
}

// TestValidatePortablePublicationEnforcesBothDirections pins the split of the
// publication rule: the protocol, which classifies by command name, refuses
// only a publication task without the block; the executing engine, which
// classifies by declared effects, also refuses the block over a plan with no
// registry or cloud effect, and admits a registry write under another command.
func TestValidatePortablePublicationEnforcesBothDirections(t *testing.T) {
	t.Parallel()
	plain := portablePlan()
	plan, err := expectedPlan(plain)
	if err != nil {
		t.Fatal(err)
	}
	blocked := runner.ExecutionRequest{Invocation: runner.InvocationBlock{Commands: []string{"build", "test"}, Publication: &runner.PublicationBlock{Barrier: []string{"test"}}}, Plan: plan}
	if err := runner.ValidatePublication(blocked.Invocation, blocked.Plan, runner.IsPublicationTask); err != nil {
		t.Fatalf("the protocol refused a block it cannot classify: %v", err)
	}
	if err := jobs.ValidatePortablePublication(blocked, plain); err == nil || !strings.Contains(err.Error(), "no planned task declares registry or cloud effects") {
		t.Fatalf("a publication block over a plan without effects was accepted: %v", err)
	}

	app := plain[0].Project
	push := portableJob(app, "build", "/app:lint")
	declareTaskEffect(push, extension.EffectRegistry)
	pushing := []*jobs.ScheduledJob{push, portableJob(app, "lint")}
	modeljobs.AttachIdentities(pushing)
	pushPlan, err := expectedPlan(pushing)
	if err != nil {
		t.Fatal(err)
	}
	request := runner.ExecutionRequest{Invocation: runner.InvocationBlock{Commands: []string{"build", "lint"}, Publication: &runner.PublicationBlock{Barrier: []string{"lint"}}}, Plan: pushPlan}
	if err := runner.ValidatePublication(request.Invocation, request.Plan, runner.IsPublicationTask); err != nil {
		t.Fatalf("the protocol refused a registry write under build: %v", err)
	}
	if err := jobs.ValidatePortablePublication(request, pushing); err != nil {
		t.Fatalf("the engine refused a registry write under build behind its barrier: %v", err)
	}
	request.Invocation.Publication = nil
	if err := jobs.ValidatePortablePublication(request, pushing); err == nil || !strings.Contains(err.Error(), "no invocation.publication") {
		t.Fatalf("a registry write under build ran without a publication block: %v", err)
	}
}
