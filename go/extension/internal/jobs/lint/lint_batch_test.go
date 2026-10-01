package lint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	pctx "go.putnami.dev/sdk/extension/context"

	"go.putnami.dev/protocol/features/spectest"
)

// recordedInvocation captures one stubbed golangci-lint call.
type recordedInvocation struct {
	args []string
	dir  string
}

// stubGolangci replaces the tool runner and binary resolver for a test and
// returns the ordered list of invocations the batch made.
func stubGolangci(
	t *testing.T,
	respond func(inv recordedInvocation) ([]byte, error),
) *[]recordedInvocation {
	t.Helper()
	var invocations []recordedInvocation
	var mu sync.Mutex // config groups run concurrently; guard the recorder
	origCmd := runGolangciCommand
	origBin := resolveGolangciBinary
	runGolangciCommand = func(binary string, args []string, dir string) ([]byte, error) {
		inv := recordedInvocation{args: args, dir: dir}
		mu.Lock()
		invocations = append(invocations, inv)
		mu.Unlock()
		return respond(inv)
	}
	resolveGolangciBinary = func(string) (string, error) { return "golangci-lint", nil }
	t.Cleanup(func() {
		runGolangciCommand = origCmd
		resolveGolangciBinary = origBin
	})
	return &invocations
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resultByID(t *testing.T, results []batchProjectResult, id string) batchProjectResult {
	t.Helper()
	for _, r := range results {
		if r.ProjectID == id {
			return r
		}
	}
	t.Fatalf("no batch result for project %q", id)
	return batchProjectResult{}
}

func invocationKind(inv recordedInvocation) string {
	if len(inv.args) > 0 {
		return inv.args[0]
	}
	return ""
}

func hasPattern(args []string, pattern string) bool {
	return slices.Contains(args, pattern)
}

// TestRunGolangciBatchSplitsByConfigAndAttributes proves the core equivalence
// guarantee: projects that resolve different golangci configs run as separate
// invocations, and each finding is attributed back to the project owning its
// file with the same workspace-relative path a solo run would emit.
func TestRunGolangciBatchSplitsByConfigAndAttributes(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "lint-batch-equivalence", "a-batched-golangci-run-splits-by-config-and-attributes-each-finding")
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse (\n\t./go/framework/api\n\t./go/framework/errors\n\t./tooling/cli\n)\n")
	mustWrite(t, filepath.Join(root, "go", "framework", ".golangci.yml"), "version: \"2\"\n")
	mustWrite(t, filepath.Join(root, "tooling", "cli", ".golangci.yml"), "version: \"2\"\nlinters: {}\n")
	for _, p := range []string{"go/framework/api", "go/framework/errors", "tooling/cli"} {
		mustWrite(t, filepath.Join(root, p, "go.mod"), "module example.com/"+filepath.Base(p)+"\n\ngo 1.26\n")
	}

	apiFile := filepath.Join(root, "go", "framework", "api", "handler.go")
	errFile := filepath.Join(root, "go", "framework", "errors", "wrap.go")

	invocations := stubGolangci(t, func(inv recordedInvocation) ([]byte, error) {
		// golangci-lint run --path-mode abs emits absolute paths.
		if hasPattern(inv.args, "./go/framework/api/...") {
			out := apiFile + ":10:2: unchecked error (errcheck)\n" +
				errFile + ":5:6: func Wrap is unused (unused)\n2 issues:\n"
			return []byte(out), fmt.Errorf("exit status 1")
		}
		return []byte("0 issues.\n"), nil // tooling/cli: clean
	})

	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Job:           pctx.Job{Name: "lint"},
		Params:        pctx.Params{"fix": json.RawMessage("false")},
		SelectedProjects: []pctx.ProjectRef{
			{ID: "/go/framework/api", Name: "api", Path: "go/framework/api", FullPath: filepath.Join(root, "go/framework/api")},
			{ID: "/go/framework/errors", Name: "errors", Path: "go/framework/errors", FullPath: filepath.Join(root, "go/framework/errors")},
			{ID: "/tooling/cli", Name: "cli", Path: "tooling/cli", FullPath: filepath.Join(root, "tooling/cli")},
		},
	}
	options := parseLintOptions(ctx.Params, []string{"--tool", "golangci-lint"})

	status, data, err := runGolangciBatch(ctx, options)
	if err != nil || status != "OK" {
		t.Fatalf("runGolangciBatch = %q, %v", status, err)
	}
	results, ok := data["batchResults"].([]batchProjectResult)
	if !ok {
		t.Fatalf("batchResults missing or wrong type: %T", data["batchResults"])
	}
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}

	// Two run invocations (fix=false → no format gate): one per config group.
	runs := 0
	var frameworkRun, cliRun recordedInvocation
	for _, inv := range *invocations {
		if invocationKind(inv) != "run" {
			t.Fatalf("unexpected invocation kind %q (fix=false should not run fmt)", invocationKind(inv))
		}
		runs++
		if hasPattern(inv.args, "./tooling/cli/...") {
			cliRun = inv
		} else {
			frameworkRun = inv
		}
	}
	if runs != 2 {
		t.Fatalf("want 2 run invocations (one per config group), got %d", runs)
	}

	// The framework group runs one process over BOTH framework modules with the
	// framework config; the cli group is separate with its own config.
	if !hasPattern(frameworkRun.args, "./go/framework/api/...") ||
		!hasPattern(frameworkRun.args, "./go/framework/errors/...") {
		t.Fatalf("framework run missing a module pattern: %v", frameworkRun.args)
	}
	frameworkCfg := filepath.Join(root, "go", "framework", ".golangci.yml")
	if !hasPattern(frameworkRun.args, frameworkCfg) {
		t.Fatalf("framework run used wrong --config: %v (want %s)", frameworkRun.args, frameworkCfg)
	}
	cliCfg := filepath.Join(root, "tooling", "cli", ".golangci.yml")
	if !hasPattern(cliRun.args, cliCfg) {
		t.Fatalf("cli run used wrong --config: %v (want %s)", cliRun.args, cliCfg)
	}
	if hasPattern(cliRun.args, "./go/framework/api/...") {
		t.Fatalf("cli run leaked a framework module: %v", cliRun.args)
	}

	// Attribution + workspace-relative path equivalence.
	api := resultByID(t, results, "/go/framework/api")
	if api.Status != "FAILED" || len(api.Diagnostics) != 1 {
		t.Fatalf("api result = %+v", api)
	}
	if api.Diagnostics[0].File != "go/framework/api/handler.go" ||
		api.Diagnostics[0].Line != 10 || api.Diagnostics[0].Column != 2 ||
		api.Diagnostics[0].Description != "unchecked error (errcheck)" {
		t.Fatalf("api diagnostic mis-attributed: %+v", api.Diagnostics[0])
	}
	if api.Summary.Errors != 1 {
		t.Fatalf("api summary errors = %d, want 1", api.Summary.Errors)
	}

	errs := resultByID(t, results, "/go/framework/errors")
	if errs.Status != "FAILED" || len(errs.Diagnostics) != 1 ||
		errs.Diagnostics[0].File != "go/framework/errors/wrap.go" {
		t.Fatalf("errors result mis-attributed: %+v", errs)
	}

	cli := resultByID(t, results, "/tooling/cli")
	if cli.Status != "OK" || len(cli.Diagnostics) != 0 {
		t.Fatalf("cli result should be clean OK, got %+v", cli)
	}
}

