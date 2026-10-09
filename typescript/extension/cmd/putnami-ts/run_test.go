package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// withMockRunBunOnce replaces the runBunOnceFunc seam so runRun's dispatch can
// be exercised without spawning a real workload, restoring it on cleanup.
func withMockRunBunOnce(t *testing.T, fn func(*jsonl.Emitter, string, string, string, int, []string) int) {
	t.Helper()
	orig := runBunOnceFunc
	t.Cleanup(func() { runBunOnceFunc = orig })
	runBunOnceFunc = fn
}

// mustLookPath resolves a binary on PATH, skipping the test if it is absent so
// the suite stays portable across environments.
func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available: %v", name, err)
	}
	return p
}

func TestResolveRunEntrypoint_Explicit(t *testing.T) {
	ep, err := resolveRunEntrypoint(t.TempDir(), "custom/start.ts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep != "custom/start.ts" {
		t.Errorf("expected explicit entrypoint, got %q", ep)
	}
}

func TestResolveRunEntrypoint_RunExport(t *testing.T) {
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"name":"x","main":"src/main.ts","exports":{"./run":"src/job.ts"}}`)
	ep, err := resolveRunEntrypoint(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep != "src/job.ts" {
		t.Errorf("expected ./run export to win, got %q", ep)
	}
}

func TestResolveRunEntrypoint_MainFallback(t *testing.T) {
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"name":"x","main":"src/entry.ts"}`)
	ep, err := resolveRunEntrypoint(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep != "src/entry.ts" {
		t.Errorf("expected main fallback, got %q", ep)
	}
}

