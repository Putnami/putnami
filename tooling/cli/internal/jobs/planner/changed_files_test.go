package planner

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const changedFilesRequirement = "a-step-reads-the-changed-files"

// raceExtension mirrors the Go extension's test pipeline: `test` runs every
// package, and `test-race` runs after it only where the change touched a Go
// file the project owns, binding the files it reads.
func raceExtension() *extension.ExtensionDescription {
	declared := &extension.TaskDeclaration{}
	return &extension.ExtensionDescription{
		Name:    "@test/go",
		Version: "1.0.0",
		Jobs: map[string]*extension.JobDefinition{
			"test": {
				ExtensionName:   "@test/go",
				Name:            "test",
				Kind:            "command",
				Command:         "true",
				Cache:           true,
				ActivationFiles: []string{"**/*.go"},
				PipelineSteps: []extension.PipelineStep{
					{ID: "test", Task: "test-exec"},
					{
						ID: "test-race", Task: "test-race-exec", If: "!params.race", DependsOn: []string{"test"},
						Activation: &extension.StepActivation{ChangedFiles: []string{"**/*.go", "**/testdata/**"}},
						With: map[string]extension.InputBinding{
							"changed-files": {From: extensionproto.InputBindingFromContext, Path: extensionproto.ContextPathChangedFiles},
						},
					},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"test-exec": {Kind: "command", Command: "true", Declares: declared},
			"test-race-exec": {Kind: "command", Command: "true", Declares: declared,
				Inputs: map[string]extension.TaskInputPort{
					"changed-files": {From: extensionproto.TaskInputFromParams},
				}},
		},
	}
}

// raceWorkspace is a library with a nested package and an app that depends on
// it, each holding Go files.
func raceWorkspace(t *testing.T) (*workspace.Workspace, *workspace.Project, *workspace.Project) {
	t.Helper()
	root := t.TempDir()
	writeProjectFile(t, root, "lib", "a.go", "package lib\n")
	writeProjectFile(t, root, "lib", "sub/b.go", "package sub\n")
	writeProjectFile(t, root, "app", "main.go", "package main\n")
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@test/go"}}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}, Extensions: []string{"@test/go"}}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app})
	ws.Name = "race-ws"
	return ws, lib, app
}

// A change gate plans its step only for a project that owns a matching
// changed file: an impacted dependent owns none, a non-matching change plans
// nothing, and a run that measured no diff plans the step nowhere. The plain
// test step is planned for every selected project either way.
func TestChangedFilesGate_PlansOnlyWhereAProjectOwnsAMatchingFile(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", changedFilesRequirement,
		"a-change-gated-step-is-planned-only-for-a-project-that-owns-a-matching-changed-file")
	spectest.Proves(t, "cli/job-planning-execution", changedFilesRequirement,
		"a-run-that-measured-no-diff-plans-no-change-gated-step")
	ws, lib, app := raceWorkspace(t)
	exts := []*extension.ExtensionDescription{raceExtension()}
	plan := func(changed []string) map[string]bool {
		t.Helper()
		planned, err := Plan(ws, []string{"test"}, []*workspace.Project{lib, app}, exts, nil, nil, nil, changed)
		if err != nil {
			t.Fatalf("plan %v: %v", changed, err)
		}
		return planKeySet(planned)
	}

	for name, tc := range map[string]struct {
		changed []string
		race    map[string]bool
	}{
		"a nested package of the library": {changed: []string{"lib/sub/b.go"}, race: map[string]bool{"/lib": true}},
		"a testdata file":                 {changed: []string{"lib/sub/testdata/case.txt"}, race: map[string]bool{"/lib": true}},
		"both projects":                   {changed: []string{"app/main.go", "lib/a.go"}, race: map[string]bool{"/lib": true, "/app": true}},
		"a file no glob matches":          {changed: []string{"lib/README.md"}, race: map[string]bool{}},
		"a file no project owns":          {changed: []string{"docs/x.go"}, race: map[string]bool{}},
		"no diff":                         {changed: nil, race: map[string]bool{}},
	} {
		keys := plan(tc.changed)
		for _, project := range []string{"/lib", "/app"} {
			if !keys[project+":test~test"] {
				t.Errorf("%s: %s:test~test is not planned", name, project)
			}
			if got := keys[project+":test~test-race"]; got != tc.race[project] {
				t.Errorf("%s: %s:test~test-race planned = %v, want %v", name, project, got, tc.race[project])
			}
		}
	}
}

// The step binds the files its project owns, project-relative and sorted,
// and the plain test step binds none.
func TestChangedFilesBinding_BindsTheOwnedFiles(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", changedFilesRequirement,
		"the-owned-changed-files-are-bound-into-the-step")
	ws, lib, app := raceWorkspace(t)
	exts := []*extension.ExtensionDescription{raceExtension()}
	planned, err := Plan(ws, []string{"test"}, []*workspace.Project{lib, app}, exts, nil, nil, nil,
		[]string{"lib/sub/b.go", "app/main.go", "lib/a.go", "lib/sub/b.go"})
	if err != nil {
		t.Fatal(err)
	}
	for project, want := range map[string][]string{"/lib": {"a.go", "sub/b.go"}, "/app": {"main.go"}} {
		race := findScheduled(planned, project+":test~test-race")
		if race == nil {
			t.Fatalf("%s:test~test-race is not planned", project)
		}
		if got := race.JobDef.BoundParams["changed-files"]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s bound changed-files = %#v, want %v", project, got, want)
		}
		if test := findScheduled(planned, project+":test~test"); test.JobDef.BoundParams != nil {
			t.Errorf("%s:test~test bound %v; only the race step reads the files", project, test.JobDef.BoundParams)
		}
	}
}

