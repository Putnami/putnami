package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/infra"
	registry "go.putnami.dev/protocol/registry"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/typescript/extension/internal/build"
	"go.putnami.dev/typescript/extension/internal/hooks"
	"go.putnami.dev/typescript/extension/internal/lint"
	"go.putnami.dev/typescript/extension/internal/parse"
	"go.putnami.dev/typescript/extension/internal/testjob"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// mockBunResolution sets up a mock for bun resolution that returns a fake path,
// for the jobs that take the bun the CLI resolved and for the provisioning
// jobs that select one themselves.
func mockBunResolution(t *testing.T) {
	t.Helper()
	orig, origProvision := resolveBunBin, provisionBunBin
	t.Cleanup(func() { resolveBunBin, provisionBunBin = orig, origProvision })
	resolveBunBin = func() (string, error) { return "/mock/bun", nil }
	provisionBunBin = func(*pctx.Context, *jsonl.Emitter, toolchain.BunMode, func(string) string) (string, error) {
		return "/mock/bun", nil
	}
}

// mockBiomeResolution sets up mocks for biome resolution.
func mockBiomeResolution(t *testing.T) {
	t.Helper()
	origBin := resolveBiomeBinFn
	origConfig := resolveBiomeConfigFn
	t.Cleanup(func() {
		resolveBiomeBinFn = origBin
		resolveBiomeConfigFn = origConfig
	})
	resolveBiomeBinFn = func(_, _ string) (string, error) { return "/mock/biome", nil }
	resolveBiomeConfigFn = func(_, _, _ string) string { return "/mock/biome.json" }
}

// mockAllExec mocks exec in all internal packages with a success result.
func mockAllExec(t *testing.T, fn func(string, []string, ...exec.Option) (*exec.Result, error)) {
	t.Helper()
	t.Cleanup(build.SetExecRunForTesting(fn))
	t.Cleanup(testjob.SetExecRunForTesting(fn))
	t.Cleanup(hooks.SetExecRunForTesting(fn))
	t.Cleanup(lint.SetExecRunForTesting(fn))
	origWs := wsExecRunFunc
	t.Cleanup(func() { wsExecRunFunc = origWs })
	wsExecRunFunc = fn
}

func successExec(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
	return &exec.Result{Success: true, ExitCode: 0}, nil
}

func writeExtensionBiomeDefault(t *testing.T, extensionRoot string) {
	t.Helper()
	configDir := filepath.Join(extensionRoot, "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "biome.json"), []byte("{\"root\":false}\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func makeTestCtx(t *testing.T) (*pctx.Context, string) {
	t.Helper()
	return makeTestCtxAt(t, t.TempDir())
}

// makeTestCtxAt is makeTestCtx for a caller-chosen workspace root, so a test can
// run the same project under two different parent directories.
func makeTestCtxAt(t *testing.T, dir string) (*pctx.Context, string) {
	t.Helper()
	outputDir := filepath.Join(dir, "output")
	os.MkdirAll(outputDir, 0755)
	os.MkdirAll(filepath.Join(dir, "project"), 0755)
	os.WriteFile(filepath.Join(dir, "project", "package.json"), []byte(`{"name":"@test/pkg"}`), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		OutputPath:    outputDir,
		Project: pctx.Project{
			Name: "@test/pkg",
			Path: "project",
		},
		Extension: pctx.Extension{
			Name: "@putnami/typescript",
			Root: dir,
		},
		Params: pctx.Params{},
	}
	return ctx, dir
}

// ---- plural ----

func TestPlural_One(t *testing.T) {
	if got := plural(1); got != "" {
		t.Errorf("plural(1) = %q, want empty", got)
	}
}

func TestCapabilityDiagnosticPattern(t *testing.T) {
	output := "capability manifest validation failed:\n" +
		"[capabilities.missing_required_provider] requiredCapabilities[0].requires[2]: project \"orders\" required capability sql declared by src/sql.ts needs readiness"
	event := captureCapabilityDiagnostic(t, output)
	if event["type"] != "diagnostic" || event["code"] != "capabilities.missing_required_provider" {
		t.Fatalf("event = %#v, want coded diagnostic", event)
	}
	message, _ := event["message"].(string)
	for _, context := range []string{"requiredCapabilities[0].requires[2]", `project "orders"`, "src/sql.ts"} {
		if !strings.Contains(message, context) {
			t.Errorf("message = %q, want context %q", message, context)
		}
	}
}

func TestHTTPRouteDiagnosticPattern(t *testing.T) {
	output := "HTTP route inventory validation failed:\n" +
		"[http_routes.unsupported_pattern] routes[3].path: catch-all patterns must be expanded"
	event := captureCapabilityDiagnostic(t, output)
	if event["type"] != "diagnostic" || event["code"] != "http_routes.unsupported_pattern" {
		t.Fatalf("event = %#v, want coded HTTP route diagnostic", event)
	}
	if message, _ := event["message"].(string); !strings.Contains(message, "routes[3].path") {
		t.Fatalf("message = %q, want field context", message)
	}
}

func captureCapabilityDiagnostic(t *testing.T, output string) map[string]any {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = w
	emitCapabilityDiagnostics(jsonl.New(), output)
	_ = w.Close()
	os.Stdout = original
	defer r.Close()
	var event map[string]any
	if err := json.NewDecoder(r).Decode(&event); err != nil {
		t.Fatalf("decode JSONL diagnostic: %v", err)
	}
	return event
}

func TestPlural_Zero(t *testing.T) {
	if got := plural(0); got != "s" {
		t.Errorf("plural(0) = %q, want 's'", got)
	}
}

func TestPlural_Many(t *testing.T) {
	if got := plural(5); got != "s" {
		t.Errorf("plural(5) = %q, want 's'", got)
	}
}

// ---- join ----

func TestJoin_Empty(t *testing.T) {
	if got := join(nil, ", "); got != "" {
		t.Errorf("join(nil) = %q, want empty", got)
	}
}

func TestJoin_Single(t *testing.T) {
	if got := join([]string{"hello"}, ", "); got != "hello" {
		t.Errorf("join([hello]) = %q, want 'hello'", got)
	}
}

func TestJoin_Multiple(t *testing.T) {
	if got := join([]string{"a", "b", "c"}, ", "); got != "a, b, c" {
		t.Errorf("join([a,b,c]) = %q, want 'a, b, c'", got)
	}
}

func TestJoin_DifferentSep(t *testing.T) {
	if got := join([]string{"x", "y"}, " | "); got != "x | y" {
		t.Errorf("join with pipe = %q, want 'x | y'", got)
	}
}

// ---- loadGenerateResult ----

func TestLoadGenerateResult_MissingFile(t *testing.T) {
	dir := t.TempDir()
	result := loadGenerateResult(dir)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Exports == nil {
		t.Error("expected non-nil exports map")
	}
	if result.Assets == nil {
		t.Error("expected non-nil assets map")
	}
}

func TestLoadGenerateResult_ValidFile(t *testing.T) {
	dir := t.TempDir()
	genDir := filepath.Join(dir, ".gen")
	os.MkdirAll(genDir, 0755)

	// The manifest stores project-relative paths so it survives being restored
	// into another checkout; loadGenerateResult hands callers absolute ones back.
	genResult := build.GenerateResult{
		Hash: "abc123",
		Mode: "build",
		Exports: map[string]string{
			"version-info": ".gen/version.json",
		},
		Assets: map[string]string{
			"style.css": ".gen/public/style.css",
		},
	}
	data, _ := json.MarshalIndent(genResult, "", "  ")
	os.WriteFile(filepath.Join(genDir, "generate-result.json"), data, 0644)

	result := loadGenerateResult(dir)
	if result.Hash != "abc123" {
		t.Errorf("hash = %q, want abc123", result.Hash)
	}
	if result.Mode != "build" {
		t.Errorf("mode = %q, want build", result.Mode)
	}
	if want := filepath.Join(dir, ".gen", "version.json"); result.Exports["version-info"] != want {
		t.Errorf("exports[version-info] = %q, want %q", result.Exports["version-info"], want)
	}
	if want := filepath.Join(dir, ".gen", "public", "style.css"); result.Assets["style.css"] != want {
		t.Errorf("assets[style.css] = %q, want %q", result.Assets["style.css"], want)
	}
}

// TestLoadGenerateResult_LegacyAbsolutePaths keeps a manifest written before
// the project-relative rule — or restored from a cache entry an older extension published — usable:
// an absolute value resolves to itself instead of being joined onto the project
// root into nonsense.
func TestLoadGenerateResult_LegacyAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	genDir := filepath.Join(dir, ".gen")
	os.MkdirAll(genDir, 0755)

	legacy := filepath.Join(dir, ".gen", "src", "serve.bundled.ts")
	data, _ := json.MarshalIndent(build.GenerateResult{
		Exports: map[string]string{"bundled-serve": legacy},
	}, "", "  ")
	os.WriteFile(filepath.Join(genDir, "generate-result.json"), data, 0644)

	if got := loadGenerateResult(dir).Exports["bundled-serve"]; got != legacy {
		t.Errorf("bundled-serve = %q, want %q", got, legacy)
	}
}

func TestLoadGenerateResult_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	genDir := filepath.Join(dir, ".gen")
	os.MkdirAll(genDir, 0755)
	os.WriteFile(filepath.Join(genDir, "generate-result.json"), []byte("not json"), 0644)

	result := loadGenerateResult(dir)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Exports == nil {
		t.Error("expected non-nil exports map for invalid JSON fallback")
	}
}