// TestRunGolangciBatchResolvesRelativeConfigPerProject guards #1: an explicit
// relative --config is project-relative, but the batch runs from the shared
// group root. Each project must load its own override (anchored to its dir),
// not one resolved against the group root.
func TestRunGolangciBatchResolvesRelativeConfigPerProject(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse (\n\t./a\n\t./b\n)\n")
	for _, p := range []string{"a", "b"} {
		mustWrite(t, filepath.Join(root, p, "go.mod"), "module example.com/"+p+"\n\ngo 1.26\n")
		mustWrite(t, filepath.Join(root, p, ".golangci-strict.yml"), "version: \"2\"\n")
	}

	invocations := stubGolangci(t, func(recordedInvocation) ([]byte, error) {
		return []byte("0 issues.\n"), nil
	})
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Job:           pctx.Job{Name: "lint"},
		Params:        pctx.Params{"fix": json.RawMessage("false"), "config": json.RawMessage(`"./.golangci-strict.yml"`)},
		SelectedProjects: []pctx.ProjectRef{
			{ID: "/a", Name: "a", Path: "a", FullPath: filepath.Join(root, "a")},
			{ID: "/b", Name: "b", Path: "b", FullPath: filepath.Join(root, "b")},
		},
	}
	options := parseLintOptions(ctx.Params, []string{"--tool", "golangci-lint"})
	if _, _, err := runGolangciBatch(ctx, options); err != nil {
		t.Fatal(err)
	}

	// Distinct per-project overrides → separate invocations, each with its OWN
	// absolute config path (never a bare relative path resolved against the root).
	wantA := filepath.Join(root, "a", ".golangci-strict.yml")
	wantB := filepath.Join(root, "b", ".golangci-strict.yml")
	sawA, sawB := false, false
	for _, inv := range *invocations {
		if hasPattern(inv.args, "./.golangci-strict.yml") {
			t.Fatalf("relative config leaked unresolved into the batch invocation: %v", inv.args)
		}
		if hasPattern(inv.args, wantA) && hasPattern(inv.args, "./a/...") {
			sawA = true
		}
		if hasPattern(inv.args, wantB) && hasPattern(inv.args, "./b/...") {
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Fatalf("each project must run with its own absolute config: sawA=%v sawB=%v", sawA, sawB)
	}
}

