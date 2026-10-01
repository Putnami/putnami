package jobs

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Shared fixtures for the package's plan-shaped tests.
//
// They outlived infra_closure_test.go, whose subject — the requirements-closure
// helpers the test-infra planner was the last consumer of — was deleted with
// that planner. Walking a project's closure and
// reading its committed manifests is an extension's job now
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

// --- jobCommandAndStep ----------------------------------------------------

func TestJobCommandAndStep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		command string
		step    string
	}{
		{"build~generate", "build", "generate"},
		{"build", "build", ""},
		{"build~infra", "build", "infra"},
	} {
		command, step := jobCommandAndStep(tc.name)
		if command != tc.command || step != tc.step {
			t.Errorf("jobCommandAndStep(%q) = (%q, %q), want (%q, %q)",
				tc.name, command, step, tc.command, tc.step)
		}
	}
}
