package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// featureCoverageJobContext is the context of a workspace that declares the
// options.sdd block given, with two members: /lib, whose putnami.json declares
// featureAuthority.none, and /app, which declares nothing.
func featureCoverageJobContext(t *testing.T, sddOptions string) *pctx.Context {
	t.Helper()
	root := t.TempDir()
	ctx := workspaceJobContext(root, []pctx.ProjectRef{
		{ID: "/app", Name: "app", Path: "app", FullPath: filepath.Join(root, "app"), Config: json.RawMessage(`{"name":"app"}`)},
		{
			ID: "/lib", Name: "lib", Path: "lib", FullPath: filepath.Join(root, "lib"),
			Config: json.RawMessage(`{"name":"lib","featureAuthority":{"none":"a wire contract is not a user outcome"}}`),
		},
	})
	ctx.Workspace.Options = map[string]json.RawMessage{"sdd": json.RawMessage(sddOptions)}
	return ctx
}

// The debt list comes from the workspace option block the job context
// carries, and each featureAuthority from its member's config on the wire: an
// uncovered member fails the job with one diagnostic, and listing it passes.
func TestFeatureCoverageValidateReadsTheWire(t *testing.T) {
	outcome := runJob(t, runFeatureCoverageValidate, featureCoverageJobContext(t, `{"featureCoverage":{"debt":[]}}`))
	if outcome.err == nil || outcome.exitCode() != protocolcli.ExitUsage {
		t.Fatalf("error = %v (exit %d), want an uncovered /app to fail with %d", outcome.err, outcome.exitCode(), protocolcli.ExitUsage)
	}
	event := findDiagnosticEvent(outcome.diagnostics(), sdd.ErrorCodeUncoveredProject)
	if event == nil || event.Location == nil || !strings.HasPrefix(event.Location.File, "app/putnami.json") {
		t.Fatalf("diagnostic events = %+v, want %s naming app/putnami.json", outcome.diagnostics(), sdd.ErrorCodeUncoveredProject)
	}
	if len(outcome.diagnostics()) != 1 {
		t.Errorf("diagnostic events = %+v, want only /app reported", outcome.diagnostics())
	}

	outcome = runJob(t, runFeatureCoverageValidate, featureCoverageJobContext(t, `{"featureCoverage":{"debt":["/app"]}}`))
	if outcome.err != nil || outcome.status != "OK" {
		t.Fatalf("status = %q err = %v, want a listed debt to pass", outcome.status, outcome.err)
	}
	if want := "feature coverage: 2 project(s), 0 with a feature or spec, 1 with featureAuthority, 1 in debt, 0 finding(s)"; !strings.Contains(outcome.summaryLine(), want) {
		t.Errorf("summary line = %q, want one containing %q", outcome.summaryLine(), want)
	}
}

// A workspace without the option passes and says how to adopt the gate.
func TestFeatureCoverageValidatePassesWithANoteWhenNotAdopted(t *testing.T) {
	outcome := runJob(t, runFeatureCoverageValidate, featureCoverageJobContext(t, `{}`))
	if outcome.err != nil || outcome.exitCode() != protocolcli.ExitSuccess {
		t.Fatalf("outcome = %v (exit %d), want a pass", outcome.err, outcome.exitCode())
	}
	if want := "feature coverage: not adopted (no options.sdd.featureCoverage), 1 project(s)"; !strings.Contains(outcome.summaryLine(), want) {
		t.Errorf("summary line = %q, want one containing %q", outcome.summaryLine(), want)
	}
}
