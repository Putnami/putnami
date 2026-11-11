package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

const lintFeature = "typescript/typescript-project-toolchain"

// mockESLintResolution makes every project resolve eslint and node.
func mockESLintResolution(t *testing.T) {
	t.Helper()
	origBin, origNode := resolveESLintBinFn, resolveNodeFn
	t.Cleanup(func() { resolveESLintBinFn, resolveNodeFn = origBin, origNode })
	resolveESLintBinFn = func(_, _ string) (string, error) { return "/mock/eslint", nil }
	resolveNodeFn = func() (string, error) { return "/mock/node", nil }
}

func writeLintScript(t *testing.T, dir, script string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"pkg","scripts":{"lint":"`+script+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An ESLint project's format phase starts no Biome, and its check phase runs
// ESLint with its fixes.
func TestRunLint_AnESLintProjectRunsNoBiomeFormatter(t *testing.T) {
	spectest.Proves(t, lintFeature, "lint-chooses-the-linter", "an-eslint-project-runs-no-biome-formatter")
	mockBiomeResolution(t)
	mockESLintResolution(t)
	ctx, root := makeTestCtx(t)
	ctx.Params = pctx.Params{"lint": []byte(`{"linter":"eslint"}`), "fix": []byte("true")}

	var programs []string
	var lastArgs []string
	mockAllExec(t, func(program string, args []string, _ ...exec.Option) (*exec.Result, error) {
		programs = append(programs, program)
		lastArgs = args
		return &exec.Result{Success: true, Stdout: "[]"}, nil
	})

	if status, _, err := runLintFormat(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runLintFormat() = %q, %v, want OK", status, err)
	}
	if len(programs) != 0 {
		t.Fatalf("the format phase started %v, want nothing", programs)
	}
	if status, _, err := runLintCheck(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runLintCheck() = %q, %v, want OK", status, err)
	}
	if !slices.Equal(programs, []string{"/mock/eslint"}) || !slices.Contains(lastArgs, "--fix") {
		t.Fatalf("check phase ran %v %v, want eslint --fix", programs, lastArgs)
	}

	// The read-only path reports without fixing.
	if status, _, err := runLint(ctx, jsonl.New(), nil); err != nil || status != "OK" {
		t.Fatalf("runLint() = %q, %v, want OK", status, err)
	}
	if slices.Contains(lastArgs, "--fix") {
		t.Errorf("read-only lint args %v hold --fix", lastArgs)
	}

	// A violation fails the read-only path.
	mockAllExec(t, func(string, []string, ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, ExitCode: 1, Stdout: `[{"filePath":"` + filepath.ToSlash(filepath.Join(root, "project", "a.js")) + `","messages":[{"ruleId":"prefer-const","severity":2,"message":"use const","line":1,"column":5}]}]`}, nil
	})
	if status, _, err := runLint(ctx, jsonl.New(), nil); err != nil || status != "FAILED" {
		t.Errorf("runLint() on a violation = %q, %v, want FAILED", status, err)
	}
}

func TestRunLint_AnUnknownLinterFails(t *testing.T) {
	ctx, _ := makeTestCtx(t)
	ctx.Params = pctx.Params{"lint": []byte(`{"linter":"oxlint"}`)}
	for name, run := range map[string]func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error){
		"lint": runLint, "lint-format": runLintFormat, "lint-check": runLintCheck,
	} {
		status, _, err := run(ctx, jsonl.New(), nil)
		if status != "FAILED" || err == nil || !strings.Contains(err.Error(), "typescript.lint_linter_unknown") {
			t.Errorf("%s = %q, %v, want FAILED with typescript.lint_linter_unknown", name, status, err)
		}
	}
}

