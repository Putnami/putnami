package lint

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

const unguardedSkipTest = "package sample\n\nimport \"testing\"\n\nfunc TestLater(t *testing.T) {\n\tt.Skip(\"later\")\n}\n"

const reviewedSkipTest = "package sample\n\nimport \"testing\"\n\nfunc TestLater(t *testing.T) {\n\t//putnami:allow-skip the harness drives it\n\tt.Skip(\"later\")\n}\n"

// skipGuardBatchWorkspace holds two modules: one with an unguarded skip, one
// with a reviewed exception.
func skipGuardBatchWorkspace(t *testing.T) (string, []pctx.ProjectRef) {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse (\n\t./a\n\t./b\n)\n")
	for _, name := range []string{"a", "b"} {
		mustWrite(t, filepath.Join(root, name, "go.mod"), "module example.com/"+name+"\n\ngo 1.26\n")
	}
	mustWrite(t, filepath.Join(root, "a", "later_test.go"), unguardedSkipTest)
	mustWrite(t, filepath.Join(root, "b", "later_test.go"), reviewedSkipTest)
	return root, []pctx.ProjectRef{
		{ID: "/a", Name: "a", Path: "a", FullPath: filepath.Join(root, "a")},
		{ID: "/b", Name: "b", Path: "b", FullPath: filepath.Join(root, "b")},
	}
}

func TestRunGolangciBatchAddsSkipGuardFindings(t *testing.T) {
	root, projects := skipGuardBatchWorkspace(t)
	stubGolangci(t, func(recordedInvocation) ([]byte, error) { return []byte("0 issues.\n"), nil })
	ctx := &pctx.Context{
		WorkspaceRoot:    root,
		Job:              pctx.Job{Name: "lint"},
		Params:           pctx.Params{"fix": json.RawMessage("false")},
		SelectedProjects: projects,
	}

	status, data, err := runGolangciBatch(ctx, parseLintOptions(ctx.Params, []string{"--tool", "golangci-lint"}))
	if err != nil || status != "OK" {
		t.Fatalf("runGolangciBatch = %q, %v", status, err)
	}
	results := data["batchResults"].([]batchProjectResult)

	a := resultByID(t, results, "/a")
	if a.Status != "FAILED" || a.Summary.Errors != 1 || len(a.Diagnostics) != 1 {
		t.Fatalf("a result = %+v, want one skip guard error", a)
	}
	if got := a.Diagnostics[0]; got.File != "a/later_test.go" || got.Line != 6 || got.Category != "skip-guard" ||
		!strings.Contains(got.Description, "t.Skip runs unconditionally") {
		t.Fatalf("a diagnostic = %+v", got)
	}
	if errors := a.Data["lintSummary"].(map[string]any)["errors"]; errors != 1 {
		t.Fatalf("a lintSummary errors = %v, want 1", errors)
	}

	b := resultByID(t, results, "/b")
	if b.Status != "OK" || b.Summary.Warnings != 1 || b.Summary.Errors != 0 ||
		!strings.Contains(b.Diagnostics[0].Description, "reviewed exception: t.Skip stays because the harness drives it") {
		t.Fatalf("b result = %+v, want the reviewed exception as a warning", b)
	}
}

func TestRunGolangciBatchSkipGuardTurnsOff(t *testing.T) {
	root, projects := skipGuardBatchWorkspace(t)
	stubGolangci(t, func(recordedInvocation) ([]byte, error) { return []byte("0 issues.\n"), nil })
	ctx := &pctx.Context{
		WorkspaceRoot:    root,
		Job:              pctx.Job{Name: "lint"},
		Params:           pctx.Params{"fix": json.RawMessage("false"), "skip-guard": json.RawMessage("false")},
		SelectedProjects: projects,
	}

	_, data, err := runGolangciBatch(ctx, parseLintOptions(ctx.Params, []string{"--tool", "golangci-lint"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range data["batchResults"].([]batchProjectResult) {
		if result.Status != "OK" || len(result.Diagnostics) != 0 {
			t.Fatalf("result = %+v, want a clean result with the skip guard off", result)
		}
	}
}

func TestRunSkipGuardCountsErrorsAndFiles(t *testing.T) {
	root, _ := skipGuardBatchWorkspace(t)
	mustWrite(t, filepath.Join(root, "a", "other_test.go"), unguardedSkipTest)

	var failures, files int
	metrics := emittedMetrics(t, func() {
		failures, files = runSkipGuard(root, filepath.Join(root, "a"), jsonl.New())
	})
	if failures != 2 || files != 2 {
		t.Fatalf("runSkipGuard = %d errors in %d files, want 2 in 2", failures, files)
	}
	if metrics["lint-errors"] != 2 {
		t.Fatalf("metrics = %v, want lint-errors 2", metrics)
	}
	if failures, files := runSkipGuard(root, filepath.Join(root, "b"), jsonl.New()); failures != 0 || files != 0 {
		t.Fatalf("a reviewed exception is not an error: %d errors in %d files", failures, files)
	}
}

func TestSkipGuardDiagnosticsReportsAnUnreadableProject(t *testing.T) {
	diagnostics := skipGuardDiagnostics(t.TempDir(), filepath.Join(t.TempDir(), "missing"))
	if len(diagnostics) != 1 || diagnostics[0].Severity != "error" ||
		!strings.Contains(diagnostics[0].Description, "skip guard could not read the project") {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
}

// emittedMetrics runs fn with stdout captured and sums the metric events it
// emits by name.
func emittedMetrics(t *testing.T, fn func()) map[string]float64 {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	output := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(reader)
		output <- data
	}()
	fn()
	os.Stdout = original
	_ = writer.Close()
	metrics := make(map[string]float64)
	scanner := bufio.NewScanner(strings.NewReader(string(<-output)))
	for scanner.Scan() {
		var event struct {
			Type  string  `json:"type"`
			Name  string  `json:"name"`
			Value float64 `json:"value"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "metric" {
			metrics[event.Name] += event.Value
		}
	}
	return metrics
}
