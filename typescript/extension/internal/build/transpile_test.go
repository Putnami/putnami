package build

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/exec"
)

// ---- defaultEntrypoints ----

func TestDefaultEntrypoints_MainTs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export {}"), 0644)

	result := defaultEntrypoints(dir)
	if len(result) != 1 || result[0] != "src/main.ts" {
		t.Errorf("expected ['src/main.ts'], got %v", result)
	}
}

func TestDefaultEntrypoints_IndexTs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)

	result := defaultEntrypoints(dir)
	if len(result) != 1 || result[0] != "src/index.ts" {
		t.Errorf("expected ['src/index.ts'], got %v", result)
	}
}

func TestDefaultEntrypoints_LibTs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "lib.ts"), []byte("export {}"), 0644)

	result := defaultEntrypoints(dir)
	if len(result) != 1 || result[0] != "src/lib.ts" {
		t.Errorf("expected ['src/lib.ts'], got %v", result)
	}
}

func TestDefaultEntrypoints_PriorityOrder(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	// Create all three — main.ts should win
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "lib.ts"), []byte("export {}"), 0644)

	result := defaultEntrypoints(dir)
	if len(result) != 1 || result[0] != "src/main.ts" {
		t.Errorf("expected ['src/main.ts'] (first in priority), got %v", result)
	}
}

func TestDefaultEntrypoints_IndexWinsOverLib(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "lib.ts"), []byte("export {}"), 0644)

	result := defaultEntrypoints(dir)
	if len(result) != 1 || result[0] != "src/index.ts" {
		t.Errorf("expected ['src/index.ts'] (second in priority), got %v", result)
	}
}

func TestDefaultEntrypoints_NoSourceFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)

	result := defaultEntrypoints(dir)
	if result != nil {
		t.Errorf("expected nil when no entrypoints found, got %v", result)
	}
}

func TestDefaultEntrypoints_NoSrcDir(t *testing.T) {
	dir := t.TempDir()

	result := defaultEntrypoints(dir)
	if result != nil {
		t.Errorf("expected nil when src/ dir missing, got %v", result)
	}
}

// ---- resolveEntrypointPlan ----

// resolveEntrypoints flattens the entrypoint plan for assertions that only care
// about the full set, not the graph a given entrypoint landed in.
func resolveEntrypoints(projectPath string) []string {
	plan := resolveEntrypointPlan(projectPath)
	return append(append([]string{}, plan.Server...), plan.Browser...)
}

func TestResolveEntrypoints_FallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)

	// No package.json — falls back to defaultEntrypoints
	result := resolveEntrypoints(dir)
	if len(result) != 1 || result[0] != "src/index.ts" {
		t.Errorf("expected fallback to ['src/index.ts'], got %v", result)
	}
}

func TestResolveEntrypoints_NoExportsFallsBack(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export {}"), 0644)

	// package.json without exports
	pkgJSON := `{"name": "test", "version": "1.0.0"}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveEntrypoints(dir)
	if len(result) != 1 || result[0] != "src/main.ts" {
		t.Errorf("expected fallback to ['src/main.ts'], got %v", result)
	}
}

func TestResolveEntrypoints_StringExports(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)

	pkgJSON := `{"name": "test", "exports": {"./index": "./src/index.ts"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveEntrypoints(dir)
	if len(result) != 1 || result[0] != "./src/index.ts" {
		t.Errorf("expected ['./src/index.ts'], got %v", result)
	}
}

func TestResolveEntrypoints_MultipleExports(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "serve.ts"), []byte("export {}"), 0644)

	pkgJSON := `{"name": "test", "exports": {"./index": "./src/index.ts", "./serve": "./src/serve.ts"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveEntrypoints(dir)
	sort.Strings(result)
	if len(result) != 2 {
		t.Fatalf("expected 2 entrypoints, got %d: %v", len(result), result)
	}
	expected := []string{"./src/index.ts", "./src/serve.ts"}
	sort.Strings(expected)
	for i, exp := range expected {
		if result[i] != exp {
			t.Errorf("result[%d] = %q, want %q", i, result[i], exp)
		}
	}
}

func TestResolveEntrypoints_ConditionExports(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)

	pkgJSON := `{"name": "test", "exports": {"./index": {"default": "./src/index.ts"}}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveEntrypoints(dir)
	if len(result) != 1 || result[0] != "./src/index.ts" {
		t.Errorf("expected ['./src/index.ts'] from condition export, got %v", result)
	}
}

func TestResolveEntrypoints_ExportsPointToMissingFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("export {}"), 0644)

	// Exports point to files that don't exist — should fall back to defaultEntrypoints
	pkgJSON := `{"name": "test", "exports": {"./index": "./src/nonexistent.ts"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveEntrypoints(dir)
	if len(result) != 1 || result[0] != "src/main.ts" {
		t.Errorf("expected fallback to ['src/main.ts'] for missing export files, got %v", result)
	}
}

// ---- resolveExternals ----

func TestResolveExternals_NoPackageJSON(t *testing.T) {
	dir := t.TempDir()

	result := resolveExternals(dir)
	if result != nil {
		t.Errorf("expected nil for missing package.json, got %v", result)
	}
}

func TestResolveExternals_Dependencies(t *testing.T) {
	dir := t.TempDir()

	pkgJSON := `{
		"name": "test",
		"dependencies": {
			"react": "^18.0.0",
			"express": "^4.0.0"
		}
	}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveExternals(dir)
	sort.Strings(result)
	if len(result) != 2 {
		t.Fatalf("expected 2 externals, got %d: %v", len(result), result)
	}
	if result[0] != "express" || result[1] != "react" {
		t.Errorf("expected ['express', 'react'], got %v", result)
	}
}

