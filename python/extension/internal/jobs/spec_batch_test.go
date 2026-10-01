package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/specreport"
)

// The merge's trust rules are pinned in extension-sdk/specreport and the
// plugin's verdict capture against real pytest in spec_plugin_test.go. What
// this test pins is the batch wiring: each member's pytest process receives
// its OWN fragment directory, the fragments it writes come back as that
// member's report at that member's output path, and the artifact row rides
// the batch wire.

// specEnvValue resolves key the way the child process would: the LAST entry
// wins. Under the real gate the extension's own Go test process already
// carries PUTNAMI_SPEC_FRAGMENTS (the Go adapter provisioned it for these
// very tests), and the per-project value this batch appends must shadow it.
func specEnvValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			value = strings.TrimPrefix(entry, key+"=")
		}
	}
	return value
}

func TestTestBatchMergesSpecFragmentsPerProject(t *testing.T) {
	stubSync(t)
	ctx := makeTestBatchWorkspace(t)

	// The fake pytest run behaves like a suite whose one bound test passed:
	// it writes a fragment into the directory the adapter provisioned, with
	// the declaration inside the member's own project root.
	orig := pytestCommandRunner
	t.Cleanup(func() { pytestCommandRunner = orig })
	pytestCommandRunner = func(_ context.Context, _ string, args []string, dir string, env []string) (string, error) {
		fragmentsDir := specEnvValue(env, spectest.FragmentDirEnv)
		if fragmentsDir == "" {
			t.Error("the pytest process received no fragment directory")
			return "1 passed", nil
		}
		pluginActivated := false
		for index, arg := range args {
			if arg == "-p" && index+1 < len(args) && args[index+1] == "putnami_spectest" {
				pluginActivated = true
			}
		}
		if !pluginActivated {
			t.Errorf("pytest args %v do not activate the spec plugin", args)
		}
		if !strings.Contains(specEnvValue(env, "PYTHONPATH"), string(os.PathListSeparator)) {
			t.Errorf("PYTHONPATH %q does not carry the plugin directory", specEnvValue(env, "PYTHONPATH"))
		}
		member := filepath.Base(dir)
		fragment := spectest.Fragment{
			Feature: "py/" + member, Requirement: "holds", Check: member + "-check",
			Status: "passed", File: filepath.Join(dir, "tests", "test_x.py"), Symbol: "test_x",
		}
		encoded, err := json.Marshal(fragment)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(fragmentsDir, "fragment.json"), encoded, 0o644); err != nil {
			return "", err
		}
		return "1 passed", nil
	}

	status, data, err := TestBatch(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("TestBatch = (%q, %v)", status, err)
	}
	results, ok := data["batchResults"].([]pyTestBatchProjectResult)
	if !ok || len(results) != 2 {
		t.Fatalf("batchResults = %#v", data["batchResults"])
	}

	for index, member := range []string{"a", "b"} {
		result := results[index]
		if result.Status != "OK" {
			t.Fatalf("project %s = %+v", member, result)
		}
		outputDir := filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", member, "test")
		written, err := os.ReadFile(filepath.Join(outputDir, specreport.ReportFilename))
		if err != nil {
			t.Fatalf("%s report missing: %v", member, err)
		}
		report, findings := featureproto.ParseAndValidateVerificationReport(written)
		if report == nil || len(findings) > 0 {
			t.Fatalf("%s report invalid: %v", member, findings)
		}
		if len(report.Observations) != 1 || report.Observations[0].Feature != "py/"+member ||
			report.Observations[0].Check != member+"-check" ||
			report.Observations[0].Provenance.Path != "tests/test_x.py" {
			t.Fatalf("%s observations = %+v; attribution leaked across members", member, report.Observations)
		}
		if len(result.Artifacts) != 1 || result.Artifacts[0].ID != featureproto.VerificationReportArtifactID {
			t.Fatalf("%s artifacts = %+v", member, result.Artifacts)
		}
		if strings.HasPrefix(result.Artifacts[0].Path, "/") {
			t.Fatalf("%s artifact path %q is not workspace-relative", member, result.Artifacts[0].Path)
		}
	}
}

// TestTestBatchWithoutFragmentsWritesNoReport pins the optionalEmpty half: a
// suite that bound nothing produces no report file and no artifact row.
func TestTestBatchWithoutFragmentsWritesNoReport(t *testing.T) {
	stubSync(t)
	ctx := makeTestBatchWorkspace(t)
	stubRunner(t, func(_ context.Context, _ string, _, _ []string) (string, error) {
		return "1 passed", nil
	})

	status, data, err := TestBatch(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("TestBatch = (%q, %v)", status, err)
	}
	results := data["batchResults"].([]pyTestBatchProjectResult)
	for index, member := range []string{"a", "b"} {
		if len(results[index].Artifacts) != 0 {
			t.Fatalf("%s artifacts = %+v, want none", member, results[index].Artifacts)
		}
		reportPath := filepath.Join(ctx.WorkspaceRoot, ".putnami", "out", member, "test", specreport.ReportFilename)
		if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
			t.Fatalf("%s: an observation-free run wrote a report (stat err = %v)", member, err)
		}
	}
}
