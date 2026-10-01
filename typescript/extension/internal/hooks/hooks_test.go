package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
)

// ---- expandTemplates ----

func TestExpandTemplates_AllPlaceholders(t *testing.T) {
	s := "{workspaceRoot}/node_modules/{extensionRoot}/bin/{projectRoot}/out"
	got := expandTemplates(s, "/ws", "/proj", "/ext")
	want := "/ws/node_modules//ext/bin//proj/out"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

func TestExpandTemplates_NoPlaceholders(t *testing.T) {
	s := "some/plain/path"
	got := expandTemplates(s, "/ws", "/proj", "/ext")
	if got != s {
		t.Errorf("expandTemplates() = %q, want %q", got, s)
	}
}

func TestExpandTemplates_MultipleSamePlaceholder(t *testing.T) {
	s := "{extensionRoot}/a/{extensionRoot}/b"
	got := expandTemplates(s, "/ws", "/proj", "/ext")
	want := "/ext/a//ext/b"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

func TestExpandTemplates_EmptyString(t *testing.T) {
	got := expandTemplates("", "/ws", "/proj", "/ext")
	if got != "" {
		t.Errorf("expandTemplates() = %q, want empty", got)
	}
}

func TestExpandTemplates_OnlyExtensionRoot(t *testing.T) {
	got := expandTemplates("{extensionRoot}/bin/hook", "/ws", "/proj", "/my/ext")
	want := "/my/ext/bin/hook"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

func TestExpandTemplates_OnlyProjectRoot(t *testing.T) {
	got := expandTemplates("{projectRoot}/src", "/ws", "/my/proj", "/ext")
	want := "/my/proj/src"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

func TestExpandTemplates_OnlyWorkspaceRoot(t *testing.T) {
	got := expandTemplates("{workspaceRoot}/.putnami", "/my/ws", "/proj", "/ext")
	want := "/my/ws/.putnami"
	if got != want {
		t.Errorf("expandTemplates() = %q, want %q", got, want)
	}
}

// ---- parseSummary ----

func TestParseSummary_ValidSummary(t *testing.T) {
	output := `{"type":"log","data":{"message":"starting"}}
{"type":"summary","data":{"exports":{"./index":"./dist/index.js"},"assets":{"main.css":"./dist/main.css"}}}
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(exports))
	}
	if exports["./index"] != "./dist/index.js" {
		t.Errorf("exports[./index] = %q, want %q", exports["./index"], "./dist/index.js")
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if assets["main.css"] != "./dist/main.css" {
		t.Errorf("assets[main.css] = %q, want %q", assets["main.css"], "./dist/main.css")
	}
}

func TestParseSummary_NoSummaryEvent(t *testing.T) {
	output := `{"type":"log","data":{"message":"hello"}}
{"type":"progress","data":{"current":1,"total":3}}
`
	_, _, _, err := parseSummary(output)
	if err == nil {
		t.Fatal("expected error when no summary event is present")
	}
	if !strings.Contains(err.Error(), "no summary event") {
		t.Errorf("expected 'no summary event' in error, got %q", err.Error())
	}
}

func TestParseSummary_EmptyOutput(t *testing.T) {
	_, _, _, err := parseSummary("")
	if err == nil {
		t.Fatal("expected error for empty output")
	}
	if !strings.Contains(err.Error(), "no summary event") {
		t.Errorf("expected 'no summary event' in error, got %q", err.Error())
	}
}

func TestParseSummary_TruncatedSummaryLine(t *testing.T) {
	// A summary cut off mid-line (e.g. the emitting process exited before its
	// stdout pipe drained) must fail loudly, not silently drop the exports.
	full := `{"type":"summary","data":{"exports":{"static-loader":"/p/.gen/src/static/.static.gen.ts"},"assets":{"public/install.sh":"/p/.gen/public/install.sh"}}}`
	truncated := full[:len(full)-40]
	_, _, _, err := parseSummary(truncated + "\n")
	if err == nil {
		t.Fatal("expected error for truncated summary line")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("expected 'not valid JSON' in error, got %q", err.Error())
	}
}

func TestParseSummary_SummaryWithEmptyMaps(t *testing.T) {
	output := `{"type":"summary","data":{"exports":{},"assets":{}}}
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(exports) != 0 {
		t.Errorf("expected empty exports, got %d entries", len(exports))
	}
	if len(assets) != 0 {
		t.Errorf("expected empty assets, got %d entries", len(assets))
	}
}

func TestParseSummary_SummaryWithNullFields(t *testing.T) {
	output := `{"type":"summary","data":{"exports":null,"assets":null}}
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// parseSummary replaces nil maps with empty maps
	if exports == nil {
		t.Error("expected non-nil exports map")
	}
	if assets == nil {
		t.Error("expected non-nil assets map")
	}
}

func TestParseSummary_MultipleEventsFirstSummaryWins(t *testing.T) {
	output := `{"type":"log","data":{}}
{"type":"summary","data":{"exports":{"a":"1"},"assets":{"b":"2"}}}
{"type":"summary","data":{"exports":{"x":"9"},"assets":{"y":"8"}}}
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["a"] != "1" {
		t.Errorf("expected first summary exports, got %v", exports)
	}
	if assets["b"] != "2" {
		t.Errorf("expected first summary assets, got %v", assets)
	}
}

func TestParseSummary_InvalidJSONLines(t *testing.T) {
	output := `not json at all
{"type":"summary","data":{"exports":{"k":"v"},"assets":{}}}
also not json
`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["k"] != "v" {
		t.Errorf("expected exports[k]=v, got %v", exports)
	}
	if len(assets) != 0 {
		t.Errorf("expected empty assets, got %d entries", len(assets))
	}
}

func TestParseSummary_BlankLines(t *testing.T) {
	output := "\n\n{\"type\":\"summary\",\"data\":{\"exports\":{\"a\":\"b\"},\"assets\":{}}}\n\n"
	exports, _, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["a"] != "b" {
		t.Errorf("expected exports[a]=b, got %v", exports)
	}
}

func TestParseSummary_MultipleExportsAndAssets(t *testing.T) {
	output := `{"type":"summary","data":{"exports":{"./a":"./d/a.js","./b":"./d/b.js"},"assets":{"x.css":"./x.css","y.js":"./y.js"}}}`
	exports, assets, _, err := parseSummary(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(exports) != 2 {
		t.Fatalf("expected 2 exports, got %d", len(exports))
	}
	if len(assets) != 2 {
		t.Fatalf("expected 2 assets, got %d", len(assets))
	}
}

func TestParseSummary_LargeStaticAssetSummary(t *testing.T) {
	data := hookSummaryData{
		Exports: map[string]string{
			"api-loader":    "/project/.gen/src/api/.api-application.gen.ts",
			"static-loader": "/project/.gen/src/static/.static.gen.ts",
		},
		Assets: map[string]string{
			"public/install.sh": "/project/.gen/public/install.sh",
		},
	}
	for i := range 3000 {
		key := fmt.Sprintf("public/static/docs/page-%04d.html.gz", i)
		data.Assets[key] = "/project/.gen/" + key
	}
	event := struct {
		Type string          `json:"type"`
		Data hookSummaryData `json:"data"`
	}{
		Type: "summary",
		Data: data,
	}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if len(line) <= 64*1024 {
		t.Fatalf("test summary should exceed bufio.Scanner default token size, got %d bytes", len(line))
	}

	exports, assets, _, err := parseSummary(string(line) + "\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["api-loader"] == "" {
		t.Fatalf("expected api-loader export, got %v", exports)
	}
	if exports["static-loader"] == "" {
		t.Fatalf("expected static-loader export, got %v", exports)
	}
	if assets["public/install.sh"] == "" {
		t.Fatalf("expected install script asset, got %v", assets)
	}
}

// ---- discoverHooks ----

func TestDiscoverHooks_NoPackageJSON(t *testing.T) {
	dir := t.TempDir()
	hooks := discoverHooks("", dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks for missing package.json, got %d", len(hooks))
	}
}

func TestDiscoverHooks_NoDependencies(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0644)

	hooks := discoverHooks("", dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks with no deps, got %d", len(hooks))
	}
}

func TestDiscoverHooks_DependencyWithHook(t *testing.T) {
	dir := t.TempDir()

	// Create a dependency with a preBuild hook
	depDir := filepath.Join(dir, "node_modules", "@putnami", "web")
	os.MkdirAll(depDir, 0755)

	manifest := map[string]any{
		"hooks": map[string]any{
			"preBuild": map[string]any{
				"kind":    "command",
				"command": "bun",
				"args":    []string{"{extensionRoot}/bin/generate.ts"},
			},
		},
	}
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), manifestData, 0644)

	pkgJSON := `{"name":"test","dependencies":{"@putnami/web":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := discoverHooks("", dir, HookPreBuild)
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}
	if hooks[0].name != "@putnami/web" {
		t.Errorf("expected hook from '@putnami/web', got %q", hooks[0].name)
	}
	if hooks[0].hook.Command != "bun" {
		t.Errorf("expected command 'bun', got %q", hooks[0].hook.Command)
	}
}

func TestDiscoverHooks_DevDependencyWithHook(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "my-dev-ext")
	os.MkdirAll(depDir, 0755)

	manifest := `{"hooks":{"preBuild":{"kind":"command","command":"node","args":["generate.js"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)

	pkgJSON := `{"name":"test","devDependencies":{"my-dev-ext":"^2.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := discoverHooks("", dir, HookPreBuild)
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook from devDependencies, got %d", len(hooks))
	}
}

func TestDiscoverHooks_DependencyWithoutHook(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "some-lib")
	os.MkdirAll(depDir, 0755)

	// Manifest without preBuild hook
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(`{"hooks":{}}`), 0644)

	pkgJSON := `{"name":"test","dependencies":{"some-lib":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := discoverHooks("", dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks when no preBuild hook, got %d", len(hooks))
	}
}

func TestDiscoverHooks_DependencyWithoutManifest(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "plain-lib")
	os.MkdirAll(depDir, 0755)
	// No putnami.extension.json

	pkgJSON := `{"name":"test","dependencies":{"plain-lib":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := discoverHooks("", dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks when no manifest, got %d", len(hooks))
	}
}

func TestDiscoverHooks_InvalidManifestJSON(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "bad-manifest")
	os.MkdirAll(depDir, 0755)
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte("not json"), 0644)

	pkgJSON := `{"name":"test","dependencies":{"bad-manifest":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := discoverHooks("", dir, HookPreBuild)
	if len(hooks) != 0 {
		t.Errorf("expected 0 hooks for invalid JSON manifest, got %d", len(hooks))
	}
}

func TestDiscoverHooks_MultipleDepsWithHooks(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"ext-a", "ext-b"} {
		depDir := filepath.Join(dir, "node_modules", name)
		os.MkdirAll(depDir, 0755)
		manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["generate.ts"]}}}`
		os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	}

	pkgJSON := `{"name":"test","dependencies":{"ext-a":"^1.0.0","ext-b":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	hooks := discoverHooks("", dir, HookPreBuild)
	if len(hooks) != 2 {
		t.Errorf("expected 2 hooks, got %d", len(hooks))
	}
}

// ---- discoverHooks: ordering contract ----

// writeHookDependency registers dep as a project dependency whose extension
// manifest declares a preBuild hook at the given rank (order 0 is written as an
// absent key, which is what a manifest without the key looks like).
func writeHookDependency(t *testing.T, projectDir, dep string, order int) {
	t.Helper()
	depDir := filepath.Join(projectDir, "node_modules", filepath.FromSlash(dep))
	if err := os.MkdirAll(depDir, 0755); err != nil {
		t.Fatal(err)
	}
	hook := map[string]any{"kind": "command", "command": "bun", "args": []string{"generate.ts"}}
	if order != 0 {
		hook["order"] = order
	}
	manifest, err := json.Marshal(map[string]any{"hooks": map[string]any{"preBuild": hook}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
}

func hookNames(hooks []discoveredHook) []string {
	names := make([]string, 0, len(hooks))
	for _, h := range hooks {
		names = append(names, h.name)
	}
	return names
}

// TestDiscoverHooks_OrderIsDeterministicAndRankedFirst pins the exact sequence
// RunHooks will invoke, not merely "it is sorted".
//
// Hooks share one .gen tree, so the sequence IS the build contract: iterating
// the dependency map made it a per-process coin flip and half the fresh-tree
// builds died in capabilityManifestForBundledServe. Go randomizes map
// order per range, so one sample can pass by luck — with 8 ranked dependencies
// the odds that a single random permutation matches are 1/8!, and the loop below
// takes enough samples that a map-order implementation cannot survive it.
//
// The fixture is also chosen so a plain name sort fails: the two extensions that
// must run LAST ("@acme/aardvark", "@putnami/application") are the two whose
// names sort FIRST.
func TestDiscoverHooks_OrderIsDeterministicAndRankedFirst(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hook-order-determinism", "ranked-manifests-are-discovered-first")
	dir := t.TempDir()

	ranks := map[string]int{
		"@acme/aardvark":         100,
		"@putnami/application":   100,
		"@putnami/web":           0,
		"@putnami/ui":            0,
		"zzz-late-producer":      0,
		"aaa-early-producer":     -50,
		"@acme/very-early":       -100,
		"middle-of-the-alphabet": 0,
	}
	deps := make(map[string]string, len(ranks))
	for name, order := range ranks {
		writeHookDependency(t, dir, name, order)
		deps[name] = "^1.0.0"
	}
	pkgJSON, err := json.Marshal(map[string]any{"name": "test", "dependencies": deps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), pkgJSON, 0644); err != nil {
		t.Fatal(err)
	}

	want := []string{
		// order -100
		"@acme/very-early",
		// order -50
		"aaa-early-producer",
		// order 0, byte order
		"@putnami/ui",
		"@putnami/web",
		"middle-of-the-alphabet",
		"zzz-late-producer",
		// order 100, byte order
		"@acme/aardvark",
		"@putnami/application",
	}

	for attempt := range 64 {
		got := hookNames(discoverHooks("", dir, HookPreBuild))
		if len(got) != len(want) {
			t.Fatalf("attempt %d: discovered %d hooks, want %d (%v)", attempt, len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("attempt %d: hook order = %v, want %v", attempt, got, want)
			}
		}
	}
}

// TestDiscoverHooks_UnrankedManifestsKeepByteOrder covers every manifest written
// before the order key existed: they all rank 0, so the sequence is byte order
// and, most importantly, is the same on every run and every platform.
func TestDiscoverHooks_UnrankedManifestsKeepByteOrder(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hook-order-determinism", "unranked-manifests-keep-a-stable-order")
	dir := t.TempDir()

	for _, name := range []string{"ext-c", "ext-a", "ext-b"} {
		writeHookDependency(t, dir, name, 0)
	}
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"test","dependencies":{"ext-c":"^1.0.0","ext-a":"^1.0.0","ext-b":"^1.0.0"}}`), 0644)

	want := []string{"ext-a", "ext-b", "ext-c"}
	for attempt := range 32 {
		got := hookNames(discoverHooks("", dir, HookPreBuild))
		if len(got) != len(want) {
			t.Fatalf("attempt %d: discovered %v, want %v", attempt, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("attempt %d: hook order = %v, want %v", attempt, got, want)
			}
		}
	}
}

// TestRunHooks_InvokesInDiscoveredOrder proves the rank reaches invocation, not
// just discovery: RunHooks walks the slice, so a producer/finalizer inversion
// here is exactly the missing-capability-manifest failure.
func TestRunHooks_InvokesInDiscoveredOrder(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "hook-order-determinism", "invocation-follows-discovery-order")
	dir := t.TempDir()
	writeHookDependency(t, dir, "@putnami/application", 100)
	writeHookDependency(t, dir, "@putnami/web", 0)
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"name":"test","dependencies":{"@putnami/application":"^1.0.0","@putnami/web":"^1.0.0"}}`), 0644)

	var invoked []string
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		contextPath := args[len(args)-1]
		data, err := os.ReadFile(contextPath) //nolint:gosec // test fixture path
		if err != nil {
			return nil, err
		}
		var ctx hookContext
		if err := json.Unmarshal(data, &ctx); err != nil {
			return nil, err
		}
		invoked = append(invoked, ctx.Extension)
		return &exec.Result{
			Success: true,
			Stdout:  `{"type":"summary","data":{"exports":{},"assets":{}}}`,
		}, nil
	})

	if _, err := RunHooks("", dir, "test", HookPreBuild, "build", "bun", false, nil); err != nil {
		t.Fatalf("RunHooks: %v", err)
	}
	want := []string{"@putnami/web", "@putnami/application"}
	if len(invoked) != len(want) || invoked[0] != want[0] || invoked[1] != want[1] {
		t.Fatalf("invocation order = %v, want %v (the finalizer must observe the producer's output)", invoked, want)
	}
}

// ---- RunHooksPreBuild ----

// ---- mockExecRun helper ----

func withMockExec(t *testing.T, fn func(string, []string, ...exec.Option) (*exec.Result, error)) {
	t.Helper()
	orig := execRunFunc
	t.Cleanup(func() { execRunFunc = orig })
	execRunFunc = fn
}

// ---- invokeHook ----

func TestInvokeHook_Success(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	summaryJSON := `{"type":"summary","data":{"exports":{"./index":"./dist/index.js"},"assets":{"style.css":"./dist/style.css"}}}`

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summaryJSON + "\n"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"{extensionRoot}/bin/generate.ts"},
		},
	}

	exports, assets, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exports["./index"] != "./dist/index.js" {
		t.Errorf("expected export ./index=./dist/index.js, got %v", exports)
	}
	if assets["style.css"] != "./dist/style.css" {
		t.Errorf("expected asset style.css=./dist/style.css, got %v", assets)
	}
}

func TestInvokeHook_Failure(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "hook error"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"generate.ts"},
		},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Error("expected error on hook failure")
	}
	if !strings.Contains(err.Error(), "hook exited") {
		t.Errorf("expected 'hook exited' in error, got %q", err.Error())
	}
}