// TestBuildGenerate_ManifestIsCheckoutRelocatable pins the determinism
// the project-relative invariant on the TypeScript side, where the Go runner's twin
// pins it too (the two manifests share one shape). The scheduler captures .gen
// as this step's cache output and stores the wire payload in the same entry, so
// generating the same project under two different parent directories must
// produce the same bytes in both — otherwise a restored manifest names a
// directory from someone else's checkout.
func TestBuildGenerate_ManifestIsCheckoutRelocatable(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	base := t.TempDir()
	run := func(root string) ([]byte, map[string]any) {
		t.Helper()
		ctx, dir := makeTestCtxAt(t, root)
		if err := os.MkdirAll(filepath.Join(dir, "project", "src"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "project", "src", "index.ts"),
			[]byte("export const x = 1;"), 0o644); err != nil {
			t.Fatal(err)
		}
		status, data, err := runBuildGenerate(ctx, jsonl.New(), nil)
		if err != nil || status != "OK" {
			t.Fatalf("runBuildGenerate(%s) = (%q, %v)", root, status, err)
		}
		manifest, err := os.ReadFile(filepath.Join(dir, "project", ".gen", "generate-result.json"))
		if err != nil {
			t.Fatalf("reading manifest: %v", err)
		}
		return manifest, data
	}

	first, firstData := run(filepath.Join(base, "checkout-a"))
	second, secondData := run(filepath.Join(base, "deeper", "nested", "checkout-b"))

	if string(first) != string(second) {
		t.Fatalf("manifest depends on the checkout location:\n--- a ---\n%s\n--- b ---\n%s", first, second)
	}
	if strings.Contains(string(first), base) {
		t.Fatalf("manifest embeds an absolute checkout path:\n%s", first)
	}

	// The payload is cached alongside the tree, so it carries the same values.
	if !reflect.DeepEqual(firstData["exports"], secondData["exports"]) {
		t.Fatalf("result data exports differ between checkouts: %v != %v",
			firstData["exports"], secondData["exports"])
	}
	exports, ok := firstData["exports"].(map[string]string)
	if !ok {
		t.Fatalf("result data exports = %T, want map[string]string", firstData["exports"])
	}
	if exports["version-info"] != ".gen/version.json" {
		t.Errorf("exports[version-info] = %q, want .gen/version.json", exports["version-info"])
	}

	// And the in-process view is unchanged: readers still get absolute paths.
	projectPath := filepath.Join(base, "checkout-a", "project")
	loaded := loadGenerateResult(projectPath)
	if want := filepath.Join(projectPath, ".gen", "version.json"); loaded.Exports["version-info"] != want {
		t.Errorf("loaded exports[version-info] = %q, want %q", loaded.Exports["version-info"], want)
	}
}

// ---- resolveCompileEntrypoint ----

// metadataContext builds a job context carrying THIS extension's namespaced
// probe metadata block.
//
// Since an earlier migration the npm-shaped `main`/`bin`/`exports` members
// are gone from job context v2: core no longer parses package.json, so it no
// longer carries fields only one language can mean anything by. The same facts
// arrive under `project.metadata["@putnami/typescript"]`, written by this
// extension's own workspace probe.
func metadataContext(block string) *pctx.Context {
	return &pctx.Context{Project: pctx.Project{
		Metadata: map[string]json.RawMessage{tsExtensionName: json.RawMessage(block)},
	}}
}

func TestResolveCompileEntrypoint_BundledServe(t *testing.T) {
	ctx := &pctx.Context{Project: pctx.Project{}}
	genResult := &build.GenerateResult{
		Exports: map[string]string{
			"bundled-serve": "/path/to/serve.bundled.ts",
		},
	}
	got := resolveCompileEntrypoint(ctx, genResult)
	if got != "/path/to/serve.bundled.ts" {
		t.Errorf("expected bundled-serve export, got %q", got)
	}
}

func TestResolveCompileEntrypoint_ServeExport(t *testing.T) {
	ctx := metadataContext(`{"exports":{"./serve":"./dist/serve.js"}}`)
	got := resolveCompileEntrypoint(ctx, nil)
	if got != "./dist/serve.js" {
		t.Errorf("expected ./dist/serve.js, got %q", got)
	}
}

func TestResolveCompileEntrypoint_BinString(t *testing.T) {
	ctx := metadataContext(`{"bin":"./bin/cli.js"}`)
	got := resolveCompileEntrypoint(ctx, nil)
	if got != "./bin/cli.js" {
		t.Errorf("expected ./bin/cli.js, got %q", got)
	}
}

func TestResolveCompileEntrypoint_Main(t *testing.T) {
	ctx := metadataContext(`{"main":"src/main.ts"}`)
	got := resolveCompileEntrypoint(ctx, nil)
	if got != "src/main.ts" {
		t.Errorf("expected src/main.ts, got %q", got)
	}
}