func TestResolveExternals_PeerDependencies(t *testing.T) {
	dir := t.TempDir()

	pkgJSON := `{
		"name": "test",
		"peerDependencies": {
			"react": "^18.0.0"
		}
	}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveExternals(dir)
	if len(result) != 1 || result[0] != "react" {
		t.Errorf("expected ['react'], got %v", result)
	}
}

func TestResolveExternals_MergedAndDeduplicated(t *testing.T) {
	dir := t.TempDir()

	pkgJSON := `{
		"name": "test",
		"dependencies": {
			"react": "^18.0.0",
			"express": "^4.0.0"
		},
		"peerDependencies": {
			"react": "^18.0.0",
			"vue": "^3.0.0"
		}
	}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveExternals(dir)
	sort.Strings(result)
	// react appears in both deps and peerDeps but should be deduplicated
	expected := []string{"express", "react", "vue"}
	if len(result) != len(expected) {
		t.Fatalf("expected %d externals, got %d: %v", len(expected), len(result), result)
	}
	for i, exp := range expected {
		if result[i] != exp {
			t.Errorf("result[%d] = %q, want %q", i, result[i], exp)
		}
	}
}

func TestResolveExternals_NoDependencies(t *testing.T) {
	dir := t.TempDir()

	pkgJSON := `{"name": "test", "version": "1.0.0"}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveExternals(dir)
	if len(result) != 0 {
		t.Errorf("expected empty externals, got %v", result)
	}
}

func TestResolveExternals_DevDepsNotIncluded(t *testing.T) {
	dir := t.TempDir()

	pkgJSON := `{
		"name": "test",
		"dependencies": {"react": "^18.0.0"},
		"devDependencies": {"typescript": "^5.0.0"}
	}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	result := resolveExternals(dir)
	if len(result) != 1 || result[0] != "react" {
		t.Errorf("expected only ['react'] (devDeps excluded), got %v", result)
	}
}

// ---- resolveBinEntrypoints ----

func TestResolveBinEntrypoints_IncludesBinFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0755)
	os.WriteFile(filepath.Join(dir, "bin", "generate.ts"), []byte("console.log('gen')"), 0644)

	pkgJSON := `{
		"name": "test",
		"bin": {"my-gen": "./bin/generate.ts"},
		"exports": {".": "./src/index.ts"}
	}`
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	eps := resolveEntrypoints(dir)
	sort.Strings(eps)

	expected := []string{"./bin/generate.ts", "./src/index.ts"}
	sort.Strings(expected)

	if len(eps) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, eps)
	}
	for i, ep := range eps {
		if ep != expected[i] {
			t.Errorf("entrypoint[%d] = %q, want %q", i, ep, expected[i])
		}
	}
}

