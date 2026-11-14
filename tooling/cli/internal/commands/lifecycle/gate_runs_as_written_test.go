package lifecycle_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/lifecycle"
)

// testBuildOnlyExtension declares test and build and no flag, the surface
// @putnami/bazel and @putnami/pants declare: no lint, no --enforce-coverage.
const testBuildOnlyExtension = `{
  "name": "@example/bazel",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "test": { "description": "Test fixture.", "run": [{ "id": "test", "task": "ok" }] },
    "build": { "description": "Build fixture.", "run": [{ "id": "build", "task": "ok" }] }
  },
  "tasks": {
    "ok": { "kind": "command", "command": "/bin/sh", "args": ["-c", "exit 0"], "cache": false }
  }
}`

// writeTestBuildOnlyWorkspace writes a committed workspace whose one project
// runs through an extension that declares only test and build.
func writeTestBuildOnlyWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"putnami.workspace.json":                 `{"name":"fixture","includes":["bazel-extension","app"]}`,
		"bazel-extension/putnami.json":           `{"name":"@example/bazel"}`,
		"bazel-extension/putnami.extension.json": testBuildOnlyExtension,
		"app/putnami.json":                       `{"name":"app","extensions":["@example/bazel"]}`,
		"app/BUILD":                              "\n",
	}
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gittest.Run(t, dir, "init", "-b", "main")
	gittest.Run(t, dir, "add", "-A")
	gittest.Run(t, dir, "commit", "-m", "initial")
	// A change to the project, so --impacted selects it.
	if err := os.WriteFile(filepath.Join(dir, "app", "BUILD"), []byte("# changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runPutnami runs one command line through the CLI in dir and returns its
// exit code and output.
func runPutnami(t *testing.T, dir string, args []string) (int, string) {
	t.Helper()
	app, err := cli.NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Chdir(dir)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = writer, writer
	captured := make(chan string)
	go func() {
		data, _ := io.ReadAll(reader)
		captured <- string(data)
	}()
	code := app.Run(context.Background(), args)
	os.Stdout, os.Stderr = stdout, stderr
	_ = writer.Close()
	return code, <-captured
}

// commandArgs splits a written `putnami ...` command line into the CLI's
// arguments, without the program name.
func commandArgs(t *testing.T, line string) []string {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "putnami" {
		t.Fatalf("%q is not a putnami command line", line)
	}
	return fields[1:]
}

// On a workspace whose extensions declare test and build and no coverage
// flag, the gate AGENTS.md names and each gate command init's Next: hint
// names run: under --dry-run, each exits 0. The same gate with
// --enforce-coverage is refused, so the derivation is what makes them run.
func TestTheWrittenGateAndTheInitHintRunWithoutLintOrCoverage(t *testing.T) {
	spectest.Proves(t, "cli/ci-document", "gate-runs-as-written", "the-gate-names-only-declared-flags")
	spectest.Proves(t, "cli/ci-document", "gate-runs-as-written", "the-init-hint-names-the-gate")
	dir := writeTestBuildOnlyWorkspace(t)

	if err := agentctx.WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("WriteAgentEntrypoints: %v", err)
	}
	guidance, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	const before, after = "run `putnami ", "` once"
	_, rest, ok := strings.Cut(string(guidance), before)
	gate, _, closed := strings.Cut(rest, after)
	if !ok || !closed {
		t.Fatalf("AGENTS.md names no gate:\n%s", guidance)
	}
	if gate != "test,build --impacted" {
		t.Fatalf("AGENTS.md gate = %q, want test,build --impacted", gate)
	}

	lines := []string{"putnami " + gate}
	for _, steps := range [][]string{
		lifecycle.InitNextSteps(dir, "", true, "", ""),
		lifecycle.InitNextSteps(dir, "", false, "", "BUILD"),
		lifecycle.InitNextSteps(dir, "app", false, "", ""),
	} {
		for _, step := range steps {
			if command, _, _ := strings.Cut(step, "#"); strings.Contains(command, "test,build") {
				lines = append(lines, strings.TrimSpace(command))
			} else if strings.Contains(command, "lint") {
				t.Fatalf("init hint names lint on a workspace that declares none: %q", step)
			}
		}
	}
	if len(lines) != 4 {
		t.Fatalf("gate commands = %q, want the guidance gate and one per hint", lines)
	}
	for _, line := range lines {
		code, out := runPutnami(t, dir, append(commandArgs(t, line), "--dry-run"))
		if code != cli.ExitSuccess {
			t.Fatalf("%s --dry-run exit = %d, want %d\n%s", line, code, cli.ExitSuccess, out)
		}
		if !strings.Contains(out, "2 jobs") {
			t.Fatalf("%s --dry-run planned no test and build job of app:\n%s", line, out)
		}
	}

	refused := append(commandArgs(t, "putnami "+gate), "--enforce-coverage", "--dry-run")
	if code, out := runPutnami(t, dir, refused); code != cli.ExitUsage || !strings.Contains(out, "--enforce-coverage is not declared") {
		t.Fatalf("%v exit = %d, want %d refusing the undeclared flag\n%s", refused, code, cli.ExitUsage, out)
	}
}