func TestResolveCompileEntrypoint_Empty(t *testing.T) {
	ctx := &pctx.Context{Project: pctx.Project{}}
	got := resolveCompileEntrypoint(ctx, nil)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestResolveCompileEntrypoint_NilGenResult(t *testing.T) {
	ctx := metadataContext(`{"exports":{"./serve":"./src/serve.ts"}}`)
	got := resolveCompileEntrypoint(ctx, nil)
	if got != "./src/serve.ts" {
		t.Errorf("expected ./src/serve.ts, got %q", got)
	}
}

func TestResolveCompileEntrypoint_GenResultNoExport(t *testing.T) {
	ctx := metadataContext(`{"main":"src/main.ts"}`)
	genResult := &build.GenerateResult{
		Exports: map[string]string{
			"version-info": "/path/to/version.json",
		},
	}
	got := resolveCompileEntrypoint(ctx, genResult)
	if got != "src/main.ts" {
		t.Errorf("expected src/main.ts as fallback, got %q", got)
	}
}

func TestResolveCompileEntrypoint_Priority(t *testing.T) {
	// bundled-serve > ./serve export > bin > main
	ctx := metadataContext(`{"exports":{"./serve":"./dist/serve.js"},"bin":"./bin/cli.js","main":"src/main.ts"}`)
	genResult := &build.GenerateResult{
		Exports: map[string]string{
			"bundled-serve": "/gen/serve.bundled.ts",
		},
	}
	got := resolveCompileEntrypoint(ctx, genResult)
	if got != "/gen/serve.bundled.ts" {
		t.Errorf("expected bundled-serve to win, got %q", got)
	}
}

// ---- emitBiomeDiagnostics ----

func TestEmitBiomeDiagnostics_Empty(_ *testing.T) {
	emit := jsonl.New()
	report := parse.BiomeReport{}
	// Should not panic
	emitBiomeDiagnostics(emit, report)
}

func TestEmitBiomeDiagnostics_WithDiagnostics(_ *testing.T) {
	emit := jsonl.New()
	report := parse.BiomeReport{
		Diagnostics: []parse.BiomeDiagnostic{
			{Severity: "error", Description: "test error", File: "src/main.ts", Line: 10, Column: 5, Category: "lint/test"},
		},
		Summary: parse.BiomeSummary{
			Errors:   1,
			Warnings: 2,
			Infos:    3,
		},
	}
	// Should not panic
	emitBiomeDiagnostics(emit, report)
}

func TestEmitBiomeDiagnostics_OnlyWarnings(_ *testing.T) {
	emit := jsonl.New()
	report := parse.BiomeReport{
		Summary: parse.BiomeSummary{
			Warnings: 5,
		},
	}
	emitBiomeDiagnostics(emit, report)
}

func TestEmitBiomeDiagnostics_OnlyInfos(_ *testing.T) {
	emit := jsonl.New()
	report := parse.BiomeReport{
		Summary: parse.BiomeSummary{
			Infos: 3,
		},
	}
	emitBiomeDiagnostics(emit, report)
}

// ---- emitCoverageSynthesis ----

func TestEmitCoverageSynthesis_EmptyFiles(_ *testing.T) {
	emit := jsonl.New()
	summary := &parse.CoverageSummary{}
	emitCoverageSynthesis(emit, summary, nil)
}

func TestEmitCoverageSynthesis_AllFullCoverage(_ *testing.T) {
	emit := jsonl.New()
	summary := &parse.CoverageSummary{
		LineCoverage:     100,
		FunctionCoverage: 100,
		CoveredLines:     50,
		TotalLines:       50,
		CoveredFunctions: 10,
		TotalFunctions:   10,
	}
	files := []parse.FileCoverage{
		{Path: "src/a.ts", Coverage: 100, CoveredLines: 10, TotalLines: 10},
		{Path: "src/b.ts", Coverage: 100, CoveredLines: 20, TotalLines: 20},
	}
	emitCoverageSynthesis(emit, summary, files)
}

func TestEmitCoverageSynthesis_MixedCoverage(_ *testing.T) {
	emit := jsonl.New()
	summary := &parse.CoverageSummary{
		LineCoverage:     75,
		FunctionCoverage: 80,
		CoveredLines:     75,
		TotalLines:       100,
		CoveredFunctions: 8,
		TotalFunctions:   10,
	}
	files := []parse.FileCoverage{
		{Path: "src/a.ts", Coverage: 50, CoveredLines: 5, TotalLines: 10},
		{Path: "src/b.ts", Coverage: 100, CoveredLines: 20, TotalLines: 20},
		{Path: "src/c.ts", Coverage: 80, CoveredLines: 16, TotalLines: 20},
	}
	emitCoverageSynthesis(emit, summary, files)
}

func TestEmitCoverageSynthesis_ManyFilesBelow100(_ *testing.T) {
	emit := jsonl.New()
	summary := &parse.CoverageSummary{
		LineCoverage:     50,
		FunctionCoverage: 50,
		CoveredLines:     50,
		TotalLines:       100,
		CoveredFunctions: 5,
		TotalFunctions:   10,
	}
	// Create 25 files below 100% — should truncate to 20
	var files []parse.FileCoverage
	for i := 0; i < 25; i++ {
		files = append(files, parse.FileCoverage{
			Path:         filepath.Join("src", "file"+string(rune('a'+i))+".ts"),
			Coverage:     float64(i * 4),
			CoveredLines: i * 4,
			TotalLines:   100,
		})
	}
	emitCoverageSynthesis(emit, summary, files)
}

func TestEmitCoverageSynthesis_WithFullCoverageCount(_ *testing.T) {
	emit := jsonl.New()
	summary := &parse.CoverageSummary{
		LineCoverage:     90,
		FunctionCoverage: 90,
		CoveredLines:     90,
		TotalLines:       100,
		CoveredFunctions: 9,
		TotalFunctions:   10,
	}
	files := []parse.FileCoverage{
		{Path: "src/a.ts", Coverage: 50, CoveredLines: 5, TotalLines: 10},
		{Path: "src/b.ts", Coverage: 100, CoveredLines: 20, TotalLines: 20},
		{Path: "src/c.ts", Coverage: 100, CoveredLines: 30, TotalLines: 30},
		{Path: "src/d.ts", Coverage: 100, CoveredLines: 40, TotalLines: 40},
	}
	emitCoverageSynthesis(emit, summary, files)
}

// ---- ensureWorkspaceDevDeps ----

func TestEnsureWorkspaceDevDeps_AddsMissingDeps(t *testing.T) {
	wsDir := t.TempDir()
	extDir := t.TempDir()

	manifest := `{"workspaceDevDependencies":{"typescript":"^5.0.0","@types/bun":"latest"}}`
	os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0644)

	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"private":true,"devDependencies":{"typescript":"^5.0.0"}}`), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: wsDir,
		Extension:     pctx.Extension{Root: extDir},
	}
	emit := jsonl.New()

	err := ensureWorkspaceDevDeps(ctx, emit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(wsDir, "package.json"))
	var pkg map[string]json.RawMessage
	json.Unmarshal(data, &pkg)

	var devDeps map[string]string
	json.Unmarshal(pkg["devDependencies"], &devDeps)

	if devDeps["typescript"] != "^5.0.0" {
		t.Errorf("typescript = %q, want ^5.0.0 (not overridden)", devDeps["typescript"])
	}
	if devDeps["@types/bun"] != "latest" {
		t.Errorf("@types/bun = %q, want latest (added)", devDeps["@types/bun"])
	}
}

func TestEnsureWorkspaceDevDeps_AllPresent(t *testing.T) {
	wsDir := t.TempDir()
	extDir := t.TempDir()

	manifest := `{"workspaceDevDependencies":{"typescript":"^5.0.0"}}`
	os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0644)

	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{"devDependencies":{"typescript":"^5.0.0"}}`), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: wsDir,
		Extension:     pctx.Extension{Root: extDir},
	}
	emit := jsonl.New()

	err := ensureWorkspaceDevDeps(ctx, emit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureWorkspaceDevDeps_NoWorkspaceDevDeps(t *testing.T) {
	wsDir := t.TempDir()
	extDir := t.TempDir()

	os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(wsDir, "package.json"), []byte(`{}`), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: wsDir,
		Extension:     pctx.Extension{Root: extDir},
	}
	emit := jsonl.New()

	err := ensureWorkspaceDevDeps(ctx, emit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureWorkspaceDevDeps_NoPackageJSON(t *testing.T) {
	wsDir := t.TempDir()
	extDir := t.TempDir()

	manifest := `{"workspaceDevDependencies":{"bun-types":"latest"}}`
	os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0644)

	// No workspace package.json — should create one
	ctx := &pctx.Context{
		WorkspaceRoot: wsDir,
		Extension:     pctx.Extension{Root: extDir},
	}
	emit := jsonl.New()

	err := ensureWorkspaceDevDeps(ctx, emit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(wsDir, "package.json"))
	var pkg map[string]json.RawMessage
	json.Unmarshal(data, &pkg)

	var devDeps map[string]string
	json.Unmarshal(pkg["devDependencies"], &devDeps)

	if devDeps["bun-types"] != "latest" {
		t.Errorf("bun-types = %q, want latest", devDeps["bun-types"])
	}
}

func TestEnsureWorkspaceDevDeps_MissingManifest(t *testing.T) {
	wsDir := t.TempDir()
	extDir := t.TempDir()

	ctx := &pctx.Context{
		WorkspaceRoot: wsDir,
		Extension:     pctx.Extension{Root: extDir},
	}
	emit := jsonl.New()

	err := ensureWorkspaceDevDeps(ctx, emit)
	if err == nil {
		t.Error("expected error for missing manifest")
	}
}

func TestEnsureWorkspaceDevDeps_InvalidManifestJSON(t *testing.T) {
	wsDir := t.TempDir()
	extDir := t.TempDir()

	os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte("not json"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: wsDir,
		Extension:     pctx.Extension{Root: extDir},
	}
	emit := jsonl.New()

	err := ensureWorkspaceDevDeps(ctx, emit)
	if err == nil {
		t.Error("expected error for invalid manifest JSON")
	}
}

// ---- ensureWorkspaceTsConfig ----

func TestEnsureWorkspaceTsConfig_CreatesWithExtends(t *testing.T) {
	wsDir := t.TempDir()
	extDir := t.TempDir()

	// Create extension config/tsconfig.json
	configDir := filepath.Join(extDir, "config")
	os.MkdirAll(configDir, 0755)
	os.WriteFile(filepath.Join(configDir, "tsconfig.json"), []byte(`{"compilerOptions":{"strict":true}}`), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: wsDir,
		Extension:     pctx.Extension{Root: extDir},
	}
	emit := jsonl.New()

	if err := ensureWorkspaceTsConfig(ctx, emit); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(wsDir, "tsconfig.json"))
	if err != nil {
		t.Fatal("tsconfig.json should have been created")
	}

	content := string(data)
	if !strings.Contains(content, `"extends"`) {
		t.Error("tsconfig should contain extends field")
	}
	if !strings.Contains(content, "config/tsconfig.json") {
		t.Errorf("tsconfig should extend extension config, got: %s", content)
	}
}

func TestEnsureWorkspaceTsConfig_SkipsIfExists(t *testing.T) {
	wsDir := t.TempDir()
	extDir := t.TempDir()

	// Pre-existing tsconfig
	os.WriteFile(filepath.Join(wsDir, "tsconfig.json"), []byte(`{"custom":true}`), 0644)

	// Extension config
	configDir := filepath.Join(extDir, "config")
	os.MkdirAll(configDir, 0755)
	os.WriteFile(filepath.Join(configDir, "tsconfig.json"), []byte(`{"compilerOptions":{"strict":true}}`), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: wsDir,
		Extension:     pctx.Extension{Root: extDir},
	}
	emit := jsonl.New()

	if err := ensureWorkspaceTsConfig(ctx, emit); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(wsDir, "tsconfig.json"))
	if string(data) != `{"custom":true}` {
		t.Error("existing tsconfig.json should not be overwritten")
	}
}

// ---- ensureWorkspaceBiomeConfig ----

func TestEnsureWorkspaceBiomeConfig_ProjectsShippedRulesAsWorkspaceRoot(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-biome-default", "a-missing-workspace-biome-config-materializes-extension-rules-as-a-root-config")
	wsDir := t.TempDir()
	extDir := extensionProjectRoot(t)
	shipped, err := os.ReadFile(filepath.Join(extDir, "config", "biome.json"))
	if err != nil {
		t.Fatalf("read shipped biome config: %v", err)
	}
	ctx := &pctx.Context{WorkspaceRoot: wsDir, Extension: pctx.Extension{Root: extDir}}

	if err := ensureWorkspaceBiomeConfig(ctx, jsonl.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wsDir, "biome.json"))
	if err != nil {
		t.Fatal("biome.json should have been created")
	}
	var shippedConfig, workspaceConfig map[string]any
	if err := json.Unmarshal(shipped, &shippedConfig); err != nil {
		t.Fatalf("parse shipped biome config: %v", err)
	}
	if err := json.Unmarshal(got, &workspaceConfig); err != nil {
		t.Fatalf("parse workspace biome config: %v", err)
	}
	if workspaceConfig["root"] != true {
		t.Fatalf("workspace biome root = %#v, want true", workspaceConfig["root"])
	}
	delete(shippedConfig, "root")
	delete(workspaceConfig, "root")
	if !reflect.DeepEqual(workspaceConfig, shippedConfig) {
		t.Fatalf("workspace biome rules differ from shipped rules\nworkspace: %#v\nshipped: %#v", workspaceConfig, shippedConfig)
	}
}

func extensionProjectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.extension.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find TypeScript extension root")
		}
		dir = parent
	}
}