func TestResolveBinEntrypoints_NoBin(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)

	pkgJSON := `{"name": "test", "exports": {".": "./src/index.ts"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	eps := resolveEntrypoints(dir)
	if len(eps) != 1 || eps[0] != "./src/index.ts" {
		t.Errorf("expected ['./src/index.ts'], got %v", eps)
	}
}

func TestResolveBinEntrypoints_MissingBinFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)

	pkgJSON := `{
		"name": "test",
		"bin": {"my-gen": "./bin/generate.ts"},
		"exports": {".": "./src/index.ts"}
	}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	eps := resolveEntrypoints(dir)
	// Bin file doesn't exist, so only exports entry
	if len(eps) != 1 || eps[0] != "./src/index.ts" {
		t.Errorf("expected ['./src/index.ts'], got %v", eps)
	}
}

// ---- browser/server graph partition ----

// writeBrowserConditionProject lays out a package shaped like @putnami/web:
// a "." export with a browser condition next to the default one, plus a bin.
func writeBrowserConditionProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.MkdirAll(filepath.Join(dir, "bin"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "index.browser.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "bin", "generate.ts"), []byte("export {}"), 0644)
	pkgJSON := `{
		"name": "test",
		"bin": {"gen": "./bin/generate.ts"},
		"exports": {
			".": {"browser": "./src/index.browser.ts", "default": "./src/index.ts"},
			"./testing": "./src/testing.ts"
		}
	}`
	os.WriteFile(filepath.Join(dir, "src", "testing.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)
	return dir
}

func TestResolveEntrypointPlan_BrowserConditionGetsOwnGraph(t *testing.T) {
	dir := writeBrowserConditionProject(t)

	plan := resolveEntrypointPlan(dir)

	wantServer := []string{"./src/index.ts", "./src/testing.ts", "./bin/generate.ts"}
	if !reflect.DeepEqual(plan.Server, wantServer) {
		t.Errorf("Server = %v, want %v", plan.Server, wantServer)
	}
	wantBrowser := []string{"./src/index.browser.ts"}
	if !reflect.DeepEqual(plan.Browser, wantBrowser) {
		t.Errorf("Browser = %v, want %v", plan.Browser, wantBrowser)
	}

	graphs := plan.graphs()
	if len(graphs) != 2 {
		t.Fatalf("expected 2 build graphs, got %d: %+v", len(graphs), graphs)
	}
	if graphs[0].name != "server" || graphs[0].browser {
		t.Errorf("graph[0] = %+v, want the non-browser server graph", graphs[0])
	}
	if graphs[1].name != "browser" || !graphs[1].browser {
		t.Errorf("graph[1] = %+v, want the browser graph", graphs[1])
	}
}

func TestResolveEntrypointPlan_NoBrowserConditionYieldsOneGraph(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "serve.ts"), []byte("export {}"), 0644)
	pkgJSON := `{"name":"test","exports":{".":{"default":"./src/index.ts"},"./serve":"./src/serve.ts"}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	plan := resolveEntrypointPlan(dir)
	if len(plan.Browser) != 0 {
		t.Errorf("expected no browser graph, got %v", plan.Browser)
	}
	graphs := plan.graphs()
	if len(graphs) != 1 || graphs[0].browser {
		t.Fatalf("expected exactly one non-browser graph, got %+v", graphs)
	}
}

func TestResolveEntrypointPlan_SharedEntryStaysOnServerGraph(t *testing.T) {
	// The same file behind both a browser and a non-browser condition must not
	// be silently relocated: the server graph keeps it, and it is not built twice.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	pkgJSON := `{"name":"test","exports":{".":{"browser":"./src/index.ts","default":"./src/index.ts"}}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	plan := resolveEntrypointPlan(dir)
	if !reflect.DeepEqual(plan.Server, []string{"./src/index.ts"}) {
		t.Errorf("Server = %v, want ['./src/index.ts']", plan.Server)
	}
	if len(plan.Browser) != 0 {
		t.Errorf("expected no browser graph for a shared entry, got %v", plan.Browser)
	}
}

func TestResolveEntrypointPlan_ReactNativeJoinsBrowserGraph(t *testing.T) {
	// @putnami/events shape: react-native and browser point at the same file.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "src", "index.browser.ts"), []byte("export {}"), 0644)
	pkgJSON := `{"name":"test","exports":{".":{"react-native":"./src/index.browser.ts","browser":"./src/index.browser.ts","default":"./src/index.ts"}}}`
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkgJSON), 0644)

	plan := resolveEntrypointPlan(dir)
	if !reflect.DeepEqual(plan.Server, []string{"./src/index.ts"}) {
		t.Errorf("Server = %v, want ['./src/index.ts']", plan.Server)
	}
	if !reflect.DeepEqual(plan.Browser, []string{"./src/index.browser.ts"}) {
		t.Errorf("Browser = %v, want ['./src/index.browser.ts']", plan.Browser)
	}
}

