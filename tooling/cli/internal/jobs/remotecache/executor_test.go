package remotecache

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

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