func TestWorkspaceBiomeDefault_FreshWorkspacePutnamiLifecycle(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-biome-default", "a-fresh-workspace-install-is-idempotent-and-lint-uses-the-root-config")
	putnami := spawningPutnami(t)
	extensionRoot := extensionProjectRoot(t)
	workspaceRoot := t.TempDir()
	assertNoAncestorBiomeConfig(t, workspaceRoot)

	workspaceManifest, err := json.Marshal(map[string]any{
		"name":       "fresh-biome-default",
		"extensions": []string{extensionRoot},
		"includes":   []string{"app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFreshWorkspaceFile(t, workspaceRoot, "putnami.workspace.json", append(workspaceManifest, '\n'))
	writeFreshWorkspaceFile(t, workspaceRoot, "package.json", []byte("{\"name\":\"fresh-biome-default\",\"private\":true,\"workspaces\":[\"app\"],\"packageManager\":\"bun@1.4.0\"}\n"))
	writeFreshWorkspaceFile(t, workspaceRoot, ".gitignore", []byte("app/ignored.ts\n"))
	if err := os.Mkdir(filepath.Join(workspaceRoot, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	projectManifest, err := json.Marshal(map[string]any{
		"name":       "@fixture/biome-default",
		"extensions": []string{"@putnami/typescript"},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFreshWorkspaceFile(t, workspaceRoot, "app/putnami.json", append(projectManifest, '\n'))
	writeFreshWorkspaceFile(t, workspaceRoot, "app/package.json", []byte("{\"name\":\"@fixture/biome-default\",\"private\":true}\n"))
	writeFreshWorkspaceFile(t, workspaceRoot, "app/src/index.ts", []byte("export const answer = 42;\n"))
	ignored := []byte("console.log('ignored by the workspace VCS rules');\n")
	writeFreshWorkspaceFile(t, workspaceRoot, "app/ignored.ts", ignored)

	installOutput := runPutnamiInWorkspace(t, putnami, workspaceRoot, "deps", "install")
	installed, err := os.ReadFile(filepath.Join(workspaceRoot, "biome.json"))
	if err != nil {
		t.Fatalf("read installed workspace biome config: %v; putnami deps install: %q", err, installOutput)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(installed, &config); err != nil {
		t.Fatalf("parse installed workspace biome config: %v", err)
	}
	if string(config["root"]) != "true" {
		t.Fatalf("installed workspace biome root = %s, want true", config["root"])
	}

	runPutnamiInWorkspace(t, putnami, workspaceRoot, "deps", "install")
	reinstalled, err := os.ReadFile(filepath.Join(workspaceRoot, "biome.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reinstalled, installed) {
		t.Fatal("second putnami deps install changed the materialized workspace biome config")
	}

	runPutnamiInWorkspace(t, putnami, workspaceRoot, "lint", "--projects", "@fixture/biome-default", "--no-cache")
	ignoredAfter, err := os.ReadFile(filepath.Join(workspaceRoot, "app", "ignored.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ignoredAfter, ignored) {
		t.Fatalf("Putnami lint rewrote a .gitignore-excluded file: %q", ignoredAfter)
	}

	writeFreshWorkspaceFile(t, workspaceRoot, "app/src/index.ts", []byte("console.log('must be rejected by the shipped rules');\n"))
	runPutnamiInWorkspace(t, putnami, workspaceRoot, "lint", "--projects", "@fixture/biome-default", "--no-cache")
	linted, err := os.ReadFile(filepath.Join(workspaceRoot, "app", "src", "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(linted, []byte("console")) {
		t.Fatalf("Putnami lint did not apply the shipped noConsole rule: %q", linted)
	}
}

func assertNoAncestorBiomeConfig(t *testing.T, path string) {
	t.Helper()
	for dir := path; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "biome.json")); err == nil {
			t.Fatalf("fresh workspace has an ancestor biome.json at %s", dir)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return
		}
	}
}

func writeFreshWorkspaceFile(t *testing.T, root, relative string, content []byte) {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
}

// spawningPutnami returns the Putnami executable that launched this suite, the
// only one a repository test may run.
//
// NOT ./putnamiw. The wrapper is the repository's bootstrap: it resolves the
// engine for the CHECKOUT IT LIVES IN, regardless of the working directory it is
// called from, and it writes this workspace's shared state — the engine link, the
// provenance marker, and possibly a compile — while a run is in flight.
//
// The engine link is no longer part of the launcher's proof, so re-pointing it
// mid-run no longer refuses the running CLI's children: the session's
// content-keyed pin replaced that pointer. What remains is still a hijack
// of shared state: a wrapper call from inside a run can spend a cold build on the
// critical path, and it leaves the checkout pointing at an engine this run never
// selected, which the next person reads as the engine that gated it.
//
// The spawning executable has neither problem: it is already the admitted engine,
// and running it writes nothing to this workspace.
func spawningPutnami(t *testing.T) string {
	t.Helper()
	executable := strings.TrimSpace(os.Getenv(registry.CLIExecutableEnv))
	if executable == "" || !filepath.IsAbs(executable) {
		t.Skipf("%s is unset: this lifecycle needs the Putnami that launched the suite", registry.CLIExecutableEnv)
	}
	return executable
}

func runPutnamiInWorkspace(t *testing.T, putnami, workspaceRoot string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := stdexec.CommandContext(ctx, putnami, args...) //nolint:gosec // the exact spawning Putnami the CLI advertises
	command.Dir = workspaceRoot
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("putnami %s timed out: %v\n%s", strings.Join(args, " "), ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("putnami %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func TestEnsureWorkspaceBiomeConfig_PreservesExistingConfig(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "workspace-biome-default", "an-existing-workspace-biome-config-is-preserved")
	wsDir := t.TempDir()
	extDir := t.TempDir()
	configDir := filepath.Join(extDir, "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "biome.json"), []byte(`{"formatter":{"lineWidth":120}}`), 0644); err != nil {
		t.Fatal(err)
	}
	existing := []byte("{\"formatter\":{\"lineWidth\":88}}\n")
	if err := os.WriteFile(filepath.Join(wsDir, "biome.json"), existing, 0644); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{WorkspaceRoot: wsDir, Extension: pctx.Extension{Root: extDir}}

	if err := ensureWorkspaceBiomeConfig(ctx, jsonl.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wsDir, "biome.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, existing) {
		t.Fatalf("existing workspace biome config changed: %q", got)
	}
}

func TestEnsureWorkspaceBiomeConfig_FailsWhenExtensionDefaultIsUnavailable(t *testing.T) {
	wsDir := t.TempDir()
	ctx := &pctx.Context{WorkspaceRoot: wsDir, Extension: pctx.Extension{Root: t.TempDir()}}

	err := ensureWorkspaceBiomeConfig(ctx, jsonl.New())
	if err == nil || !strings.Contains(err.Error(), "read extension biome.json") {
		t.Fatalf("error = %v, want safe missing-default diagnostic", err)
	}
	if _, statErr := os.Stat(filepath.Join(wsDir, "biome.json")); !os.IsNotExist(statErr) {
		t.Fatalf("workspace biome.json should not be created, stat error = %v", statErr)
	}
}

// ---- ensureWorkspaceNpmrc ----

// npmrcContext is a workspace-install context carrying the registries entry the
// scope lines are rendered from.
func npmrcContext(t *testing.T, wsDir string, scopes map[string]string) *pctx.Context {
	t.Helper()
	entry, err := json.Marshal(map[string]any{"npm": map[string]any{"scopes": scopes}})
	if err != nil {
		t.Fatal(err)
	}
	return &pctx.Context{WorkspaceRoot: wsDir, Params: pctx.Params{"registries": entry}}
}

func TestEnsureWorkspaceNpmrc_CreatesFile(t *testing.T) {
	wsDir := t.TempDir()

	ctx := npmrcContext(t, wsDir, map[string]string{"@putnami": "https://npm.putnami.dev"})
	if err := ensureWorkspaceNpmrc(ctx, jsonl.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(wsDir, ".npmrc"))
	if err != nil {
		t.Fatal(".npmrc should have been created")
	}
	if string(data) != "@putnami:registry=https://npm.putnami.dev\n" {
		t.Errorf(".npmrc should map the declared scope, got: %s", data)
	}
}

func TestEnsureWorkspaceNpmrc_WritesOneLinePerDeclaredScope(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "registries-drive-npmrc", "npmrc-lines-come-from-registries")

	wsDir := t.TempDir()
	ctx := npmrcContext(t, wsDir, map[string]string{
		"@putnami": "https://npm.putnami.dev",
		"@acme":    "https://npm.acme.test",
	})
	if err := ensureWorkspaceNpmrc(ctx, jsonl.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(wsDir, ".npmrc"))
	want := "@acme:registry=https://npm.acme.test\n@putnami:registry=https://npm.putnami.dev\n"
	if string(data) != want {
		t.Errorf(".npmrc = %q, want %q", data, want)
	}
}

func TestEnsureWorkspaceNpmrc_WritesNothingWithoutADeclaration(t *testing.T) {
	wsDir := t.TempDir()

	ctx := &pctx.Context{WorkspaceRoot: wsDir, Params: pctx.Params{}}
	if err := ensureWorkspaceNpmrc(ctx, jsonl.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wsDir, ".npmrc")); !os.IsNotExist(err) {
		t.Error("a workspace that declares no npm scopes must get no generated .npmrc")
	}
}

func TestEnsureWorkspaceNpmrc_PreservesUnrelatedLines(t *testing.T) {
	wsDir := t.TempDir()
	original := "registry=https://registry.example.com/\n//registry.example.com/:_authToken=abc\n"
	os.WriteFile(filepath.Join(wsDir, ".npmrc"), []byte(original), 0644)

	ctx := npmrcContext(t, wsDir, map[string]string{"@putnami": "https://npm.putnami.dev"})
	if err := ensureWorkspaceNpmrc(ctx, jsonl.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(wsDir, ".npmrc"))
	content := string(data)
	if !strings.Contains(content, original) {
		t.Errorf("original .npmrc content lost, got: %s", content)
	}
	if !strings.Contains(content, "@putnami:registry=https://npm.putnami.dev") {
		t.Errorf(".npmrc should append the declared scope, got: %s", content)
	}
}

func TestEnsureWorkspaceNpmrc_PreservesLongUnrelatedLines(t *testing.T) {
	wsDir := t.TempDir()
	// bufio.Scanner's default 64 KiB token limit used to stop the rewrite here
	// without reporting its error, silently deleting this line and everything
	// after it from the user's .npmrc.
	longLine := "#" + strings.Repeat("x", 70*1024)
	original := longLine + "\nalways-auth=false\n"
	if err := os.WriteFile(filepath.Join(wsDir, ".npmrc"), []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := npmrcContext(t, wsDir, map[string]string{"@putnami": "https://npm.putnami.dev"})
	if err := ensureWorkspaceNpmrc(ctx, jsonl.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(wsDir, ".npmrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), original) {
		t.Fatal("rewriting the managed scope removed long or following user-owned lines")
	}
}

// The declaration is the source of truth: a stale generated line pointing at
// the previous registry is replaced in place, not left to send installs
// somewhere the workspace no longer names.
func TestEnsureWorkspaceNpmrc_ReplacesAStaleScopeLine(t *testing.T) {
	wsDir := t.TempDir()
	os.WriteFile(filepath.Join(wsDir, ".npmrc"), []byte("@putnami:registry=https://mirror.example.com\n//mirror.example.com/:_authToken=abc\n"), 0644)

	ctx := npmrcContext(t, wsDir, map[string]string{"@putnami": "https://npm.putnami.dev"})
	if err := ensureWorkspaceNpmrc(ctx, jsonl.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(wsDir, ".npmrc"))
	want := "@putnami:registry=https://npm.putnami.dev\n//mirror.example.com/:_authToken=abc\n"
	if string(data) != want {
		t.Errorf(".npmrc = %q, want %q", data, want)
	}
}

func TestEnsureWorkspaceNpmrc_LeavesAMatchingFileAlone(t *testing.T) {
	wsDir := t.TempDir()
	original := "@putnami:registry=https://npm.putnami.dev\n"
	path := filepath.Join(wsDir, ".npmrc")
	os.WriteFile(path, []byte(original), 0644)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := npmrcContext(t, wsDir, map[string]string{"@putnami": "https://npm.putnami.dev"})
	if err := ensureWorkspaceNpmrc(ctx, jsonl.New()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Errorf(".npmrc = %q, want it untouched", data)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an already-correct .npmrc was rewritten")
	}
}

// ---- runTest ----

func TestRunTest_NoTestFiles(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, _ := makeTestCtx(t)
	emit := jsonl.New()

	status, _, err := runTest(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK for project with no test files, got %q", status)
	}
}

func TestRunTest_WithTestFiles(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	// Create test files
	os.MkdirAll(filepath.Join(projectPath, "test"), 0755)
	os.WriteFile(filepath.Join(projectPath, "test", "foo.test.ts"), []byte(""), 0644)

	// Mock exec to return success and create JUnit output
	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		// Write JUnit file in output path
		junitXML := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="1" failures="0">
  <testsuite name="test" tests="1" failures="0">
    <testcase name="passes" classname="test"/>
  </testsuite>
</testsuites>`
		os.MkdirAll(ctx.OutputPath, 0755)
		os.WriteFile(filepath.Join(ctx.OutputPath, "results.junit.xml"), []byte(junitXML), 0644)
		return &exec.Result{Success: true, ExitCode: 0, Stdout: "all tests passed"}, nil
	})

	emit := jsonl.New()
	status, _, err := runTest(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

func TestRunTest_FailedTests(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "test"), 0755)
	os.WriteFile(filepath.Join(projectPath, "test", "foo.test.ts"), []byte(""), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		junitXML := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="2" failures="1">
  <testsuite name="test" tests="2" failures="1">
    <testcase name="passes" classname="test"/>
    <testcase name="fails" classname="test">
      <failure message="assertion failed" type="AssertionError"/>
    </testcase>
  </testsuite>
</testsuites>`
		os.MkdirAll(ctx.OutputPath, 0755)
		os.WriteFile(filepath.Join(ctx.OutputPath, "results.junit.xml"), []byte(junitXML), 0644)
		return &exec.Result{Success: false, ExitCode: 1, Stdout: "", Stderr: "test failed"}, nil
	})

	emit := jsonl.New()
	status, _, err := runTest(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

func TestRunTest_WithCoverage(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"enforce-coverage": json.RawMessage(`true`)}
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "test"), 0755)
	os.WriteFile(filepath.Join(projectPath, "test", "foo.test.ts"), []byte(""), 0644)
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("line1\nline2\n"), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		junitXML := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="1" failures="0">
  <testsuite name="test" tests="1" failures="0">
    <testcase name="passes" classname="test"/>
  </testsuite>
</testsuites>`
		os.MkdirAll(ctx.OutputPath, 0755)
		os.WriteFile(filepath.Join(ctx.OutputPath, "results.junit.xml"), []byte(junitXML), 0644)
		os.WriteFile(filepath.Join(ctx.OutputPath, "lcov.info"), []byte("SF:src/index.ts\nDA:1,1\nDA:2,0\nend_of_record\n"), 0644)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	emit := jsonl.New()
	status, _, err := runTest(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

// lcov50pct is an LCOV report whose LF/LH records report 1 of 2 instrumented
// lines hit → 50% line coverage.
const lcov50pct = "SF:src/index.ts\nLF:2\nLH:1\nend_of_record\n"

// lcovNoInstrumentedLines is an LCOV report that names a source file but
// contains no LF/LH records, so the parser produces TotalLines 0. This is the
// "no usable coverage data" case that must not pass a threshold.
const lcovNoInstrumentedLines = "SF:src/index.ts\nend_of_record\n"

// setupCoverageRun creates a project with one test file and a source file, then
// mocks exec so the test run "succeeds" and writes lcovContent to lcov.info.
// An empty lcovContent writes no lcov.info at all, exercising the missing-file
// path.
func setupCoverageRun(t *testing.T, params pctx.Params, lcovContent string) *pctx.Context {
	t.Helper()
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params = params
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "test"), 0755)
	os.WriteFile(filepath.Join(projectPath, "test", "foo.test.ts"), []byte(""), 0644)
	// The LCOV parser reconciles coverage against source files on disk, so the
	// referenced file must exist for its records to count.
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("line1\nline2\n"), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		junitXML := `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="1" failures="0">
  <testsuite name="test" tests="1" failures="0">
    <testcase name="passes" classname="test"/>
  </testsuite>
</testsuites>`
		os.MkdirAll(ctx.OutputPath, 0755)
		os.WriteFile(filepath.Join(ctx.OutputPath, "results.junit.xml"), []byte(junitXML), 0644)
		if lcovContent != "" {
			os.WriteFile(filepath.Join(ctx.OutputPath, "lcov.info"), []byte(lcovContent), 0644)
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	return ctx
}

func TestRunTest_CoverageThresholdNotMet(t *testing.T) {
	// Validation cadence: --enforce-coverage collects the profile and the gate
	// fires. 50% line coverage cannot meet a 90% threshold → FAILED, even though
	// the test passes.
	ctx := setupCoverageRun(t, pctx.Params{
		"enforce-coverage":   json.RawMessage(`true`),
		"coverage-threshold": json.RawMessage(`90`),
	}, lcov50pct)

	status, _, err := runTest(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED (coverage below threshold), got %q", status)
	}
}

func TestRunTest_CoverageThresholdMet(t *testing.T) {
	// Validation cadence: --enforce-coverage on. 50% line coverage clears a 40%
	// threshold → OK.
	ctx := setupCoverageRun(t, pctx.Params{
		"enforce-coverage":   json.RawMessage(`true`),
		"coverage-threshold": json.RawMessage(`40`),
	}, lcov50pct)

	status, _, err := runTest(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK (coverage above threshold), got %q", status)
	}
}

// TestRunTest_DefaultEnforcesCoverage pins the TS default, mirroring the Go
// path: a plain `putnami test` at 50% against a 90% threshold FAILS, with no
// flag passed. Under the old opt-in cadence this passed.
func TestRunTest_DefaultEnforcesCoverage(t *testing.T) {
	ctx := setupCoverageRun(t, pctx.Params{"coverage-threshold": json.RawMessage(`90`)}, lcov50pct)

	status, data, err := runTest(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED (the gate is on by default), got %q", status)
	}
	if _, ok := data["coverageSummary"]; !ok {
		t.Error("data has no coverageSummary; the default run must measure")
	}
}

// TestRunTest_NoEnforceStillMeasuresButNeverFails pins the TS escape hatch:
// --no-enforce-coverage keeps the measurement and the reported summary, but the
// threshold cannot fail the run.
func TestRunTest_NoEnforceStillMeasuresButNeverFails(t *testing.T) {
	ctx := setupCoverageRun(t, pctx.Params{
		"enforce-coverage":   json.RawMessage(`false`),
		"coverage-threshold": json.RawMessage(`90`),
	}, lcov50pct)

	status, data, err := runTest(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK (--no-enforce-coverage cannot fail), got %q", status)
	}
	if _, ok := data["coverageSummary"]; !ok {
		t.Error("data has no coverageSummary; the escape hatch must still measure")
	}
}

// TestRunTest_CoverageOptOutSkipsInstrumentation pins the TS opt-out: with
// coverage:false nothing is measured and the inherited threshold is inert, even
// though the gate now defaults on.
func TestRunTest_CoverageOptOutSkipsInstrumentation(t *testing.T) {
	ctx := setupCoverageRun(t, pctx.Params{
		"coverage":           json.RawMessage(`false`),
		"coverage-threshold": json.RawMessage(`90`),
	}, lcov50pct)

	status, data, err := runTest(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK (opt-out zeroes the threshold), got %q", status)
	}
	if _, ok := data["coverageSummary"]; ok {
		t.Errorf("data has coverageSummary %v, want none (coverage not collected)", data["coverageSummary"])
	}
}

func TestRunTest_CoverageThresholdNoData(t *testing.T) {
	// A threshold is set and --enforce-coverage is on but no coverage file is
	// produced → FAILED rather than silently passing.
	ctx := setupCoverageRun(t, pctx.Params{
		"enforce-coverage":   json.RawMessage(`true`),
		"coverage-threshold": json.RawMessage(`80`),
	}, "")

	status, _, err := runTest(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED (no coverage data), got %q", status)
	}
}

func TestRunTest_CoverageThresholdEmptyLCOV(t *testing.T) {
	// An lcov.info that exists but has no instrumented lines parses to
	// LineCoverage 100 over TotalLines 0. It must be treated as "no coverage
	// data" and fail the threshold, not pass as a spurious 100%.
	ctx := setupCoverageRun(t, pctx.Params{
		"enforce-coverage":   json.RawMessage(`true`),
		"coverage-threshold": json.RawMessage(`80`),
	}, lcovNoInstrumentedLines)

	status, _, err := runTest(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED (empty LCOV has no usable coverage data), got %q", status)
	}
}

// TestRunTest_CoverageOptOutSurvivesEnforce is the TS twin of the Go
// TestRun_CoverageThresholdSkippedWhenCoverageDisabled and the review
// regression: --enforce-coverage is a global cadence switch that must NOT override
// a project's coverage:false opt-out. A vocabulary-only project that opts out with
// an inherited threshold must pass under validation rather than failing with "no
// coverage data was produced".
func TestRunTest_CoverageOptOutSurvivesEnforce(t *testing.T) {
	ctx := setupCoverageRun(t, pctx.Params{
		"enforce-coverage":   json.RawMessage(`true`),
		"coverage-threshold": json.RawMessage(`80`),
		"coverage":           json.RawMessage(`false`),
	}, lcovNoInstrumentedLines)

	status, data, err := runTest(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK (coverage:false opt-out survives --enforce-coverage), got %q", status)
	}
	if _, ok := data["coverageSummary"]; ok {
		t.Errorf("data has coverageSummary %v, want none (opted out)", data["coverageSummary"])
	}
}

func TestCapitalize(t *testing.T) {
	cases := map[string]string{
		"":                "",
		"coverage 5% low": "Coverage 5% low",
		"Already":         "Already",
		"123":             "123",
	}
	for in, want := range cases {
		if got := capitalize(in); got != want {
			t.Errorf("capitalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- runLint ----

func TestRunLint_Success(t *testing.T) {
	mockBiomeResolution(t)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"diagnostics":[]}`}, nil
	})

	ctx, _ := makeTestCtx(t)
	emit := jsonl.New()

	status, _, err := runLint(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

func TestRunLint_FormatFails(t *testing.T) {
	mockBiomeResolution(t)

	callCount := 0
	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		callCount++
		if callCount == 1 {
			// Format fails
			return &exec.Result{Success: false, ExitCode: 1, Stdout: `{"diagnostics":[{"severity":"error","description":"bad format","file":"src/a.ts","line":1,"column":1,"category":"format"}]}`}, nil
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"diagnostics":[]}`}, nil
	})

	ctx, _ := makeTestCtx(t)
	emit := jsonl.New()

	status, _, err := runLint(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED when format fails, got %q", status)
	}
}

func TestRunLintFormat_Success(t *testing.T) {
	mockBiomeResolution(t)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"diagnostics":[]}`}, nil
	})

	ctx, _ := makeTestCtx(t)
	emit := jsonl.New()

	status, _, err := runLintFormat(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

func TestRunLintCheck_Success(t *testing.T) {
	mockBiomeResolution(t)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"diagnostics":[]}`}, nil
	})

	ctx, _ := makeTestCtx(t)
	emit := jsonl.New()

	status, _, err := runLintCheck(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

// ---- runWorkspaceInstall ----

func TestRunWorkspaceInstall_Success(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	// Create extension manifest
	os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"private":true}`), 0644)
	writeExtensionBiomeDefault(t, dir)

	mockAllExec(t, successExec)

	emit := jsonl.New()
	status, _, err := runWorkspaceInstall(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

func TestRunWorkspaceInstall_ExecFails(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(`{}`), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"private":true}`), 0644)
	writeExtensionBiomeDefault(t, dir)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "install failed"}, nil
	})

	emit := jsonl.New()
	status, _, err := runWorkspaceInstall(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED, got %q", status)
	}
}

// ---- runPackageNpm ----

func TestRunPackageNpm_Success(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	// Create build output in the workspace .putnami/out location
	buildDir := filepath.Join(dir, ".putnami", "out", "project", "build", "lib")
	os.MkdirAll(buildDir, 0755)
	os.WriteFile(filepath.Join(buildDir, "index.js"), []byte("exports.x = 1;"), 0644)

	// Create package.json with exports
	pkgJSON := `{"name":"@test/pkg","version":"1.0.0","exports":{"./index":"./src/index.ts"}}`
	os.WriteFile(filepath.Join(projectPath, "package.json"), []byte(pkgJSON), 0644)

	ctx.Version = &pctx.Version{SHA: "abc123", Branch: "main"}

	emit := jsonl.New()
	status, data, err := runPackageNpm(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
	if data["version"] == nil {
		t.Error("expected version in data")
	}
}

func TestRunPackageNpm_NoBuildOutput(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	pkgJSON := `{"name":"@test/pkg","version":"1.0.0"}`
	os.WriteFile(filepath.Join(projectPath, "package.json"), []byte(pkgJSON), 0644)

	ctx.Version = &pctx.Version{SHA: "abc123", Branch: "main"}

	emit := jsonl.New()
	status, _, err := runPackageNpm(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "SKIP" {
		t.Errorf("expected SKIP for no build output, got %q", status)
	}
}

// ---- runPackageDocker ----

func TestRunPackageDocker_DryRun(t *testing.T) {
	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	// Create compile output in the workspace .putnami/out location
	compileDir := filepath.Join(dir, ".putnami", "out", "project", "build", "compile")
	os.MkdirAll(compileDir, 0755)
	os.WriteFile(filepath.Join(compileDir, "serve-linux-x64"), []byte("binary"), 0755)

	os.WriteFile(filepath.Join(projectPath, "package.json"), []byte(`{"name":"@test/pkg","version":"1.0.0"}`), 0644)

	ctx.Version = &pctx.Version{SHA: "abc123", Branch: "main"}
	ctx.Params = pctx.Params{"dry-run": json.RawMessage(`true`), "port": json.RawMessage(`3000`)}

	emit := jsonl.New()
	status, _, err := runPackageDocker(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

// ---- runBuildGenerate ----

func TestRunBuildGenerate_Success(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	// Create src for content hash
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export const x = 1;"), 0644)

	emit := jsonl.New()
	status, data, err := runBuildGenerate(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
	if data["hash"] == nil {
		t.Error("expected hash in data")
	}
}

// ---- doGenerate ----

func TestCapabilityManifestForBundledServe_FailsClosedForServerLoaderWithoutManifest(t *testing.T) {
	path, generate, err := capabilityManifestForBundledServe(
		map[string]string{"api-loader": "/project/.gen/src/api.gen.ts"},
		map[string]string{},
	)
	if err == nil || !strings.Contains(err.Error(), "require schema/capabilities.json") {
		t.Fatalf("expected missing manifest error, got path=%q generate=%v err=%v", path, generate, err)
	}
}

func TestCapabilityManifestForBundledServe_ManifestIsAuthoritativeWithoutExportMap(t *testing.T) {
	want := "/project/.gen/schema/capabilities.json"
	path, generate, err := capabilityManifestForBundledServe(
		map[string]string{},
		map[string]string{"schema/capabilities.json": want},
	)
	if err != nil || !generate || path != want {
		t.Fatalf("path=%q generate=%v err=%v, want manifest-driven generation", path, generate, err)
	}
}

func TestCapabilityManifestForBundledServe_IgnoresClientAndNonCodeLoaders(t *testing.T) {
	path, generate, err := capabilityManifestForBundledServe(
		map[string]string{
			"react-client-loader": "/project/.gen/src/react-client.ts",
			"config-loader":       "/project/.gen/conf/.env.test.yaml",
		},
		map[string]string{},
	)
	if err != nil || generate || path != "" {
		t.Fatalf("path=%q generate=%v err=%v, want no bundled activation", path, generate, err)
	}
}

func TestDoGenerate_Success(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("const x = 1;"), 0644)

	emit := jsonl.New()
	result, err := doGenerate(ctx, emit, projectPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Hash == "" {
		t.Error("expected non-empty hash")
	}
	if result.Mode != "build" {
		t.Errorf("expected mode 'build', got %q", result.Mode)
	}
}

func TestDoGenerate_WithMode(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"mode": json.RawMessage(`"serve"`)}
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("const x = 1;"), 0644)

	emit := jsonl.New()
	result, err := doGenerate(ctx, emit, projectPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Mode != "serve" {
		t.Errorf("expected mode 'serve', got %q", result.Mode)
	}
}

func TestDoGenerate_SyncsCommittedInfraRequirements(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("const x = 1;"), 0o644); err != nil {
		t.Fatal(err)
	}

	depDir := filepath.Join(projectPath, "node_modules", "infra-ext")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","dependencies":{"infra-ext":"^1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		sidecar := filepath.Join(projectPath, ".gen", "infra", "database.json")
		if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sidecar,
			[]byte(`{"protocolVersion":2,"databases":[{"name":"primary","engine":"postgres","schemas":["app"]}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{}}` + "\n"}, nil
	})

	emit := jsonl.New()
	if _, err := doGenerate(ctx, emit, projectPath); err != nil {
		t.Fatalf("doGenerate: %v", err)
	}

	data, err := os.ReadFile(infra.ProjectRequirementsPath(projectPath))
	if err != nil {
		t.Fatalf("read committed infra requirements: %v", err)
	}
	var manifest infra.PerProjectManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse committed infra requirements: %v", err)
	}
	if len(manifest.Databases) != 1 || manifest.Databases[0].Name != "primary" {
		t.Fatalf("databases = %+v, want primary", manifest.Databases)
	}
}

// secretsFragment is the scratch fragment the @putnami/application preBuild hook
// writes from its activated config registry (bin/generate.ts →
// emitInfraRequirements). Canonical NAMES only: the emitter never has a value to
// serialize, and these tests assert the sync keeps it that way.
func secretsFragment(names ...string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, strconv.Quote(n))
	}
	return `{"$schema":"https://putnami.dev/schemas/putnami-infra.json","protocolVersion":2,"secrets":[` +
		strings.Join(quoted, ",") + `]}`
}

const databaseFragment = `{"protocolVersion":2,"databases":[{"name":"primary","engine":"postgres","schemas":["app"]}]}`

const migrationFragment = `{"protocolVersion":2,"databases":[{"name":"ledger","engine":"postgres","schemas":["ledger"]}]}`

// newInfraHookProject builds a project whose package.json declares one
// dependency that registers a preBuild hook, which is what makes doGenerate
// actually run hooks — and therefore run the committed infra sync.
func newInfraHookProject(t *testing.T) (*pctx.Context, string) {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("const x = 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	depDir := filepath.Join(projectPath, "node_modules", "infra-ext")
	if err := os.MkdirAll(depDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
		[]byte(`{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "package.json"),
		[]byte(`{"name":"@test/pkg","dependencies":{"infra-ext":"^1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return ctx, projectPath
}

// mockInfraFragmentHook makes the mocked preBuild hook write the given
// <slug>.json scratch fragments under <project>/.gen/infra, the way the real bun
// hooks do: @putnami/application writes secrets.json, the database and migration
// producers write their own slugs. Every call rewrites the same set, so a test
// can run doGenerate twice with different sets and watch the committed manifest
// converge on the second one.
func mockInfraFragmentHook(t *testing.T, projectPath string, fragments map[string]string) {
	t.Helper()
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		dir := filepath.Join(projectPath, ".gen", "infra")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		for slug, body := range fragments {
			if err := os.WriteFile(filepath.Join(dir, slug+".json"), []byte(body), 0o644); err != nil {
				return nil, err
			}
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{}}` + "\n"}, nil
	})
}

func readCommittedInfra(t *testing.T, projectPath string) infra.PerProjectManifest {
	t.Helper()
	data, err := os.ReadFile(infra.ProjectRequirementsPath(projectPath))
	if err != nil {
		t.Fatalf("read committed infra requirements: %v", err)
	}
	var manifest infra.PerProjectManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse committed infra requirements: %v", err)
	}
	return manifest
}

// TestDoGenerate_CommitsSecretNamesAlongsideOtherProducers is the lifecycle
// regression for the dropped-secret defect: the @putnami/application preBuild
// hook emitted its secrets fragment from a config registry that had not been
// activated (no generated-loader imports, no ConfigContributor walk), so the
// committed manifest carried the database requirement and nothing else while
// schema/config.json declared the sensitive field. The Go side owns the other
// half of that contract — one sync, after every producer, merging every slug.
func TestDoGenerate_CommitsSecretNamesAlongsideOtherProducers(t *testing.T) {
	mockBunResolution(t)
	ctx, projectPath := newInfraHookProject(t)
	mockInfraFragmentHook(t, projectPath, map[string]string{
		"database":  databaseFragment,
		"migration": migrationFragment,
		"secrets":   secretsFragment("analytics.secret", "billing.api_key"),
	})

	if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
		t.Fatalf("doGenerate: %v", err)
	}

	manifest := readCommittedInfra(t, projectPath)
	if !reflect.DeepEqual(manifest.Secrets, []string{"analytics.secret", "billing.api_key"}) {
		t.Fatalf("secrets = %+v, want both canonical names", manifest.Secrets)
	}
	// Adding secrets must not cost the other producers their resources: the sync
	// merges every fragment slug rather than replacing the committed file with
	// whichever producer wrote last.
	names := make([]string, 0, len(manifest.Databases))
	for _, db := range manifest.Databases {
		names = append(names, db.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"ledger", "primary"}) {
		t.Fatalf("databases = %+v, want ledger and primary to survive the merge", names)
	}
}

