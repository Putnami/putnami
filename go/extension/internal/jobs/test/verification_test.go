package test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/specreport"
)

// The merge's own trust rules (provenance resolution, foreign-fragment
// refusal, dedupe, wire validation) are pinned where the logic lives, in
// go.putnami.dev/sdk/extension/specreport. What stays here is the Go
// adapter's wiring: batch attribution over one shared fragment directory,
// and the whole loop through the real solo job.

// eventsMention reports whether any captured JSONL event's rendering carries
// the substring, which is enough to assert an artifact id or a warning text.
func eventsMention(events []map[string]any, substring string) bool {
	for _, event := range events {
		if strings.Contains(fmt.Sprint(event), substring) {
			return true
		}
	}
	return false
}

// TestAttachBatchVerificationKeepsPerProjectAttribution pins the batching
// half: one shared fragment directory, two members, and each member's report
// carries exactly its own project's observations at its own output path.
func TestAttachBatchVerificationKeepsPerProjectAttribution(t *testing.T) {
	workspace := t.TempDir()
	alphaRoot := filepath.Join(workspace, "alpha")
	betaRoot := filepath.Join(workspace, "beta")
	for _, root := range []string{alphaRoot, betaRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "check_test.go"), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fragments := []spectest.Fragment{
		{Feature: "go/alpha", Requirement: "holds", Check: "alpha-check", Status: "passed",
			File: filepath.Join(alphaRoot, "check_test.go"), Symbol: "TestAlpha"},
		{Feature: "go/beta", Requirement: "holds", Check: "beta-check", Status: "failed",
			File: filepath.Join(betaRoot, "check_test.go"), Symbol: "TestBeta"},
	}

	for _, member := range []struct {
		root, feature, check string
	}{
		{alphaRoot, "go/alpha", "alpha-check"},
		{betaRoot, "go/beta", "beta-check"},
	} {
		project := &batchProject{
			ref:       pctx.ProjectRef{ID: "/" + filepath.Base(member.root)},
			fullPath:  member.root,
			outputDir: filepath.Join(workspace, ".putnami", "out", filepath.Base(member.root), "test"),
			workspace: workspace,
		}
		result := batchProjectResult{ProjectID: project.ref.ID}
		attachBatchVerification(&result, fragments, project)

		if len(result.Artifacts) != 1 || result.Artifacts[0].ID != featureproto.VerificationReportArtifactID {
			t.Fatalf("%s artifacts = %+v", member.root, result.Artifacts)
		}
		if strings.HasPrefix(result.Artifacts[0].Path, "/") {
			t.Fatalf("%s artifact path %q is not workspace-relative", member.root, result.Artifacts[0].Path)
		}
		written, err := os.ReadFile(filepath.Join(project.outputDir, specreport.ReportFilename))
		if err != nil {
			t.Fatal(err)
		}
		report, findings := featureproto.ParseAndValidateVerificationReport(written)
		if report == nil || len(findings) > 0 {
			t.Fatalf("%s report invalid: %v", member.root, findings)
		}
		if len(report.Observations) != 1 || report.Observations[0].Feature != member.feature ||
			report.Observations[0].Check != member.check {
			t.Fatalf("%s observations = %+v; attribution leaked across members", member.root, report.Observations)
		}
	}
}

func TestFragmentOwnedByAnyMember(t *testing.T) {
	workspace := t.TempDir()
	ownedPath := filepath.Join(workspace, "alpha", "x_test.go")
	orphanPath := filepath.Join(workspace, "gamma", "x_test.go")
	for _, path := range []string{ownedPath, orphanPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	member := &batchProject{fullPath: filepath.Join(workspace, "alpha")}
	if !fragmentOwnedByAnyMember(spectest.Fragment{File: ownedPath}, []*batchProject{member}) {
		t.Error("an owned fragment was reported orphaned")
	}
	if fragmentOwnedByAnyMember(spectest.Fragment{File: orphanPath}, []*batchProject{member}) {
		t.Error("an orphaned fragment was claimed")
	}
}

// specTestModule writes the self-contained module fixture both end-to-end
// tests execute: a bare go.mod whose replace directives pin the protocol
// modules to this repository (local-path replaces need no go.sum entries),
// plus the job context Run receives. One copy of the layout means a new
// protocol dependency or a moved module edits one place.
func specTestModule(t *testing.T, module string) (dir string, ctx *pctx.Context) {
	t.Helper()
	dir = t.TempDir()
	outDir := t.TempDir()
	repoRoot, err := filepath.Abs("../../../../..")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "go.mod"), `module `+module+`

go 1.25.7

require go.putnami.dev/protocol/features v0.0.0

require (
	go.putnami.dev/protocol/capabilities v0.0.0 // indirect
	go.putnami.dev/protocol/diagnostic v0.0.0 // indirect
)

replace go.putnami.dev/protocol/features => `+filepath.Join(repoRoot, "protocols", "features")+`

replace go.putnami.dev/protocol/capabilities => `+filepath.Join(repoRoot, "protocols", "capabilities")+`

replace go.putnami.dev/protocol/diagnostic => `+filepath.Join(repoRoot, "protocols", "diagnostic")+`
`)
	return dir, &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outDir,
		Project:       pctx.Project{Name: module, FullPath: dir},
		Params: pctx.Params{
			"coverage": json.RawMessage(`false`),
		},
	}
}