func TestResolveEntrypointPlan_Deterministic(t *testing.T) {
	// Entrypoint order drives bun's chunk layout, so it must not depend on Go
	// map iteration order.
	dir := writeBrowserConditionProject(t)
	first := resolveEntrypointPlan(dir)
	for i := 0; i < 20; i++ {
		got := resolveEntrypointPlan(dir)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("iteration %d produced %+v, want %+v", i, got, first)
		}
	}
}

// ---- RunTranspile graph isolation ----

func captureTranspileInvocations(t *testing.T, fail func(call int) *exec.Result) *[][]string {
	t.Helper()
	calls := &[][]string{}
	withMockExec(t, func(name string, args []string, opts ...exec.Option) (*exec.Result, error) {
		*calls = append(*calls, append([]string{}, args...))
		if fail != nil {
			if r := fail(len(*calls)); r != nil {
				return r, nil
			}
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})
	return calls
}

func hasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestRunTranspile_BrowserEntryBuildsInIsolatedGraph(t *testing.T) {
	dir := writeBrowserConditionProject(t)
	outDir := filepath.Join(dir, "lib")

	calls := captureTranspileInvocations(t, nil)

	_, errors, err := RunTranspile("bun", dir, outDir, TranspileParams{Target: "bun", Splitting: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errors) != 0 {
		t.Fatalf("unexpected errors: %v", errors)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected 2 bun build invocations, got %d: %v", len(*calls), *calls)
	}

	server, browser := (*calls)[0], (*calls)[1]

	// Server graph: default export + bin, never the browser entry.
	if !containsArg(server, filepath.Join(dir, "./src/index.ts")) {
		t.Errorf("server graph missing default entry: %v", server)
	}
	if !containsArg(server, filepath.Join(dir, "./bin/generate.ts")) {
		t.Errorf("server graph missing bin entry: %v", server)
	}
	if containsArg(server, filepath.Join(dir, "./src/index.browser.ts")) {
		t.Errorf("server graph must not include the browser entry: %v", server)
	}
	if !hasFlagValue(server, "--target", "bun") {
		t.Errorf("server graph target = %v, want bun", server)
	}

	// Browser graph: only the browser entry, built with browser resolution.
	if !containsArg(browser, filepath.Join(dir, "./src/index.browser.ts")) {
		t.Errorf("browser graph missing browser entry: %v", browser)
	}
	if containsArg(browser, filepath.Join(dir, "./src/index.ts")) ||
		containsArg(browser, filepath.Join(dir, "./bin/generate.ts")) {
		t.Errorf("browser graph must contain only browser entrypoints: %v", browser)
	}
	if !hasFlagValue(browser, "--target", "browser") {
		t.Errorf("browser graph target = %v, want browser", browser)
	}

	// Both graphs must emit into the same tree so the published exports resolve.
	for _, args := range [][]string{server, browser} {
		if !hasFlagValue(args, "--outdir", outDir) {
			t.Errorf("expected --outdir %s, got %v", outDir, args)
		}
		if !hasFlagValue(args, "--root", dir) {
			t.Errorf("expected --root %s, got %v", dir, args)
		}
		if !containsArg(args, "--splitting") {
			t.Errorf("expected --splitting, got %v", args)
		}
	}
}

func TestRunTranspile_BrowserGraphIgnoresConfiguredServerTarget(t *testing.T) {
	dir := writeBrowserConditionProject(t)
	calls := captureTranspileInvocations(t, nil)

	if _, errors, err := RunTranspile("bun", dir, filepath.Join(dir, "lib"), TranspileParams{Target: "node"}); err != nil || len(errors) != 0 {
		t.Fatalf("unexpected failure: err=%v errors=%v", err, errors)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected 2 invocations, got %d", len(*calls))
	}
	if !hasFlagValue((*calls)[0], "--target", "node") {
		t.Errorf("server graph should honor the configured target: %v", (*calls)[0])
	}
	if !hasFlagValue((*calls)[1], "--target", "browser") {
		t.Errorf("browser graph must always use the browser target: %v", (*calls)[1])
	}
}

func TestRunTranspile_NoBrowserConditionKeepsSingleInvocation(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","dependencies":{"react":"^19.0.0"},"exports":{".":{"default":"./src/index.ts"}}}`), 0644)
	outDir := filepath.Join(dir, "lib")

	calls := captureTranspileInvocations(t, nil)

	params := TranspileParams{Target: "bun", Splitting: true, Sourcemap: "external"}
	if _, errors, err := RunTranspile("bun", dir, outDir, params); err != nil || len(errors) != 0 {
		t.Fatalf("unexpected failure: err=%v errors=%v", err, errors)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly 1 bun build invocation, got %d: %v", len(*calls), *calls)
	}

	want := buildTranspileArgs(dir, outDir, params, []string{"./src/index.ts"}, []string{"react"}, "")
	got := (*calls)[0]
	// tsconfig resolution depends on the temp dir layout; compare ignoring it.
	if idx := indexOfArg(got, "--tsconfig"); idx >= 0 {
		got = append(append([]string{}, got[:idx]...), got[idx+2:]...)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("single-graph args changed:\n got %v\nwant %v", got, want)
	}
}

func indexOfArg(args []string, flag string) int {
	for i, a := range args {
		if a == flag {
			return i
		}
	}
	return -1
}

func TestRunTranspile_BrowserGraphFailureNamesTheGraph(t *testing.T) {
	dir := writeBrowserConditionProject(t)
	calls := captureTranspileInvocations(t, func(call int) *exec.Result {
		if call == 2 {
			return &exec.Result{Success: false, ExitCode: 1, Stderr: "browser boom"}
		}
		return nil
	})

	_, errors, err := RunTranspile("bun", dir, filepath.Join(dir, "lib"), TranspileParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected 2 invocations, got %d", len(*calls))
	}
	if len(errors) != 1 {
		t.Fatalf("expected 1 error, got %v", errors)
	}
	if !strings.Contains(errors[0], "browser graph") || !strings.Contains(errors[0], "browser boom") {
		t.Errorf("error must identify the failing graph and carry stderr, got %q", errors[0])
	}
}

func TestRunTranspile_ServerGraphFailureStopsBeforeBrowserGraph(t *testing.T) {
	dir := writeBrowserConditionProject(t)
	calls := captureTranspileInvocations(t, func(call int) *exec.Result {
		return &exec.Result{Success: false, ExitCode: 1, Stderr: "server boom"}
	})

	_, errors, err := RunTranspile("bun", dir, filepath.Join(dir, "lib"), TranspileParams{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected the browser graph to be skipped after a server failure, got %d invocations", len(*calls))
	}
	if len(errors) != 1 || !strings.Contains(errors[0], "server graph") {
		t.Errorf("expected a server-graph error, got %v", errors)
	}
}

// ---- buildTranspileArgs ----

func TestBuildTranspileArgs_Defaults(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{}, []string{"src/index.ts"}, nil, "")
	if args[0] != "build" {
		t.Errorf("expected first arg 'build', got %q", args[0])
	}
	// Should have --target bun (default)
	found := false
	for i, a := range args {
		if a == "--target" && i+1 < len(args) && args[i+1] == "bun" {
			found = true
		}
	}
	if !found {
		t.Error("expected --target bun in args")
	}
}

func TestBuildTranspileArgs_WithExternals(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{}, []string{"src/index.ts"}, []string{"react", "express"}, "")
	externCount := 0
	for _, a := range args {
		if a == "--external" {
			externCount++
		}
	}
	if externCount != 2 {
		t.Errorf("expected 2 --external flags, got %d", externCount)
	}
}

func TestBuildTranspileArgs_CustomTarget(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{Target: "node"}, []string{"src/index.ts"}, nil, "")
	found := false
	for i, a := range args {
		if a == "--target" && i+1 < len(args) && args[i+1] == "node" {
			found = true
		}
	}
	if !found {
		t.Error("expected --target node")
	}
}

