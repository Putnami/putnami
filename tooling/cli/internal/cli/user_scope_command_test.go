package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
)

// The App.Run tests of the user scope set HOME, the working directory and the
// process streams, so they live in internal/cli/e2e/userscope.

// TestWithInvocationReadPreparation_UserScopeSkipsTheWorkspace pins that
// `extensions list --user` never prepares the workspace it runs in, while the
// workspace listing in the same place still does.
func TestWithInvocationReadPreparation_UserScopeSkipsTheWorkspace(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(`{"name":"prep-ws"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &wsproto.Config{Name: "prep-ws"}

	userScope := &ParsedArgs{Commands: []string{"extensions"}, Subcommand: "list", RawJobArgs: []string{"--user"}}
	if _, prepared := lifecycle.ReadPreparationFromContext(
		withInvocationReadPreparation(context.Background(), wsRoot, cfg, userScope)); prepared {
		t.Fatal("extensions list --user prepared the workspace it runs in")
	}
	workspaceList := &ParsedArgs{Commands: []string{"extensions"}, Subcommand: "list"}
	if _, prepared := lifecycle.ReadPreparationFromContext(
		withInvocationReadPreparation(context.Background(), wsRoot, cfg, workspaceList)); !prepared {
		t.Fatal("extensions list no longer prepares the workspace")
	}
}
