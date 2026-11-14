package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// writeNoStateFixture is writeWorkspaceFixture with one more subcommand,
// `demo whoami`, that declares workspaceState none, and a lock file with no
// recorded install state beside it: a fresh checkout the first-use bootstrap
// would install.
func writeNoStateFixture(t *testing.T, wsRoot string) {
	t.Helper()
	writeWorkspaceFixture(t, wsRoot)
	path := filepath.Join(wsRoot, "demo-extension", "putnami.extension.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = `"login": {`
	if !strings.Contains(string(data), anchor) {
		t.Fatalf("fixture manifest has no %s", anchor)
	}
	manifest := strings.Replace(string(data), anchor,
		`"whoami": { "command": "demo-login", "interactive": true, "workspaceState": "none", "description": "Who am I." },
        `+anchor, 1)
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.lock.json"), []byte(`{"version": 4}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runFixtureSubcommand runs `putnami demo <sub>` in wsRoot and reports whether
// the first-use bootstrap started: it marks the workspace before it installs.
func runFixtureSubcommand(t *testing.T, wsRoot, sub string) (bootstrapped bool) {
	t.Helper()
	t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "")
	captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), []string{"demo", sub}); code != ExitSuccess {
			t.Fatalf("demo %s exit = %d, want %d", sub, code, ExitSuccess)
		}
	})
	return os.Getenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED") == wsRoot
}

func TestAppRun_NoStateSubcommandRunsNoBootstrap(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	spectest.Proves(t, "cli/first-use-path", "a-subcommand-that-reads-no-workspace-state-skips-the-bootstrap", "a-no-state-subcommand-runs-no-bootstrap")
	spectest.Proves(t, "cli/first-use-path", "a-subcommand-that-reads-no-workspace-state-skips-the-bootstrap", "every-other-command-keeps-the-bootstrap")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	wsRoot := t.TempDir()
	writeNoStateFixture(t, wsRoot)
	if runFixtureSubcommand(t, wsRoot, "whoami") {
		t.Fatal("demo whoami declares workspaceState none, and the first-use bootstrap ran before it")
	}
	if _, err := os.Stat(filepath.Join(wsRoot, ".putnami", installStateFile)); !os.IsNotExist(err) {
		t.Fatalf("demo whoami recorded an install state: %v", err)
	}

	other := t.TempDir()
	writeNoStateFixture(t, other)
	if !runFixtureSubcommand(t, other, "login") {
		t.Fatal("demo login declares no workspace state, and the first-use bootstrap did not run before it")
	}
}

func TestReadsNoWorkspaceState(t *testing.T) {
	spectest.Proves(t, "cli/first-use-path", "a-subcommand-that-reads-no-workspace-state-skips-the-bootstrap", "every-other-command-keeps-the-bootstrap")
	job := &extension.JobDefinition{}
	owner := &extension.ExtensionDescription{
		Name: "@acme/demo",
		Jobs: map[string]*extension.JobDefinition{"demo-whoami": job, "demo-login": job},
		CommandGroups: map[string]extension.CommandGroupDefinition{
			"demo": {Default: "whoami", Subcommands: map[string]extension.SubcommandDefinition{
				"whoami": {Command: "demo-whoami", Interactive: true, WorkspaceState: "none"},
				"login":  {Command: "demo-login", Interactive: true},
				"odd":    {Command: "demo-login", Interactive: true, WorkspaceState: "installed"},
			}},
		},
	}
	rival := &extension.ExtensionDescription{
		Name: "@acme/rival",
		Jobs: map[string]*extension.JobDefinition{"demo-whoami": job},
		CommandGroups: map[string]extension.CommandGroupDefinition{
			"demo": {Subcommands: map[string]extension.SubcommandDefinition{
				"whoami": {Command: "demo-whoami", Interactive: true, WorkspaceState: "none"},
			}},
		},
	}
	groups := map[string]bool{"demo": true}
	single := []*extension.ExtensionDescription{owner}
	for _, tc := range []struct {
		name       string
		parsed     *ParsedArgs
		extensions []*extension.ExtensionDescription
		want       bool
	}{
		{"declared none", &ParsedArgs{Commands: []string{"demo"}, Subcommand: "whoami"}, single, true},
		{"the group default", &ParsedArgs{Commands: []string{"demo"}}, single, true},
		{"no declaration", &ParsedArgs{Commands: []string{"demo"}, Subcommand: "login"}, single, false},
		{"an unknown value", &ParsedArgs{Commands: []string{"demo"}, Subcommand: "odd"}, single, false},
		{"an unknown subcommand", &ParsedArgs{Commands: []string{"demo"}, Subcommand: "nope"}, single, false},
		{"two owners", &ParsedArgs{Commands: []string{"demo"}, Subcommand: "whoami"}, []*extension.ExtensionDescription{owner, rival}, false},
		{"a job command", &ParsedArgs{Commands: []string{"build"}}, single, false},
		{"several commands", &ParsedArgs{Commands: []string{"demo", "build"}, Subcommand: "whoami"}, single, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readsNoWorkspaceState(tc.parsed, t.TempDir(), tc.extensions, groups); got != tc.want {
				t.Fatalf("readsNoWorkspaceState = %v, want %v", got, tc.want)
			}
		})
	}
}