// TestRunGolangciBatchIsolatesAProjectItsExclusionsHide keeps a batch equal to
// solo runs under a config whose exclusions match paths relative to the working
// directory: seen from the go.work directory, every file of dist/proj matches
// the dist pattern and every file of internal/proj the internal/ rule path, so
// those projects run from their own directories instead.
func TestRunGolangciBatchIsolatesAProjectItsExclusionsHide(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse (\n\t./app\n\t./dist/proj\n\t./internal/proj\n\t./tools\n)\n")
	for _, p := range []string{"app", "dist/proj", "internal/proj", "tools"} {
		mustWrite(t, filepath.Join(root, p, "go.mod"), "module example.com/"+filepath.Base(p)+"\n\ngo 1.26\n")
	}
	config := filepath.Join(t.TempDir(), ".golangci.yml")
	mustWrite(t, config, "version: \"2\"\nrun:\n  relative-path-mode: wd\nlinters:\n  exclusions:\n    rules:\n      - path: internal/\n        text: \"exported:\"\n    paths:\n      - ^([^/]*[^./][^/]*/)*dist(/|$)\n")

	invocations := stubGolangci(t, func(recordedInvocation) ([]byte, error) {
		return []byte("0 issues.\n"), nil
	})
	params := pctx.Params{"fix": json.RawMessage("false"), "config": json.RawMessage(fmt.Sprintf("%q", config))}
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Job:           pctx.Job{Name: "lint"},
		Params:        params,
		SelectedProjects: []pctx.ProjectRef{
			{ID: "/app", Name: "app", Path: "app", FullPath: filepath.Join(root, "app")},
			{ID: "/dist/proj", Name: "proj", Path: "dist/proj", FullPath: filepath.Join(root, "dist", "proj")},
			{ID: "/internal/proj", Name: "proj", Path: "internal/proj", FullPath: filepath.Join(root, "internal", "proj")},
			{ID: "/tools", Name: "tools", Path: "tools", FullPath: filepath.Join(root, "tools")},
		},
	}
	options := parseLintOptions(ctx.Params, []string{"--tool", "golangci-lint"})
	if _, _, err := runGolangciBatch(ctx, options); err != nil {
		t.Fatal(err)
	}

	if len(*invocations) != 3 {
		t.Fatalf("want 3 invocations (the shared group and two isolated projects), got %d: %+v", len(*invocations), *invocations)
	}
	sawShared, sawDist, sawInternal := false, false, false
	for _, inv := range *invocations {
		switch inv.dir {
		case root:
			sawShared = hasPattern(inv.args, "./app/...") && hasPattern(inv.args, "./tools/...") &&
				!hasPattern(inv.args, "./dist/proj/...") && !hasPattern(inv.args, "./internal/proj/...")
		case filepath.Join(root, "dist", "proj"):
			sawDist = hasPattern(inv.args, "./...")
		case filepath.Join(root, "internal", "proj"):
			sawInternal = hasPattern(inv.args, "./...")
		}
	}
	if !sawShared || !sawDist || !sawInternal {
		t.Fatalf("dist/proj and internal/proj must each run alone from their directory and the others together: %+v", *invocations)
	}
}

