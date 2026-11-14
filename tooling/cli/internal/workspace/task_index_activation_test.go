package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// activationIndexExtensions are three manifests in the shapes this repository
// loads: a Go extension whose tasks read Go sources, a second stack whose docs
// check reads the whole candidate tree and whose Docker packaging reads every
// file of the project and of its dependency closure, and a specification
// extension with a per-project check activated by its manifest and a
// workspace-once check, both reading the whole tree.
func activationIndexExtensions() []*extension.ExtensionDescription {
	goExt := &extension.ExtensionDescription{
		Name: "@fixture/go", RelPath: "go/extension",
		Tasks: map[string]extproto.TaskDefinition{
			"go-compile": {Inputs: map[string]extproto.TaskInputPort{
				"sources": {From: extproto.TaskInputFromProject, Files: []string{"**/*.go"}},
			}},
		},
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				CommandName: "build", Name: "build", ExtensionName: "@fixture/go", ExtensionPath: "go/extension",
				PipelineSteps: []extproto.PipelineStep{{ID: "compile", Task: "go-compile"}},
			},
		},
	}
	otherExt := &extension.ExtensionDescription{
		Name: "@fixture/dart", RelPath: "dart/extension",
		Tasks: map[string]extproto.TaskDefinition{
			"lint-docs": {Inputs: map[string]extproto.TaskInputPort{
				"repository": {From: extproto.TaskInputFromWorkspace, Files: []string{"git:**"}},
			}},
			"package-docker": {Inputs: map[string]extproto.TaskInputPort{
				"application": {From: extproto.TaskInputFromProject, Files: []string{"**/*"}},
				"references":  {From: extproto.TaskInputFromClosure, Files: []string{"**/*"}},
			}},
		},
		Jobs: map[string]*extension.JobDefinition{
			"lint": {
				CommandName: "lint", Name: "lint", ExtensionName: "@fixture/dart", ExtensionPath: "dart/extension",
				PipelineSteps: []extproto.PipelineStep{{ID: "docs", Task: "lint-docs"}},
			},
			"package": {
				CommandName: "package", Name: "package", ExtensionName: "@fixture/dart", ExtensionPath: "dart/extension",
				PipelineSteps: []extproto.PipelineStep{{ID: "docker", Task: "package-docker"}},
			},
		},
	}
	specExt := &extension.ExtensionDescription{
		Name: "@fixture/sdd", RelPath: "sdd/extension",
		Tasks: map[string]extproto.TaskDefinition{
			"features-validate": {Inputs: map[string]extproto.TaskInputPort{
				"repository": {From: extproto.TaskInputFromWorkspace, Files: []string{"git:**"}},
			}},
			"decisions-validate": {Inputs: map[string]extproto.TaskInputPort{
				"repository": {From: extproto.TaskInputFromProject, Files: []string{"git:**"}},
			}},
		},
		Jobs: map[string]*extension.JobDefinition{
			"validate": {
				CommandName: "validate", Name: "validate", ExtensionName: "@fixture/sdd", ExtensionPath: "sdd/extension",
				ActivationFiles: []string{"putnami.features.json"},
				PipelineSteps:   []extproto.PipelineStep{{ID: "features", Task: "features-validate"}},
			},
			"validate-workspace": {
				CommandName: "validate-workspace", Name: "validate-workspace", ExtensionName: "@fixture/sdd",
				ExtensionPath: "sdd/extension", Activation: workspaceOnceActivation,
				PipelineSteps: []extproto.PipelineStep{{ID: "decisions", Task: "decisions-validate"}},
			},
		},
	}
	return []*extension.ExtensionDescription{goExt, otherExt, specExt}
}

