package dotnet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
)

// runTestJob runs the test job of the extension's runtime, the one main
// registers, on the project rel with params.
func runTestJob(t *testing.T, root, rel string, params job.Params) (string, map[string]any, string, error) {
	t.Helper()
	result := probe(t, root)
	var meta json.RawMessage
	for _, p := range result.Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Params: params, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	out := filepath.Join(root, ".putnami/out", strings.ReplaceAll(rel, "/", "_"), "test")
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devnull
	status, data, runErr := TestJob(stack(t))(ctx, jsonl.New(), []string{"--report-dir", out})
	os.Stdout = stdout
	return status, data, out, runErr
}

func TestTestPassesTheFilterToDotnetTestAndKeysTheTaskOnIt(t *testing.T) {
	spectest.Proves(t, feature, "filters-the-tests", "test-passes-the-filter-to-dotnet-test-and-keys-the-task-on-it")
	root := fixture(t)
	log := stubDotnet(t, "8.0.404", trxPass)
	params := job.Params{testFilterFlag: json.RawMessage(`"Calculator Adds"`)}
	status, data, _, err := runTestJob(t, root, "tests/Pricing.Tests", params)
	if status != "OK" || err != nil || data["testSummary"].(map[string]any)["passed"] != 2 {
		t.Fatalf("test: %s %v %v", status, err, data)
	}
	ran, _ := os.ReadFile(log) //nolint:gosec // the test's own log
	if !strings.Contains(string(ran), "--collect XPlat Code Coverage --filter Calculator Adds\n") {
		t.Errorf("test ran\n%s", ran)
	}

	// A project in which the filter selects no test writes no TRX report: the
	// run passes with zero tests.
	t.Setenv("STUB_NO_TRX", "1")
	status, data, _, err = runTestJob(t, root, "tests/Pricing.Tests", params)
	if status != "OK" || err != nil || data["testSummary"].(map[string]any)["total"] != 0 {
		t.Errorf("an empty selection: %s %v %v", status, err, data)
	}
	t.Setenv("STUB_NO_TRX", "")

	// The filter keys the task: the test task reads it as a parameter input.
	raw, err := os.ReadFile("putnami.extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Inputs map[string]struct {
				From string `json:"from"`
			} `json:"inputs"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if in, ok := manifest.Tasks["test-run"].Inputs[testFilterFlag]; !ok || in.From != "params" {
		t.Errorf("test-run inputs = %+v", manifest.Tasks["test-run"].Inputs)
	}
	if got := testFilter(&pctx.Context{}, stacks.RootStep{}, nil); got != nil {
		t.Errorf("no filter adds %q", got)
	}
}