// The bound files key the step: another change gives another key, the same
// change in any order and spelling gives the same key, an edit the gate does
// not select gives the same key, and the plain test step's key does not move
// with the change at all.
func TestChangedFilesBinding_KeysTheStepAndOnlyTheStep(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", changedFilesRequirement,
		"the-owned-changed-files-key-the-step")
	ws, lib, app := raceWorkspace(t)
	exts := []*extension.ExtensionDescription{raceExtension()}
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))
	keysFor := func(changed ...string) map[string]string {
		t.Helper()
		planned, err := Plan(ws, []string{"test"}, []*workspace.Project{lib, app}, exts, nil, nil, nil, changed)
		if err != nil {
			t.Fatalf("plan %v: %v", changed, err)
		}
		keys, err := cachekey.PrecomputeKeys(ws, planned, nil, nil, cache, cachekey.CacheBypass{})
		if err != nil {
			t.Fatalf("keys %v: %v", changed, err)
		}
		return keys
	}

	first := keysFor("lib/a.go", "lib/sub/b.go")
	again := keysFor("./lib/sub/b.go", "lib/a.go", "lib/a.go")
	other := keysFor("lib/a.go")
	readme := keysFor("lib/a.go", "lib/README.md", "lib/sub/b.go")
	none := keysFor()

	const race, test = "/lib:test~test-race", "/lib:test~test"
	if first[race] == "" || other[race] == "" {
		t.Fatalf("the race step was not keyed: %v / %v", first, other)
	}
	if first[race] != again[race] {
		t.Errorf("one change spelled twice keyed the race step %q and %q", first[race], again[race])
	}
	if first[race] == other[race] {
		t.Errorf("two changes keyed the race step alike (%q): the bound files are not a key input", first[race])
	}
	if readme[race] != first[race] {
		t.Errorf("a README edit beside the Go edit moved the race step's key: %q, want %q", readme[race], first[race])
	}
	for name, keys := range map[string]map[string]string{"same": again, "other": other, "no diff": none} {
		if keys[test] != first[test] {
			t.Errorf("%s change moved the test step's key: %q, want %q", name, keys[test], first[test])
		}
	}
}