func TestBuildTranspileArgs_Sourcemap(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{Sourcemap: "external"}, []string{"src/index.ts"}, nil, "")
	found := false
	for _, a := range args {
		if a == "--sourcemap=external" {
			found = true
		}
	}
	if !found {
		t.Error("expected --sourcemap=external")
	}
}

func TestBuildTranspileArgs_SourcemapNone(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{Sourcemap: "none"}, []string{"src/index.ts"}, nil, "")
	for _, a := range args {
		if strings.Contains(a, "sourcemap") {
			t.Errorf("sourcemap=none should not add --sourcemap flag, found %q", a)
		}
	}
}

func TestBuildTranspileArgs_Minify(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{Minify: true}, []string{"src/index.ts"}, nil, "")
	foundSyntax, foundWhitespace, foundIdentifiers := false, false, false
	for _, a := range args {
		switch a {
		case "--minify-syntax":
			foundSyntax = true
		case "--minify-whitespace":
			foundWhitespace = true
		case "--minify-identifiers", "--minify":
			foundIdentifiers = true
		}
	}
	if !foundSyntax || !foundWhitespace {
		t.Error("expected --minify-syntax and --minify-whitespace")
	}
	if !foundIdentifiers {
		t.Error("expected --minify-identifiers (double-minify identifier collision fixed in Bun 1.4)")
	}
}

