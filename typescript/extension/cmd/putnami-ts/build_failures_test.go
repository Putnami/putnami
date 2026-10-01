package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/errs"
)

// failBunResolution forces resolveBunBin to error for the duration of a test.
func failBunResolution(t *testing.T) {
	t.Helper()
	orig := resolveBunBin
	t.Cleanup(func() { resolveBunBin = orig })
	resolveBunBin = func() (string, error) { return "", errors.New("bun not found") }
}

// setupTranspileProject creates a project whose package.json exposes a real
// entrypoint, so build.RunTranspile resolves work to do instead of no-op'ing.
func setupTranspileProject(t *testing.T) *pctx.Context {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","exports":{"./index":"./src/index.ts"}}`), 0644)
	return ctx
}

// ---- runBuild: bun resolution ----

func TestRunBuild_BunResolutionFails(t *testing.T) {
	failBunResolution(t)
	ctx, _ := makeTestCtx(t)
	ctx.Params = pctx.Params{"transpile": json.RawMessage(`true`)}

	status, _, err := runBuild(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when bun resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunBuild_TranspileFails(t *testing.T) {
	mockBunResolution(t)
	ctx := setupTranspileProject(t)
	ctx.Params = pctx.Params{"transpile": json.RawMessage(`true`)}

	// A non-success exec turns into a transpile error diagnostic → FAILED.
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "syntax error"}, nil
	})

	status, _, err := runBuild(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED when transpile reports errors, got %q", status)
	}
}

// ---- runBuildTranspile ----

func TestRunBuildTranspile_BunResolutionFails(t *testing.T) {
	failBunResolution(t)
	ctx, _ := makeTestCtx(t)

	status, _, err := runBuildTranspile(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when bun resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunBuildTranspile_TranspileErrors(t *testing.T) {
	mockBunResolution(t)
	ctx := setupTranspileProject(t)

	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "bundle failed"}, nil
	})

	status, _, err := runBuildTranspile(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED on transpile errors, got %q", status)
	}
}

// ---- runBuildTypes ----

func TestRunBuildTypes_BunResolutionFails(t *testing.T) {
	failBunResolution(t)
	ctx, _ := makeTestCtx(t)

	status, _, err := runBuildTypes(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when bun resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunBuildTypes_DiagnosticsReported(t *testing.T) {
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export const x: number = 'no';"), 0644)

	// tsc "fails" and emits a parseable diagnostic line.
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{
			Success:  false,
			ExitCode: 2,
			Stdout:   "src/index.ts(1,14): error TS2322: Type 'string' is not assignable to type 'number'.\n",
		}, nil
	})

	status, _, err := runBuildTypes(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED when type check fails, got %q", status)
	}
}

func TestRunBuildTypes_RawOutputFallback(t *testing.T) {
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export const x = 1;"), 0644)

	// Failure with output that the tsc parser produces no diagnostics from,
	// exercising the raw-output fallback branch.
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "tsc crashed unexpectedly"}, nil
	})

	status, _, err := runBuildTypes(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

// ---- runBuildCompile ----

func TestRunBuildCompile_BunResolutionFails(t *testing.T) {
	failBunResolution(t)
	ctx, _ := makeTestCtx(t)

	status, _, err := runBuildCompile(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when bun resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunBuildCompile_CompileErrors(t *testing.T) {
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	// The entrypoint arrives in this extension's own namespaced probe metadata
	// since an earlier migration; job context v2 no longer carries `main`.
	ctx.Project.Metadata = map[string]json.RawMessage{
		tsExtensionName: json.RawMessage(`{"main":"src/main.ts"}`),
	}
	ctx.Params = pctx.Params{"compile-target": json.RawMessage(`"bun-linux-x64"`)}
	projectPath := filepath.Join(dir, "project")
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "main.ts"), []byte("console.log('hi')"), 0644)

	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "compile blew up"}, nil
	})

	status, _, err := runBuildCompile(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED on compile errors, got %q", status)
	}
}

// ---- runServe: bun resolution ----

func TestRunServe_BunResolutionFails(t *testing.T) {
	failBunResolution(t)
	ctx, _ := makeTestCtx(t)
	// An explicit entrypoint avoids the resolve phase so the bun-resolution
	// failure (which happens first) is the path under test.
	ctx.Params = pctx.Params{"entrypoint": json.RawMessage(`"src/serve.ts"`)}

	status, _, err := runServe(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when bun resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

// ---- runWorkspaceInstall: failure paths ----

func TestRunWorkspaceInstall_BunResolutionFails(t *testing.T) {
	failBunResolution(t)
	ctx, _ := makeTestCtx(t)

	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected error when bun resolution fails")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunWorkspaceInstall_InstallExecError(t *testing.T) {
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"private":true}`), 0644)
	writeExtensionBiomeDefault(t, dir)

	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return nil, errors.New("bun install crashed")
	})

	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if err == nil {
		t.Fatal("expected install exec error to propagate")
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunWorkspaceInstall_ForceFlag(t *testing.T) {
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"force": json.RawMessage(`true`)}
	os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"private":true}`), 0644)
	writeExtensionBiomeDefault(t, dir)

	var gotArgs []string
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		gotArgs = args
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
	hasForce := false
	for _, a := range gotArgs {
		if a == "--force" {
			hasForce = true
		}
	}
	if !hasForce {
		t.Errorf("expected --force in install args, got %v", gotArgs)
	}
}

