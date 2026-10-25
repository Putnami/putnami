package workspacewarnings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// launchManifest declares a "demo" group whose one subcommand, "login", is
// interactive: its task copies the job context it receives to
// launch-context.json in the workspace root.
const launchManifest = `{
  "name": "@fixture/demo",
  "version": "0.1.0",
  "cliContract": 4,
  "commandGroups": {
    "demo": {
      "description": "Demo group",
      "subcommands": {"login": {"command": "demo-login", "interactive": true}}
    }
  },
  "commands": {
    "demo-login": {"description": "Demo login fixture.", "run": [{"id": "login", "task": "copy-context"}]}
  },
  "tasks": {
    "copy-context": {"kind": "command", "command": "{extensionRoot}/copy-context.sh", "cache": false, "timeoutMs": -1}
  }
}`

const copyContextScript = `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    cp "$2" launch-context.json
  fi
  shift
done
`

// launchWorkspace writes a workspace holding the demo extension and one
// project, app. When declaresJobs is true, app's putnami.json declares a
// `jobs` key, which the load warns about.
func launchWorkspace(t *testing.T, declaresJobs bool) string {
	t.Helper()
	clitest.RequireShell(t)
	root := t.TempDir()
	project := `{"name":"app"}`
	if declaresJobs {
		project = `{"name":"app","jobs":{}}`
	}
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"launch-warnings","includes":["demo-extension","app"]}`)
	clitest.WriteFile(t, filepath.Join(root, "demo-extension", "putnami.json"), `{"name":"@fixture/demo"}`)
	clitest.WriteFile(t, filepath.Join(root, "demo-extension", "putnami.extension.json"), launchManifest)
	clitest.WriteFile(t, filepath.Join(root, "app", "putnami.json"), project)
	if err := os.WriteFile(filepath.Join(root, "demo-extension", "copy-context.sh"), []byte(copyContextScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// launch runs `putnami demo login` with args in root and returns its exit
// code, its stderr and the job context its task received, parsed strictly.
func launch(t *testing.T, root string, args ...string) (int, string, *protocoljob.Context) {
	t.Helper()
	var code int
	_, stderr := clitest.CaptureStreams(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(root)
		code = app.Run(context.Background(), append([]string{"demo", "login"}, args...))
	})
	data, err := os.ReadFile(filepath.Join(root, "launch-context.json"))
	if err != nil {
		t.Fatalf("the task received no job context (exit %d): %v\nstderr:\n%s", code, err, stderr)
	}
	parsed, diags := protocoljob.ParseAndValidate(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the launch's job context violates the protocol: %v\n%s", diags, data)
	}
	return code, stderr, parsed
}

// TestInteractiveLaunchHandsTheWarningsOfItsLoadToTheSubcommand holds the
// launch of an interactive extension subcommand to reporting the warnings of
// its workspace load as every command does: each once on stderr, unless
// --quiet, and, quiet or not, to the subcommand in its job context's
// workspaceWarnings, so the envelope the subcommand writes can carry them. A
// load that warned of nothing hands no member.
func TestInteractiveLaunchHandsTheWarningsOfItsLoadToTheSubcommand(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory",
		"an-interactive-launch-hands-its-load-warnings-to-the-subcommand")
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "a-command-reports-the-warnings-of-its-own-load-once")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("DO_NOT_TRACK", "1")

	for _, quiet := range []bool{false, true} {
		name, args := "default", []string(nil)
		if quiet {
			name, args = "quiet", []string{"--quiet"}
		}
		t.Run(name, func(t *testing.T) {
			code, stderr, launched := launch(t, launchWorkspace(t, true), args...)
			if code != cli.ExitSuccess {
				t.Fatalf("exit code = %d\nstderr:\n%s", code, stderr)
			}
			if len(launched.WorkspaceWarnings) != 1 || launched.WorkspaceWarnings[0].Code != string(workspace.WarningCodeJobsIgnored) {
				t.Fatalf("workspaceWarnings = %+v, want the one ignored-jobs warning", launched.WorkspaceWarnings)
			}
			want := 1
			if quiet {
				want = 0
			}
			line := "putnami: warning: " + launched.WorkspaceWarnings[0].Message
			if count := strings.Count(stderr, line); count != want || strings.Count(stderr, "putnami: warning: ") != want {
				t.Errorf("stderr carries the warning %d times, want %d:\n%s", count, want, stderr)
			}
		})
	}

	code, stderr, launched := launch(t, launchWorkspace(t, false))
	if code != cli.ExitSuccess {
		t.Fatalf("clean workspace exit code = %d\nstderr:\n%s", code, stderr)
	}
	if launched.WorkspaceWarnings != nil || strings.Contains(stderr, "putnami: warning: ") {
		t.Errorf("a load with no warning reported one: %+v\nstderr:\n%s", launched.WorkspaceWarnings, stderr)
	}
}
