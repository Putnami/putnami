package agentctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

// The containment check follows directory links on both ends, a symbolic link
// on Unix and a junction on Windows: a link under the workspace that names a
// directory outside it is refused, and a link that stays inside, or a root
// spelled through a link, is accepted.
func TestRequireInsideWorkspaceFollowsDirectoryLinks(t *testing.T) {
	ws := t.TempDir()
	inside := filepath.Join(ws, "content")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(ws, "escape")
	if err := dirlink.Create(t.TempDir(), escape); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(ws, "alias")
	if err := dirlink.Create(inside, alias); err != nil {
		t.Fatal(err)
	}
	wsLink := filepath.Join(t.TempDir(), "workspace")
	if err := dirlink.Create(ws, wsLink); err != nil {
		t.Fatal(err)
	}

	if err := requireInsideWorkspace(ws, escape); err == nil || !strings.Contains(err.Error(), "resolves outside the workspace") {
		t.Errorf("a link that leaves the workspace: error = %v, want it refused as outside the workspace", err)
	}
	if err := requireInsideWorkspace(ws, alias); err != nil {
		t.Errorf("a link that stays inside the workspace: %v", err)
	}
	if err := requireInsideWorkspace(wsLink, inside); err != nil {
		t.Errorf("a workspace root spelled through a link: %v", err)
	}
}