// In one batch, the Biome members share one Biome run and each ESLint member
// runs its own ESLint; each result belongs to its project.
func TestRunLintBatch_EachProjectLintsWithItsOwnLinter(t *testing.T) {
	spectest.Proves(t, lintFeature, "lint-chooses-the-linter", "a-batch-lints-each-project-with-its-own-linter")
	mockBiomeResolution(t)
	mockESLintResolution(t)
	ctx, root := makeLintBatchContext(t)
	writeLintScript(t, filepath.Join(root, "packages/b"), "eslint .")

	var biomeArgs [][]string
	var eslintRuns int
	var mu sync.Mutex
	mockAllExec(t, func(program string, args []string, _ ...exec.Option) (*exec.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		if program == "/mock/eslint" {
			eslintRuns++
			return &exec.Result{Success: false, ExitCode: 1, Stdout: `[{"filePath":"` + filepath.ToSlash(filepath.Join(root, "packages", "b", "src", "b.js")) + `","messages":[{"ruleId":"prefer-const","severity":2,"message":"use const","line":3,"column":1}]}]`}, nil
		}
		biomeArgs = append(biomeArgs, args)
		return &exec.Result{Success: true, Stdout: `{"diagnostics":[],"summary":{"errors":0,"warnings":0,"infos":0}}`}, nil
	})

	_, data, err := runLintBatch(ctx, lintBatchCheck)
	if err != nil {
		t.Fatal(err)
	}
	results := data["batchResults"].([]lintBatchProjectResult)
	if len(results) != 2 || results[0].ProjectID != "/packages/a" || results[1].ProjectID != "/packages/b" {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Status != "OK" || results[1].Status != "FAILED" {
		t.Errorf("statuses = %s, %s, want OK for Biome's a and FAILED for ESLint's b", results[0].Status, results[1].Status)
	}
	if len(results[1].Diagnostics) != 1 || results[1].Diagnostics[0].File != "packages/b/src/b.js" || results[1].Diagnostics[0].Category != "prefer-const" {
		t.Errorf("b diagnostics = %+v", results[1].Diagnostics)
	}
	if eslintRuns != 1 || len(biomeArgs) != 1 {
		t.Fatalf("eslint runs = %d, biome runs = %d, want one each", eslintRuns, len(biomeArgs))
	}
	// Biome runs over a alone: a group of one runs from its project, as a
	// singleton task does.
	if slices.Contains(biomeArgs[0], "packages/b") {
		t.Errorf("biome args %v name packages/b, which lints with ESLint", biomeArgs[0])
	}

	// The format phase starts Biome for a alone.
	biomeArgs, eslintRuns = nil, 0
	if _, data, err = runLintBatch(ctx, lintBatchFormat); err != nil {
		t.Fatal(err)
	}
	if eslintRuns != 0 || len(biomeArgs) != 1 || slices.Contains(biomeArgs[0], "packages/b") {
		t.Errorf("format phase: eslint runs = %d, biome args = %v, want Biome over packages/a alone", eslintRuns, biomeArgs)
	}
	for _, result := range data["batchResults"].([]lintBatchProjectResult) {
		if result.Status != "OK" {
			t.Errorf("format result %+v, want OK", result)
		}
	}

	// A member whose eslint cannot start fails alone.
	resolveESLintBinFn = func(projectPath, _ string) (string, error) {
		return "", os.ErrNotExist
	}
	_, data, err = runLintBatch(ctx, lintBatchCombined)
	if err != nil {
		t.Fatal(err)
	}
	results = data["batchResults"].([]lintBatchProjectResult)
	if results[0].Status != "OK" || results[1].Status != "FAILED" || len(results[1].Diagnostics) != 1 {
		t.Errorf("results = %+v, want a OK and b FAILED with the reason", results)
	}
}

// The ESLint members of a batch run at once, up to the CPU budget, and each
// result stays with its project.
func TestRunLintBatch_RunsItsESLintMembersInParallel(t *testing.T) {
	spectest.Proves(t, lintFeature, "lint-chooses-the-linter", "a-batch-runs-its-eslint-members-in-parallel")
	mockESLintResolution(t)
	ctx, root := makeTestCtx(t)
	ctx.Params = pctx.Params{"lint": []byte(`{"linter":"eslint"}`)}
	for _, name := range []string{"a", "b", "c", "d"} {
		writeLintScript(t, filepath.Join(root, "packages", name), "eslint .")
		ctx.SelectedProjects = append(ctx.SelectedProjects, pctx.ProjectRef{ID: "/packages/" + name, Name: name, Path: "packages/" + name})
	}
	t.Setenv("PUTNAMI_CPU_BUDGET", "2")

	var running, peak atomic.Int32
	mockAllExec(t, func(string, []string, ...exec.Option) (*exec.Result, error) {
		now := running.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		running.Add(-1)
		return &exec.Result{Success: true, Stdout: "[]"}, nil
	})

	_, data, err := runLintBatch(ctx, lintBatchCombined)
	if err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got != 2 {
		t.Errorf("peak concurrent ESLint runs = %d, want the budget of 2", got)
	}
	for i, result := range data["batchResults"].([]lintBatchProjectResult) {
		if result.ProjectID != ctx.SelectedProjects[i].ID || result.Status != "OK" {
			t.Errorf("result %d = %+v, want %s OK", i, result, ctx.SelectedProjects[i].ID)
		}
	}
}
