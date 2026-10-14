package cachekey

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

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
