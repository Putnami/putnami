package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// verdictPolicyJobContext is the context of a workspace with one member, app.
// A non-empty policy is written in app's directory.
func verdictPolicyJobContext(t *testing.T, policy string) *pctx.Context {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app"}`)
	if policy != "" {
		writeFixture(t, filepath.Join(root, "app", "putnami.verdict-policy.json"), policy)
	}
	return workspaceJobContext(root, []pctx.ProjectRef{{
		ID: "/app", Name: "app", Path: "app", FullPath: filepath.Join(root, "app"), Config: json.RawMessage(`{"name":"app"}`),
	}})
}

// A valid file in a member's directory passes, and the summary counts it.
func TestVerdictPolicyValidatePassesAValidFile(t *testing.T) {
	outcome := runJob(t, runVerdictPolicyValidate, verdictPolicyJobContext(t, `{"gaps":{"no-requirement":"inform"}}`))
	if outcome.err != nil || outcome.status != "OK" {
		t.Fatalf("status = %q err = %v, want a valid policy to pass", outcome.status, outcome.err)
	}
	if want := "verdict policy: 1 file(s), 0 diagnostic(s)"; !strings.Contains(outcome.summaryLine(), want) {
		t.Errorf("summary line = %q, want one containing %q", outcome.summaryLine(), want)
	}
}

// A defect fails the job with a usage exit, and the finding names the file and
// the member a reader has to open.
func TestVerdictPolicyValidateFailsNamingTheFile(t *testing.T) {
	outcome := runJob(t, runVerdictPolicyValidate, verdictPolicyJobContext(t, `{"gaps":{"no-requirement":"ignore"}}`))
	if outcome.err == nil || outcome.exitCode() != protocolcli.ExitUsage {
		t.Fatalf("error = %v (exit %d), want an unknown value to fail with %d", outcome.err, outcome.exitCode(), protocolcli.ExitUsage)
	}
	event := findDiagnosticEvent(outcome.diagnostics(), "ci.verdict_policy_value")
	if event == nil || event.Location == nil || !strings.HasPrefix(event.Location.File, "app/putnami.verdict-policy.json") {
		t.Fatalf("diagnostic events = %+v, want ci.verdict_policy_value naming app/putnami.verdict-policy.json", outcome.diagnostics())
	}
	if len(outcome.diagnostics()) != 1 {
		t.Errorf("diagnostic events = %+v, want one", outcome.diagnostics())
	}
	if policies, _ := outcome.data["policies"].([]any); len(policies) != 1 {
		t.Errorf("data = %+v, want the one policy file listed", outcome.data)
	}
}

// A file outside the root, a scope and a project fails with the step's own
// code.
func TestVerdictPolicyValidateRefusesAMisplacedFile(t *testing.T) {
	ctx := verdictPolicyJobContext(t, "")
	writeFixture(t, filepath.Join(ctx.WorkspaceRoot, "docs", "putnami.verdict-policy.json"), `{}`)
	outcome := runJob(t, runVerdictPolicyValidate, ctx)
	if outcome.err == nil || outcome.exitCode() != protocolcli.ExitUsage {
		t.Fatalf("error = %v (exit %d), want a misplaced file to fail with %d", outcome.err, outcome.exitCode(), protocolcli.ExitUsage)
	}
	event := findDiagnosticEvent(outcome.diagnostics(), sdd.ErrorCodeVerdictPolicyLocation)
	if event == nil || event.Location == nil || event.Location.File != "docs/putnami.verdict-policy.json" {
		t.Fatalf("diagnostic events = %+v, want %s naming docs/putnami.verdict-policy.json", outcome.diagnostics(), sdd.ErrorCodeVerdictPolicyLocation)
	}
}

func TestVerdictPolicyValidateRefusesAContextWithoutWorkspaceMembership(t *testing.T) {
	ctx := verdictPolicyJobContext(t, "")
	ctx.WorkspaceProjects = nil
	if _, _, err := runVerdictPolicyValidate(ctx, jsonl.New(), nil); err == nil {
		t.Fatal("verdict-policy-validate accepted a context without the workspace membership")
	}
}