// TestExecuteGolangciGroupRetriesETXTBSY guards #2: a transient "text file busy"
// must be retried (as the singleton path does) rather than failing every project.
func TestExecuteGolangciGroupRetriesETXTBSY(t *testing.T) {
	origDelay := golangciRetryDelay
	golangciRetryDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() { golangciRetryDelay = origDelay })

	group, _ := newGroup(t, "a", "b")
	calls := 0
	stubGolangci(t, func(recordedInvocation) ([]byte, error) {
		calls++
		if calls <= 2 {
			return []byte("fork/exec: ETXTBSY\n"), fmt.Errorf("exit status 1")
		}
		return []byte("0 issues.\n"), nil
	})
	results := executeGolangciGroup("golangci-lint", lintOptions{tool: "golangci-lint"}, group, filepath.Dir(group.groupRoot))
	if calls != 3 {
		t.Fatalf("expected 2 ETXTBSY retries then success (3 calls), got %d", calls)
	}
	for _, r := range results {
		if r.Status != "OK" {
			t.Fatalf("%s should succeed after retry, got %s", r.ProjectID, r.Status)
		}
	}
}

func TestExecuteGolangciGroupUsesDerivedBatchDeadline(t *testing.T) {
	group, _ := newGroup(t, "a", "b")
	invocations := stubGolangci(t, func(recordedInvocation) ([]byte, error) {
		return []byte("0 issues.\n"), nil
	})
	t.Setenv("PUTNAMI_TASK_DEADLINE_MS", "1200000")
	options := parseLintOptions(pctx.Params{}, []string{"--tool", "golangci-lint"})
	results := executeGolangciGroup("golangci-lint", options, group, filepath.Dir(group.groupRoot))
	for _, result := range results {
		if result.Status != "OK" {
			t.Fatalf("batch result = %+v, want OK", result)
		}
	}
	for _, invocation := range *invocations {
		if invocationKind(invocation) == "run" && hasPattern(invocation.args, "1080000ms") {
			return
		}
	}
	t.Fatalf("batch invocations = %+v, want run with derived n× deadline argument", *invocations)
}

func newGroup(t *testing.T, ids ...string) (*lintBatchGroup, string) {
	t.Helper()
	root := t.TempDir()
	group := &lintBatchGroup{config: filepath.Join(root, ".golangci.yml"), groupRoot: root}
	for _, id := range ids {
		full := filepath.Join(root, id)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		group.projects = append(group.projects, &lintBatchProject{
			ref:      pctx.ProjectRef{ID: "/" + id, Name: id, Path: id, FullPath: full},
			fullPath: full,
		})
	}
	return group, root
}

func TestExecuteGolangciGroupToolchainCompat(t *testing.T) {
	group, _ := newGroup(t, "a", "b")
	stubGolangci(t, func(recordedInvocation) ([]byte, error) {
		return []byte("level=error msg=\"package requires newer Go version go1.99 (application built with go1.26)\"\n"), fmt.Errorf("exit status 1")
	})
	results := executeGolangciGroup("golangci-lint", lintOptions{tool: "golangci-lint"}, group, filepath.Dir(group.groupRoot))
	for _, r := range results {
		if r.Status != "FAILED" {
			t.Fatalf("%s should FAIL on toolchain mismatch: %+v", r.ProjectID, r)
		}
		if len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Description, "older than the") {
			t.Fatalf("%s missing the actionable toolchain diagnostic: %+v", r.ProjectID, r.Diagnostics)
		}
	}
}

