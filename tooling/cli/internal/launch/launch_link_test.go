package launch

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

// A directory link to the workspace root, a symbolic link on Unix and a
// junction on Windows, is one more spelling of the root: the marker and the
// discovered root match in both directions.
func TestRelaunchSourceWorkspaceResolvesADirectoryLinkedRoot(t *testing.T) {
	real := t.TempDir()
	writeSourceWorkspaceLock(t, real)
	link := filepath.Join(t.TempDir(), "worktree")
	if err := dirlink.Create(real, link); err != nil {
		t.Fatal(err)
	}
	var cap capture
	c := baseConfig(&cap, workspaceEngine(t, real))
	c.getenv = envFunc(map[string]string{FromSourceEnv: real})
	mustRun(t, link, c, "a directory-linked spelling of the same workspace root")

	c.getenv = envFunc(map[string]string{FromSourceEnv: link})
	mustRun(t, real, c, "the real spelling of a directory-linked workspace root")
}
