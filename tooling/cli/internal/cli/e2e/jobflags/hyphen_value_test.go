// Package jobflags drives job flags through the real CLI, from the argument
// vector to the task process: the terminal adapter, extension discovery, the
// engine, the scheduler and the job context a task reads its params from. The
// parser-level rows stay in internal/cli (parse_validation_test.go and
// parse_key_stability_test.go); these swap the process streams and working
// directory, so they run in their own test binary.
package jobflags

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// runFixture writes a workspace whose one project, example, runs a `run`
// command that declares the value flags of the TypeScript extension's `run`
// (typescript/extension/putnami.extension.json). Its task copies the job context
// the CLI hands it to example/context.json, which is where a task reads its
// params from.
func runFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"run-args-fixture","includes":["extension","example"]}`)
	clitest.WriteFile(t, filepath.Join(root, "example", "putnami.json"), `{"name":"example","extensions":["@fixture/run"]}`)
	clitest.WriteFile(t, filepath.Join(root, "example", "run.project"), "")
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.json"), `{"name":"@fixture/run"}`)
	clitest.WriteFile(t, filepath.Join(root, "extension", "run.sh"), `while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    cp "$2" "$PUTNAMI_PROJECT_ROOT/context.json"
  fi
  shift
done
`)
	disabled := false
	manifest := extensionproto.Manifest{
		Name: "@fixture/run", Version: "1.0.0", CLIContract: protocolcli.CurrentContract,
		Commands: map[string]extensionproto.CommandDefinition{"run": {
			ActivationFiles: []string{"run.project"},
			Flags: map[string]extensionproto.FlagDefinition{
				"entrypoint": {Type: "string"},
				"port":       {Type: "number"},
				"args":       {Type: "string"},
			},
			Run: []extensionproto.PipelineStep{{ID: "run", Task: "run-app"}},
		}},
		Tasks: map[string]extensionproto.TaskDefinition{"run-app": {
			Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/run.sh"},
			Cache: &extensionproto.TaskCachePolicy{Enabled: &disabled}, TimeoutMs: 60000,
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.extension.json"), string(data))
	return root
}

// TestRunArgsValueThatBeginsWithAHyphenReachesTheTask drives a hyphen-leading
// `--args` value from the argument vector to the task's job context. A
// multi-word value binds in both spellings and a one-word value binds in the
// inline spelling. In each, the task receives the one string the user quoted,
// the CLI warns about no flag, and --dry-run inside the value is not the global
// preview flag.
func TestRunArgsValueThatBeginsWithAHyphenReachesTheTask(t *testing.T) {
	clitest.RequireShell(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"separate multi-word value", []string{"run", "--projects", "example", "--args", "--check --dry-run"}, "--check --dry-run"},
		{"inline multi-word value", []string{"run", "--projects", "example", "--args=--check --dry-run"}, "--check --dry-run"},
		{"inline one-word value", []string{"run", "--projects", "example", "--args=--gate"}, "--gate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
			t.Setenv("DO_NOT_TRACK", "1")
			root := runFixture(t)

			code, output := clitest.RunGateArgs(t, root, tc.args...)
			if code != 0 {
				t.Fatalf("putnami %s exited %d:\n%s", strings.Join(tc.args, " "), code, output)
			}
			if strings.Contains(output, "is not declared") {
				t.Errorf("the CLI warned about an undeclared flag:\n%s", output)
			}

			data, err := os.ReadFile(filepath.Join(root, "example", "context.json"))
			if err != nil {
				t.Fatalf("the task did not run, or received no job context: %v\n%s", err, output)
			}
			var jobContext struct {
				Params map[string]json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(data, &jobContext); err != nil {
				t.Fatalf("decode job context: %v", err)
			}
			var args string
			if err := json.Unmarshal(jobContext.Params["args"], &args); err != nil || args != tc.want {
				t.Errorf("params.args = %s, want the JSON string %q", jobContext.Params["args"], tc.want)
			}
			for _, stray := range []string{"check --dry-run", "check", "dry-run", "gate"} {
				if value, ok := jobContext.Params[stray]; ok {
					t.Errorf("params[%q] = %s: part of the value became a param of its own", stray, value)
				}
			}
		})
	}
}