// TestDoGenerate_DropsOnlyTheRemovedSecret pins the removal half. The committed
// manifest is rebuilt from the fragments on every run — never patched — so a
// sensitive field deleted from the source removes exactly its own secret name
// and leaves every other requirement standing.
func TestDoGenerate_DropsOnlyTheRemovedSecret(t *testing.T) {
	mockBunResolution(t)
	ctx, projectPath := newInfraHookProject(t)

	mockInfraFragmentHook(t, projectPath, map[string]string{
		"database": databaseFragment,
		"secrets":  secretsFragment("analytics.secret", "billing.api_key"),
	})
	if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
		t.Fatalf("first doGenerate: %v", err)
	}

	mockInfraFragmentHook(t, projectPath, map[string]string{
		"database": databaseFragment,
		"secrets":  secretsFragment("analytics.secret"),
	})
	if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
		t.Fatalf("second doGenerate: %v", err)
	}

	manifest := readCommittedInfra(t, projectPath)
	if !reflect.DeepEqual(manifest.Secrets, []string{"analytics.secret"}) {
		t.Fatalf("secrets = %+v, want only analytics.secret", manifest.Secrets)
	}
	if len(manifest.Databases) != 1 || manifest.Databases[0].Name != "primary" {
		t.Fatalf("databases = %+v, want primary to survive the secret removal", manifest.Databases)
	}
}