// activationIndexWorkspace holds a Go library a project of the other stack
// depends on, a Go library nothing depends on, the other stack's application,
// and a Go library that declares features.
func activationIndexWorkspace(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "libs/specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "libs/specs/putnami.features.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	projects := []*Project{
		{ID: "/go/extension", Name: "@fixture/go", Path: "go/extension"},
		{ID: "/dart/extension", Name: "@fixture/dart", Path: "dart/extension"},
		{ID: "/sdd/extension", Name: "@fixture/sdd", Path: "sdd/extension"},
		{ID: "/libs/http", Name: "http", Path: "libs/http", Extensions: []string{"/go/extension"}},
		{ID: "/libs/core", Name: "core", Path: "libs/core", Extensions: []string{"/go/extension"}},
		{ID: "/libs/specs", Name: "specs", Path: "libs/specs", Extensions: []string{"/go/extension", "@fixture/sdd"}},
		{ID: "/apps/web", Name: "web", Path: "apps/web", Extensions: []string{"/dart/extension"}, Dependencies: []string{"/libs/http"}},
	}
	ws := NewWorkspace(root, nil, projects)
	ws.Graph = BuildGraph(projects)
	return ws
}

// A file pattern is read only where the planner may run the command that
// declares it. A text file in a Go project is read by no task of another
// stack's docs check or packaging, even though their patterns select every
// path, while its Go sources still select the Go tasks. A closure input is
// read where a dependent may run its command, and its closure scope reaches
// that dependent. A per-project check reads where its activation file is, and
// a workspace-once command, planned on every impacted run, reads for no
// project.
func TestTaskIndex_APatternIsReadOnlyWhereItsCommandMayBePlanned(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure",
		"a-file-claims-only-the-tasks-its-project-may-plan")
	ws := activationIndexWorkspace(t)
	idx := NewTaskIndex(ws, activationIndexExtensions(), nil)
	if idx == nil {
		t.Fatal("NewTaskIndex returned nil for a workspace with three manifests")
	}
	cases := []struct {
		project, path string
		want          []string
	}{
		{project: "/libs/core", path: "libs/core/scratch.txt"},
		{project: "/libs/core", path: "libs/core/core.go", want: []string{"build~compile"}},
		{project: "/libs/http", path: "libs/http/scratch.txt", want: []string{closureScopePrefix + "package~docker"}},
		{project: "/libs/http", path: "libs/http/http.go", want: []string{"build~compile", closureScopePrefix + "package~docker"}},
		{project: "/apps/web", path: "apps/web/scratch.txt", want: []string{closureScopePrefix + "package~docker", "lint~docs", "package~docker"}},
		{project: "/libs/specs", path: "libs/specs/scratch.txt", want: []string{"validate~features"}},
	}
	for _, tc := range cases {
		if got := idx.TasksReadingPath(tc.project, tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s is read by %v, want %v", tc.path, got, tc.want)
		}
	}
	reached := idx.TasksReachedFrom(idx.TasksReadingPath("/libs/http", "libs/http/scratch.txt"))
	if !slices.Contains(reached, "package~docker") {
		t.Errorf("a dependent reaches %v, want package~docker", reached)
	}
}

// Change impact over the index: a text file in a Go library selects no
// project, a Go source selects the library, and a text file in a library whose
// dependent packages its closure selects that dependent's packaging alone.
func TestTraceChangeImpact_AFileOfAnotherStackSelectsNoProject(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure",
		"a-file-claims-only-the-tasks-its-project-may-plan")
	ws := activationIndexWorkspace(t)
	idx := NewTaskIndex(ws, activationIndexExtensions(), nil)
	opts := ChangeImpactOptions{Tasks: idx}
	if got := traceChangeImpact(ws, []string{"libs/core/scratch.txt"}, opts); len(got.Projects) != 0 {
		t.Errorf("a text file in a Go library selects %v, want nothing", impactProjectIDs(got.Projects))
	}
	if got := traceChangeImpact(ws, []string{"libs/core/core.go"}, opts); !slices.Equal(impactProjectIDs(got.Projects), []string{"/libs/core"}) {
		t.Errorf("a Go source selects %v, want /libs/core", impactProjectIDs(got.Projects))
	}
	got := traceChangeImpact(ws, []string{"libs/http/scratch.txt"}, opts)
	if scope := got.Trace.ScopeOf("/apps/web"); !slices.Contains(scope, "package~docker") || slices.Contains(scope, "lint~docs") {
		t.Errorf("the dependent's scope is %v, want its packaging and not its docs check", scope)
	}
}

