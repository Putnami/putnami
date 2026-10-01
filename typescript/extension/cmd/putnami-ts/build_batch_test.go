package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// TestTypesBuildInfoDir_Isolation pins the warm-dir keying invariants for the
// incremental .tsbuildinfo: (a) two commands that can target the same project
// concurrently ("build" vs "build-types") derive DISTINCT paths, so they never
// share and corrupt one another's incremental state; (b) the path is STABLE for
// the same (command, project) across runs, which is what makes warm reuse
// possible; (c) different projects never collide; and (d) an empty CacheRoot
// yields "" so RunTypes falls back to cold, non-incremental compilation.
func TestTypesBuildInfoDir_Isolation(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "phase-isolation", "incremental-build-info-state-is-per-project")
	// The root is in the host's form, as the CLI hands it over.
	cacheRoot := filepath.FromSlash("/ws/.putnami/cache")
	const proj = "typescript/framework/utils"

	buildDir := typesBuildInfoDir(cacheRoot, "build", proj)
	buildTypesDir := typesBuildInfoDir(cacheRoot, "build-types", proj)

	if buildDir == "" || buildTypesDir == "" {
		t.Fatal("expected non-empty buildinfo dirs when CacheRoot is set")
	}
	if buildDir == buildTypesDir {
		t.Errorf("expected distinct buildinfo dirs for build vs build-types, both = %q", buildDir)
	}
	if again := typesBuildInfoDir(cacheRoot, "build-types", proj); again != buildTypesDir {
		t.Errorf("buildinfo dir not stable across runs: %q != %q", again, buildTypesDir)
	}
	if other := typesBuildInfoDir(cacheRoot, "build-types", "typescript/framework/application"); other == buildTypesDir {
		t.Error("expected distinct buildinfo dirs for different projects under the same command")
	}
	if got := typesBuildInfoDir("", "build-types", proj); got != "" {
		t.Errorf("empty CacheRoot must yield empty buildinfo dir (cold fallback), got %q", got)
	}
	if !strings.HasPrefix(buildTypesDir, cacheRoot) {
		t.Errorf("buildinfo dir %q must be rooted under CacheRoot %q", buildTypesDir, cacheRoot)
	}
}