// TestDoGenerate_DropsSecretsWhenTheProducerStops covers the producer that stops
// emitting entirely: ClearGeneratedRequirementSidecars runs before the hooks, so
// a fragment nobody rewrote cannot survive into the committed manifest. Without
// that ordering a workload that removed its last sensitive field would keep
// requiring a secret forever.
func TestDoGenerate_DropsSecretsWhenTheProducerStops(t *testing.T) {
	mockBunResolution(t)
	ctx, projectPath := newInfraHookProject(t)

	mockInfraFragmentHook(t, projectPath, map[string]string{
		"database": databaseFragment,
		"secrets":  secretsFragment("analytics.secret"),
	})
	if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
		t.Fatalf("first doGenerate: %v", err)
	}

	mockInfraFragmentHook(t, projectPath, map[string]string{"database": databaseFragment})
	if _, err := doGenerate(ctx, jsonl.New(), projectPath); err != nil {
		t.Fatalf("second doGenerate: %v", err)
	}

	manifest := readCommittedInfra(t, projectPath)
	if len(manifest.Secrets) != 0 {
		t.Fatalf("secrets = %+v, want none once the producer stopped emitting", manifest.Secrets)
	}
	if len(manifest.Databases) != 1 {
		t.Fatalf("databases = %+v, want the database requirement untouched", manifest.Databases)
	}
}

