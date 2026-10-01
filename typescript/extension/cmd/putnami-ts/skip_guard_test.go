package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

func writeTestSource(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cleanBiome(t *testing.T) {
	t.Helper()
	mockBiomeResolution(t)
	mockAllExec(t, func(string, []string, ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"diagnostics":[]}`}, nil
	})
}

func TestRunLintRefusesAFocusedTest(t *testing.T) {
	cleanBiome(t)
	ctx, root := makeTestCtx(t)
	writeTestSource(t, filepath.Join(root, "project", "test", "a.test.ts"), "test.only('a', () => {});\n")

	if status, _, err := runLint(ctx, jsonl.New(), nil); err != nil || status != "FAILED" {
		t.Fatalf("runLint = %q, %v; want FAILED for a focused test", status, err)
	}
	if status, _, err := runLintCheck(ctx, jsonl.New(), nil); err != nil || status != "FAILED" {
		t.Fatalf("runLintCheck = %q, %v; want FAILED for a focused test", status, err)
	}
}

func TestRunLintSkipGuardTurnsOff(t *testing.T) {
	cleanBiome(t)
	ctx, root := makeTestCtx(t)
	writeTestSource(t, filepath.Join(root, "project", "test", "a.test.ts"), "test.only('a', () => {});\n")
	ctx.Params["skip-guard"] = []byte("false")

	if status, _, err := runLint(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runLint = %q, %v; want OK with the skip guard off", status, err)
	}
	if status, _, err := runLintCheck(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runLintCheck = %q, %v; want OK with the skip guard off", status, err)
	}
}

func TestRunLintKeepsAReviewedException(t *testing.T) {
	cleanBiome(t)
	ctx, root := makeTestCtx(t)
	writeTestSource(t, filepath.Join(root, "project", "test", "a.test.ts"),
		"// putnami:allow-skip proves skip registration\ntest.skip('a', () => {});\n")

	if status, _, err := runLint(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runLint = %q, %v; want OK for a reviewed exception", status, err)
	}
}

func TestRunLintBatchAddsSkipGuardFindings(t *testing.T) {
	cleanBiome(t)
	ctx, root := makeLintBatchContext(t)
	writeTestSource(t, filepath.Join(root, "packages", "a", "test", "a.test.ts"), "describe.skip('a', () => {});\n")
	writeTestSource(t, filepath.Join(root, "packages", "b", "test", "b.test.ts"),
		"// putnami:allow-skip proves skip registration\ntest.skip('b', () => {});\n")

	for _, mode := range []string{lintBatchCombined, lintBatchCheck} {
		_, data, err := runLintBatch(ctx, mode)
		if err != nil {
			t.Fatalf("%s: runLintBatch: %v", mode, err)
		}
		results := data["batchResults"].([]lintBatchProjectResult)
		a, b := results[0], results[1]
		if a.Status != "FAILED" || a.Summary.Errors != 1 || len(a.Diagnostics) != 1 {
			t.Fatalf("%s: project a = %+v, want one skip guard error", mode, a)
		}
		if got := a.Diagnostics[0]; got.File != "packages/a/test/a.test.ts" || got.Line != 1 || got.Category != "skip-guard" ||
			!strings.Contains(got.Description, "`describe.skip` skips unconditionally") {
			t.Fatalf("%s: project a diagnostic = %+v", mode, got)
		}
		if b.Status != "OK" || b.Summary.Warnings != 1 || b.Summary.Errors != 0 {
			t.Fatalf("%s: project b = %+v, want the reviewed exception as a warning", mode, b)
		}
	}

	_, data, err := runLintBatch(ctx, lintBatchFormat)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range data["batchResults"].([]lintBatchProjectResult) {
		if result.Status != "OK" || len(result.Diagnostics) != 0 {
			t.Fatalf("the format pass does not run the skip guard: %+v", result)
		}
	}

	ctx.Params["skip-guard"] = []byte("false")
	_, data, err = runLintBatch(ctx, lintBatchCombined)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range data["batchResults"].([]lintBatchProjectResult) {
		if result.Status != "OK" || len(result.Diagnostics) != 0 {
			t.Fatalf("result = %+v, want a clean result with the skip guard off", result)
		}
	}
}

func TestSkipGuardDiagnosticsReportsAnUnreadableProject(t *testing.T) {
	diagnostics := skipGuardDiagnostics(t.TempDir(), filepath.Join(t.TempDir(), "missing"))
	if len(diagnostics) != 1 || diagnostics[0].Severity != "error" ||
		!strings.Contains(diagnostics[0].Description, "skip guard could not read the project") {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
}

func TestRunSkipGuardEmitsLintMetrics(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	writeTestSource(t, filepath.Join(project, "a.test.ts"),
		"test.only('a', () => {});\n// putnami:allow-skip proves skip registration\ntest.skip('b', () => {});\n")

	var failures, warnings int
	metrics := emittedMetrics(t, func() {
		failures, warnings = runSkipGuard(jsonl.New(), root, project)
	})
	if failures != 1 || warnings != 1 {
		t.Fatalf("runSkipGuard = %d errors, %d warnings; want 1 and 1", failures, warnings)
	}
	if metrics["lint-errors"] != 1 || metrics["lint-warnings"] != 1 {
		t.Fatalf("metrics = %v, want lint-errors 1 and lint-warnings 1", metrics)
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

// TestBuiltInBiomeConfigLeavesSkipsToTheSkipGuard pins that Biome's
// noSkippedTests stays off: its fix removes `.skip` when lint writes, which
// would undo a platform guard or a reviewed exception the skip guard allows.
func TestBuiltInBiomeConfigLeavesSkipsToTheSkipGuard(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "biome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Linter struct {
			Rules struct {
				Suspicious map[string]any `json:"suspicious"`
			} `json:"rules"`
		} `json:"linter"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if got := config.Linter.Rules.Suspicious["noSkippedTests"]; got != "off" {
		t.Fatalf("noSkippedTests = %v, want off", got)
	}
}
