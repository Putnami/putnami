package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/specreport"
)

// The merge's trust rules are pinned in go.putnami.dev/sdk/extension/specreport;
// what these tests pin is the TypeScript adapter's wiring, through REAL `bun
// test` runs: the fragment directory reaches the suite, the helper's fragments
// come back, and the reserved report artifact lands per project — solo and
// batched.

// writeSpecSuite writes a project test suite whose single test binds itself to
// a declared check through the real @putnami/runtime/spectest helper, imported
// by absolute path so the scratch project needs no dependency install.
func writeSpecSuite(t *testing.T, projectDir, feature, check string) {
	t.Helper()
	repoRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	// Slash form: in a JavaScript string a Windows path's backslashes are
	// escape sequences, and bun resolves "C:/..." as the same file.
	helper := filepath.ToSlash(filepath.Join(repoRoot, "typescript", "framework", "runtime", "src", "spectest", "index.ts"))
	suite := fmt.Sprintf(`import { specTest } from '%s';

specTest('holds', { feature: '%s', requirement: 'holds', check: '%s' }, () => {});
`, helper, feature, check)
	if err := os.MkdirAll(filepath.Join(projectDir, "test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "test", "spec.test.ts"), []byte(suite), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skipWithoutSpectestOnWindows skips a real spec suite run on a Windows host
// where @putnami/spectest does not resolve. The helper the suite imports
// re-exports that workspace package, which only an install of the source
// workspace links, and contributors, who install it, stay on macOS and Linux.
func skipWithoutSpectestOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" && !spectestResolves(t) {
		t.Skip("@putnami/spectest does not resolve from @putnami/runtime: the source workspace's JavaScript dependencies are not installed")
	}
}

// spectestResolves reports whether @putnami/spectest resolves from
// @putnami/runtime the way bun resolves it: through the node_modules of each
// directory from the importing package up.
func spectestResolves(t *testing.T) bool {
	t.Helper()
	repoRoot, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	for dir := filepath.Join(repoRoot, "typescript", "framework", "runtime"); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "node_modules", "@putnami", "spectest", "package.json")); err == nil {
			return true
		}
		if dir == repoRoot || dir == filepath.Dir(dir) {
			return false
		}
	}
}

func readVerificationReport(t *testing.T, outputDir string) *featureproto.VerificationReport {
	t.Helper()
	written, err := os.ReadFile(filepath.Join(outputDir, specreport.ReportFilename))
	if err != nil {
		t.Fatalf("no verification report was published: %v", err)
	}
	report, findings := featureproto.ParseAndValidateVerificationReport(written)
	if report == nil || len(findings) > 0 {
		t.Fatalf("published report fails its own strict reader: %v", findings)
	}
	return report
}

// TestRunTest_EndToEndSpecVerification is the whole solo loop through a real
// bun run: a project whose test binds itself with specTest, executed by
// runTest, must publish the reserved report artifact with the passing
// observation resolved to project-relative provenance.
func TestRunTest_EndToEndSpecVerification(t *testing.T) {
	if _, err := resolveBunBin(); err != nil {
		t.Skipf("bun unavailable: %v", err)
	}
	skipWithoutSpectestOnWindows(t)
	ctx, root := makeTestCtx(t)
	writeSpecSuite(t, filepath.Join(root, "project"), "gate/tsmod", "tsmod-check")

	var status string
	var err error
	events := captureEvents(t, func() {
		status, _, err = runTest(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("runTest = (%q, %v); events %v", status, err, events)
	}

	report := readVerificationReport(t, ctx.OutputPath)
	if len(report.Observations) != 1 {
		t.Fatalf("observations = %+v", report.Observations)
	}
	observation := report.Observations[0]
	if observation.Feature != "gate/tsmod" || observation.Check != "tsmod-check" ||
		observation.Status != featureproto.ObservationPassed ||
		observation.Provenance.Path != "test/spec.test.ts" || observation.Provenance.Symbol != "holds" {
		t.Fatalf("observation = %+v", observation)
	}

	artifactSeen := false
	for _, event := range events {
		if event["type"] == "artifact" && event["id"] == featureproto.VerificationReportArtifactID {
			artifactSeen = true
		}
	}
	if !artifactSeen {
		t.Fatalf("no %s artifact event: %v", featureproto.VerificationReportArtifactID, events)
	}
}

// TestRunTestBatch_KeepsPerProjectSpecAttribution runs two real suites through
// the batched path and asserts each member's report carries exactly its own
// observation at its own output path, with the artifact row on the batch wire.
func TestRunTestBatch_KeepsPerProjectSpecAttribution(t *testing.T) {
	if _, err := resolveBunBin(); err != nil {
		t.Skipf("bun unavailable: %v", err)
	}
	skipWithoutSpectestOnWindows(t)
	ctx, root := makeTestBatchContext(t)
	for _, member := range []string{"a", "b"} {
		projectDir := filepath.Join(root, "packages", member)
		// Replace the placeholder empty test file with a real bound suite.
		if err := os.Remove(filepath.Join(projectDir, "foo.test.ts")); err != nil {
			t.Fatal(err)
		}
		writeSpecSuite(t, projectDir, "gate/"+member, member+"-check")
	}

	status, data, err := runTestBatch(ctx, jsonl.New())
	if err != nil || status != "OK" {
		t.Fatalf("runTestBatch = (%q, %v)", status, err)
	}
	results, ok := data["batchResults"].([]testBatchProjectResult)
	if !ok || len(results) != 2 {
		t.Fatalf("batchResults = %#v", data["batchResults"])
	}

	for index, member := range []string{"a", "b"} {
		result := results[index]
		if result.Status != "OK" {
			t.Fatalf("project %s = %+v", member, result)
		}
		outputDir := filepath.Join(root, ".putnami", "out", "packages", member, "test")
		report := readVerificationReport(t, outputDir)
		if len(report.Observations) != 1 || report.Observations[0].Feature != "gate/"+member ||
			report.Observations[0].Check != member+"-check" {
			t.Fatalf("%s observations = %+v; attribution leaked across members", member, report.Observations)
		}
		row := findBatchArtifact(result, featureproto.VerificationReportArtifactID)
		if row == nil {
			t.Fatalf("%s carries no verification artifact row: %+v", member, result.Artifacts)
		}
		if strings.HasPrefix(row.Path, "/") || !strings.Contains(row.Path, filepath.Join("packages", member)) {
			t.Fatalf("%s artifact path %q is not this member's workspace-relative report", member, row.Path)
		}
	}
}

func findBatchArtifact(result testBatchProjectResult, id string) *testBatchArtifact {
	for index := range result.Artifacts {
		if result.Artifacts[index].ID == id {
			return &result.Artifacts[index]
		}
	}
	return nil
}