// TestDoGenerate_CommittedInfraIsBatchEqualToSolo pins the property the whole
// sidecar protocol rests on: build-generate may run concurrently for the build
// and the test pipeline, and batched for several projects in one process, so the
// committed bytes must depend only on the fragments — not on which path wrote
// them. Compared as BYTES because the committed manifest is reviewed in a diff.
func TestDoGenerate_CommittedInfraIsBatchEqualToSolo(t *testing.T) {
	mockBunResolution(t)

	fragments := map[string]string{
		"database": databaseFragment,
		"secrets":  secretsFragment("billing.api_key", "analytics.secret"),
	}

	soloCtx, soloPath := newInfraHookProject(t)
	mockInfraFragmentHook(t, soloPath, fragments)
	if _, err := doGenerate(soloCtx, jsonl.New(), soloPath); err != nil {
		t.Fatalf("solo doGenerate: %v", err)
	}
	solo, err := os.ReadFile(infra.ProjectRequirementsPath(soloPath))
	if err != nil {
		t.Fatalf("read solo committed manifest: %v", err)
	}

	batchCtx, batchPath := newInfraHookProject(t)
	mockInfraFragmentHook(t, batchPath, fragments)
	batchCtx.SelectedProjects = []pctx.ProjectRef{
		{ID: "/project", Name: "@test/pkg", Path: "project", FullPath: batchPath},
	}
	if _, _, err := runBuildGenerateBatch(batchCtx); err != nil {
		t.Fatalf("batch generate: %v", err)
	}
	batch, err := os.ReadFile(infra.ProjectRequirementsPath(batchPath))
	if err != nil {
		t.Fatalf("read batch committed manifest: %v", err)
	}

	if string(solo) != string(batch) {
		t.Fatalf("batch committed manifest != solo\nsolo:\n%s\nbatch:\n%s", solo, batch)
	}
	// The fragment listed its secrets unsorted; the committed bytes are sorted
	// and deterministic, which is what makes two writers interchangeable.
	if !strings.Contains(string(solo), `"analytics.secret",`) {
		t.Fatalf("committed manifest is not in sorted order:\n%s", solo)
	}
}

// ---- runServe ----

