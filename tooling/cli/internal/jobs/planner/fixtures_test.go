package planner

import (
	"os"
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Shared fixtures for the package's plan-shaped tests.
//
// Reading a project's committed manifests is an extension's job now
// (go.putnami.dev/sdk/extension/dbtestenv, infraagg), reached through the job
// context's project.dependencyClosure.

func testWorkspace(root string, projects ...*workspace.Project) *workspace.Workspace {
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "ws"
	return ws
}

func writeProjectFile(t *testing.T, root, projPath, rel, content string) {
	t.Helper()
	full := filepath.Join(root, projPath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// findJobsRepoRoot locates the workspace root so the tests can read the REAL
// manifests. It skips rather than fails when the tree is unavailable, so a
// packaged-source test run stays green.
func findJobsRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("could not find the Putnami repo root from %s", start)
		}
		dir = parent
	}
}

// makeJob builds a minimal ScheduledJob for DAG tests.
func makeJob(projectName, jobName string, dependsOn []string) *jobmodel.ScheduledJob {
	return &jobmodel.ScheduledJob{
		Project:   &workspace.Project{ID: "/" + projectName, Name: projectName, Path: projectName},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: jobName},
		DependsOn: dependsOn,
	}
}

// cacheableJob builds a minimal cache-enabled job for a project.
func cacheableJob(name, projID, projName, projPath string, deps ...string) *jobmodel.ScheduledJob {
	const task = "cacheable"
	return &jobmodel.ScheduledJob{
		Project: &workspace.Project{ID: projID, Name: projName, Path: projPath},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Tasks: map[string]extension.TaskDefinition{
				task: {Declares: &extension.TaskDeclaration{}},
			},
		},
		JobDef: &extension.JobDefinition{
			Name:          name,
			ExtensionName: "@test/ext",
			Cache:         true,
		},
		Step:      &extension.PipelineStep{Task: task},
		DependsOn: deps,
	}
}

// makeExecutorTestWorkspace creates a temp workspace with required .putnami subdirectories
// and a project directory for test jobs.
func makeExecutorTestWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".putnami", "store"), 0o755)
	os.MkdirAll(filepath.Join(root, ".putnami", "out"), 0o755)
	os.MkdirAll(filepath.Join(root, "test-proj"), 0o755)
	os.MkdirAll(filepath.Join(root, "proj"), 0o755)
	return &workspace.Workspace{Root: root}
}

// makeIntegrationWorkspace lays out a real workspace on disk with two
// interdependent projects (utils ← app) so the planner has a dependency edge
// between two projects to expand a pipeline over.
func makeIntegrationWorkspace(t *testing.T) (*workspace.Workspace, *extension.ExtensionDescription) {
	t.Helper()
	wsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsRoot, "packages", "utils"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wsRoot, "packages", "app"), 0o755); err != nil {
		t.Fatal(err)
	}

	projects := []*workspace.Project{
		{ID: "/packages/utils", Name: "utils", Path: "packages/utils", Extensions: []string{"@test/ts"}},
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"utils"}, Extensions: []string{"@test/ts"}},
	}
	ws := workspace.NewWorkspace(wsRoot, nil, projects)
	ws.Name = "integration-ws"

	// Nothing here runs the command: the tests plan and validate only, so the
	// dependency wiring (^generate, transpile→generate, cross-project
	// utils→app) is the only thing under test.
	const proc = "true"

	ext := &extension.ExtensionDescription{
		Name: "@test/ts",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: "@test/ts",
				Name:          "build",
				Kind:          "command",
				Command:       proc,
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
					{ID: "transpile", Task: "build-transpile", DependsOn: []string{"generate"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-generate":  {Kind: "command", Command: proc, Declares: &extension.TaskDeclaration{}},
			"build-transpile": {Kind: "command", Command: proc, Declares: &extension.TaskDeclaration{}},
		},
	}
	return ws, ext
}