// A finalizer waits for the consumers the plan holds. A consumer an
// activation gate removed never runs and never uses the resource, so the
// relation keeps the others rather than leaving the resource without cleanup;
// a plan that holds none of them leaves the finalizer inert.
func TestResolveInvocationRelations_TrimsTheFrontierToPlannedConsumers(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", changedFilesRequirement,
		"cleanup-waits-for-the-consumers-the-plan-holds")
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "."}
	ext := &extension.ExtensionDescription{Name: "@acme/provider"}
	node := func(step extension.PipelineStep) *jobmodel.ScheduledJob {
		return &jobmodel.ScheduledJob{
			Project: project, Extension: ext, Step: &step,
			JobDef: &extension.JobDefinition{
				Name: extension.StepJobName("test", step.ID), ExtensionName: ext.Name, CommandName: "test", StepID: step.ID,
			},
		}
	}
	plan := func() (producer, test, race, finalizer *jobmodel.ScheduledJob) {
		return node(extension.PipelineStep{ID: "test-env", Task: "env"}),
			node(extension.PipelineStep{ID: "test", Task: "test-exec", DependsOn: []string{"test-env"}}),
			node(extension.PipelineStep{ID: "test-race", Task: "test-race-exec", DependsOn: []string{"test-env"}}),
			node(extension.PipelineStep{ID: "teardown", Task: "env-down", RunOn: extensionproto.StepRunOnFinally,
				Finalizes: &extensionproto.FinalizesRelation{Producer: "test-env", Consumers: []string{"test", "test-race"}}})
	}

	producer, test, race, finalizer := plan()
	relations := ResolveInvocationRelations([]*jobmodel.ScheduledJob{producer, test, race, finalizer})
	if len(relations) != 1 || len(relations[0].Consumers) != 2 {
		t.Fatalf("whole frontier resolved %+v, want one relation with two consumers", relations)
	}

	producer, test, _, finalizer = plan()
	relations = ResolveInvocationRelations([]*jobmodel.ScheduledJob{producer, test, finalizer})
	if len(relations) != 1 || len(relations[0].Consumers) != 1 || relations[0].Consumers[0] != test {
		t.Fatalf("trimmed frontier resolved %+v, want one relation whose consumer is test", relations)
	}
	if test.InvocationProducer != producer {
		t.Error("the planned consumer's producing action was not stamped")
	}

	producer, _, _, finalizer = plan()
	if got := ResolveInvocationRelations([]*jobmodel.ScheduledJob{producer, finalizer}); len(got) != 0 {
		t.Errorf("a plan without a consumer resolved %d relations, want none", len(got))
	}
}