func TestExecuteGolangciGroupUnattributedFailureSurfacesTail(t *testing.T) {
	group, _ := newGroup(t, "a", "b")
	stubGolangci(t, func(recordedInvocation) ([]byte, error) {
		// A crash / config error with no file-positioned findings.
		return []byte("Error: can't load config: unknown linter \"bogus\"\n"), fmt.Errorf("exit status 3")
	})
	results := executeGolangciGroup("golangci-lint", lintOptions{tool: "golangci-lint"}, group, filepath.Dir(group.groupRoot))
	for _, r := range results {
		if r.Status != "FAILED" {
			t.Fatalf("%s should FAIL when the run errors with no findings", r.ProjectID)
		}
		if len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Description, "unknown linter") {
			t.Fatalf("%s should carry the raw error tail, got %+v", r.ProjectID, r.Diagnostics)
		}
	}
}

func TestExecuteGolangciGroupFormatGateAttributesUnformattedFiles(t *testing.T) {
	group, root := newGroup(t, "a", "b")
	stubGolangci(t, func(inv recordedInvocation) ([]byte, error) {
		if invocationKind(inv) == "fmt" {
			// fmt --diff reports group-root-relative paths.
			return []byte("diff a/messy.go.orig a/messy.go\n@@ -1 +1 @@\n"), fmt.Errorf("exit status 1")
		}
		return []byte("0 issues.\n"), nil
	})
	workspaceRoot := filepath.Dir(root)
	results := executeGolangciGroup("golangci-lint", lintOptions{tool: "golangci-lint", fix: true}, group, workspaceRoot)

	wantFile := filepath.ToSlash(filepath.Join(filepath.Base(root), "a", "messy.go"))
	a := resultByID(t, results, "/a")
	if a.Status != "FAILED" || len(a.Diagnostics) != 1 {
		t.Fatalf("format drift not attributed to project a: %+v", a)
	}
	if a.Diagnostics[0].File != wantFile {
		t.Fatalf("format diagnostic path = %q, want %q", a.Diagnostics[0].File, wantFile)
	}
	if !strings.Contains(a.Diagnostics[0].Description, "not properly formatted") {
		t.Fatalf("format diagnostic wording changed: %+v", a.Diagnostics[0])
	}
	b := resultByID(t, results, "/b")
	if b.Status != "OK" {
		t.Fatalf("project b has no drift and should be OK: %+v", b)
	}
}

