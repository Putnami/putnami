package main

import (
	"errors"
	"testing"

	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// failBiomeResolution forces resolveBiomeBinFn to error for the test.
func failBiomeResolution(t *testing.T) {
	t.Helper()
	origBin := resolveBiomeBinFn
	origConfig := resolveBiomeConfigFn
	t.Cleanup(func() {
		resolveBiomeBinFn = origBin
		resolveBiomeConfigFn = origConfig
	})
	resolveBiomeBinFn = func(_, _ string) (string, error) { return "", errors.New("biome not found") }
	resolveBiomeConfigFn = func(_, _, _ string) string { return "" }
}

// ---- runLint ----

func TestRunLint_BiomeResolutionFails(t *testing.T) {
	failBiomeResolution(t)
	ctx, _ := makeTestCtx(t)

	status, _, err := runLint(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when biome resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunLint_LintStepFails(t *testing.T) {
	mockBiomeResolution(t)

	// The combined `biome check` pass reports a lint diagnostic and exits 1.
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: `{"diagnostics":[{"severity":"error","description":"unused var","file":"src/a.ts","line":1,"column":1,"category":"lint/correctness/noUnusedVariables"}]}`}, nil
	})

	ctx, _ := makeTestCtx(t)
	status, _, err := runLint(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED when lint step fails, got %q", status)
	}
}

// ---- runLintFormat ----

func TestRunLintFormat_BiomeResolutionFails(t *testing.T) {
	failBiomeResolution(t)
	ctx, _ := makeTestCtx(t)

	status, _, err := runLintFormat(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when biome resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunLintFormat_Fails(t *testing.T) {
	mockBiomeResolution(t)

	// Non-success with diagnostics present but no stderr → ok=false, no error.
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: `{"diagnostics":[{"severity":"error","description":"bad format","file":"src/a.ts","line":1,"column":1,"category":"format"}]}`}, nil
	})

	ctx, _ := makeTestCtx(t)
	status, _, err := runLintFormat(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED when format fails, got %q", status)
	}
}

// ---- runLintCheck ----

func TestRunLintCheck_BiomeResolutionFails(t *testing.T) {
	failBiomeResolution(t)
	ctx, _ := makeTestCtx(t)

	status, _, err := runLintCheck(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when biome resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunLintCheck_Fails(t *testing.T) {
	mockBiomeResolution(t)

	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: `{"diagnostics":[{"severity":"error","description":"unused","file":"src/a.ts","line":2,"column":3,"category":"lint/correctness/noUnusedVariables"}]}`}, nil
	})

	ctx, _ := makeTestCtx(t)
	status, _, err := runLintCheck(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED when lint check fails, got %q", status)
	}
}