func impactProjectIDs(projects []*Project) []string {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	slices.Sort(ids)
	return ids
}

// A step a dependent's step names through `^` is read, project and workspace
// ports alike, where a project that depends on this one may run its command:
// the dependent's toolchain may read the sources and build files of a project
// that runs no such step. Where no dependent may run it, it reads nothing.
func TestTaskIndex_AnUpstreamStepIsReadWhereADependentMayRunIt(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "impacted-closure",
		"a-file-claims-only-the-tasks-its-project-may-plan")
	javaExt := &extension.ExtensionDescription{
		Name: "@fixture/java", RelPath: "java/extension",
		Tasks: map[string]extproto.TaskDefinition{
			"java-build": {Inputs: map[string]extproto.TaskInputPort{
				"sources": {From: extproto.TaskInputFromProject, Files: []string{"**/*.java"}},
				"poms":    {From: extproto.TaskInputFromWorkspace, Files: []string{"*/*/pom.xml"}},
			}},
		},
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				CommandName: "build", Name: "build", ExtensionName: "@fixture/java", ExtensionPath: "java/extension",
				PipelineSteps: []extproto.PipelineStep{{ID: "build", Task: "java-build", DependsOn: []string{"^build"}}},
			},
		},
	}
	projects := []*Project{
		{ID: "/java/extension", Name: "@fixture/java", Path: "java/extension"},
		{ID: "/libs/shared", Name: "shared", Path: "libs/shared"},
		{ID: "/libs/alone", Name: "alone", Path: "libs/alone"},
		{ID: "/apps/svc", Name: "svc", Path: "apps/svc", Extensions: []string{"/java/extension"}, Dependencies: []string{"/libs/shared"}},
	}
	ws := NewWorkspace(t.TempDir(), nil, projects)
	ws.Graph = BuildGraph(projects)
	idx := NewTaskIndex(ws, []*extension.ExtensionDescription{javaExt}, nil)
	for _, tc := range []struct {
		project, path string
		want          []string
	}{
		{project: "/libs/shared", path: "libs/shared/Shared.java", want: []string{"build~build"}},
		{project: "/libs/shared", path: "libs/shared/pom.xml", want: []string{"build~build"}},
		{project: "/libs/alone", path: "libs/alone/Alone.java"},
		{project: "/libs/alone", path: "libs/alone/pom.xml"},
	} {
		if got := idx.TasksReadingPath(tc.project, tc.path); !slices.Equal(got, tc.want) {
			t.Errorf("%s is read by %v, want %v", tc.path, got, tc.want)
		}
	}
}

// A workspace-once command reads for no project, so manifests whose only file
// declarations are workspace-once attribute nothing: the index is nil and the
// selection keeps the project-level mapping instead of selecting nothing.
func TestNewTaskIndex_OnlyWorkspaceOnceDeclarationsIsANilInterface(t *testing.T) {
	ext := &extension.ExtensionDescription{
		Name: "@fixture/sdd", RelPath: "sdd/extension",
		Tasks: map[string]extproto.TaskDefinition{
			"decisions-validate": {Inputs: map[string]extproto.TaskInputPort{
				"repository": {From: extproto.TaskInputFromProject, Files: []string{"git:**"}},
			}},
		},
		Jobs: map[string]*extension.JobDefinition{
			"validate-workspace": {
				CommandName: "validate-workspace", Name: "validate-workspace", ExtensionName: "@fixture/sdd",
				ExtensionPath: "sdd/extension", Activation: workspaceOnceActivation,
				PipelineSteps: []extproto.PipelineStep{{ID: "decisions", Task: "decisions-validate"}},
			},
		},
	}
	if idx := NewTaskIndex(activationIndexWorkspace(t), []*extension.ExtensionDescription{ext}, nil); idx != nil {
		t.Fatalf("NewTaskIndex = %T, want a nil interface", idx)
	}
}

