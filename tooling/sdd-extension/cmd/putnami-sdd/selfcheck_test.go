package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// TestSelfcheckReportsTheResolvedSelection is the check the whole command
// exists for: `selection` sits on the job wire, and every later SDD
// verdict reads it. A report that dropped the block would make a workspace
// whose CLI never sends one look identical to one that does.
func TestSelfcheckReportsTheResolvedSelection(t *testing.T) {
	ctx := &pctx.Context{
		ProtocolVersion: 2,
		WorkspaceRoot:   "/w",
		Project:         pctx.Project{Name: "@putnami/sdd"},
		Selection: &pctx.Selection{
			Mode:           pctx.SelectionModeImpacted,
			Scoped:         true,
			Baseline:       "origin/main",
			BaselineSource: "trunk",
			ProjectIDs:     []string{"/tooling/cli", "/tooling/sdd-extension"},
		},
	}

	report := selfcheckReport(ctx)
	if report["extension"] != extensionName {
		t.Errorf("extension = %v, want %v", report["extension"], extensionName)
	}
	if report["protocolVersion"] != 2 {
		t.Errorf("protocolVersion = %v, want 2", report["protocolVersion"])
	}
	if report["project"] != "@putnami/sdd" {
		t.Errorf("project = %v, want @putnami/sdd", report["project"])
	}

	selection, ok := report["selection"].(map[string]any)
	if !ok {
		t.Fatalf("selection = %#v, want a map", report["selection"])
	}
	if selection["mode"] != pctx.SelectionModeImpacted {
		t.Errorf("selection.mode = %v, want %v", selection["mode"], pctx.SelectionModeImpacted)
	}
	if selection["scoped"] != true {
		t.Errorf("selection.scoped = %v, want true", selection["scoped"])
	}
	if selection["baseline"] != "origin/main" {
		t.Errorf("selection.baseline = %v, want origin/main", selection["baseline"])
	}
	if selection["baselineSource"] != "trunk" {
		t.Errorf("selection.baselineSource = %v, want trunk", selection["baselineSource"])
	}
	if selection["emptyImpact"] != false {
		t.Errorf("selection.emptyImpact = %v, want false", selection["emptyImpact"])
	}
	projects, ok := selection["projects"].([]string)
	if !ok || len(projects) != 2 || projects[0] != "/tooling/cli" {
		t.Errorf("selection.projects = %#v, want the two sorted ids", selection["projects"])
	}
}

// TestSelfcheckReportsAnUnresolvedSelectionAsPresentAndNull pins the honest
// answer for a CLI that resolved nothing. `selection: null` is a fact a
// consumer must act on — it may NOT assume the run was unscoped — and an
// omitted key is indistinguishable from a report that forgot to look.
func TestSelfcheckReportsAnUnresolvedSelectionAsPresentAndNull(t *testing.T) {
	report := selfcheckReport(&pctx.Context{ProtocolVersion: 1, WorkspaceRoot: "/w"})

	value, present := report["selection"]
	if !present {
		t.Fatal("the selection key is absent; a consumer cannot tell that from a report that never looked")
	}
	if value != nil {
		t.Fatalf("selection = %#v, want nil", value)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if got := string(decoded["selection"]); got != "null" {
		t.Errorf("encoded selection = %s, want null", got)
	}
}

// TestSelfcheckToleratesAMissingContext keeps the report total. A nil context
// is not something the SDK hands a job today, but a report that panicked on one
// would turn a missing-context bug into a crash with no diagnosis.
func TestSelfcheckToleratesAMissingContext(t *testing.T) {
	report := selfcheckReport(nil)
	if report["extension"] != extensionName {
		t.Errorf("extension = %v, want %v", report["extension"], extensionName)
	}
	if report["selection"] != nil {
		t.Errorf("selection = %#v, want nil", report["selection"])
	}
	if got := describeSelection(nil); got == "" {
		t.Error("describeSelection(nil) is empty; the human line must still say something")
	}
}

// TestSelfcheckSucceedsAndSaysWhatItSaw runs the job body the way the SDK
// runner does, so the status contract, the structured data and the human line
// on the JSONL stream are all covered.
func TestSelfcheckSucceedsAndSaysWhatItSaw(t *testing.T) {
	ctx := &pctx.Context{
		ProtocolVersion: 2,
		WorkspaceRoot:   "/w",
		Selection:       &pctx.Selection{Mode: pctx.SelectionModeAll},
	}

	var status string
	var data map[string]any
	var err error
	stream := captureStdout(t, func() {
		status, data, err = runSelfcheck(ctx, jsonl.New(), nil)
	})
	if err != nil {
		t.Fatalf("selfcheck: %v", err)
	}
	if status != "OK" {
		t.Errorf("status = %q, want OK", status)
	}
	if data["workspaceRoot"] != "/w" {
		t.Errorf("workspaceRoot = %v, want /w", data["workspaceRoot"])
	}
	if !strings.Contains(stream, "selection mode=all") {
		t.Errorf("JSONL stream %q does not report the selection it saw", stream)
	}
	if got, want := describeSelection(ctx), "selection mode=all"; got != want {
		t.Errorf("human line = %q, want %q", got, want)
	}
}

// TestDescribeSelectionNamesTheBaseline covers the `--impacted` shape, where
// the ref the run resolved to is the part a reader needs: two runs with the
// same mode and different baselines answered different questions.
func TestDescribeSelectionNamesTheBaseline(t *testing.T) {
	ctx := &pctx.Context{Selection: &pctx.Selection{
		Mode:     pctx.SelectionModeImpacted,
		Scoped:   true,
		Baseline: "origin/main",
	}}
	if got, want := describeSelection(ctx), "selection mode=impacted baseline=origin/main"; got != want {
		t.Errorf("human line = %q, want %q", got, want)
	}
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what was
// written. The emitter binds to the current os.Stdout at emit time, which is
// what makes this possible without threading a writer through the job
// signature the SDK owns.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = write
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(read)
		done <- string(out)
	}()

	fn()

	os.Stdout = saved
	if err := write.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out := <-done
	if err := read.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	return out
}