func TestInvokeHook_ExecError(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return nil, os.ErrNotExist
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"generate.ts"},
		},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Error("expected error on exec failure")
	}
}

func TestInvokeHook_BunCommand(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	var calledCommand string
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		calledCommand = name
		return &exec.Result{Success: true, ExitCode: 0, Stdout: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"generate.ts"},
		},
	}

	_, _, _, _ = invokeHook(dir, dir, "", "build", "/usr/bin/bun", h, false, nil)
	if calledCommand != "/usr/bin/bun" {
		t.Errorf("expected bun to be replaced with bunBin, got %q", calledCommand)
	}
}

func TestInvokeHook_CustomCommand(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	var calledCommand string
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		calledCommand = name
		return &exec.Result{Success: true, ExitCode: 0, Stdout: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "node",
			Args:    []string{"generate.js"},
		},
	}

	_, _, _, _ = invokeHook(dir, dir, "", "build", "/usr/bin/bun", h, false, nil)
	if calledCommand != "node" {
		t.Errorf("expected custom command 'node', got %q", calledCommand)
	}
}

func TestInvokeHook_CustomTimeout(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:      "command",
			Command:   "bun",
			Args:      []string{"gen.ts"},
			TimeoutMs: 30000,
		},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, true, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInvokeHook_SuccessWithoutSummaryFails(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"v":1,"type":"log","level":"info","message":"running"}` + "\n"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error when a successful hook emits no summary")
	}
	if !strings.Contains(err.Error(), "no summary event") {
		t.Errorf("expected 'no summary event' in error, got %q", err.Error())
	}
}

func TestInvokeHook_TruncatedSummaryFails(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	// Simulate a summary line cut off at a pipe-buffer boundary: the hook
	// process exited before stdout drained, so the line is not valid JSON.
	full := `{"v":1,"type":"summary","level":"info","message":"preBuild completed","data":{"exports":{"static-loader":"/p/.gen/src/static/.static.gen.ts"},"assets":{"public/install.sh":"/p/.gen/public/install.sh"}}}`
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: full[:128]}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error for truncated summary output")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("expected 'not valid JSON' in error, got %q", err.Error())
	}
}

func TestInvokeHook_TemplateExpansionInArgs(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	var calledArgs []string
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		calledArgs = args
		return &exec.Result{Success: true, ExitCode: 0, Stdout: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"{extensionRoot}/bin/gen.ts", "--project={projectRoot}"},
		},
	}

	_, _, _, _ = invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	// Check that templates were expanded
	foundExpandedExt := false
	for _, a := range calledArgs {
		if strings.Contains(a, extRoot) {
			foundExpandedExt = true
		}
		if strings.Contains(a, "{extensionRoot}") {
			t.Errorf("template not expanded in arg: %q", a)
		}
	}
	if !foundExpandedExt {
		t.Error("expected extensionRoot template to be expanded in args")
	}
}

func TestInvokeHook_LongStderrTruncated(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	longStderr := strings.Repeat("x", 5000)
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: longStderr}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook: &hookDefinition{
			Kind:    "command",
			Command: "bun",
			Args:    []string{"gen.ts"},
		},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error on failure")
	}
	// Error message should contain truncated stderr (last 2000 chars) plus prefix
	errMsg := err.Error()
	if len(errMsg) > 2100 {
		t.Errorf("expected truncated error message, got length %d", len(errMsg))
	}
	if len(errMsg) < 2000 {
		t.Errorf("expected at least 2000 chars from truncated stderr, got length %d", len(errMsg))
	}
}

func TestInvokeHook_FailureSurfacesStdoutErrorEvent(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	stdout := `{"v":1,"type":"meta","level":"info","message":"Starting preBuild","data":{"extension":"@putnami/web","hook":"preBuild"}}
{"v":1,"type":"error","level":"error","message":"react-client.gen.tsx: Could not resolve './missing'","data":{"code":"BuildError","stack":"Error: Could not resolve './missing'\n    at Bundler.build (/path/to/bundler.ts:42:11)"}}
`

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: stdout, Stderr: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@putnami/web",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error on hook failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Could not resolve './missing'") {
		t.Errorf("expected stdout error message to be surfaced, got %q", msg)
	}
	if !strings.Contains(msg, "Bundler.build") {
		t.Errorf("expected first stack frame to be surfaced, got %q", msg)
	}
}

func TestInvokeHook_FailureFallsBackToStderrWhenNoErrorEvent(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{
			Success:  false,
			ExitCode: 1,
			Stdout:   `{"v":1,"type":"log","level":"info","message":"running"}` + "\n",
			Stderr:   "bun: command failed",
		}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bun: command failed") {
		t.Errorf("expected stderr fallback, got %q", err.Error())
	}
}

func TestInvokeHook_FailureSurfacesBothStdoutErrorAndStderr(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	stdout := `{"v":1,"type":"error","level":"error","message":"build failed"}` + "\n"
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: stdout, Stderr: "warning: deprecated API"}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "build failed") {
		t.Errorf("expected stdout error message, got %q", msg)
	}
	if !strings.Contains(msg, "warning: deprecated API") {
		t.Errorf("expected stderr to also be included, got %q", msg)
	}
}

func TestInvokeHook_FailureNoOutputAtAll(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: "", Stderr: ""}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no diagnostic output") {
		t.Errorf("expected diagnostic hint when no output, got %q", err.Error())
	}
}

func TestInvokeHook_FailureMultipleErrorEvents(t *testing.T) {
	dir := t.TempDir()
	extRoot := filepath.Join(dir, "ext")
	os.MkdirAll(extRoot, 0755)

	stdout := `{"v":1,"type":"error","level":"error","message":"first failure"}
{"v":1,"type":"error","level":"error","message":"second failure"}
`
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: stdout}, nil
	})

	h := discoveredHook{
		kind:          HookPreBuild,
		name:          "@test/ext",
		extensionRoot: extRoot,
		hook:          &hookDefinition{Kind: "command", Command: "bun", Args: []string{"gen.ts"}},
	}

	_, _, _, err := invokeHook(dir, dir, "", "build", "bun", h, false, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "first failure") || !strings.Contains(msg, "second failure") {
		t.Errorf("expected both error messages, got %q", msg)
	}
}

// ---- RunHooksPreBuild with mocked exec ----

func TestRunHooksPreBuild_WithHookSuccess(t *testing.T) {
	dir := t.TempDir()

	// Set up a dependency with a preBuild hook
	depDir := filepath.Join(dir, "node_modules", "my-ext")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"my-ext":"^1.0.0"}}`), 0644)

	summaryJSON := `{"type":"summary","data":{"exports":{"loader":"./loader.js"},"assets":{"css":"./style.css"}}}`
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summaryJSON + "\n"}, nil
	})

	result, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Exports["loader"] != "./loader.js" {
		t.Errorf("expected export loader=./loader.js, got %v", result.Exports)
	}
	if result.Assets["css"] != "./style.css" {
		t.Errorf("expected asset css=./style.css, got %v", result.Assets)
	}
}