// TestRun_EndToEndSpecVerification is the whole loop through the real solo
// job: a project whose test binds itself with spectest.Proves, executed by
// Run, must publish the reserved report artifact with the passing
// observation resolved to project-relative provenance.
func TestRun_EndToEndSpecVerification(t *testing.T) {
	dir, ctx := specTestModule(t, "specmod")
	mustWrite(t, filepath.Join(dir, "specmod.go"), "package specmod\n\nfunc Holds() bool { return true }\n")
	mustWrite(t, filepath.Join(dir, "specmod_test.go"), `package specmod

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestHolds(t *testing.T) {
	spectest.Proves(t, "gate/specmod", "holds", "specmod-check")
	if !Holds() {
		t.Fatal("does not hold")
	}
}
`)
	events := captureJobEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Run(ctx, emit, nil)
		if err != nil || status != "OK" {
			t.Errorf("Run = (%q, %v)", status, err)
		}
	})

	written, err := os.ReadFile(filepath.Join(ctx.OutputPath, specreport.ReportFilename))
	if err != nil {
		t.Fatalf("the end-to-end run published no report: %v (events %v)", err, events)
	}
	report, findings := featureproto.ParseAndValidateVerificationReport(written)
	if report == nil || len(findings) > 0 {
		t.Fatalf("published report invalid: %v", findings)
	}
	if len(report.Observations) != 1 {
		t.Fatalf("observations = %+v", report.Observations)
	}
	observation := report.Observations[0]
	if observation.Check != "specmod-check" || observation.Status != featureproto.ObservationPassed ||
		observation.Provenance.Path != "specmod_test.go" || observation.Provenance.Symbol != "TestHolds" {
		t.Fatalf("observation = %+v", observation)
	}
	if !eventsMention(events, featureproto.VerificationReportArtifactID) {
		t.Fatalf("no artifact event: %v", events)
	}
}

// TestRun_EndToEndMeasuredVerification is the threshold producer's whole loop
// through the real solo job: a test that measures a declared check with
// spectest.ObserveMeasurement, executed by Run, must publish the reserved
// report artifact with one measured observation — aggregate, honest
// invocation window, project-relative provenance, and no verdict of its own.
func TestRun_EndToEndMeasuredVerification(t *testing.T) {
	dir, ctx := specTestModule(t, "measuremod")
	mustWrite(t, filepath.Join(dir, "measuremod.go"), "package measuremod\n\nfunc Latency() float64 { return 12.5 }\n")
	mustWrite(t, filepath.Join(dir, "measuremod_test.go"), `package measuremod

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestFlushLatency(t *testing.T) {
	spectest.ObserveMeasurement(t, "gate/measuremod", "flush-latency", "flush-benchmark", spectest.Measurement{
		Name: "measuremod.flush.duration", Aggregation: "p95", Value: Latency(), Unit: "ms",
	})
	if Latency() <= 0 {
		t.Fatal("no latency was measured")
	}
}
`)

	events := captureJobEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Run(ctx, emit, nil)
		if err != nil || status != "OK" {
			t.Errorf("Run = (%q, %v)", status, err)
		}
	})

	written, err := os.ReadFile(filepath.Join(ctx.OutputPath, specreport.ReportFilename))
	if err != nil {
		t.Fatalf("the measured run published no report: %v (events %v)", err, events)
	}
	report, findings := featureproto.ParseAndValidateVerificationReport(written)
	if report == nil || len(findings) > 0 {
		t.Fatalf("published report invalid: %v", findings)
	}
	if len(report.Observations) != 1 {
		t.Fatalf("observations = %+v", report.Observations)
	}
	observation := report.Observations[0]
	if observation.Status != "" || observation.Measurement == nil ||
		observation.Measurement.Value != 12.5 || string(observation.Measurement.Aggregation) != "p95" ||
		observation.Window == nil ||
		observation.Provenance.Path != "measuremod_test.go" || observation.Provenance.Symbol != "TestFlushLatency" {
		t.Fatalf("observation = %+v", observation)
	}
}
