package jobs

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func makeTestExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@test/ts",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: "@test/ts",
				Name:          "build",
				Kind:          "command",
				Command:       "bun",
				Cache:         true,
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
					{ID: "transpile", Task: "build-transpile", DependsOn: []string{"generate"}},
				},
			},
			"test": {
				ExtensionName: "@test/ts",
				Name:          "test",
				Kind:          "command",
				Command:       "bun",
				Cache:         true,
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-generate":  {Kind: "command", Command: "bun"},
			"build-transpile": {Kind: "command", Command: "bun"},
		},
	}
}

func makeTestWorkspace() *workspace.Workspace {
	projects := []*workspace.Project{
		{ID: "/packages/utils", Name: "utils", Path: "packages/utils", Extensions: []string{"@test/ts"}},
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"utils"}, Extensions: []string{"@test/ts"}},
	}
	ws := workspace.NewWorkspace("/workspace", nil, projects)
	ws.Name = "test-ws"
	return ws
}

func firstMatch(cmdName string, proj *workspace.Project, jobMap map[string][]*extension.JobDefinition) *extension.JobDefinition {
	matches := matchJobs(cmdName, proj, jobMap, nil, nil, "", nil)
	if len(matches) == 0 {
		return nil
	}
	return matches[0]
}

func TestPlan_BasicPipeline(t *testing.T) {
	t.Parallel()
	ws := makeTestWorkspace()
	ext := makeTestExtension()

	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// 2 projects × 2 pipeline steps = 4 jobs
	if len(planned) != 4 {
		names := make([]string, len(planned))
		for i, j := range planned {
			names[i] = j.Key()
		}
		t.Fatalf("expected 4 jobs, got %d: %v", len(planned), names)
	}
}

func TestPlan_CrossProjectDeps(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan", "the-dag-is-deterministic")
	ws := makeTestWorkspace()
	ext := makeTestExtension()

	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Find app:build~generate — it should depend on utils:build~generate
	var appGenerate *ScheduledJob
	for _, j := range planned {
		if j.Project.Name == "app" && j.JobDef.Name == "build~generate" {
			appGenerate = j
			break
		}
	}

	if appGenerate == nil {
		t.Fatal("app:build~generate not found in plan")
	}

	// Should have cross-project dep on /packages/utils:build~generate
	found := false
	for _, dep := range appGenerate.DependsOn {
		if dep == "/packages/utils:build~generate" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("app:build~generate deps = %v, want to contain /packages/utils:build~generate", appGenerate.DependsOn)
	}
}

func TestPlan_IntraPipelineDeps(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan", "the-dag-is-deterministic")
	ws := makeTestWorkspace()
	ext := makeTestExtension()

	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Find utils:build~transpile — it should depend on utils:build~generate
	var utilsTranspile *ScheduledJob
	for _, j := range planned {
		if j.Project.Name == "utils" && j.JobDef.Name == "build~transpile" {
			utilsTranspile = j
			break
		}
	}

	if utilsTranspile == nil {
		t.Fatal("utils:build~transpile not found in plan")
	}

	found := false
	for _, dep := range utilsTranspile.DependsOn {
		if dep == "/packages/utils:build~generate" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("utils:build~transpile deps = %v, want to contain /packages/utils:build~generate", utilsTranspile.DependsOn)
	}
}

func TestPlan_DisabledJobs(t *testing.T) {
	t.Parallel()
	ws := makeTestWorkspace()
	ext := makeTestExtension()

	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, []string{"build"}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(planned) != 0 {
		t.Errorf("expected 0 jobs with build disabled, got %d", len(planned))
	}
}