func TestRunHooksPreBuild_HookError(t *testing.T) {
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "bad-ext")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"bad-ext":"^1.0.0"}}`), 0644)

	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		return nil, os.ErrPermission
	})

	_, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err == nil {
		t.Error("expected error when hook fails")
	}
	if !strings.Contains(err.Error(), "bad-ext") {
		t.Errorf("expected hook name in error, got %q", err.Error())
	}
}

func TestRunHooksPreBuild_MergesMultipleHooks(t *testing.T) {
	dir := t.TempDir()

	// Two hooks
	for i, name := range []string{"ext-a", "ext-b"} {
		depDir := filepath.Join(dir, "node_modules", name)
		os.MkdirAll(depDir, 0755)
		manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
		os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
		_ = i
	}
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"ext-a":"^1.0.0","ext-b":"^1.0.0"}}`), 0644)

	callCount := 0
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		callCount++
		key := "a"
		if callCount == 2 {
			key = "b"
		}
		summary := `{"type":"summary","data":{"exports":{"` + key + `":"` + key + `.js"},"assets":{}}}`
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summary + "\n"}, nil
	})

	result, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Exports) != 2 {
		t.Errorf("expected 2 merged exports, got %d: %v", len(result.Exports), result.Exports)
	}
	if result.HookCount != 2 {
		t.Errorf("expected HookCount=2, got %d", result.HookCount)
	}
}

