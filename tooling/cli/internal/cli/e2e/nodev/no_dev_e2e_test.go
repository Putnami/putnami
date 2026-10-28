// Package nodev drives the real CLI in a workspace without a development
// extension: its one extension declares a command that is no development verb,
// and no vocabulary. The core declares no development verb, so help and
// completion show none, and every entry point refuses one.
// These tests swap the process streams and the working directory, so they run
// in their own test binary.
package nodev

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// developmentVerbs are the eleven commands a development extension declares,
// and developmentAliases the short names its vocabulary gives them.
var (
	developmentVerbs   = []string{"build", "test", "lint", "serve", "run", "format", "package", "publish", "deploy", "generate", "validate"}
	developmentAliases = []string{"b", "t", "l", "s", "f", "p", "P", "d", "D"}
)

// refusal is the usage error of a run that names commands no loaded
// extension declares.
func refusal(commands string) string {
	return "no extension of this workspace declares " + commands + "; name a language extension in putnami.workspace.json, then run: putnami extensions install"
}

// noDevFixture writes a workspace, in a git repository, whose one extension
// declares the inspect command and no vocabulary. Its task records each
// execution in app/calls.
func noDevFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"nodev-fixture","includes":["extension","app"]}`)
	clitest.WriteFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/tool"]}`)
	clitest.WriteFile(t, filepath.Join(root, "app", "tool.project"), "")
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.json"), `{"name":"@fixture/tool"}`)
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.extension.json"), `{
  "name": "@fixture/tool",
  "version": "1.0.0",
  "cliContract": 4,
  "commands": {
    "inspect": {
      "description": "Inspect the project.",
      "activationFiles": ["tool.project"],
      "run": [{"id": "inspect", "task": "inspect"}]
    }
  },
  "tasks": {
    "inspect": {
      "kind": "command",
      "command": "/bin/sh",
      "args": ["-c", "echo ran >> \"$PUTNAMI_PROJECT_ROOT/calls\""],
      "cache": {"enabled": false},
      "timeoutMs": -1
    }
  }
}`)
	clitest.InitGitRepo(t, root)
	return root
}

// TestAWorkspaceWithoutADevelopmentExtensionShowsNoVerb holds help and
// completion to what the workspace's extension declares: its inspect command,
// and no development verb, alias or provider-bound flag.
func TestAWorkspaceWithoutADevelopmentExtensionShowsNoVerb(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "no-core-development-verb", "a-workspace-without-a-development-extension-shows-no-verb-alias-or-flag")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	root := noDevFixture(t)

	code, help := clitest.RunGateArgs(t, root, "--help")
	if code != 0 {
		t.Fatalf("--help exit %d:\n%s", code, help)
	}
	if !strings.Contains(help, "Build (from extensions):\n  inspect\n") {
		t.Errorf("help does not list inspect alone as the workspace's job command:\n%s", help)
	}
	for _, hidden := range []string{"Aliases:", "--where", "--cache-trust", "--providers", "putnami build"} {
		if strings.Contains(help, hidden) {
			t.Errorf("help lists %q, which no extension of the workspace declares:\n%s", hidden, help)
		}
	}

	code, bash := clitest.RunGateArgs(t, root, "completion", "bash")
	if code != 0 {
		t.Fatalf("completion bash exit %d:\n%s", code, bash)
	}
	commands := bashCommands(t, bash)
	if !slices.Contains(commands, "inspect") {
		t.Errorf("bash completion does not offer inspect: %v", commands)
	}
	for _, name := range append(slices.Clone(developmentVerbs), developmentAliases...) {
		if slices.Contains(commands, name) {
			t.Errorf("bash completion offers %q, which no extension of the workspace declares", name)
		}
	}
	code, zsh := clitest.RunGateArgs(t, root, "completion", "zsh")
	if code != 0 || !strings.Contains(zsh, "'inspect:Inspect the project.'") {
		t.Fatalf("zsh completion exit %d does not offer inspect:\n%s", code, zsh)
	}
	for _, name := range developmentVerbs {
		if strings.Contains(zsh, "'"+name+":") {
			t.Errorf("zsh completion offers %q, which no extension of the workspace declares", name)
		}
	}
}

// TestAnUndeclaredJobCommandIsRefusedInEveryEntryPoint runs a development verb
// through each entry point that runs or describes a job: each refuses it with
// exit 2 and the one usage error that says how to add an extension, and no
// task runs.
func TestAnUndeclaredJobCommandIsRefusedInEveryEntryPoint(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "no-core-development-verb", "an-undeclared-job-command-is-refused-once-in-every-entry-point")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	root := noDevFixture(t)
	for _, tc := range []struct {
		args       []string
		undeclared string
	}{
		{[]string{"build"}, "build"},
		{[]string{"build", "--projects", "app"}, "build"},
		{[]string{"build,inspect,lint,build"}, "build, lint"},
		{[]string{"b"}, "b"},
		{[]string{"help", "build"}, "build"},
		{[]string{"build", "--help"}, "build"},
		{[]string{"impact-plan", "build", "--base", "HEAD", "--no-cache", "--output=json"}, "build"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, output := clitest.RunGateArgs(t, root, tc.args...)
			want := refusal(tc.undeclared)
			if code != 2 || !strings.Contains(output, want) {
				t.Fatalf("exit %d, want 2 and %q:\n%s", code, want, output)
			}
			// impact-plan reports the run's error a second time, prefixed
			// by the plan it was making.
			if tc.args[0] != "impact-plan" && strings.Count(output, want) != 1 {
				t.Errorf("the refusal is reported %d times, want once:\n%s", strings.Count(output, want), output)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a refused run executed a task: %v", err)
	}

	// Outside a workspace, the help of a job command reports what a run of it
	// reports, no workspace, with the exit code of its entry point: a job
	// command's, or the help command's usage error.
	outside := t.TempDir()
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"build"}, 1},
		{[]string{"build", "--help"}, 1},
		{[]string{"help", "build"}, 2},
	} {
		code, output := clitest.RunGateArgs(t, outside, tc.args...)
		if code != tc.code || !strings.Contains(output, "putnami: no workspace found") || strings.Contains(output, "declares") {
			t.Errorf("%s outside a workspace: exit %d, want %d and no workspace found:\n%s", strings.Join(tc.args, " "), code, tc.code, output)
		}
	}

	// Control: the command the extension declares runs.
	if code, output := clitest.RunGateArgs(t, root, "inspect", "--projects", "app", "--no-cache"); code != 0 {
		t.Fatalf("inspect exit %d:\n%s", code, output)
	}
	if calls, err := os.ReadFile(filepath.Join(root, "app", "calls")); err != nil || string(calls) != "ran\n" {
		t.Fatalf("calls = %q (%v), want one execution of inspect", calls, err)
	}
}

// bashCommands returns the top-level command words a bash completion script
// offers.
func bashCommands(t *testing.T, script string) []string {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(script))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// The list is an array; a plain command name is a bare word of it.
		if words, ok := strings.CutPrefix(line, `local -a commands=(`); ok {
			return strings.Fields(strings.TrimSuffix(words, `)`))
		}
	}
	t.Fatalf("no command list in the bash completion:\n%s", script)
	return nil
}