func TestRunServe_NoEntrypoint(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, _ := makeTestCtx(t)
	emit := jsonl.New()

	status, _, err := runServe(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "FAILED" {
		t.Errorf("expected FAILED for missing entrypoint, got %q", status)
	}
}

// ---- runBuild ----

func TestRunBuild_OnlyGenerate(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"generate": json.RawMessage(`true`)}
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export const x = 1;"), 0644)

	emit := jsonl.New()
	status, _, err := runBuild(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

func TestRunBuild_TranspileSuccess(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"transpile": json.RawMessage(`true`)}
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(projectPath, "package.json"), []byte(`{"name":"@test/pkg","exports":{"./index":"./src/index.ts"}}`), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		// Create transpile output
		libDir := filepath.Join(ctx.OutputPath, "lib")
		os.MkdirAll(libDir, 0755)
		os.WriteFile(filepath.Join(libDir, "index.js"), []byte("const x = 1;"), 0644)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	emit := jsonl.New()
	status, _, err := runBuild(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

func TestRunBuild_TypesSuccess(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"types": json.RawMessage(`true`)}
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export type Foo = string;"), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		typesDir := filepath.Join(ctx.OutputPath, "types")
		os.MkdirAll(typesDir, 0755)
		os.WriteFile(filepath.Join(typesDir, "index.d.ts"), []byte("export type Foo = string;"), 0644)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	emit := jsonl.New()
	status, _, err := runBuild(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

func TestRunBuild_CompileSuccess(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"compile": json.RawMessage(`true`)}
	// The entrypoint arrives in this extension's OWN namespaced probe metadata
	// since an earlier migration; job context v2 no longer carries `main`.
	// Seeding the superseded ctx.Project.Main still COMPILES (the wire type keeps
	// the doc-marked-superseded member) but resolves to no entrypoint, so this
	// test used to return at runBuild's empty-entrypoint guard and emit JSONL
	// byte-identical to its own _CompileNoEntrypoint twin.
	ctx.Project.Metadata = map[string]json.RawMessage{
		tsExtensionName: json.RawMessage(`{"main":"src/main.ts"}`),
	}
	projectPath := filepath.Join(dir, "project")
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "main.ts"), []byte("console.log('hi')"), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		for i, a := range args {
			if a == "--outfile" && i+1 < len(args) {
				os.MkdirAll(filepath.Dir(args[i+1]), 0755)
				os.WriteFile(args[i+1], []byte("binary"), 0755)
			}
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	events := captureEvents(t, func() {
		status, _, err := runBuild(ctx, jsonl.New(), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" {
			t.Errorf("expected OK, got %q", status)
		}
	})

	// "OK" alone cannot tell the compiled branch from the skipped one — both
	// return OK. The metric and the artifacts on disk only exist if RunCompile
	// actually ran.
	assertCompiledExecutables(t, events, len(build.DefaultBunTargets))
	assertCompileArtifacts(t, ctx.OutputPath, "main")
	if findEvent(events, func(e map[string]any) bool {
		return e["type"] == "log" && strings.Contains(str(e["message"]), "No compile entrypoint found")
	}) != nil {
		t.Error("the compile phase was skipped for want of an entrypoint")
	}
}

func TestRunBuild_CompileNoEntrypoint(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, _ := makeTestCtx(t)
	ctx.Params = pctx.Params{"compile": json.RawMessage(`true`)}

	events := captureEvents(t, func() {
		status, _, err := runBuild(ctx, jsonl.New(), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" {
			t.Errorf("expected OK (skip compile), got %q", status)
		}
	})

	// runBuild opens the phase before resolving, so the skip is visible as the
	// log line plus the ABSENCE of the metric — not as a missing phase.
	if findEvent(events, func(e map[string]any) bool {
		return e["type"] == "log" && strings.Contains(str(e["message"]), "No compile entrypoint found")
	}) == nil {
		t.Errorf("the skip was not reported; events = %v", events)
	}
	if findEvent(events, func(e map[string]any) bool {
		return e["type"] == "metric" && e["name"] == "compiled-executables"
	}) != nil {
		t.Errorf("a project with no entrypoint reported compiled executables; events = %v", events)
	}
}

// ---- runBuildTranspile ----

func TestRunBuildTranspile_Success(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export const x = 1;"), 0644)
	os.WriteFile(filepath.Join(projectPath, "package.json"), []byte(`{"name":"@test/pkg","exports":{"./index":"./src/index.ts"}}`), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		libDir := filepath.Join(ctx.OutputPath, "lib")
		os.MkdirAll(libDir, 0755)
		os.WriteFile(filepath.Join(libDir, "index.js"), []byte("const x = 1;"), 0644)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	emit := jsonl.New()
	status, _, err := runBuildTranspile(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

// ---- runBuildTypes ----

func TestRunBuildTypes_Success(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	projectPath := filepath.Join(dir, "project")

	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "index.ts"), []byte("export type Foo = string;"), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		typesDir := filepath.Join(ctx.OutputPath, "types")
		os.MkdirAll(typesDir, 0755)
		os.WriteFile(filepath.Join(typesDir, "index.d.ts"), []byte("export type Foo = string;"), 0644)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	emit := jsonl.New()
	status, _, err := runBuildTypes(ctx, emit, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "OK" {
		t.Errorf("expected OK, got %q", status)
	}
}

// ---- runBuildCompile ----

// captureEvents runs fn with os.Stdout pointed at a temp file and returns the
// JSONL events it emitted, decoded.
//
// A job's observable contract IS its event stream, and a "did the phase run?"
// question cannot be answered from the returned status: a skipped compile and a
// successful one both return OK. Writing to a file rather than a pipe avoids
// deadlocking on a stream larger than the pipe buffer.
func captureEvents(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	out, err := os.Create(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = out
	func() {
		defer func() {
			os.Stdout = original
			_ = out.Close()
		}()
		fn()
	}()

	data, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("emitted a line that is not JSONL: %q (%v)", line, err)
		}
		events = append(events, event)
	}
	return events
}

func findEvent(events []map[string]any, match func(map[string]any) bool) map[string]any {
	for _, event := range events {
		if match(event) {
			return event
		}
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// assertCompiledExecutables pins the compile phase's SUCCESS branch: the
// compiled-executables metric and the phase-end success event are emitted only
// after build.RunCompile returned without errors.
func assertCompiledExecutables(t *testing.T, events []map[string]any, want int) {
	t.Helper()
	metric := findEvent(events, func(e map[string]any) bool {
		return e["type"] == "metric" && e["name"] == "compiled-executables"
	})
	if metric == nil {
		t.Fatalf("no compiled-executables metric — the compile phase never ran; events = %v", events)
	}
	if value, _ := metric["value"].(float64); int(value) != want {
		t.Errorf("compiled-executables = %v, want %d", metric["value"], want)
	}
	if findEvent(events, func(e map[string]any) bool {
		return e["type"] == "phase" && e["name"] == "compile" &&
			str(e["action"]) == "end" && str(e["status"]) == "success"
	}) == nil {
		t.Errorf("no successful compile phase-end event; events = %v", events)
	}
}

// assertCompileArtifacts pins the tree effect: one executable per default bun
// target, under the output path's compile directory.
func assertCompileArtifacts(t *testing.T, outputPath, entrypointBase string) {
	t.Helper()
	for _, target := range build.DefaultBunTargets {
		artifact := filepath.Join(outputPath, "compile", entrypointBase+build.TargetSuffix(target))
		if _, err := os.Stat(artifact); err != nil {
			t.Errorf("compiled artifact %s is missing: %v", artifact, err)
		}
	}
}

func TestRunBuildCompile_Success(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	// See TestRunBuild_CompileSuccess: seeding the superseded ctx.Project.Main
	// compiles but resolves to nothing, so this test used to return at
	// runBuildCompile's empty-entrypoint guard without emitting a single compile
	// event — indistinguishable from its own _NoEntrypoint twin.
	ctx.Project.Metadata = map[string]json.RawMessage{
		tsExtensionName: json.RawMessage(`{"main":"src/main.ts"}`),
	}
	projectPath := filepath.Join(dir, "project")
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "main.ts"), []byte("console.log('hi')"), 0644)

	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		for i, a := range args {
			if a == "--outfile" && i+1 < len(args) {
				os.MkdirAll(filepath.Dir(args[i+1]), 0755)
				os.WriteFile(args[i+1], []byte("binary"), 0755)
			}
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	events := captureEvents(t, func() {
		status, _, err := runBuildCompile(ctx, jsonl.New(), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" {
			t.Errorf("expected OK, got %q", status)
		}
	})

	assertCompiledExecutables(t, events, len(build.DefaultBunTargets))
	assertCompileArtifacts(t, ctx.OutputPath, "main")
}

// TestRunBuildCompile_DockerChannelCompilesOneTarget is the TypeScript half of
// Channel scoping: `package --docker` compiles the image's bun target and
// nothing else.
//
// The package pipeline schedules this step only for the docker channel, and an
// image carries ONE executable — the three other targets were compiled and
// discarded on every packaged service. The assertion is on the `--target`
// arguments bun was actually invoked with, because that is the work being paid
// for, and on the resulting file name, because that is what the docker packager
// looks up by suffix.
func TestRunBuildCompile_DockerChannelCompilesOneTarget(t *testing.T) {
	mockBunResolution(t)

	ctx, dir := makeTestCtx(t)
	ctx.Project.Metadata = map[string]json.RawMessage{
		tsExtensionName: json.RawMessage(`{"main":"src/main.ts"}`),
	}
	ctx.Params = pctx.Params{
		"docker":   json.RawMessage(`true`),
		"platform": json.RawMessage(`"linux/arm64"`),
	}
	projectPath := filepath.Join(dir, "project")
	os.MkdirAll(filepath.Join(projectPath, "src"), 0755)
	os.WriteFile(filepath.Join(projectPath, "src", "main.ts"), []byte("console.log('hi')"), 0644)

	var compiledTargets []string
	mockAllExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		for i, a := range args {
			if a == "--target" && i+1 < len(args) {
				compiledTargets = append(compiledTargets, args[i+1])
			}
			if a == "--outfile" && i+1 < len(args) {
				os.MkdirAll(filepath.Dir(args[i+1]), 0755)
				os.WriteFile(args[i+1], []byte("binary"), 0755)
			}
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	events := captureEvents(t, func() {
		status, _, err := runBuildCompile(ctx, jsonl.New(), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" {
			t.Errorf("expected OK, got %q", status)
		}
	})

	if want := []string{"bun-linux-arm64"}; !reflect.DeepEqual(compiledTargets, want) {
		t.Errorf("bun compiled %v, want %v: the image consumes one executable", compiledTargets, want)
	}
	assertCompiledExecutables(t, events, 1)
	artifact := filepath.Join(ctx.OutputPath, "compile", "main-linux-arm64")
	if _, err := os.Stat(artifact); err != nil {
		t.Errorf("compiled artifact %s is missing: %v", artifact, err)
	}
}

func TestRunBuildCompile_NoEntrypoint(t *testing.T) {
	mockBunResolution(t)
	mockAllExec(t, successExec)

	ctx, _ := makeTestCtx(t)

	events := captureEvents(t, func() {
		status, _, err := runBuildCompile(ctx, jsonl.New(), nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" {
			t.Errorf("expected OK (skip compile), got %q", status)
		}
	})

	// Assert the SKIP, not just the status: OK is what the success branch
	// returns too, so without this the case is indistinguishable from
	// TestRunBuildCompile_Success.
	if findEvent(events, func(e map[string]any) bool { return e["type"] == "phase" && e["name"] == "compile" }) != nil {
		t.Errorf("a project with no entrypoint emitted a compile phase; events = %v", events)
	}
}
