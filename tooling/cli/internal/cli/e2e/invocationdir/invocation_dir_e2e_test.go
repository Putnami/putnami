// Package invocationdir drives an extension command from a directory inside a
// workspace through the real CLI, and reads the directory the task is told it
// was typed in. The tests set the environment, change the working directory
// and swap the process streams, so they run in their own test binary.
package invocationdir

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// The requirement these tests prove.
const (
	invocationDirFeature     = "cli/job-planning-execution"
	invocationDirRequirement = "an-interactive-launch-names-the-directory-it-was-typed-in"
)

// demoManifest declares a "demo" group with an interactive "login" subcommand
// and a planned "deploy" subcommand that the app project activates.
const demoManifest = `{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@putnami/demo-extension",
  "version": "0.1.0",
  "cliContract": 4,
  "commandGroups": {
    "demo": {
      "description": "Demo group",
      "subcommands": {
        "login": { "command": "demo-login", "interactive": true },
        "deploy": { "command": "deploy" }
      }
    }
  },
  "commands": {
    "demo-login": {
      "description": "Demo login fixture.",
      "run": [{ "id": "login", "task": "fixture-task" }]
    },
    "deploy": {
      "description": "Demo deploy fixture.",
      "activationFiles": ["demo.deploy"],
      "run": [{ "id": "deploy", "task": "fixture-task" }]
    }
  },
  "tasks": {
    "fixture-task": {
      "kind": "command",
      "command": "{extensionRoot}/fixture.sh",
      "cache": false,
      "timeoutMs": -1
    }
  }
}`

// writeFixture writes the workspace with script as the task every command runs.
func writeFixture(t *testing.T, wsRoot, script string) {
	t.Helper()
	clitest.RequireShell(t)
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), `{"name":"fixture-ws","includes":["demo-extension","app"]}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "demo-extension", "putnami.json"), `{"name":"@putnami/demo-extension"}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "demo-extension", "putnami.extension.json"), demoManifest)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@putnami/demo-extension"]}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "demo.deploy"), "")
	if err := os.WriteFile(filepath.Join(wsRoot, "demo-extension", "fixture.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}
}

// runApp runs one CLI invocation from dir and returns its exit code and
// combined output.
func runApp(t *testing.T, dir string, argv ...string) (int, string) {
	t.Helper()
	var code int
	output := clitest.CaptureStdoutStderr(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(dir)
		code = app.Run(context.Background(), argv)
	})
	return code, output
}

// contextDocument is the part of a job context document these tests read.
type contextDocument struct {
	InvocationDir string `json:"invocationDir"`
	UserScope     *struct {
		CallerDir string `json:"callerDir"`
	} `json:"userScope"`
}

// TestAppRun_ExtensionStructuredCommand_InteractiveCarriesTheInvocationDir pins
// the fix for a command typed in a subdirectory: the task runs in the
// workspace root, so the context and the environment must name the directory
// the user typed it in, while userScope and PUTNAMI_CALLER_DIR keep saying
// that a workspace exists.
func TestAppRun_ExtensionStructuredCommand_InteractiveCarriesTheInvocationDir(t *testing.T) {
	spectest.Proves(t, invocationDirFeature, invocationDirRequirement, "the-context-and-environment-carry-the-typed-directory")
	wsRoot := t.TempDir()
	writeFixture(t, wsRoot, `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
echo "invocation_env=$PUTNAMI_INVOCATION_DIR"
echo "caller_env=${PUTNAMI_CALLER_DIR-unset}"
echo "cwd=$(pwd -P)"
cat "$ctx"
echo
`)
	typedIn := filepath.Join(wsRoot, "docs")
	if err := os.MkdirAll(typedIn, 0o755); err != nil {
		t.Fatal(err)
	}
	// An outer user-scope run leaves this in the environment; it must not
	// reach a workspace job.
	t.Setenv("PUTNAMI_CALLER_DIR", "/inherited/outer/caller")
	t.Setenv("PUTNAMI_INVOCATION_DIR", "/inherited/outer/invocation")

	code, output := runApp(t, typedIn, "demo", "login")
	if code != cli.ExitSuccess {
		t.Fatalf("demo login exit = %d, want %d:\n%s", code, cli.ExitSuccess, output)
	}
	want, err := filepath.EvalSymlinks(typedIn)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	// os.Getwd may return the symlinked spelling of the directory (macOS
	// temporary directories do), so compare the directories, not the spellings.
	sameDir := func(got string) bool {
		resolved, err := filepath.EvalSymlinks(got)
		return err == nil && resolved == want
	}
	envLine := ""
	for _, line := range strings.Split(output, "\n") {
		if v, ok := strings.CutPrefix(line, "invocation_env="); ok {
			envLine = v
		}
	}
	if !filepath.IsAbs(envLine) || !sameDir(envLine) {
		t.Errorf("PUTNAMI_INVOCATION_DIR = %q, want the directory %q:\n%s", envLine, want, output)
	}
	if !strings.Contains(output, "caller_env=unset\n") {
		t.Errorf("PUTNAMI_CALLER_DIR reaches a workspace job:\n%s", output)
	}
	if !strings.Contains(output, "cwd="+root+"\n") {
		t.Errorf("the task no longer runs in the workspace root %q:\n%s", root, output)
	}
	start, end := strings.Index(output, "{"), strings.LastIndex(output, "}")
	if start < 0 || end < start {
		t.Fatalf("no context document in the output:\n%s", output)
	}
	var document contextDocument
	if err := json.Unmarshal([]byte(output[start:end+1]), &document); err != nil {
		t.Fatalf("the interactive context is not JSON: %v\n%s", err, output)
	}
	if !filepath.IsAbs(document.InvocationDir) || !sameDir(document.InvocationDir) {
		t.Errorf("invocationDir = %q, want the directory %q", document.InvocationDir, want)
	}
	if document.UserScope != nil {
		t.Errorf("userScope = %+v, want absent inside a workspace", document.UserScope)
	}
}

// TestAppRun_PlannedJobCarriesNoInvocationDir pins the cache claim: a planned
// job, whose result is cached, gets neither the context member nor the
// variable, even when the environment inherited one.
func TestAppRun_PlannedJobCarriesNoInvocationDir(t *testing.T) {
	spectest.Proves(t, invocationDirFeature, invocationDirRequirement, "a-planned-job-carries-none")
	wsRoot := t.TempDir()
	dump := filepath.Join(wsRoot, "planned-dump.txt")
	writeFixture(t, wsRoot, `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --putnamiContext) ctx="$2"; shift 2 ;;
    *) shift ;;
  esac
done
{
  echo "env=${PUTNAMI_INVOCATION_DIR-unset}"
  cat "$ctx"
} > "`+dump+`"
exit 0
`)
	// deploy refuses a root Git does not manage.
	clitest.RunGit(t, wsRoot, "init")
	t.Setenv("PUTNAMI_INVOCATION_DIR", "/inherited/outer/invocation")

	if code, output := runApp(t, wsRoot, "demo", "deploy", "app"); code != cli.ExitSuccess {
		t.Fatalf("demo deploy app exit = %d, want %d:\n%s", code, cli.ExitSuccess, output)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the planned job wrote no dump: %v", err)
	}
	if !strings.HasPrefix(string(data), "env=unset\n") {
		t.Errorf("PUTNAMI_INVOCATION_DIR reaches a planned job:\n%s", data)
	}
	if strings.Contains(string(data), "invocationDir") {
		t.Errorf("a planned job context carries invocationDir:\n%s", data)
	}
}