func TestResolveRunEntrypoint_SrcMainDefault(t *testing.T) {
	dir := t.TempDir()
	writePackageJSON(t, dir, `{"name":"x"}`)
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.ts"), []byte("export {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ep, err := resolveRunEntrypoint(dir, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep != "src/main.ts" {
		t.Errorf("expected src/main.ts default, got %q", ep)
	}
}

func TestResolveRunEntrypoint_NoneFound(t *testing.T) {
	if _, err := resolveRunEntrypoint(t.TempDir(), ""); err == nil {
		t.Error("expected error when no entrypoint can be resolved")
	}
}

func writePackageJSON(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- runRun ----

func TestRunRun_NoEntrypoint(t *testing.T) {
	// With no resolvable entrypoint, runRun must fail at the resolve phase and
	// never reach the workload. It returns FAILED with a nil error (the job
	// emits a diagnostic; the workload simply never ran).
	mockBunResolution(t)
	called := false
	withMockRunBunOnce(t, func(*jsonl.Emitter, string, string, string, int, []string) int {
		called = true
		return 0
	})

	ctx, _ := makeTestCtx(t)
	status, _, err := runRun(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED for missing entrypoint, got %q", status)
	}
	if called {
		t.Error("workload must not run when the entrypoint cannot be resolved")
	}
}

func TestRunRun_SuccessForwardsExitCode(t *testing.T) {
	// A clean workload exit (0) maps to OK and carries exit-code 0 in the result
	// data the CLI forwards.
	mockBunResolution(t)
	withMockRunBunOnce(t, func(_ *jsonl.Emitter, _, _, entrypoint string, _ int, _ []string) int {
		if entrypoint != "src/start.ts" {
			t.Errorf("entrypoint = %q, want src/start.ts", entrypoint)
		}
		return 0
	})

	ctx, _ := makeTestCtx(t)
	ctx.Params = pctx.Params{"entrypoint": json.RawMessage(`"src/start.ts"`)}

	status, data, err := runRun(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
	if got := data[runExitCodeKey]; got != 0 {
		t.Errorf("exit-code data = %v, want 0", got)
	}
}

func TestRunRun_NonZeroExitIsFailedWithNilError(t *testing.T) {
	// A non-zero workload exit is reported as FAILED with a nil error so the SDK
	// keeps the result data (and the exit code the CLI forwards).
	mockBunResolution(t)
	withMockRunBunOnce(t, func(*jsonl.Emitter, string, string, string, int, []string) int {
		return 42
	})

	ctx, _ := makeTestCtx(t)
	ctx.Params = pctx.Params{"entrypoint": json.RawMessage(`"src/start.ts"`)}

	status, data, err := runRun(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("error must stay nil so the SDK keeps the exit code, got: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
	if got := data[runExitCodeKey]; got != 42 {
		t.Errorf("exit-code data = %v, want 42", got)
	}
}

func TestRunRun_PassesArgsAndPort(t *testing.T) {
	// --args is split into fields and --port is forwarded to runBunOnce.
	mockBunResolution(t)
	var gotArgs []string
	var gotPort int
	withMockRunBunOnce(t, func(_ *jsonl.Emitter, _, _, _ string, port int, extraArgs []string) int {
		gotPort = port
		gotArgs = extraArgs
		return 0
	})

	ctx, _ := makeTestCtx(t)
	ctx.Params = pctx.Params{
		"entrypoint": json.RawMessage(`"src/start.ts"`),
		"args":       json.RawMessage(`"--flag value"`),
		"port":       json.RawMessage(`8080`),
	}

	if _, _, err := runRun(ctx, jsonl.New(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPort != 8080 {
		t.Errorf("port = %d, want 8080", gotPort)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "--flag" || gotArgs[1] != "value" {
		t.Errorf("extraArgs = %v, want [--flag value]", gotArgs)
	}
}

func TestRunRun_ArgsThatBeginWithAHyphenReachTheProgram(t *testing.T) {
	// `putnami run --args "--check --dry-run"` and `--args="--check --dry-run"`
	// both deliver the one string param; the program receives its fields.
	mockBunResolution(t)
	var gotArgs []string
	withMockRunBunOnce(t, func(_ *jsonl.Emitter, _, _, _ string, _ int, extraArgs []string) int {
		gotArgs = extraArgs
		return 0
	})

	ctx, _ := makeTestCtx(t)
	ctx.Params = pctx.Params{
		"entrypoint": json.RawMessage(`"src/start.ts"`),
		"args":       json.RawMessage(`"--check --dry-run"`),
	}

	if _, _, err := runRun(ctx, jsonl.New(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "--check" || gotArgs[1] != "--dry-run" {
		t.Errorf("extraArgs = %q, want [--check --dry-run]", gotArgs)
	}
}

func TestRunRun_BunResolutionFails(t *testing.T) {
	// If the bun binary cannot be resolved, runRun returns the error directly.
	orig := resolveBunBin
	t.Cleanup(func() { resolveBunBin = orig })
	resolveBunBin = func() (string, error) { return "", os.ErrNotExist }

	ctx, _ := makeTestCtx(t)
	status, _, err := runRun(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected an error when bun cannot be resolved")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

// ---- runBunOnce ----

func TestRunBunOnce_CleanExit(t *testing.T) {
	// Drive the real subprocess supervision path with a harmless binary that
	// exits 0 — exercising pipe setup, Start, Wait, and exit-code mapping
	// without bun or docker.
	bin := mustLookPath(t, "true")
	code := runBunOnce(jsonl.New(), bin, t.TempDir(), "ignored", 0, nil)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRunBunOnce_NonZeroExit(t *testing.T) {
	// A workload that exits non-zero has its exact code forwarded.
	bin := mustLookPath(t, "false")
	code := runBunOnce(jsonl.New(), bin, t.TempDir(), "ignored", 0, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestRunBunOnce_StartFailure(t *testing.T) {
	// A bun binary path that does not exist fails at Start and maps to exit
	// code 1.
	missing := filepath.Join(t.TempDir(), "no-such-bun")
	code := runBunOnce(jsonl.New(), missing, t.TempDir(), "ignored", 0, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 for start failure", code)
	}
}