// makeBuildBatchContext builds a ctx selecting two sibling projects, each with a
// real transpilable entrypoint, and an OutputPath whose base ("build") names the
// command — so batchOutputPath resolves each project's own captured-output dir
// exactly as the scheduler prepares it.
func makeBuildBatchContext(t *testing.T) (*pctx.Context, string) {
	t.Helper()
	ctx, root := makeTestCtx(t)
	for _, path := range []string{"packages/a", "packages/b"} {
		if err := os.MkdirAll(filepath.Join(root, path, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path, "src", "index.ts"),
			[]byte("export const x = 1;"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path, "package.json"),
			[]byte(`{"name":"@test/pkg","exports":{"./index":"./src/index.ts"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Command segment must be "build" so batchOutputPath(base of OutputPath)
	// mirrors the scheduler's .putnami/out/{project}/build convention.
	ctx.OutputPath = filepath.Join(root, ".putnami", "out", "packages/a", "build")
	ctx.SelectedProjects = []pctx.ProjectRef{
		{ID: "/packages/a", Name: "a", Path: "packages/a", FullPath: filepath.Join(root, "packages/a")},
		{ID: "/packages/b", Name: "b", Path: "packages/b", FullPath: filepath.Join(root, "packages/b")},
	}
	return ctx, root
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func batchResultsFor(t *testing.T, data map[string]any) []buildBatchProjectResult {
	t.Helper()
	results, ok := data["batchResults"].([]buildBatchProjectResult)
	if !ok {
		t.Fatalf("batchResults = %#v", data["batchResults"])
	}
	return results
}

func resultByID(results []buildBatchProjectResult, id string) (buildBatchProjectResult, bool) {
	for _, r := range results {
		if r.ProjectID == id {
			return r, true
		}
	}
	return buildBatchProjectResult{}, false
}

func metricValue(res buildBatchProjectResult, name string) (float64, bool) {
	for _, m := range res.Metrics {
		if m.Name == name {
			return m.Value, true
		}
	}
	return 0, false
}

func TestRunBuildTranspileBatch_WritesEachProjectToItsOwnLibDir(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "phase-isolation", "each-project-writes-only-into-its-own-output-directory")
	mockBunResolution(t)
	ctx, root := makeBuildBatchContext(t)

	var calls int
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		outdir := argAfter(args, "--outdir")
		if outdir == "" {
			t.Fatalf("transpile args missing --outdir: %v", args)
		}
		if err := os.MkdirAll(outdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outdir, "index.js"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, data, err := runBuildTranspileBatch(ctx)
	if err != nil || status != "OK" {
		t.Fatalf("transpile batch status=%q err=%v", status, err)
	}
	if calls != 2 {
		t.Fatalf("transpile exec calls = %d, want one per project", calls)
	}
	results := batchResultsFor(t, data)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, id := range []string{"/packages/a", "/packages/b"} {
		res, ok := resultByID(results, id)
		if !ok || res.Status != "OK" {
			t.Fatalf("project %s result = %+v", id, res)
		}
		if v, ok := metricValue(res, "transpiled-files"); !ok || v != 1 {
			t.Fatalf("project %s transpiled-files metric = %v (present=%v)", id, v, ok)
		}
	}
	// Each project's transpiled output lands in its own out/{path}/build/lib dir.
	for _, path := range []string{"packages/a", "packages/b"} {
		out := filepath.Join(root, ".putnami", "out", path, "build", "lib", "index.js")
		if _, err := os.Stat(out); err != nil {
			t.Fatalf("missing per-project transpile output %s: %v", out, err)
		}
	}
}

func TestRunBuildTranspileBatch_IsolatesPerProjectFailure(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "batch-failure-isolation", "one-project-failing-still-produces-sibling-outputs")
	mockBunResolution(t)
	ctx, _ := makeBuildBatchContext(t)

	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		// RunTranspile passes each project's path via --root; fail only project a.
		if filepath.Base(argAfter(args, "--root")) == "a" {
			return &exec.Result{Success: false, ExitCode: 1, Stderr: "type boom"}, nil
		}
		outdir := argAfter(args, "--outdir")
		if outdir != "" {
			_ = os.MkdirAll(outdir, 0o755)
			_ = os.WriteFile(filepath.Join(outdir, "index.js"), []byte("x"), 0o644)
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, data, err := runBuildTranspileBatch(ctx)
	if err != nil || status != "OK" {
		t.Fatalf("transpile batch process status=%q err=%v, want OK isolation", status, err)
	}
	results := batchResultsFor(t, data)
	a, _ := resultByID(results, "/packages/a")
	b, _ := resultByID(results, "/packages/b")
	if a.Status != "FAILED" || len(a.Diagnostics) == 0 {
		t.Fatalf("failed project a = %+v, want FAILED with diagnostics", a)
	}
	if a.Diagnostics[0].Category != "TRANSPILE_ERROR" || a.Diagnostics[0].Severity != "error" {
		t.Fatalf("project a diagnostic = %+v", a.Diagnostics[0])
	}
	if b.Status != "OK" {
		t.Fatalf("peer project b = %+v, want OK isolation", b)
	}
	if v, ok := metricValue(b, "transpiled-files"); !ok || v != 1 {
		t.Fatalf("peer b transpiled-files metric = %v (present=%v)", v, ok)
	}
}

func TestRunBuildTypesBatch_ProducesPerProjectDeclarations(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "phase-isolation", "the-types-phase-emits-per-project-declarations")
	mockBunResolution(t)
	ctx, root := makeBuildBatchContext(t)

	var calls int
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		outdir := argAfter(args, "--outDir")
		if outdir == "" {
			t.Fatalf("types args missing --outDir: %v", args)
		}
		if err := os.MkdirAll(outdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outdir, "index.d.ts"), []byte("export declare const x: number;"), 0o644); err != nil {
			t.Fatal(err)
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, data, err := runBuildTypesBatch(ctx)
	if err != nil || status != "OK" {
		t.Fatalf("types batch status=%q err=%v", status, err)
	}
	if calls != 2 {
		t.Fatalf("types exec calls = %d, want one per project", calls)
	}
	results := batchResultsFor(t, data)
	for _, id := range []string{"/packages/a", "/packages/b"} {
		res, ok := resultByID(results, id)
		if !ok || res.Status != "OK" {
			t.Fatalf("project %s types result = %+v", id, res)
		}
		if v, ok := metricValue(res, "type-declarations"); !ok || v != 1 {
			t.Fatalf("project %s type-declarations metric = %v (present=%v)", id, v, ok)
		}
	}
	for _, path := range []string{"packages/a", "packages/b"} {
		out := filepath.Join(root, ".putnami", "out", path, "build", "types", "index.d.ts")
		if _, err := os.Stat(out); err != nil {
			t.Fatalf("missing per-project types output %s: %v", out, err)
		}
	}
}

func TestRunBuildGenerateBatch_ProducesPerProjectDataAndGen(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "phase-isolation", "the-generate-phase-emits-per-project-data")
	mockBunResolution(t)
	mockAllExec(t, successExec)
	ctx, root := makeBuildBatchContext(t)

	status, data, err := runBuildGenerateBatch(ctx)
	if err != nil || status != "OK" {
		t.Fatalf("generate batch status=%q err=%v", status, err)
	}
	results := batchResultsFor(t, data)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, id := range []string{"/packages/a", "/packages/b"} {
		res, ok := resultByID(results, id)
		if !ok || res.Status != "OK" {
			t.Fatalf("project %s generate result = %+v", id, res)
		}
		if res.Data == nil || res.Data["hash"] == nil {
			t.Fatalf("project %s generate data = %#v, want hash/exports/assets", id, res.Data)
		}
		if _, ok := res.Data["exports"]; !ok {
			t.Fatalf("project %s generate data missing exports: %#v", id, res.Data)
		}
		if v, ok := metricValue(res, "generate-hash"); !ok || v != 1 {
			t.Fatalf("project %s generate-hash metric = %v (present=%v)", id, v, ok)
		}
	}
	// Each project's generate writes into its OWN .gen tree.
	for _, path := range []string{"packages/a", "packages/b"} {
		gen := filepath.Join(root, path, ".gen", "generate-result.json")
		if _, err := os.Stat(gen); err != nil {
			t.Fatalf("missing per-project .gen manifest %s: %v", gen, err)
		}
	}
}

// TestRunBuildGenerateBatch_EqualsSolo pins the load-bearing equivalence: the
// per-project wire Data a batched generate produces is byte-identical to what
// the solo handler returns for the same project (the scheduler reads this Data
// to locate clients/ and to cache the result).
func TestRunBuildGenerateBatch_EqualsSolo(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "batch-equals-solo", "a-batched-phase-matches-the-solo-artifacts")
	mockBunResolution(t)
	mockAllExec(t, successExec)

	// Solo run for a standalone project.
	soloCtx, soloRoot := makeTestCtx(t)
	if err := os.MkdirAll(filepath.Join(soloRoot, "project", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(soloRoot, "project", "src", "index.ts"),
		[]byte("export const x = 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, soloData, err := runBuildGenerate(soloCtx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("solo generate: %v", err)
	}

	// Batch run for a single, content-identical project.
	batchCtx, batchRoot := makeTestCtx(t)
	if err := os.MkdirAll(filepath.Join(batchRoot, "project", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchRoot, "project", "src", "index.ts"),
		[]byte("export const x = 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	batchCtx.SelectedProjects = []pctx.ProjectRef{
		{ID: "/project", Name: "@test/pkg", Path: "project", FullPath: filepath.Join(batchRoot, "project")},
	}
	_, data, err := runBuildGenerateBatch(batchCtx)
	if err != nil {
		t.Fatalf("batch generate: %v", err)
	}
	results := batchResultsFor(t, data)
	if len(results) != 1 {
		t.Fatalf("batch results = %d, want 1", len(results))
	}
	got := results[0].Data

	if got["hash"] != soloData["hash"] {
		t.Fatalf("batch hash %v != solo hash %v", got["hash"], soloData["hash"])
	}
	if _, ok := got["exports"]; !ok {
		t.Fatalf("batch data missing exports: %#v", got)
	}
	// The two runs live under different roots, so equal exports/assets is also
	// the checkout-independence the cached payload owes — solo and batch
	// must not disagree on a value the scheduler stores in the cache entry.
	if !reflect.DeepEqual(got["exports"], soloData["exports"]) {
		t.Fatalf("batch exports %v != solo exports %v", got["exports"], soloData["exports"])
	}
	if !reflect.DeepEqual(got["assets"], soloData["assets"]) {
		t.Fatalf("batch assets %v != solo assets %v", got["assets"], soloData["assets"])
	}
	// clientOutput presence must match (both absent here — no client generator).
	_, soloHasClient := soloData["clientOutput"]
	_, batchHasClient := got["clientOutput"]
	if soloHasClient != batchHasClient {
		t.Fatalf("clientOutput presence mismatch: solo=%v batch=%v", soloHasClient, batchHasClient)
	}
}

func TestRunBuildCompileBatch_NoEntrypointIsOKNoOp(t *testing.T) {
	mockBunResolution(t)
	ctx, _ := makeBuildBatchContext(t)
	// Projects expose only ./index (no serve/bin/main) so compile is a no-op.
	var calls int
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, data, err := runBuildCompileBatch(ctx)
	if err != nil || status != "OK" {
		t.Fatalf("compile batch status=%q err=%v", status, err)
	}
	if calls != 0 {
		t.Fatalf("compile exec calls = %d, want zero for no-entrypoint no-op", calls)
	}
	results := batchResultsFor(t, data)
	for _, id := range []string{"/packages/a", "/packages/b"} {
		res, ok := resultByID(results, id)
		if !ok || res.Status != "OK" {
			t.Fatalf("project %s compile no-op result = %+v", id, res)
		}
		if len(res.Metrics) != 0 {
			t.Fatalf("no-op compile emitted metrics: %+v", res.Metrics)
		}
	}
}

func TestRunBuildCompileBatch_CompilesResolvableEntrypoint(t *testing.T) {
	mockBunResolution(t)
	ctx, root := makeBuildBatchContext(t)
	// Give each project a main entrypoint so compile resolves work.
	for _, path := range []string{"packages/a", "packages/b"} {
		if err := os.WriteFile(filepath.Join(root, path, "package.json"),
			[]byte(`{"name":"@test/pkg","main":"src/index.ts"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx.Params = pctx.Params{"compile-target": []byte(`"bun-linux-x64"`)}

	var calls int
	mockAllExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, data, err := runBuildCompileBatch(ctx)
	if err != nil || status != "OK" {
		t.Fatalf("compile batch status=%q err=%v", status, err)
	}
	if calls != 2 {
		t.Fatalf("compile exec calls = %d, want one per project (single target)", calls)
	}
	results := batchResultsFor(t, data)
	for _, id := range []string{"/packages/a", "/packages/b"} {
		res, ok := resultByID(results, id)
		if !ok || res.Status != "OK" {
			t.Fatalf("project %s compile result = %+v", id, res)
		}
		if v, ok := metricValue(res, "compiled-executables"); !ok || v != 1 {
			t.Fatalf("project %s compiled-executables metric = %v (present=%v)", id, v, ok)
		}
	}
}

// TestRunBuildGenerateBatch_ForwardsPerProjectNameToHooks pins that a batched
// generate runs each project's pre-build hooks under THAT project's putnami
// identity, not the batch leader's. doGenerate forwards ctx.Project.Name to the
// hooks, and the batch loop must give it a per-project context — otherwise every
// follower's hooks (config extraction, generated metadata) run under the leader's
// name.
func TestRunBuildGenerateBatch_ForwardsPerProjectNameToHooks(t *testing.T) {
	mockBunResolution(t)
	ctx, root := makeBuildBatchContext(t)

	// Distinct putnami identity + a discoverable preBuild hook per member.
	names := map[string]string{"packages/a": "@scope/alpha", "packages/b": "@scope/beta"}
	for path, name := range names {
		depDir := filepath.Join(root, path, "node_modules", "@test", "genhook")
		if err := os.MkdirAll(depDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(depDir, "putnami.extension.json"),
			[]byte(`{"hooks":{"preBuild":{"kind":"command","command":"bun","args":["hook.ts"]}}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path, "package.json"),
			[]byte(`{"name":"`+name+`","dependencies":{"@test/genhook":"workspace:*"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx.SelectedProjects = []pctx.ProjectRef{
		{ID: "/packages/a", Name: names["packages/a"], Path: "packages/a", FullPath: filepath.Join(root, "packages/a")},
		{ID: "/packages/b", Name: names["packages/b"], Path: "packages/b", FullPath: filepath.Join(root, "packages/b")},
	}

	// The mocked hook subprocess records the projectName each invocation received
	// through its --putnami-context file, keyed by project root.
	seen := map[string]string{}
	mockAllExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if ctxPath := argAfter(args, "--putnami-context"); ctxPath != "" {
			if data, err := os.ReadFile(ctxPath); err == nil {
				var hc struct {
					ProjectName string `json:"projectName"`
					ProjectRoot string `json:"projectRoot"`
				}
				if json.Unmarshal(data, &hc) == nil && hc.ProjectRoot != "" {
					seen[hc.ProjectRoot] = hc.ProjectName
				}
			}
		}
		return &exec.Result{Success: true, ExitCode: 0, Stdout: `{"type":"summary","data":{"exports":{},"assets":{}}}` + "\n"}, nil
	})

	status, _, err := runBuildGenerateBatch(ctx)
	if err != nil || status != "OK" {
		t.Fatalf("generate batch status=%q err=%v", status, err)
	}
	for path, name := range names {
		if got := seen[filepath.Join(root, path)]; got != name {
			t.Fatalf("project %s pre-build hook saw name %q, want %q (leader identity leaked into follower)", path, got, name)
		}
	}
}

// TestBuildProducerTasksDeclareBatchBound guards the batch-size bound on the
// shipped manifest. The build producer batch handlers loop projects sequentially
// under the task's single timeout, so an unbounded ready group could exceed it —
// and a timeout emits no batchResults, failing EVERY member (including
// already-built peers). Each producer's batchable policy must cap group size.
func TestBuildProducerTasksDeclareBatchBound(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Batchable *struct {
				Tool        string `json:"tool"`
				MaxProjects int    `json:"maxProjects"`
			} `json:"batchable"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{"build-generate", "build-transpile", "build-types", "build-compile"} {
		b := manifest.Tasks[task].Batchable
		if b == nil {
			t.Fatalf("%s: missing batchable policy", task)
		}
		if b.MaxProjects <= 0 || b.MaxProjects > 16 {
			t.Fatalf("%s: maxProjects=%d, want a bound in 1..16 so the sequential batch stays under the task timeout", task, b.MaxProjects)
		}
	}
}