func TestRunHooksPreBuild_RejectsConflictingExportKeys(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ext-a", "ext-b"} {
		depDir := filepath.Join(dir, "node_modules", name)
		if err := os.MkdirAll(depDir, 0755); err != nil {
			t.Fatal(err)
		}
		manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["gen.ts"]}}}`
		if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"ext-a":"^1.0.0","ext-b":"^1.0.0"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	callCount := 0
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		callCount++
		path := fmt.Sprintf(".gen/src/loader-%d.ts", callCount)
		summary := `{"type":"summary","data":{"exports":{"api-loader":"` + path + `"},"assets":{}}}`
		return &exec.Result{Success: true, ExitCode: 0, Stdout: summary + "\n"}, nil
	})

	_, err := RunHooks(dir, dir, "", HookPreBuild, "build", "bun", false, nil)
	if err == nil || !strings.Contains(err.Error(), `export "api-loader" conflicts`) {
		t.Fatalf("expected conflicting export error, got %v", err)
	}
}

func TestRunHooksPreBuild_NoHooks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test"}`), 0644)

	result, err := RunHooks("", dir, "", HookPreBuild, "", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if len(result.Exports) != 0 {
		t.Errorf("expected empty exports, got %d", len(result.Exports))
	}
	if len(result.Assets) != 0 {
		t.Errorf("expected empty assets, got %d", len(result.Assets))
	}
	if result.HookCount != 0 {
		t.Errorf("expected HookCount=0, got %d", result.HookCount)
	}
}