func TestPlan_DisabledExtension(t *testing.T) {
	t.Parallel()
	ws := makeTestWorkspace()
	ext := makeTestExtension()

	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, []string{"@test/ts"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(planned) != 0 {
		t.Errorf("expected 0 jobs with extension disabled, got %d", len(planned))
	}
}

func TestPlan_MultiCommand(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "selection-before-plan", "one-selection-applies-to-every-command")
	ws := makeTestWorkspace()
	ext := makeTestExtension()

	// Only utils has test activation files (we don't check files in test, so both will match)
	planned, err := Plan(ws, []string{"build", "test"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// build: 4 jobs + test: 2 jobs (no activation file check without workspace root) = 6
	if len(planned) != 6 {
		names := make([]string, len(planned))
		for i, j := range planned {
			names[i] = j.Key()
		}
		t.Fatalf("expected 6 jobs, got %d: %v", len(planned), names)
	}
}

func TestPlan_AlsoRunsPlansTheCompanionCommand(t *testing.T) {
	t.Parallel()
	ws := makeTestWorkspace()
	ext := &extension.ExtensionDescription{
		Name: "@test/sdd",
		Jobs: map[string]*extension.JobDefinition{
			"validate": {
				ExtensionName: "@test/sdd",
				Name:          "validate",
				Kind:          "command",
				Command:       "run",
				// Matches nothing in the test workspace: the companion must
				// still plan, because the expansion is request-level and the
				// companion keeps its own workspace-once activation. This is
				// the case dependsOn and sessionPrerequisites cannot cover —
				// a change touching only workspace-level declarations.
				ActivationFiles: []string{"specs/*.json"},
				AlsoRuns:        []string{"validate-workspace"},
			},
			"validate-workspace": {
				ExtensionName: "@test/sdd",
				Name:          "validate-workspace",
				Kind:          "command",
				Command:       "run",
				Activation:    "workspace-once",
			},
		},
	}
	for _, proj := range ws.Projects {
		proj.Extensions = append(proj.Extensions, "@test/sdd")
	}

	planned, err := Plan(ws, []string{"validate"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var companions []string
	for _, job := range planned {
		if job.JobDef.Name == "validate-workspace" {
			companions = append(companions, job.Key())
		}
	}
	if len(companions) != 1 {
		names := make([]string, len(planned))
		for i, j := range planned {
			names[i] = j.Key()
		}
		t.Fatalf("expected exactly one planned validate-workspace companion, got %v (all: %v)", companions, names)
	}

	// Requesting both explicitly stays a single plan of each: expansion
	// dedupes against the visited set instead of double-planning.
	both, err := Plan(ws, []string{"validate", "validate-workspace"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan(both): %v", err)
	}
	count := 0
	for _, job := range both {
		if job.JobDef.Name == "validate-workspace" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("explicit request + expansion double-planned the companion: %d", count)
	}
}

func TestScheduledJob_Key(t *testing.T) {
	t.Parallel()
	j := &ScheduledJob{
		Project: &workspace.Project{ID: "@putnami/utils", Name: "@putnami/utils"},
		JobDef:  &extension.JobDefinition{Name: "build~transpile"},
	}
	if got := j.Key(); got != "@putnami/utils:build~transpile" {
		t.Errorf("Key() = %q, want %q", got, "@putnami/utils:build~transpile")
	}
}

func TestMatchJob_Priority(t *testing.T) {
	t.Parallel()
	high := &extension.JobDefinition{ExtensionName: "ext-a", Name: "build", Priority: 10}
	low := &extension.JobDefinition{ExtensionName: "ext-b", Name: "build", Priority: 1}

	jobMap := map[string][]*extension.JobDefinition{
		"build": {low, high},
	}

	proj := &workspace.Project{Name: "test", Extensions: []string{"ext-a", "ext-b"}}
	result := firstMatch("build", proj, jobMap)
	if result != high {
		t.Errorf("expected high priority extension, got %s", result.ExtensionName)
	}
}

func TestMatchJob_PrefersProjectExtension(t *testing.T) {
	t.Parallel()
	// Two extensions provide "serve" with the same priority and no activation files
	tsServe := &extension.JobDefinition{ExtensionName: "@putnami/typescript", Name: "serve"}
	pyServe := &extension.JobDefinition{ExtensionName: "@putnami/python", Name: "serve"}

	jobMap := map[string][]*extension.JobDefinition{
		"serve": {pyServe, tsServe},
	}

	// Project lists @putnami/typescript as an extension (from devDependencies)
	proj := &workspace.Project{
		Name:       "my-app",
		Extensions: []string{"@putnami/typescript"},
	}

	result := firstMatch("serve", proj, jobMap)
	if result.ExtensionName != "@putnami/typescript" {
		t.Errorf("expected @putnami/typescript, got %s", result.ExtensionName)
	}
}

func TestMatchJob_NoExtensionsReturnsNil(t *testing.T) {
	t.Parallel()
	// Without project extensions, no match should be found (matches TS SDK behavior)
	tsServe := &extension.JobDefinition{ExtensionName: "@putnami/typescript", Name: "serve"}
	pyServe := &extension.JobDefinition{ExtensionName: "@putnami/python", Name: "serve"}

	jobMap := map[string][]*extension.JobDefinition{
		"serve": {pyServe, tsServe},
	}

	proj := &workspace.Project{Name: "my-app"}
	result := firstMatch("serve", proj, jobMap)
	if result != nil {
		t.Errorf("expected nil for project with no extensions, got %s", result.ExtensionName)
	}
}

func TestMatchJob_GroupedExtensionPathUsesLogicalID(t *testing.T) {
	t.Parallel()
	groupedExtension := &extension.JobDefinition{
		ExtensionName: "@putnami/typescript",
		ExtensionPath: "typescript/(extensions)/typescript",
		Name:          "build",
	}
	jobMap := map[string][]*extension.JobDefinition{
		"build": {groupedExtension},
	}
	proj := &workspace.Project{
		Name:       "my-app",
		Extensions: []string{"/typescript/typescript"},
	}

	if got := firstMatch("build", proj, jobMap); got != groupedExtension {
		t.Fatalf("logical grouped extension ID did not match: got %#v", got)
	}
}

// A published build pinned over a workspace project carries that project's
// path: a consumer that names the project by path runs the pinned build.
func TestMatchJob_PathEntryMatchesThePinnedBuildOfThatProject(t *testing.T) {
	t.Parallel()
	pinned := &extension.JobDefinition{
		ExtensionName: "@acme/tool",
		ExtensionPath: "tools/acme",
		Name:          "build",
	}
	jobMap := map[string][]*extension.JobDefinition{"build": {pinned}}
	proj := &workspace.Project{Name: "my-app", Extensions: []string{"/tools/acme"}}

	if got := firstMatch("build", proj, jobMap); got != pinned {
		t.Fatalf("a path entry did not match the pinned build: got %#v", got)
	}
}

func TestMatchJob_SelfPublish(t *testing.T) {
	t.Parallel()
	// Extension projects can publish themselves without declaring the extension as a dependency
	extPublish := &extension.JobDefinition{ExtensionName: "@putnami/python", Name: "publish"}
	tsPublish := &extension.JobDefinition{ExtensionName: "@putnami/typescript", Name: "publish"}

	jobMap := map[string][]*extension.JobDefinition{
		"publish": {tsPublish, extPublish},
	}

	proj := &workspace.Project{Name: "@putnami/python"}
	result := firstMatch("publish", proj, jobMap)
	if result == nil {
		t.Fatal("expected a match for self-publish, got nil")
	}
	if result.ExtensionName != "@putnami/python" {
		t.Errorf("expected @putnami/python (self-publish), got %s", result.ExtensionName)
	}
}

func TestResolveExternalDeps_UpstreamCaret(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/utils", Name: "utils", Path: "utils"},
		{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"utils"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test"

	jobs := []*ScheduledJob{
		{
			Project: projects[0], // utils
			JobDef:  &extension.JobDefinition{Name: "build~generate"},
		},
		{
			Project: projects[0], // utils
			JobDef:  &extension.JobDefinition{Name: "build~transpile", DependsOn: []string{"build~generate"}},
		},
		{
			Project: projects[1], // app
			JobDef:  &extension.JobDefinition{Name: "build~generate", DependsOn: []string{"^generate"}},
		},
		{
			Project: projects[1], // app
			JobDef:  &extension.JobDefinition{Name: "build~transpile", DependsOn: []string{"build~generate"}},
		},
	}

	if err := resolveExternalDeps(jobs, ws); err != nil {
		t.Fatalf("resolveExternalDeps: %v", err)
	}

	// /app:build~generate should depend on /utils:build~generate
	appGen := jobs[2]
	sort.Strings(appGen.DependsOn)
	if len(appGen.DependsOn) != 1 || appGen.DependsOn[0] != "/utils:build~generate" {
		t.Errorf("app:build~generate deps = %v, want [/utils:build~generate]", appGen.DependsOn)
	}

	// /app:build~transpile should depend on /app:build~generate (intra-pipeline)
	appTrans := jobs[3]
	if len(appTrans.DependsOn) != 1 || appTrans.DependsOn[0] != "/app:build~generate" {
		t.Errorf("app:build~transpile deps = %v, want [/app:build~generate]", appTrans.DependsOn)
	}
}

func TestPlan_SingleProjectEmitsUpstreamDeps(t *testing.T) {
	t.Parallel()
	ws := makeTestWorkspace()
	ext := makeTestExtension()

	// Plan only "app" (which depends on "utils").
	// The planner should emit utils:build~generate as an upstream dep.
	appOnly := []*workspace.Project{ws.Projects[1]} // app
	planned, err := Plan(ws, []string{"build"}, appOnly, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Expect: /packages/utils:build~generate (emitted upstream) + /packages/app:build~generate + /packages/app:build~transpile = 3
	keys := make([]string, len(planned))
	for i, j := range planned {
		keys[i] = j.Key()
	}
	sort.Strings(keys)

	expected := []string{
		"/packages/app:build~generate",
		"/packages/app:build~transpile",
		"/packages/utils:build~generate",
	}
	if len(keys) != len(expected) {
		t.Fatalf("expected %d jobs %v, got %d: %v", len(expected), expected, len(keys), keys)
	}
	for i, k := range expected {
		if keys[i] != k {
			t.Errorf("job[%d] = %q, want %q", i, keys[i], k)
		}
	}

	// /packages/app:build~generate should depend on /packages/utils:build~generate
	for _, j := range planned {
		if j.Key() == "/packages/app:build~generate" {
			found := false
			for _, dep := range j.DependsOn {
				if dep == "/packages/utils:build~generate" {
					found = true
				}
			}
			if !found {
				t.Errorf("/packages/app:build~generate deps = %v, want to contain /packages/utils:build~generate", j.DependsOn)
			}
		}
	}
}

func TestPlan_TransitiveUpstreamDeps(t *testing.T) {
	t.Parallel()
	// A → B → C (C has no deps, B depends on C, A depends on B)
	projects := []*workspace.Project{
		{ID: "/packages/c", Name: "C", Path: "packages/c", Extensions: []string{"@test/ts"}},
		{ID: "/packages/b", Name: "B", Path: "packages/b", Dependencies: []string{"C"}, Extensions: []string{"@test/ts"}},
		{ID: "/packages/a", Name: "A", Path: "packages/a", Dependencies: []string{"B"}, Extensions: []string{"@test/ts"}},
	}
	ws := workspace.NewWorkspace("/workspace", nil, projects)
	ws.Name = "test-ws"
	ext := makeTestExtension()

	// Plan only "A" — should emit B:build~generate and C:build~generate
	planned, err := Plan(ws, []string{"build"}, []*workspace.Project{projects[2]}, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	keys := make([]string, len(planned))
	for i, j := range planned {
		keys[i] = j.Key()
	}
	sort.Strings(keys)

	expected := []string{
		"/packages/a:build~generate",
		"/packages/a:build~transpile",
		"/packages/b:build~generate",
		"/packages/c:build~generate",
	}
	if len(keys) != len(expected) {
		t.Fatalf("expected %d jobs %v, got %d: %v", len(expected), expected, len(keys), keys)
	}
	for i, k := range expected {
		if keys[i] != k {
			t.Errorf("job[%d] = %q, want %q", i, keys[i], k)
		}
	}

	// Verify dependency chain: /packages/a:build~generate → /packages/b:build~generate → /packages/c:build~generate
	depMap := make(map[string][]string)
	for _, j := range planned {
		depMap[j.Key()] = j.DependsOn
	}
	if !containsStr(depMap["/packages/a:build~generate"], "/packages/b:build~generate") {
		t.Errorf("/packages/a:build~generate deps = %v, want /packages/b:build~generate", depMap["/packages/a:build~generate"])
	}
	if !containsStr(depMap["/packages/b:build~generate"], "/packages/c:build~generate") {
		t.Errorf("/packages/b:build~generate deps = %v, want /packages/c:build~generate", depMap["/packages/b:build~generate"])
	}
	if len(depMap["/packages/c:build~generate"]) != 0 {
		t.Errorf("/packages/c:build~generate deps = %v, want empty", depMap["/packages/c:build~generate"])
	}
}

func containsStr(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func TestHasActivationFiles_DoubleStarGlob(t *testing.T) {
	t.Parallel()
	// Create a temp directory with nested test files
	dir := t.TempDir()
	// Create nested structure: test/api/handler.test.ts
	nestedDir := filepath.Join(dir, "test", "api")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "handler.test.ts"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	// ** pattern should match deeply nested files
	if !hasActivationFiles(dir, []string{"**/*.test.ts"}, nil) {
		t.Error("expected **/*.test.ts to match test/api/handler.test.ts")
	}

	// Non-matching pattern should return false
	if hasActivationFiles(dir, []string{"**/*.go"}, nil) {
		t.Error("expected **/*.go to not match any file")
	}

	// Simple glob (no **) at root should not match nested files
	if hasActivationFiles(dir, []string{"*.test.ts"}, nil) {
		t.Error("expected *.test.ts to not match nested file")
	}
}

func TestMatchJob_LibrarySkipsServe(t *testing.T) {
	t.Parallel()
	serveJob := &extension.JobDefinition{ExtensionName: "@putnami/typescript", Name: "serve", Traits: extension.CommandTraits{RequiresRunnable: true}}
	buildJob := &extension.JobDefinition{ExtensionName: "@putnami/typescript", Name: "build"}

	jobMap := map[string][]*extension.JobDefinition{
		"serve": {serveJob},
		"build": {buildJob},
	}

	lib := &workspace.Project{
		Name:       "my-lib",
		Type:       "library",
		Extensions: []string{"@putnami/typescript"},
	}

	// Library projects should not match serve
	result := firstMatch("serve", lib, jobMap)
	if result != nil {
		t.Errorf("expected nil for library project serve, got %s", result.ExtensionName)
	}

	// Library projects should still match build
	result = firstMatch("build", lib, jobMap)
	if result == nil {
		t.Fatal("expected build job for library project, got nil")
	}

	// Application projects should still match serve
	app := &workspace.Project{
		Name:       "my-app",
		Type:       "application",
		Extensions: []string{"@putnami/typescript"},
	}
	result = firstMatch("serve", app, jobMap)
	if result == nil {
		t.Fatal("expected serve job for application project, got nil")
	}

	// Projects with no type (default) should still match serve
	noType := &workspace.Project{
		Name:       "my-project",
		Extensions: []string{"@putnami/typescript"},
	}
	result = firstMatch("serve", noType, jobMap)
	if result == nil {
		t.Fatal("expected serve job for project with no type, got nil")
	}
}

func TestMatchJob_LibrarySkipsRun(t *testing.T) {
	t.Parallel()
	runJob := &extension.JobDefinition{ExtensionName: "@putnami/go", Name: "run", Traits: extension.CommandTraits{RequiresRunnable: true}}
	jobMap := map[string][]*extension.JobDefinition{"run": {runJob}}

	lib := &workspace.Project{
		Name:       "my-lib",
		Type:       "library",
		Extensions: []string{"@putnami/go"},
	}
	if result := firstMatch("run", lib, jobMap); result != nil {
		t.Errorf("expected nil for library project run, got %s", result.ExtensionName)
	}

	app := &workspace.Project{
		Name:       "my-app",
		Type:       "application",
		Extensions: []string{"@putnami/go"},
	}
	if result := firstMatch("run", app, jobMap); result == nil {
		t.Fatal("expected run job for application project, got nil")
	}
}

func TestResolveExternalDeps_Star(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A", Extensions: []string{"@test/ts"}},
		{ID: "/B", Name: "B", Path: "B", Extensions: []string{"@test/ts"}},
		{ID: "/C", Name: "C", Path: "C", Extensions: []string{"@test/ts"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test"

	jobs := []*ScheduledJob{
		{Project: projects[0], JobDef: &extension.JobDefinition{Name: "build~compile"}},
		{Project: projects[1], JobDef: &extension.JobDefinition{Name: "build~compile"}},
		{Project: projects[2], JobDef: &extension.JobDefinition{Name: "build~compile"}},
		{Project: projects[0], JobDef: &extension.JobDefinition{Name: "deploy", DependsOn: []string{"*compile"}}},
	}

	if err := resolveExternalDeps(jobs, ws); err != nil {
		t.Fatalf("resolveExternalDeps: %v", err)
	}

	// /A:deploy should depend on all build~compile jobs including same project
	deploy := jobs[3]
	sort.Strings(deploy.DependsOn)
	if len(deploy.DependsOn) != 3 {
		t.Fatalf("deploy deps = %v, want 3 deps", deploy.DependsOn)
	}
	expected := []string{"/A:build~compile", "/B:build~compile", "/C:build~compile"}
	for i, exp := range expected {
		if deploy.DependsOn[i] != exp {
			t.Errorf("deploy deps[%d] = %q, want %q", i, deploy.DependsOn[i], exp)
		}
	}
}

func TestResolveExternalDeps_StarPrefixMatch(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A"},
		{ID: "/B", Name: "B", Path: "B"},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test"

	jobs := []*ScheduledJob{
		{Project: projects[0], JobDef: &extension.JobDefinition{Name: "package~template"}},
		{Project: projects[1], JobDef: &extension.JobDefinition{Name: "package~template"}},
		{Project: projects[0], JobDef: &extension.JobDefinition{Name: "publish~npm", DependsOn: []string{"*package"}}},
	}

	if err := resolveExternalDeps(jobs, ws); err != nil {
		t.Fatalf("resolveExternalDeps: %v", err)
	}

	// publish~npm in A should depend on package~template in both A and B (prefix match)
	pub := jobs[2]
	sort.Strings(pub.DependsOn)
	if len(pub.DependsOn) != 2 {
		t.Fatalf("publish deps = %v, want 2 deps", pub.DependsOn)
	}
	if pub.DependsOn[0] != "/A:package~template" || pub.DependsOn[1] != "/B:package~template" {
		t.Errorf("publish deps = %v, want [/A:package~template, /B:package~template]", pub.DependsOn)
	}
}

func TestEmitCommandDeps_AutoPlansPrerequisites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create activation file for project B only
	projBDir := filepath.Join(dir, "B")
	os.MkdirAll(projBDir, 0o755)
	os.WriteFile(filepath.Join(projBDir, "putnami.template.json"), []byte("{}"), 0o644)

	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A"},
		{ID: "/B", Name: "B", Path: "B", Extensions: []string{"@putnami/ci"}},
	}
	ws := workspace.NewWorkspace(dir, nil, projects)
	ws.Name = "test"

	ciExt := &extension.ExtensionDescription{
		Name: "@putnami/ci",
		Jobs: map[string]*extension.JobDefinition{
			"publish": {
				ExtensionName:    "@putnami/ci",
				Name:             "publish",
				Activation:       "workspace",
				CommandDependsOn: []string{"package"},
				PipelineSteps: []extension.PipelineStep{
					{ID: "npm", Task: "publish-npm"},
				},
			},
			"package": {
				ExtensionName:   "@putnami/ci",
				Name:            "package",
				ActivationFiles: []string{"putnami.template.json"},
				PipelineSteps: []extension.PipelineStep{
					{ID: "template", Task: "package-template"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"publish-npm":      {Kind: "command", Command: "ci-run"},
			"package-template": {Kind: "command", Command: "ci-run"},
		},
	}

	// Plan only "publish" — emitCommandDeps should auto-plan "package" for B
	planned, err := Plan(ws, []string{"publish"}, ws.Projects, []*extension.ExtensionDescription{ciExt}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	keys := make([]string, len(planned))
	for i, j := range planned {
		keys[i] = j.Key()
	}
	sort.Strings(keys)

	// Expect: publish~npm for A and B, plus package~template for B (auto-emitted)
	expected := []string{
		"/A:publish~npm",
		"/B:package~template",
		"/B:publish~npm",
	}
	if len(keys) != len(expected) {
		t.Fatalf("expected %d jobs %v, got %d: %v", len(expected), expected, len(keys), keys)
	}
	for i, k := range expected {
		if keys[i] != k {
			t.Errorf("job[%d] = %q, want %q", i, keys[i], k)
		}
	}

	// B:publish~npm (root step, no intra-pipeline deps) should depend on B:package~template
	// A:publish~npm should have no deps (no package for A)
	for _, j := range planned {
		if j.Key() == "/B:publish~npm" {
			if len(j.DependsOn) != 1 || j.DependsOn[0] != "/B:package~template" {
				t.Errorf("/B:publish~npm deps = %v, want [/B:package~template]", j.DependsOn)
			}
		}
		if j.Key() == "/A:publish~npm" {
			if len(j.DependsOn) != 0 {
				t.Errorf("/A:publish~npm deps = %v, want empty", j.DependsOn)
			}
		}
	}
}

func TestPlan_SessionCommandDepAutoPlansAndGatesSelectedScope(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A", Extensions: []string{"@putnami/ci"}},
		{ID: "/B", Name: "B", Path: "B", Extensions: []string{"@putnami/ci"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test"

	ciExt := releaseGateExtension(false)

	planned, err := Plan(ws, []string{"publish"}, ws.Projects, []*extension.ExtensionDescription{ciExt}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	keys := jobKeys(planned)
	for _, want := range []string{
		"/A:build~compile",
		"/A:lint~lint",
		"/A:test~test",
		"/A:publish~push",
		"/B:build~compile",
		"/B:lint~lint",
		"/B:test~test",
		"/B:publish~push",
	} {
		if !containsStr(keys, want) {
			t.Fatalf("missing %s in plan: %v", want, keys)
		}
	}

	wantDeps := []string{
		"/A:build~compile",
		"/A:lint~lint",
		"/A:test~test",
		"/B:build~compile",
		"/B:lint~lint",
		"/B:test~test",
	}
	for _, key := range []string{"/A:publish~push", "/B:publish~push"} {
		job := scheduledJobByKey(planned, key)
		if job == nil {
			t.Fatalf("%s not planned", key)
		}
		if !reflect.DeepEqual(job.DependsOn, wantDeps) {
			t.Fatalf("%s deps = %v, want %v", key, job.DependsOn, wantDeps)
		}
	}
}

func TestPlan_SessionCommandDepChainsThroughAutoPlannedCommands(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A", Extensions: []string{"@putnami/ci"}},
		{ID: "/B", Name: "B", Path: "B", Extensions: []string{"@putnami/ci"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test-workspace"

	ciExt := releaseGateExtension(true)

	planned, err := Plan(ws, []string{"deploy"}, ws.Projects, []*extension.ExtensionDescription{ciExt}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	for _, want := range []string{
		"/A:build~compile",
		"/B:build~compile",
		"/A:publish~push",
		"/B:publish~push",
		"test-workspace:deploy~apply",
	} {
		if !containsStr(jobKeys(planned), want) {
			t.Fatalf("missing %s in plan: %v", want, jobKeys(planned))
		}
	}

	wantPublishDeps := []string{
		"/A:build~compile",
		"/A:lint~lint",
		"/A:test~test",
		"/B:build~compile",
		"/B:lint~lint",
		"/B:test~test",
	}
	for _, key := range []string{"/A:publish~push", "/B:publish~push"} {
		job := scheduledJobByKey(planned, key)
		if job == nil {
			t.Fatalf("%s not planned", key)
		}
		if !reflect.DeepEqual(job.DependsOn, wantPublishDeps) {
			t.Fatalf("%s deps = %v, want %v", key, job.DependsOn, wantPublishDeps)
		}
	}

	deploy := scheduledJobByKey(planned, "test-workspace:deploy~apply")
	if deploy == nil {
		t.Fatalf("deploy not planned; jobs=%v", jobKeys(planned))
	}
	wantDeployDeps := []string{"/A:publish~push", "/B:publish~push"}
	if !reflect.DeepEqual(deploy.DependsOn, wantDeployDeps) {
		t.Fatalf("deploy deps = %v, want %v", deploy.DependsOn, wantDeployDeps)
	}
}

func TestResolveExternalDeps_PreservesCommandDepsWhenResolvingStepDeps(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A"},
		{ID: "/B", Name: "B", Path: "B"},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test"

	jobs := []*ScheduledJob{
		{Project: projects[0], JobDef: &extension.JobDefinition{Name: "build~compile"}},
		{Project: projects[1], JobDef: &extension.JobDefinition{Name: "package~archive"}},
		{
			Project: projects[0],
			JobDef: &extension.JobDefinition{
				Name:             "publish~push",
				DependsOn:        []string{"*package"},
				CommandDependsOn: []string{"build"},
			},
		},
	}

	if err := resolveExternalDeps(jobs, ws); err != nil {
		t.Fatalf("resolveExternalDeps: %v", err)
	}

	pub := scheduledJobByKey(jobs, "/A:publish~push")
	if pub == nil {
		t.Fatal("publish job not found")
	}
	want := []string{"/A:build~compile", "/B:package~archive"}
	if !reflect.DeepEqual(pub.DependsOn, want) {
		t.Fatalf("publish deps = %v, want %v", pub.DependsOn, want)
	}
}

func TestPlan_PublishSerializesPackageGenerationAfterBuild(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan", "write-serialization-stays-separate-from-functional-deps")
	projects := []*workspace.Project{
		{ID: "/auth/server", Name: "auth/server", Path: "auth/server", Extensions: []string{"@putnami/typescript", "@putnami/ci"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test"

	tsExt := publishGraphLanguageExtension("@putnami/typescript")
	ciExt := publishGraphCIExtension()
	planned, err := Plan(ws, []string{"publish"}, ws.Projects, []*extension.ExtensionDescription{tsExt, ciExt}, map[string]any{
		"docker":   true,
		"config":   true,
		"no-cache": true,
	}, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	keys := jobKeys(planned)
	for _, want := range []string{
		"/auth/server:build~generate",
		"/auth/server:package~generate",
		"/auth/server:publish~docker",
		"/auth/server:publish~config",
	} {
		if !containsStr(keys, want) {
			t.Fatalf("missing %s in plan: %v", want, keys)
		}
	}

	pkgGenerate := scheduledJobByKey(planned, "/auth/server:package~generate")
	if pkgGenerate == nil {
		t.Fatal("package~generate not planned")
	}
	// build and package share the project .gen tree (build-generate writes "gen";
	// build-transpile/types/compile read it), so package~generate is serialized
	// after build's generate writer and its readers — without a functional dep.
	for _, wantDep := range []string{"/auth/server:build~compile", "/auth/server:build~types"} {
		if !containsStr(pkgGenerate.SerializeAfter, wantDep) {
			t.Fatalf("package~generate serializeAfter = %v, want %s", pkgGenerate.SerializeAfter, wantDep)
		}
	}
	if containsStr(pkgGenerate.DependsOn, "/auth/server:build~compile") {
		t.Fatalf("write serialization must not add a functional dependency; deps=%v", pkgGenerate.DependsOn)
	}
	if !dependsTransitively(planned, "/auth/server:package~generate", "/auth/server:build~generate") {
		t.Fatalf("package~generate must wait for build~generate; serializeAfter=%v", pkgGenerate.SerializeAfter)
	}
}

func TestPlan_GoPublishSerializesPackageGenerationAfterBuild(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan", "write-serialization-stays-separate-from-functional-deps")
	projects := []*workspace.Project{
		{ID: "/go/app", Name: "go/app", Path: "go/app", Extensions: []string{"@putnami/go", "@putnami/ci"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test"

	goExt := publishGraphGoExtension()
	ciExt := publishGraphCIExtension()
	planned, err := Plan(ws, []string{"publish"}, ws.Projects, []*extension.ExtensionDescription{goExt, ciExt}, map[string]any{
		"docker": true,
	}, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	pkgGenerate := scheduledJobByKey(planned, "/go/app:package~generate")
	if pkgGenerate == nil {
		t.Fatalf("package~generate not planned; jobs=%v", jobKeys(planned))
	}
	// build-cross-compile reads the .gen tree that package~generate rewrites, so
	// the writer is serialized after the build leaf via SerializeAfter, not deps.
	if !containsStr(pkgGenerate.SerializeAfter, "/go/app:build~compile") {
		t.Fatalf("package~generate serializeAfter = %v, want build leaf", pkgGenerate.SerializeAfter)
	}
	if !dependsTransitively(planned, "/go/app:package~generate", "/go/app:build~generate") {
		t.Fatalf("package~generate must wait for build~generate; serializeAfter=%v", pkgGenerate.SerializeAfter)
	}
}

func TestPlan_ConfigExtractActivatesForGoProjectWithoutExplicitExtension(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "go.mod"), []byte("module example.com/app\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := workspace.NewWorkspace(dir, nil, []*workspace.Project{project})
	ws.Name = "test"
	goExt := &extension.ExtensionDescription{
		Name: "@putnami/go",
		Jobs: map[string]*extension.JobDefinition{
			"config-extract": {
				ExtensionName:   "@putnami/go",
				Name:            "config-extract",
				ActivationFiles: []string{"**/*.go", "go.mod"},
				PipelineSteps: []extension.PipelineStep{
					{ID: "extract", Task: "config-extract-exec"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"config-extract-exec": {Kind: "command", Command: "run"},
		},
	}

	planned, err := Plan(ws, []string{"config-extract"}, ws.Projects, []*extension.ExtensionDescription{goExt}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	keys := jobKeys(planned)
	want := []string{"/app:config-extract~extract"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("job keys = %v, want %v", keys, want)
	}
}

func TestPlan_CloudPublishConfigActivatesFromGeneratedSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	if err := os.MkdirAll(filepath.Join(appDir, ".gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, ".gen", "config-schema.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	project := &workspace.Project{ID: "/app", Name: "app", Path: "app", Extensions: []string{"@putnami/cloud"}}
	ws := workspace.NewWorkspace(dir, nil, []*workspace.Project{project})
	ws.Name = "test"
	cloud := &extension.ExtensionDescription{
		Name: "@putnami/cloud",
		Jobs: map[string]*extension.JobDefinition{
			"publish": {
				ExtensionName:   "@putnami/cloud",
				Name:            "publish",
				ActivationFiles: []string{"schema/config.json", ".gen/config-schema.json"},
				PipelineSteps: []extension.PipelineStep{
					{ID: "cloud-publish-config", Task: "publish-config"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"publish-config": {Kind: "command", Command: "run"},
		},
	}

	planned, err := Plan(ws, []string{"publish"}, ws.Projects, []*extension.ExtensionDescription{cloud}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	keys := jobKeys(planned)
	want := []string{"/app:publish~cloud-publish-config"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("job keys = %v, want %v", keys, want)
	}
}

func TestPlanWorkspaceOnceCommandRunsOnce(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A"},
		{ID: "/B", Name: "B", Path: "B"},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test-workspace"

	ciExt := &extension.ExtensionDescription{
		Name: "@putnami/ci",
		Jobs: map[string]*extension.JobDefinition{
			"deploy": {
				ExtensionName: "@putnami/ci",
				Name:          "deploy",
				Activation:    "workspace-once",
				PipelineSteps: []extension.PipelineStep{
					{ID: "apply", Task: "deploy-apply"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"deploy-apply": {Kind: "command", Command: "ci-run"},
		},
	}

	planned, err := Plan(ws, []string{"deploy"}, ws.Projects, []*extension.ExtensionDescription{ciExt}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(planned) != 1 {
		t.Fatalf("planned %d jobs, want 1", len(planned))
	}
	if got, want := planned[0].Key(), "test-workspace:deploy~apply"; got != want {
		t.Fatalf("job key = %q, want %q", got, want)
	}
}

func TestPlanWorkspaceOnceCommandCarriesSelectedProjects(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "selection-before-plan", "one-selection-applies-to-every-command")
	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A"},
		{ID: "/B", Name: "B", Path: "B"},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test-workspace"

	ciExt := &extension.ExtensionDescription{
		Name: "@putnami/ci",
		Jobs: map[string]*extension.JobDefinition{
			"deploy": {
				ExtensionName: "@putnami/ci",
				Name:          "deploy",
				Activation:    "workspace-once",
				PipelineSteps: []extension.PipelineStep{
					{ID: "apply", Task: "deploy-apply"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"deploy-apply": {Kind: "command", Command: "ci-run"},
		},
	}

	planned, err := Plan(ws, []string{"deploy"}, projects[:1], []*extension.ExtensionDescription{ciExt}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(planned) != 1 {
		t.Fatalf("planned %d jobs, want 1", len(planned))
	}
	if got, want := len(planned[0].SelectedProjects), 1; got != want {
		t.Fatalf("selected projects = %d, want %d", got, want)
	}
	if got, want := planned[0].SelectedProjects[0].Name, "A"; got != want {
		t.Fatalf("selected project name = %q, want %q", got, want)
	}
}

func TestPlanWorkspaceOnceCommandCanDependOnAllPublishJobs(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A"},
		{ID: "/B", Name: "B", Path: "B"},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test-workspace"

	ciExt := &extension.ExtensionDescription{
		Name: "@putnami/ci",
		Jobs: map[string]*extension.JobDefinition{
			"publish": {
				ExtensionName: "@putnami/ci",
				Name:          "publish",
				Activation:    "workspace",
				PipelineSteps: []extension.PipelineStep{
					{ID: "npm", Task: "publish-npm"},
				},
			},
			"deploy": {
				ExtensionName: "@putnami/ci",
				Name:          "deploy",
				Activation:    "workspace-once",
				PipelineSteps: []extension.PipelineStep{
					{ID: "apply", Task: "deploy-apply", DependsOn: []string{"*publish"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"publish-npm":  {Kind: "command", Command: "ci-run"},
			"deploy-apply": {Kind: "command", Command: "ci-run"},
		},
	}

	planned, err := Plan(ws, []string{"publish", "deploy"}, ws.Projects, []*extension.ExtensionDescription{ciExt}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var deploy *ScheduledJob
	for _, job := range planned {
		if job.Key() == "test-workspace:deploy~apply" {
			deploy = job
			break
		}
	}
	if deploy == nil {
		t.Fatalf("deploy job not planned; jobs: %v", jobKeys(planned))
	}

	sort.Strings(deploy.DependsOn)
	want := []string{"/A:publish~npm", "/B:publish~npm"}
	if !reflect.DeepEqual(deploy.DependsOn, want) {
		t.Fatalf("deploy deps = %v, want %v", deploy.DependsOn, want)
	}
}

func TestPlanSessionDependencyWorkspaceOnceCarriesSelectedProjects(t *testing.T) {
	t.Parallel()
	projects := []*workspace.Project{
		{ID: "/A", Name: "A", Path: "A"},
		{ID: "/B", Name: "B", Path: "B"},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "test-workspace"

	ciExt := &extension.ExtensionDescription{
		Name: "@putnami/ci",
		Jobs: map[string]*extension.JobDefinition{
			"release": {
				ExtensionName:    "@putnami/ci",
				Name:             "release",
				Activation:       "workspace",
				CommandDependsOn: []string{"!deploy"},
				PipelineSteps:    []extension.PipelineStep{{ID: "tag", Task: "release-tag"}},
			},
			"deploy": {
				ExtensionName: "@putnami/ci",
				Name:          "deploy",
				Activation:    "workspace-once",
				PipelineSteps: []extension.PipelineStep{{ID: "apply", Task: "deploy-apply"}},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"release-tag":  {Kind: "command", Command: "ci-run"},
			"deploy-apply": {Kind: "command", Command: "ci-run"},
		},
	}

	planned, err := Plan(ws, []string{"release"}, projects[:1], []*extension.ExtensionDescription{ciExt}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var deploy *ScheduledJob
	for _, job := range planned {
		if job.Key() == "test-workspace:deploy~apply" {
			deploy = job
			break
		}
	}
	if deploy == nil {
		t.Fatalf("deploy job not planned; jobs: %v", jobKeys(planned))
	}
	if got, want := len(deploy.SelectedProjects), 1; got != want {
		t.Fatalf("selected projects = %d, want %d", got, want)
	}
	if got, want := deploy.SelectedProjects[0].Name, "A"; got != want {
		t.Fatalf("selected project name = %q, want %q", got, want)
	}
}

func TestPlan_ComposesRootCommandContributions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cloud.deploy.json", "Pulumi.yaml"} {
		if err := os.WriteFile(filepath.Join(appDir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	project := &workspace.Project{
		ID:         "/app",
		Name:       "app",
		Path:       "app",
		Extensions: []string{"@putnami/cloud", "@putnami/pulumi"},
	}
	ws := workspace.NewWorkspace(dir, nil, []*workspace.Project{project})
	ws.Name = "test"
	cloud := deployExtension("@putnami/cloud", "cloud.deploy.json")
	pulumi := deployExtension("@putnami/pulumi", "Pulumi.yaml")

	planned, err := Plan(ws, []string{"deploy"}, ws.Projects, []*extension.ExtensionDescription{cloud, pulumi}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	keys := jobKeys(planned)
	want := []string{
		"/app:deploy~@putnami/cloud~deploy",
		"/app:deploy~@putnami/pulumi~deploy",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("job keys = %v, want %v", keys, want)
	}
	for _, job := range planned {
		if got := job.DisplayName(); got != "deploy~deploy" {
			t.Fatalf("display name = %q, want deploy~deploy", got)
		}
	}
}

func TestPlan_DeployActivatesFromCommittedInfraMarkersWithoutGen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	if err := os.MkdirAll(filepath.Join(appDir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "infra", "runtime.json"), []byte(`{"ingress":{"public":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	project := &workspace.Project{
		ID:         "/app",
		Name:       "app",
		Path:       "app",
		Extensions: []string{"@putnami/cloud"},
	}
	ws := workspace.NewWorkspace(dir, nil, []*workspace.Project{project})
	ws.Name = "test"
	cloud := deployExtension("@putnami/cloud", "")
	cloud.Jobs["deploy"].ActivationFiles = []string{"infra/runtime.json", "infra/requirements.json"}

	planned, err := Plan(ws, []string{"deploy"}, ws.Projects, []*extension.ExtensionDescription{cloud}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	keys := jobKeys(planned)
	want := []string{"/app:deploy~deploy"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("job keys = %v, want %v", keys, want)
	}
	if _, err := os.Stat(filepath.Join(appDir, ".gen", "requirements.json")); !os.IsNotExist(err) {
		t.Fatalf("test setup should have no .gen requirements artifact, stat err = %v", err)
	}
}

func TestPlan_ComposedRootCommandResolvesUpstreamStepRefs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, projectDir := range []string{"lib", "app"} {
		abs := filepath.Join(dir, projectDir)
		if err := os.MkdirAll(abs, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"cloud.deploy.json", "Pulumi.yaml"} {
			if err := os.WriteFile(filepath.Join(abs, name), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@putnami/cloud", "@putnami/pulumi"}}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}, Extensions: []string{"@putnami/cloud", "@putnami/pulumi"}}
	projects := []*workspace.Project{lib, app}
	ws := workspace.NewWorkspace(dir, nil, projects)
	ws.Name = "test"
	cloud := deployExtension("@putnami/cloud", "cloud.deploy.json")
	pulumi := deployExtension("@putnami/pulumi", "Pulumi.yaml")

	planned, err := Plan(ws, []string{"deploy"}, []*workspace.Project{app}, []*extension.ExtensionDescription{cloud, pulumi}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	keys := jobKeys(planned)
	wantKeys := []string{
		"/app:deploy~@putnami/cloud~deploy",
		"/app:deploy~@putnami/pulumi~deploy",
		"/lib:deploy~@putnami/cloud~deploy",
		"/lib:deploy~@putnami/pulumi~deploy",
	}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("job keys = %v, want %v", keys, wantKeys)
	}

	wantDeps := []string{
		"/lib:deploy~@putnami/cloud~deploy",
		"/lib:deploy~@putnami/pulumi~deploy",
	}
	for _, job := range planned {
		if job.Project.ID != "/app" {
			continue
		}
		if !reflect.DeepEqual(job.DependsOn, wantDeps) {
			t.Fatalf("%s deps = %v, want %v", job.Key(), job.DependsOn, wantDeps)
		}
	}
}

func TestPlan_ConflictingCommandFlagsFailValidation(t *testing.T) {
	t.Parallel()
	project := &workspace.Project{
		ID:         "/app",
		Name:       "app",
		Path:       "app",
		Extensions: []string{"@putnami/cloud", "@putnami/pulumi"},
	}
	ws := workspace.NewWorkspace("", nil, []*workspace.Project{project})
	ws.Name = "test"
	cloud := deployExtension("@putnami/cloud", "")
	cloud.Jobs["deploy"].Flags = map[string]extension.FlagDefinition{
		"target": {Type: "string", Description: "Cloud target"},
	}
	pulumi := deployExtension("@putnami/pulumi", "")
	pulumi.Jobs["deploy"].Flags = map[string]extension.FlagDefinition{
		"target": {Type: "boolean", Description: "Target all stacks"},
	}

	_, err := Plan(ws, []string{"deploy"}, ws.Projects, []*extension.ExtensionDescription{cloud, pulumi}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected flag conflict")
	}
	if !strings.Contains(err.Error(), "--target") {
		t.Fatalf("error = %v, want --target conflict", err)
	}
}

func TestPlan_PipelineExpansionErrorWrapsProjectAndCommand(t *testing.T) {
	t.Parallel()
	ws := makeTestWorkspace()
	ext := makeTestExtension()
	// Point the build pipeline's first step at a task that is absent from the
	// extension's (non-nil) Tasks map so ExpandPipelineWithOptions fails on the
	// primary expansion path with "references unknown task" — exercising the
	// `expand pipeline for %s/%s` wrap in Plan that no other test reaches.
	ext.Jobs["build"].PipelineSteps[0].Task = "build-missing"

	_, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected pipeline expansion error")
	}
	// utils is the first selected project, so its direct expansion fails first.
	// Format is "expand pipeline for <proj.Name>/<cmdName>" (planner.go:263).
	if !strings.Contains(err.Error(), "expand pipeline for utils/build") {
		t.Fatalf("error = %v, want to contain %q", err, "expand pipeline for utils/build")
	}
	// The underlying cause should still be wrapped through.
	if !strings.Contains(err.Error(), "unknown task 'build-missing'") {
		t.Fatalf("error = %v, want wrapped unknown-task cause", err)
	}
}

func TestPlan_UpstreamPipelineExpansionErrorWrapsStepAndProject(t *testing.T) {
	t.Parallel()
	// app depends on utils. Only app is selected, so app's direct expansion
	// succeeds; emitUpstreamJobs then expands utils's `generate` step (reached
	// via the `^generate` ref) which references a task absent from utils's
	// extension, failing on the upstream path with the
	// `expand upstream step %s for %s` wrap (planner_deps.go:134).
	utilsProj := &workspace.Project{ID: "/packages/utils", Name: "utils", Path: "packages/utils", Extensions: []string{"@test/ts-broken"}}
	appProj := &workspace.Project{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"utils"}, Extensions: []string{"@test/ts-good"}}
	ws := workspace.NewWorkspace("/workspace", nil, []*workspace.Project{utilsProj, appProj})
	ws.Name = "test-ws"

	// app's extension expands cleanly.
	goodExt := makeTestExtension()
	goodExt.Name = "@test/ts-good"
	goodExt.Jobs["build"].ExtensionName = "@test/ts-good"
	goodExt.Jobs["test"].ExtensionName = "@test/ts-good"

	// utils's extension shares the `generate` step id (so the `^generate`
	// upstream lookup resolves to it) but points it at a missing task.
	brokenExt := makeTestExtension()
	brokenExt.Name = "@test/ts-broken"
	brokenExt.Jobs["build"].ExtensionName = "@test/ts-broken"
	brokenExt.Jobs["test"].ExtensionName = "@test/ts-broken"
	brokenExt.Jobs["build"].PipelineSteps[0].Task = "build-missing"

	appOnly := []*workspace.Project{appProj}
	_, err := Plan(ws, []string{"build"}, appOnly, []*extension.ExtensionDescription{goodExt, brokenExt}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected upstream pipeline expansion error")
	}
	// Format is "expand upstream step <stepID> for <depName>" (planner_deps.go:134):
	// stepID is the `^`-stripped ref ("generate"), depName is the upstream
	// project ID ("/packages/utils").
	if !strings.Contains(err.Error(), "expand upstream step generate for /packages/utils") {
		t.Fatalf("error = %v, want to contain %q", err, "expand upstream step generate for /packages/utils")
	}
	if !strings.Contains(err.Error(), "unknown task 'build-missing'") {
		t.Fatalf("error = %v, want wrapped unknown-task cause", err)
	}
}

func deployExtension(name, activationFile string) *extension.ExtensionDescription {
	job := &extension.JobDefinition{
		ExtensionName: name,
		Name:          "deploy",
		Activation:    "",
		PipelineSteps: []extension.PipelineStep{
			{ID: "deploy", Task: "deploy-task", DependsOn: []string{"^deploy"}},
		},
	}
	if activationFile != "" {
		job.ActivationFiles = []string{activationFile}
	}
	return &extension.ExtensionDescription{
		Name: name,
		Jobs: map[string]*extension.JobDefinition{
			"deploy": job,
		},
		Tasks: map[string]extension.TaskDefinition{
			"deploy-task": {Kind: "command", Command: "deploy"},
		},
	}
}

func publishGraphLanguageExtension(name string) *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: name,
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: name,
				Name:          "build",
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
					{ID: "transpile", Task: "build-transpile", DependsOn: []string{"generate"}},
					{ID: "types", Task: "build-types", DependsOn: []string{"generate", "transpile"}},
					{ID: "compile", Task: "build-compile", DependsOn: []string{"generate"}, If: "params.docker"},
				},
			},
			"package": {
				ExtensionName: name,
				Name:          "package",
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
					{ID: "transpile", Task: "build-transpile", DependsOn: []string{"generate"}},
					{ID: "types", Task: "build-types", DependsOn: []string{"generate", "transpile"}},
					{ID: "compile", Task: "build-compile", DependsOn: []string{"generate"}, If: "params.docker"},
					{ID: "docker", Task: "package-docker", DependsOn: []string{"compile"}, If: "params.docker"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-generate":  {Kind: "command", Command: "run", Writes: []extension.ResourceRef{{ID: "gen"}}},
			"build-transpile": {Kind: "command", Command: "run", Reads: []extension.ResourceRef{{ID: "gen"}}},
			"build-types":     {Kind: "command", Command: "run", Reads: []extension.ResourceRef{{ID: "gen"}}},
			"build-compile":   {Kind: "command", Command: "run", Reads: []extension.ResourceRef{{ID: "gen"}}},
			"package-docker":  {Kind: "command", Command: "run"},
		},
	}
}

func publishGraphCIExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@putnami/ci",
		Jobs: map[string]*extension.JobDefinition{
			"publish": {
				ExtensionName:    "@putnami/ci",
				Name:             "publish",
				Activation:       "workspace",
				CommandDependsOn: []string{"build", "package"},
				PipelineSteps: []extension.PipelineStep{
					{ID: "docker", Task: "publish-docker", If: "params.docker"},
					{ID: "config", Task: "publish-config", If: "params.config"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"publish-docker": {Kind: "command", Command: "run"},
			"publish-config": {Kind: "command", Command: "run"},
		},
	}
}

func releaseGateExtension(includeDeploy bool) *extension.ExtensionDescription {
	jobs := map[string]*extension.JobDefinition{
		"build": {
			ExtensionName: "@putnami/ci",
			Name:          "build",
			PipelineSteps: []extension.PipelineStep{{ID: "compile", Task: "build-compile"}},
		},
		"lint": {
			ExtensionName: "@putnami/ci",
			Name:          "lint",
			PipelineSteps: []extension.PipelineStep{{ID: "lint", Task: "lint-run"}},
		},
		"test": {
			ExtensionName: "@putnami/ci",
			Name:          "test",
			PipelineSteps: []extension.PipelineStep{{ID: "test", Task: "test-run"}},
		},
		"publish": {
			ExtensionName:    "@putnami/ci",
			Name:             "publish",
			Activation:       "workspace",
			CommandDependsOn: []string{"!lint", "!test", "!build"},
			PipelineSteps:    []extension.PipelineStep{{ID: "push", Task: "publish-push"}},
		},
	}
	if includeDeploy {
		jobs["deploy"] = &extension.JobDefinition{
			ExtensionName:    "@putnami/ci",
			Name:             "deploy",
			Activation:       "workspace-once",
			CommandDependsOn: []string{"!publish"},
			PipelineSteps:    []extension.PipelineStep{{ID: "apply", Task: "deploy-apply"}},
		}
	}

	return &extension.ExtensionDescription{
		Name: "@putnami/ci",
		Jobs: jobs,
		Tasks: map[string]extension.TaskDefinition{
			"build-compile": {Kind: "command", Command: "run"},
			"lint-run":      {Kind: "command", Command: "run"},
			"test-run":      {Kind: "command", Command: "run"},
			"publish-push":  {Kind: "command", Command: "run"},
			"deploy-apply":  {Kind: "command", Command: "run"},
		},
	}
}

func publishGraphGoExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@putnami/go",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: "@putnami/go",
				Name:          "build",
				PipelineSteps: []extension.PipelineStep{
					{ID: "tidy", Task: "build-tidy", DependsOn: []string{"^tidy"}},
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate", "tidy"}},
					{ID: "describe", Task: "build-describe", DependsOn: []string{"^describe", "generate"}},
					{ID: "compile", Task: "build-cross-compile", DependsOn: []string{"describe"}},
				},
			},
			"package": {
				ExtensionName: "@putnami/go",
				Name:          "package",
				PipelineSteps: []extension.PipelineStep{
					{ID: "tidy", Task: "build-tidy", DependsOn: []string{"^tidy"}},
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate", "tidy"}},
					{ID: "describe", Task: "build-describe", DependsOn: []string{"^describe", "generate"}},
					{ID: "compile", Task: "build-cross-compile", DependsOn: []string{"describe"}},
					{ID: "docker", Task: "package-docker", DependsOn: []string{"compile"}, If: "params.docker"},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-tidy":          {Kind: "command", Command: "run"},
			"build-generate":      {Kind: "command", Command: "run", Writes: []extension.ResourceRef{{ID: "gen"}}},
			"build-describe":      {Kind: "command", Command: "run", Reads: []extension.ResourceRef{{ID: "gen"}}},
			"build-cross-compile": {Kind: "command", Command: "run", Reads: []extension.ResourceRef{{ID: "gen"}}},
			"package-docker":      {Kind: "command", Command: "run"},
		},
	}
}

func scheduledJobByKey(jobs []*ScheduledJob, key string) *ScheduledJob {
	for _, job := range jobs {
		if job.Key() == key {
			return job
		}
	}
	return nil
}

func dependsTransitively(jobs []*ScheduledJob, from, to string) bool {
	index := make(map[string]*ScheduledJob, len(jobs))
	for _, job := range jobs {
		index[job.Key()] = job
	}

	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(key string) bool {
		if key == to {
			return true
		}
		if seen[key] {
			return false
		}
		seen[key] = true
		job := index[key]
		if job == nil {
			return false
		}
		// Follow both functional dependencies and write-serialization edges:
		// either guarantees `to` completes before `from` runs.
		for _, dep := range job.SchedulingPredecessors() {
			if walk(dep) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

func jobKeys(jobs []*ScheduledJob) []string {
	keys := make([]string, len(jobs))
	for i, job := range jobs {
		keys[i] = job.Key()
	}
	sort.Strings(keys)
	return keys
}
