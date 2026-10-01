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

// TestClaimPackageRoot_KeepsTheRoot pins when a package.json root stays the
// root: a putnami.workspace.json at or above the working directory always
// wins, and an invocation that cannot name an extension command group, such as
// a comma list, or that the release-set provider runs, never reads the user
// scope. The cases that read the user scope run App.Run in
// internal/cli/e2e/userscope.
func TestClaimPackageRoot_KeepsTheRoot(t *testing.T) {
	t.Parallel()
	writePackageRoot := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "packages", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"mono","workspaces":["packages/*"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "putnami.workspace.json"), []byte(`{"name":"outer"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(ws, "mono")
	writePackageRoot(nested)
	plain := t.TempDir()
	writePackageRoot(plain)

	tests := []struct {
		name          string
		args          []string
		cwd           string
		wsRoot        string
		providerChild bool
	}{
		{name: "no root", args: []string{"audit", "run"}, cwd: plain, wsRoot: ""},
		{name: "workspace manifest root", args: []string{"audit", "run"}, cwd: ws, wsRoot: ws},
		{name: "workspace manifest above the package root", args: []string{"audit", "run"}, cwd: filepath.Join(nested, "packages", "app"), wsRoot: nested},
		{name: "built-in command", args: []string{"help"}, cwd: plain, wsRoot: plain},
		{name: "comma list", args: []string{"audit,build"}, cwd: plain, wsRoot: plain},
		{name: "release-set provider mode", args: []string{"audit", "run"}, cwd: plain, wsRoot: plain, providerChild: true},
		{name: "flag", args: []string{"--version"}, cwd: plain, wsRoot: plain},
		{name: "no argument", args: nil, cwd: plain, wsRoot: plain},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claim := claimPackageRoot(context.Background(), tc.args, tc.cwd, tc.wsRoot, tc.providerChild)
			if claim.owns || claim.repair || claim.root(tc.wsRoot) != tc.wsRoot {
				t.Fatalf("the user scope claimed %v in %s (root %q): %+v", tc.args, tc.cwd, tc.wsRoot, claim)
			}
		})
	}
}

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