// A `^command` dependency carries a moved prerequisite through the walk: the
// prerequisite travels on through a dependent that plans nothing of it, and
// reaches the dependent command only where a command that declares the
// dependency may be planned.
func TestTraceChangeImpact_AClosureDependencyCarriesThePrerequisiteToTheDependentCommand(t *testing.T) {
	lintExt := &extension.ExtensionDescription{
		Name: "@fixture/lint", RelPath: "lint/extension",
		Tasks: map[string]extproto.TaskDefinition{
			"lint-exec": {Inputs: map[string]extproto.TaskInputPort{
				"sources": {From: extproto.TaskInputFromProject, Files: []string{"**/*.go"}},
			}},
		},
		Jobs: map[string]*extension.JobDefinition{
			"lint": {
				CommandName: "lint", CommandDependsOn: []string{"^client-generate"},
				PipelineSteps: []extproto.PipelineStep{{ID: "lint", Task: "lint-exec"}},
			},
		},
	}
	clientgenExt := &extension.ExtensionDescription{
		Name: "@fixture/clientgen", RelPath: "clientgen/extension",
		Tasks: map[string]extproto.TaskDefinition{"client-go": {Inputs: map[string]extproto.TaskInputPort{
			"contract": {From: extproto.TaskInputFromProject, Files: []string{"clientgen.json"}},
		}}},
		Jobs: map[string]*extension.JobDefinition{
			"client-generate": {CommandName: "client-generate", PipelineSteps: []extproto.PipelineStep{{ID: "go", Task: "client-go"}}},
		},
	}
	projects := []*Project{
		{ID: "/lint/extension", Name: "@fixture/lint", Path: "lint/extension"},
		{ID: "/clientgen/extension", Name: "@fixture/clientgen", Path: "clientgen/extension"},
		{ID: "/libs/api", Name: "api", Path: "libs/api", Extensions: []string{"/clientgen/extension"}},
		{ID: "/libs/mid", Name: "mid", Path: "libs/mid", Dependencies: []string{"/libs/api"}},
		{ID: "/apps/web", Name: "web", Path: "apps/web", Extensions: []string{"/lint/extension"}, Dependencies: []string{"/libs/mid"}},
		{ID: "/apps/bare", Name: "bare", Path: "apps/bare", Dependencies: []string{"/libs/api"}},
	}
	ws := NewWorkspace(t.TempDir(), nil, projects)
	ws.Graph = BuildGraph(projects)
	idx := NewTaskIndex(ws, []*extension.ExtensionDescription{lintExt, clientgenExt}, nil)
	if idx == nil {
		t.Fatal("NewTaskIndex returned nil")
	}
	ti := idx.(*taskIndex)
	moved := []string{"client-generate~go"}
	if got, want := ti.TasksReachedIn("/apps/web", moved), []string{"client-generate~go", "lint~lint"}; !slices.Equal(got, want) {
		t.Errorf("a dependent that lints runs %v when client-generate~go moved, want %v", got, want)
	}
	if got, want := ti.TasksReachedIn("/apps/bare", moved), []string{"client-generate~go"}; !slices.Equal(got, want) {
		t.Errorf("a dependent that lints nothing carries %v, want the prerequisite alone", got)
	}
	if got := ti.TasksReachedIn("/apps/web", nil); !slices.Contains(got, "lint~lint") || !slices.Contains(got, "client-generate~go") {
		t.Errorf("a dependent of a whole project carries %v, want lint~lint and client-generate~go", got)
	}
	got := traceChangeImpact(ws, []string{"libs/api/clientgen.json"}, ChangeImpactOptions{Tasks: idx})
	if scope := got.Trace.ScopeOf("/apps/web"); !slices.Contains(scope, "lint~lint") {
		t.Errorf("the dependent two edges up runs %v, want lint~lint", scope)
	}
}