// ---- discoverHooks: configExtract kind ----

func TestDiscoverHooks_ConfigExtractKind(t *testing.T) {
	// A dependency that registers BOTH preBuild and configExtract hooks
	// should be discoverable under either kind, independently.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["pre.ts"]},"configExtract":{"kind":"command","command":"bun","args":["extract.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)

	pkg := `{"name":"test","dependencies":{"@putnami/application":"workspace:*"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0644)

	pre := discoverHooks("", dir, HookPreBuild)
	if len(pre) != 1 || pre[0].kind != HookPreBuild {
		t.Errorf("expected 1 preBuild hook, got %d", len(pre))
	}

	ext := discoverHooks("", dir, HookConfigExtract)
	if len(ext) != 1 || ext[0].kind != HookConfigExtract {
		t.Errorf("expected 1 configExtract hook with kind=%q, got %d (kind=%q)", HookConfigExtract, len(ext), kindOrEmpty(ext))
	}
	if ext[0].hook.Args[0] != "extract.ts" {
		t.Errorf("expected configExtract args[0]='extract.ts', got %q", ext[0].hook.Args[0])
	}
}

func TestDiscoverHooks_OnlyOneKindRegistered(t *testing.T) {
	// A dependency that registers configExtract only should not surface
	// under preBuild — and vice versa.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "extract-only")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)

	pkg := `{"name":"test","dependencies":{"extract-only":"^1.0.0"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0644)

	if got := discoverHooks("", dir, HookPreBuild); len(got) != 0 {
		t.Errorf("expected 0 preBuild hooks, got %d", len(got))
	}
	if got := discoverHooks("", dir, HookConfigExtract); len(got) != 1 {
		t.Errorf("expected 1 configExtract hook, got %d", len(got))
	}
}

func kindOrEmpty(hooks []discoveredHook) string {
	if len(hooks) == 0 {
		return ""
	}
	return hooks[0].kind
}

// ---- RunHooks: configExtract dispatch ----

func TestRunHooks_ConfigExtractForwardsKind(t *testing.T) {
	// Verify the hook kind makes it into the JSON context the subprocess
	// reads — that's what lets a multi-hook extension script dispatch
	// to the right handler.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	var capturedContext hookContext
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		// The runner appends --putnami-context <path> after the script args.
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--putnami-context" {
				data, err := os.ReadFile(args[i+1])
				if err == nil {
					_ = json.Unmarshal(data, &capturedContext)
				}
			}
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	result, err := RunHooks(dir, dir, "", HookConfigExtract, "config-extract", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.HookCount != 1 {
		t.Errorf("expected HookCount=1, got %d", result.HookCount)
	}
	if capturedContext.Hook != HookConfigExtract {
		t.Errorf("expected context.hook=%q, got %q", HookConfigExtract, capturedContext.Hook)
	}
	// A summary without a status yields "" — legacy hooks stay trusted.
	if len(result.Statuses) != 1 || result.Statuses[0] != "" {
		t.Errorf("expected Statuses=[\"\"], got %v", result.Statuses)
	}
}

func TestRunHooks_ForwardsProjectName(t *testing.T) {
	// The putnami project identity (ctx.Project.Name) must reach the bun hook
	// through the context file so the config-extract hook can write it as the
	// manifest appName instead of the npm package.json name.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"auth.example.com","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	var rawContext map[string]any
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--putnami-context" {
				if data, err := os.ReadFile(args[i+1]); err == nil {
					_ = json.Unmarshal(data, &rawContext)
				}
			}
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	const projectName = "identity/workloads/auth-server"
	if _, err := RunHooks(dir, dir, projectName, HookConfigExtract, "config-extract", "bun", false, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := rawContext["projectName"].(string); got != projectName {
		t.Errorf("expected context.projectName=%q (the putnami identity, not the package.json name), got %q", projectName, got)
	}
}

func TestRunHooks_OmitsEmptyProjectName(t *testing.T) {
	// Backward compatibility: when no project identity is supplied the field is
	// omitempty on the wire, so a context file from an older runner is
	// byte-identical to the pre-fix one and the TS side falls back to
	// getCurrentProject().name.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	var rawContext map[string]any
	withMockExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--putnami-context" {
				if data, err := os.ReadFile(args[i+1]); err == nil {
					_ = json.Unmarshal(data, &rawContext)
				}
			}
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	if _, err := RunHooks(dir, dir, "", HookConfigExtract, "config-extract", "bun", false, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := rawContext["projectName"]; present {
		t.Errorf("expected projectName absent from context when empty, got %v", rawContext["projectName"])
	}
}

func TestRunHooks_CollectsSummaryStatus(t *testing.T) {
	// The config-extract runner distinguishes "hook regenerated its output"
	// from "hook ran but produced nothing" via the summary status — a stale
	// file on disk cannot make that distinction.
	dir := t.TempDir()

	depDir := filepath.Join(dir, "node_modules", "@putnami", "application")
	os.MkdirAll(depDir, 0755)
	manifest := `{"hooks":{"configExtract":{"kind":"command","command":"bun","args":["e.ts"]}}}`
	os.WriteFile(filepath.Join(depDir, "putnami.extension.json"), []byte(manifest), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"@putnami/application":"workspace:*"}}`), 0644)

	withMockExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true, ExitCode: 0,
			Stdout: `{"type":"summary","data":{"exports":{},"assets":{},"status":"empty"}}` + "\n"}, nil
	})

	result, err := RunHooks(dir, dir, "", HookConfigExtract, "config-extract", "bun", false, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Statuses) != 1 || result.Statuses[0] != "empty" {
		t.Errorf("expected Statuses=[\"empty\"], got %v", result.Statuses)
	}
}