// The shipped Go manifest plans its race step on the project that owns the
// changed Go file, after the test step, and binds that file. A dependent the
// change reaches through an edge and a run without a diff plan test alone.
// --race and --no-race-changed keep both steps planned: the step skips itself
// at run time, and test, the prerequisite only the race step depends on,
// stays planned under every flag.
func TestGoManifestPlansTheRaceStepOnTheTouchedProject(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", changedFilesRequirement,
		"a-change-gated-step-is-planned-only-for-a-project-that-owns-a-matching-changed-file")
	ext := loadGoPlannerExtension(t)
	root := t.TempDir()
	projects := make([]*workspace.Project, 0, 2)
	for _, name := range []string{"lib", "app"} {
		writeProjectFile(t, root, name, "go.mod", "module example.com/"+name+"\n\ngo 1.25\n")
		writeProjectFile(t, root, name, name+".go", "package "+name+"\n")
		writeProjectFile(t, root, name, name+"_test.go", "package "+name+"\n")
		project := &workspace.Project{
			ID: "/" + name, Name: name, Path: name, Type: "library", Extensions: []string{"@putnami/go"},
			Config: wsproto.LoadProjectConfig(filepath.Join(root, name)),
		}
		if name == "app" {
			project.Dependencies = []string{"lib"}
		}
		projects = append(projects, project)
	}
	ws := testWorkspace(root, projects...)
	exts := []*extension.ExtensionDescription{ext}

	for name, params := range map[string]map[string]any{
		"the defaults":      nil,
		"--race":            {"race": true},
		"--no-race-changed": {"race-changed": false},
	} {
		planned, err := Plan(ws, []string{"test"}, projects, exts, params, nil, nil,
			[]string{"lib/README.md", "lib/lib.go", "lib/.gen/client/x.go"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		keys := planKeySet(planned)
		if !keys["/lib:test~test"] || !keys["/app:test~test"] || keys["/app:test~test-race"] {
			t.Errorf("%s: plan = %v; want test for both projects and no race step for /app", name, keys)
		}
		race := findScheduled(planned, "/lib:test~test-race")
		if race == nil {
			t.Fatalf("%s: /lib:test~test-race is not planned: %v", name, keys)
		}
		if !slices.Contains(race.DependsOn, "/lib:test~test") {
			t.Errorf("%s: /lib race step depends on %v, want it after /lib:test~test", name, race.DependsOn)
		}
		if got := race.JobDef.BoundParams["changed-files"]; !reflect.DeepEqual(got, []string{"lib.go"}) {
			t.Errorf("%s: /lib race step bound %#v, want [lib.go] alone", name, got)
		}
	}

	planned, err := Plan(ws, []string{"test"}, projects, exts, nil, nil, nil, []string{"lib/.gen/client/x.go"})
	if err != nil {
		t.Fatal(err)
	}
	if keys := planKeySet(planned); keys["/lib:test~test-race"] || !keys["/lib:test~test"] {
		t.Errorf("a generated-code change: plan = %v; want test without a race step", keys)
	}

	planned, err = Plan(ws, []string{"test"}, projects, exts, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if keys := planKeySet(planned); keys["/lib:test~test-race"] || keys["/app:test~test-race"] || !keys["/lib:test~test"] {
		t.Errorf("no diff: plan = %v; want test without a race step", keys)
	}
}

// The CLI project tunes its race step by the step name the scheduler resolves,
// test~test-race, so the step runs under the CLI test step's deadline rather
// than the Go manifest's default.
func TestTheCLIProjectTunesItsRaceStepDeadline(t *testing.T) {
	t.Parallel()
	config, diags := wsproto.LoadProjectConfigWithDiagnostics(filepath.Join(findJobsRepoRoot(t), "tooling", "cli"))
	if len(diags) > 0 {
		t.Fatalf("tooling/cli/putnami.json: %v", diags)
	}
	root := t.TempDir()
	writeProjectFile(t, root, "cli", "go.mod", "module example.com/cli\n\ngo 1.25\n")
	writeProjectFile(t, root, "cli", "cli.go", "package cli\n")
	project := &workspace.Project{
		ID: "/tooling/cli", Name: "cli", Path: "cli", Type: "library", Extensions: []string{"@putnami/go"}, Config: config,
	}
	planned, err := Plan(testWorkspace(root, project), []string{"test"}, []*workspace.Project{project},
		[]*extension.ExtensionDescription{loadGoPlannerExtension(t)}, nil, nil, nil, []string{"cli/cli.go"})
	if err != nil {
		t.Fatal(err)
	}
	race := findScheduled(planned, "/tooling/cli:test~test-race")
	test := findScheduled(planned, "/tooling/cli:test~test")
	if race == nil || test == nil {
		t.Fatalf("plan = %v; want test and test-race on /tooling/cli", planKeySet(planned))
	}
	if got, want := race.EffectiveTimeoutMs(), test.EffectiveTimeoutMs(); got != want || got <= race.JobDef.TimeoutMs {
		t.Errorf("race step deadline = %d ms; want the test step's %d ms, above the manifest's %d ms",
			got, want, race.JobDef.TimeoutMs)
	}
}