func TestRunWorkspaceInstall_MissingBiomeDefaultFailsExplicitly(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-biome-default", "a-missing-extension-biome-default-fails-install")
	mockBunResolution(t)
	ctx, dir := makeTestCtx(t)
	os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"private":true}`), 0644)

	status, _, err := runWorkspaceInstall(ctx, jsonl.New(), nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "read extension biome.json") {
		t.Fatalf("status = %q, error = %v, want explicit missing-default failure", status, err)
	}
}

// ---- generate failures always carry a diagnostic ----

// setupUnmanifestedLoaderProject makes the generate phase fail the way a missing capability manifest does:
// a pre-build hook reports an importable server loader, nothing reports the
// capability manifest, and bundled activation cannot be resolved. That error text
// matches no capability/http-route diagnostic pattern, so before the shared
// generateDiagnostics fallback the machine stream carried only `Job FAILED`.
func setupUnmanifestedLoaderProject(t *testing.T) (*pctx.Context, string) {
	t.Helper()
	mockBunResolution(t)
	ctx, root := makeTestCtx(t)
	projectPath := filepath.Join(root, "project")
	depDir := filepath.Join(projectPath, "node_modules", "@test", "genhook")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["hook.ts"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","dependencies":{"@test/genhook":"workspace:*"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":` +
			`{"exports":{"react-loader":"` + filepath.ToSlash(projectPath) + `/.gen/src/app/.react-application.gen.tsx"},"assets":{}}}` + "\n"}, nil
	})
	return ctx, projectPath
}

func TestGenerateDiagnostics_FallsBackWhenNoFramePatternMatches(t *testing.T) {
	diags := generateDiagnostics(errs.Wrapf(
		errors.New("generated server loaders require schema/capabilities.json"),
		errs.CodeGenerateFailed, "resolving bundled capability activation"))
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %#v, want exactly one fallback frame", diags)
	}
	if diags[0].Code != errs.CodeGenerateFailed.String() {
		t.Errorf("code = %q, want %q", diags[0].Code, errs.CodeGenerateFailed)
	}
	if !strings.Contains(diags[0].Message, "require schema/capabilities.json") {
		t.Errorf("message = %q, want the failure text", diags[0].Message)
	}
}

func TestGenerateDiagnostics_KeepsPromotedCapabilityFrames(t *testing.T) {
	err := errors.New("capability manifest validation failed:\n[capabilities.missing_project] project: project is required")
	diags := generateDiagnostics(err)
	if len(diags) != 1 || diags[0].Code != "capabilities.missing_project" {
		t.Fatalf("diagnostics = %#v, want the promoted protocol-coded frame, not the fallback", diags)
	}
}

func TestGenerateDiagnostics_NilErrorHasNoFrames(t *testing.T) {
	if diags := generateDiagnostics(nil); len(diags) != 0 {
		t.Fatalf("diagnostics = %#v, want none for a successful generate", diags)
	}
}

// TestRunBuildGenerate_FailureAlwaysEmitsDiagnostic pins the contract on the
// solo path: a jsonl consumer must never see a generate failure as `Job FAILED`
// with an empty diagnostic list.
func TestRunBuildGenerate_FailureAlwaysEmitsDiagnostic(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "batch-failure-isolation", "a-solo-generate-failure-emits-a-diagnostic")
	ctx, _ := setupUnmanifestedLoaderProject(t)

	var status string
	var genErr error
	events := captureEvents(t, func() {
		status, _, genErr = runBuildGenerate(ctx, jsonl.New(), nil)
	})
	if genErr == nil || status != "FAILED" {
		t.Fatalf("status=%q err=%v, want the bundled-activation failure", status, genErr)
	}

	diagnostic := findEvent(events, func(e map[string]any) bool { return e["type"] == "diagnostic" })
	if diagnostic == nil {
		t.Fatalf("no diagnostic event in %d emitted events; a failed generate must be diagnosable from the machine stream", len(events))
	}
	if message, _ := diagnostic["message"].(string); !strings.Contains(message, "schema/capabilities.json") {
		t.Errorf("diagnostic message = %q, want the generate failure text", message)
	}
}

// TestRunBuildGenerateBatch_FailureAlwaysEmitsDiagnostic pins the same contract
// on the batch path, which reconstructs diagnostics without an emitter.
func TestRunBuildGenerateBatch_FailureAlwaysEmitsDiagnostic(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "batch-failure-isolation", "a-batched-generate-failure-emits-a-diagnostic")
	ctx, projectPath := setupUnmanifestedLoaderProject(t)
	ctx.SelectedProjects = []pctx.ProjectRef{
		{ID: "/project", Name: "@test/pkg", Path: "project", FullPath: projectPath},
	}

	status, data, err := runBuildGenerateBatch(ctx)
	if err != nil || status != "OK" {
		t.Fatalf("batch status=%q err=%v, want a per-project failure inside an OK batch", status, err)
	}
	results := batchResultsFor(t, data)
	res, ok := resultByID(results, "/project")
	if !ok || res.Status != "FAILED" {
		t.Fatalf("results = %#v, want /project FAILED", results)
	}
	if len(res.Diagnostics) == 0 {
		t.Fatal("batch reported a failed generate with no diagnostics")
	}
	if !strings.Contains(res.Diagnostics[0].Description, "schema/capabilities.json") {
		t.Errorf("diagnostic = %q, want the generate failure text", res.Diagnostics[0].Description)
	}
}