// TestManifestGolangciBatchAvoidsConfigDigestTrap guards the P1 pitfall shared
// by every same-tool batch slice: if a batchable configFiles candidate is a
// per-project file that always exists (e.g. {projectRoot}/go.mod), every project
// digests to a distinct path and batching silently never happens. The golangci
// candidates must be deliberate per-project overrides plus a shared fallback.
func TestManifestGolangciBatchAvoidsConfigDigestTrap(t *testing.T) {
	data, err := os.ReadFile("../../../putnami.extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Batchable *struct {
				Tool        string   `json:"tool"`
				ConfigFiles []string `json:"configFiles"`
				MaxProjects int      `json:"maxProjects"`
			} `json:"batchable"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}

	wantConfig := []string{
		"{projectRoot}/.golangci.yml",
		"{projectRoot}/.golangci.yaml",
		"{extensionRoot}/config/.golangci.yml",
	}
	for _, name := range []string{"lint-golangci-fix", "lint-golangci-readonly"} {
		task := manifest.Tasks[name]
		if task.Batchable == nil {
			t.Fatalf("%s must declare batchable", name)
		}
		if task.Batchable.Tool != "golangci-lint" {
			t.Fatalf("%s batch tool = %q", name, task.Batchable.Tool)
		}
		if task.Batchable.MaxProjects < 2 {
			t.Fatalf("%s maxProjects must cap the measured range (>=2), got %d", name, task.Batchable.MaxProjects)
		}
		if fmt.Sprint(task.Batchable.ConfigFiles) != fmt.Sprint(wantConfig) {
			t.Fatalf("%s configFiles = %v, want %v", name, task.Batchable.ConfigFiles, wantConfig)
		}
		for _, c := range task.Batchable.ConfigFiles {
			if strings.Contains(c, "go.mod") || strings.Contains(c, "{projectRoot}/go.") {
				t.Fatalf("%s configFiles includes an always-present per-project file %q (P1 trap)", name, c)
			}
		}
	}

	// staticcheck batches too, under the SAME size cap, but deliberately
	// declares NO configFiles: it resolves staticcheck.conf per package, so a
	// config partition would only fragment groups for nothing. An empty candidate
	// list is a stable "none" digest in the scheduler, not a disabled batch.
	staticcheck := manifest.Tasks["lint-staticcheck"]
	if staticcheck.Batchable == nil {
		t.Fatal("lint-staticcheck must declare batchable")
	}
	if staticcheck.Batchable.Tool != "staticcheck" {
		t.Fatalf("lint-staticcheck batch tool = %q", staticcheck.Batchable.Tool)
	}
	if len(staticcheck.Batchable.ConfigFiles) != 0 {
		t.Fatalf("lint-staticcheck must declare no configFiles, got %v", staticcheck.Batchable.ConfigFiles)
	}
	if want := manifest.Tasks["lint-golangci-readonly"].Batchable.MaxProjects; staticcheck.Batchable.MaxProjects != want {
		t.Fatalf("lint-staticcheck maxProjects = %d, want the golangci cap %d",
			staticcheck.Batchable.MaxProjects, want)
	}
}

// TestManifestStaticcheckBatchKeepsPerProjectCacheIdentity guards the binding
// prior: batching must not coarsen a cache key. The scheduler's batch
// key is a DISPATCH grouping and never reaches the cache key, so what has to
// stay true in the manifest is that every member still declares its OWN
// project-scoped source/module inputs and its own entry. The shared go.work and
// go.work.sum input is intentionally workspace-scoped: it re-keys every member
// without replacing the per-project inputs that keep their identities distinct.
func TestManifestStaticcheckBatchKeepsPerProjectCacheIdentity(t *testing.T) {
	data, err := os.ReadFile("../../../putnami.extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Inputs map[string]struct {
				From  string   `json:"from"`
				Files []string `json:"files"`
			} `json:"inputs"`
			// `cache` is a bool OR an object in the manifest contract, so it is
			// read raw and asserted on its object form here.
			Cache json.RawMessage `json:"cache"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}

	task := manifest.Tasks["lint-staticcheck"]
	var cache struct {
		Enabled  *bool `json:"enabled"`
		NoOutput bool  `json:"noOutput"`
	}
	if err := json.Unmarshal(task.Cache, &cache); err != nil {
		t.Fatalf("lint-staticcheck cache policy = %s: %v", task.Cache, err)
	}
	if cache.Enabled == nil || !*cache.Enabled {
		t.Fatalf("lint-staticcheck must stay cacheable: %s", task.Cache)
	}
	if !cache.NoOutput {
		t.Fatal("lint-staticcheck must stay a noOutput task; batching produces no artifact to share")
	}
	wantInputs := map[string]string{
		"sources":          "project",
		"modules":          "project",
		"workspaceModules": "workspace",
		"extensionVersion": "runtime",
	}
	if len(task.Inputs) != len(wantInputs) {
		t.Fatalf("lint-staticcheck inputs = %v, want exactly %v", task.Inputs, wantInputs)
	}
	for name, from := range wantInputs {
		input, ok := task.Inputs[name]
		if !ok {
			t.Fatalf("lint-staticcheck lost its %q input", name)
		}
		if input.From != from {
			t.Fatalf("lint-staticcheck input %q from = %q, want %q", name, input.From, from)
		}
	}
	workspaceModules := task.Inputs["workspaceModules"].Files
	if len(workspaceModules) != 2 || workspaceModules[0] != "go.work" || workspaceModules[1] != "go.work.sum" {
		t.Fatalf("lint-staticcheck workspaceModules = %v, want [go.work go.work.sum]", workspaceModules)
	}
}
