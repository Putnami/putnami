package jobflags

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// shortAliasFixture writes a workspace whose one project, example, runs a
// `probe` command that declares --filter with the short alias -f, the shape of
// the language extensions' `test --test`/`-t`. The command group `suite probe`
// routes to the same command. Its task declares the filter param as an input,
// appends one line to $PROBE_OUT/runs.log each time it runs and copies its job
// context to $PROBE_OUT/context.json.
func shortAliasFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"short-alias-fixture","includes":["extension","example"]}`)
	clitest.WriteFile(t, filepath.Join(root, "example", "putnami.json"), `{"name":"example","extensions":["@fixture/probe"]}`)
	clitest.WriteFile(t, filepath.Join(root, "example", "probe.project"), "")
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.json"), `{"name":"@fixture/probe"}`)
	clitest.WriteFile(t, filepath.Join(root, "extension", "probe.sh"), `echo ran >> "$PROBE_OUT/runs.log"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    cp "$2" "$PROBE_OUT/context.json"
  fi
  shift
done
`)
	disabled := false
	manifest := extensionproto.Manifest{
		Name: "@fixture/probe", Version: "1.0.0", CLIContract: protocolcli.CurrentContract,
		CommandGroups: map[string]extensionproto.CommandGroupDefinition{"suite": {
			Subcommands: map[string]extensionproto.SubcommandDefinition{"probe": {Command: "probe"}},
		}},
		Commands: map[string]extensionproto.CommandDefinition{"probe": {
			ActivationFiles: []string{"probe.project"},
			Flags: map[string]extensionproto.FlagDefinition{
				"filter": {Type: "string", Short: "-f"},
			},
			Run: []extensionproto.PipelineStep{{ID: "probe", Task: "probe-run"}},
		}},
		Tasks: map[string]extensionproto.TaskDefinition{"probe-run": {
			Kind: "command", Command: "/bin/sh", Args: []string{"{extensionRoot}/probe.sh"},
			Inputs: map[string]extensionproto.TaskInputPort{"filter": {From: "params"}},
			Cache:  &extensionproto.TaskCachePolicy{Enabled: &disabled}, TimeoutMs: -1,
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.extension.json"), string(data))
	clitest.InitGitRepo(t, root)
	t.Setenv("PROBE_OUT", t.TempDir())
	return root
}

// probeParams reads the params of the job context the task last received.
func probeParams(t *testing.T, root, output string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("PROBE_OUT"), "context.json"))
	if err != nil {
		t.Fatalf("the task did not run, or received no job context: %v\n%s", err, output)
	}
	var jobContext struct {
		Params map[string]json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &jobContext); err != nil {
		t.Fatalf("decode job context: %v", err)
	}
	return jobContext.Params
}

func probeRuns(t *testing.T, root string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("PROBE_OUT"), "runs.log"))
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "ran\n")
}

// `probe -f x` reaches the task as `filter: x`, never as `f`, with the params
// `--filter x` gives: the task, its params inputs and its cache key read one
// map, so they cannot tell the spellings apart. A flag spelled both ways is a
// usage error that runs nothing.
func TestAJobShortAliasReachesTheTaskUnderItsLongName(t *testing.T) {
	clitest.RequireShell(t)
	spectest.Proves(t, "cli/job-planning-execution", "a-short-alias-reaches-the-task-under-its-long-name", "a-job-short-alias-binds-the-long-name")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("DO_NOT_TRACK", "1")
	root := shortAliasFixture(t)

	run := func(args ...string) {
		t.Helper()
		code, output := clitest.RunGateArgs(t, root, args...)
		if code != 0 {
			t.Fatalf("putnami %s exited %d:\n%s", strings.Join(args, " "), code, output)
		}
	}
	run("probe", "--projects", "example", "-f", "Unit")
	params := probeParams(t, root, "")
	if string(params["filter"]) != `"Unit"` {
		t.Errorf("params.filter = %s, want \"Unit\"", params["filter"])
	}
	if value, kept := params["f"]; kept {
		t.Errorf("params.f = %s: the short name reached the task", value)
	}

	run("probe", "--projects", "example", "--filter", "Unit")
	if long := probeParams(t, root, ""); string(long["filter"]) != `"Unit"` || len(long) != len(params) {
		t.Errorf("--filter Unit params = %v, want those -f Unit gave: %v", long, params)
	}

	code, output := clitest.RunGateArgs(t, root, "probe", "--projects", "example", "-f", "a", "--filter", "b")
	if code != 2 || !strings.Contains(output, "flags -f and --filter name the same flag") {
		t.Errorf("both spellings exited %d, want 2 with the refusal:\n%s", code, output)
	}
	if runs := probeRuns(t, root); runs != 2 {
		t.Errorf("the task ran %d times, want 2: a refused invocation runs nothing", runs)
	}
}

// An extension subcommand resolves its short aliases against its effective
// flags: `suite probe -f x` reaches the task as `filter: x`, and spelling the
// flag both ways is refused.
func TestAnExtensionSubcommandShortAliasReachesTheTaskUnderItsLongName(t *testing.T) {
	clitest.RequireShell(t)
	spectest.Proves(t, "cli/job-planning-execution", "a-short-alias-reaches-the-task-under-its-long-name", "an-extension-subcommand-short-alias-binds-the-long-name")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
	t.Setenv("DO_NOT_TRACK", "1")
	root := shortAliasFixture(t)

	code, output := clitest.RunGateArgs(t, root, "suite", "probe", "example", "-f", "Edge")
	if code != 0 {
		t.Fatalf("putnami suite probe exited %d:\n%s", code, output)
	}
	params := probeParams(t, root, output)
	if string(params["filter"]) != `"Edge"` {
		t.Errorf("params.filter = %s, want \"Edge\"", params["filter"])
	}
	if value, kept := params["f"]; kept {
		t.Errorf("params.f = %s: the short name reached the task", value)
	}

	code, output = clitest.RunGateArgs(t, root, "suite", "probe", "example", "-f", "a", "--filter", "b")
	if code != 2 || !strings.Contains(output, "flags -f and --filter name the same flag") {
		t.Errorf("both spellings exited %d, want 2 with the refusal:\n%s", code, output)
	}
}