func TestBuildTranspileArgs_Splitting(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{Splitting: true}, []string{"src/index.ts"}, nil, "")
	foundSplit, foundFormat := false, false
	for _, a := range args {
		if a == "--splitting" {
			foundSplit = true
		}
		if a == "--format=esm" {
			foundFormat = true
		}
	}
	if !foundSplit || !foundFormat {
		t.Error("expected --splitting --format=esm")
	}
}

func TestBuildTranspileArgs_WithTsconfig(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{}, []string{"src/index.ts"}, nil, "/proj/tsconfig.json")
	found := false
	for i, a := range args {
		if a == "--tsconfig" && i+1 < len(args) && args[i+1] == "/proj/tsconfig.json" {
			found = true
		}
	}
	if !found {
		t.Error("expected --tsconfig /proj/tsconfig.json")
	}
}

func TestBuildTranspileArgs_MultipleEntrypoints(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{}, []string{"src/index.ts", "src/serve.ts"}, nil, "")
	// Last two args should be the entrypoints (joined with projectPath)
	last2 := args[len(args)-2:]
	sort.Strings(last2)
	if last2[0] != filepath.Join("/proj", "src/index.ts") || last2[1] != filepath.Join("/proj", "src/serve.ts") {
		t.Errorf("expected entrypoints at end of args, got %v", last2)
	}
}

func TestBuildTranspileArgs_AllOptions(t *testing.T) {
	args := buildTranspileArgs("/proj", "/out", TranspileParams{
		Target:    "node",
		Sourcemap: "inline",
		Minify:    true,
		Splitting: true,
	}, []string{"src/index.ts"}, []string{"react"}, "/proj/tsconfig.json")

	checks := map[string]bool{
		"--minify-syntax":      false,
		"--minify-whitespace":  false,
		"--minify-identifiers": false,
		"--splitting":          false,
		"--format=esm":         false,
		"--sourcemap=inline":   false,
	}
	for _, a := range args {
		if _, ok := checks[a]; ok {
			checks[a] = true
		}
	}
	for flag, found := range checks {
		if !found {
			t.Errorf("expected %s in args", flag)
		}
	}
}

// TestRunTranspile_CleansStaleOutputBeforeBuilding pins the reproducibility
// contract. Bun emits content-hashed shared chunks; a stale chunk from a prior
// build survives in lib/ forever otherwise, npm packaging copies lib/
// wholesale, and two publishers pack different bytes for the same version —
// the managed release-set byte verification then fails with a size mismatch.
func TestRunTranspile_CleansStaleOutputBeforeBuilding(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0755)
	os.WriteFile(filepath.Join(dir, "src", "index.ts"), []byte("export {}"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","exports":{".":{"default":"./src/index.ts"}}}`), 0644)
	outDir := filepath.Join(dir, "lib")
	os.MkdirAll(outDir, 0755)
	stale := filepath.Join(outDir, "tsconfig-stalehash.js")
	os.WriteFile(stale, []byte("// stale chunk from a previous build"), 0644)

	captureTranspileInvocations(t, nil)

	if _, errors, err := RunTranspile("bun", dir, outDir, TranspileParams{Target: "bun"}); err != nil || len(errors) != 0 {
		t.Fatalf("unexpected failure: err=%v errors=%v", err, errors)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale chunk %s survived the rebuild; npm staging would pack it", stale)
	}
}