// A `^command` a project names under commands.<command>.dependsOn of its
// putnami.json reaches the dependent command in that project: a build the tool
// runs in a dependency carries a change to the test that waits for it, and not
// to the test of a project whose file names no such entry.
func TestTaskIndex_AProjectCommandDependencyReachesTheDependentCommand(t *testing.T) {
	toolExt := &extension.ExtensionDescription{
		Name: "@fixture/tool", RelPath: "tool/extension",
		Tasks: map[string]extproto.TaskDefinition{"build-exec": {Inputs: map[string]extproto.TaskInputPort{
			"sources": {From: extproto.TaskInputFromProject, Files: []string{"src/**"}},
		}}},
		Jobs: map[string]*extension.JobDefinition{
			"build": {CommandName: "build", PipelineSteps: []extproto.PipelineStep{{ID: "build", Task: "build-exec"}}},
		},
	}
	tsExt := &extension.ExtensionDescription{
		Name: "@fixture/ts", RelPath: "ts/extension",
		Tasks: map[string]extproto.TaskDefinition{"test-exec": {Inputs: map[string]extproto.TaskInputPort{
			"sources": {From: extproto.TaskInputFromProject, Files: []string{"test/**"}},
		}}},
		Jobs: map[string]*extension.JobDefinition{
			"test": {CommandName: "test", PipelineSteps: []extproto.PipelineStep{{ID: "test", Task: "test-exec"}}},
		},
	}
	index := func(config *wsproto.ProjectConfig) *taskIndex {
		projects := []*Project{
			{ID: "/tool/extension", Name: "@fixture/tool", Path: "tool/extension"},
			{ID: "/ts/extension", Name: "@fixture/ts", Path: "ts/extension"},
			{ID: "/libs/types", Name: "types", Path: "libs/types", Extensions: []string{"/tool/extension"}},
			{ID: "/apps/report", Name: "report", Path: "apps/report", Extensions: []string{"/ts/extension"}, Dependencies: []string{"/libs/types"}, Config: config},
			{ID: "/apps/other", Name: "other", Path: "apps/other", Extensions: []string{"/ts/extension"}, Dependencies: []string{"/libs/types"}},
		}
		ws := NewWorkspace(t.TempDir(), nil, projects)
		ws.Graph = BuildGraph(projects)
		idx := NewTaskIndex(ws, []*extension.ExtensionDescription{toolExt, tsExt}, nil)
		if idx == nil {
			t.Fatal("NewTaskIndex returned nil")
		}
		return idx.(*taskIndex)
	}
	moved := []string{"build~build"}
	if got := index(nil).TasksReachedIn("/apps/report", moved); slices.Contains(got, "test~test") {
		t.Fatalf("without the project's entry the test is reached: %v", got)
	}
	waits := &wsproto.ProjectConfig{Commands: map[string]wsproto.ProjectCommand{"test": {DependsOn: []string{"^build"}}}}
	waiting := index(waits)
	if got := waiting.TasksReachedIn("/apps/report", moved); !slices.Contains(got, "test~test") {
		t.Errorf("a test that waits for ^build runs %v when the dependency's build moved, want test~test", got)
	}
	if got := waiting.TasksReachedIn("/apps/other", moved); slices.Contains(got, "test~test") {
		t.Errorf("a project whose putnami.json names no entry runs %v, want no test~test: the entry is read in its own project alone", got)
	}
	if got := waiting.TasksReachedFrom(moved); !slices.Contains(got, "test~test") {
		t.Errorf("an unknown project carries %v, want test~test", got)
	}
}
