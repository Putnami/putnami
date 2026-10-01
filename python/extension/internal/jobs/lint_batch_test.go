package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
)

// stubSyncWorkspace makes the one-time UV workspace sync a no-op so batch tests
// exercise the split logic without a real `uv` toolchain on PATH.
func stubSyncWorkspace(t *testing.T) {
	t.Helper()
	orig := syncWorkspace
	t.Cleanup(func() { syncWorkspace = orig })
	syncWorkspace = func(string, *jsonl.Emitter) bool { return true }
}

// stubRuffExec replaces the ruff subprocess seam.
func stubRuffExec(t *testing.T, fn func(string, []string, ...exec.Option) (*exec.Result, error)) {
	t.Helper()
	orig := ruffExecRun
	t.Cleanup(func() { ruffExecRun = orig })
	ruffExecRun = fn
}

// jsonString is value as a JSON string literal, the way ruff writes a file
// name: a Windows path keeps its backslashes, escaped.
func jsonString(t *testing.T, value string) string {
	t.Helper()
	quoted, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(quoted)
}

func makeLintBatchContext(t *testing.T) (*pctx.Context, string) {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{"pkg_a", "pkg_b"} {
		if err := os.MkdirAll(filepath.Join(root, path, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path, "pyproject.toml"),
			[]byte("[project]\nname = \""+path+"\"\nversion = \"0.1.0\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Project:       pctx.Project{Name: "pkg_a", Path: "pkg_a", FullPath: filepath.Join(root, "pkg_a")},
		Params:        pctx.Params{},
		SelectedProjects: []pctx.ProjectRef{
			{ID: "/pkg_a", Name: "pkg_a", Path: "pkg_a", FullPath: filepath.Join(root, "pkg_a")},
			{ID: "/pkg_b", Name: "pkg_b", Path: "pkg_b", FullPath: filepath.Join(root, "pkg_b")},
		},
	}
	return ctx, root
}

func TestRunLintBatch_CheckOneInvocationSplitsStatusesAndDiagnostics(t *testing.T) {
	stubSyncWorkspace(t)
	ctx, root := makeLintBatchContext(t)

	var calls int
	var gotArgs []string
	stubRuffExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		gotArgs = append([]string(nil), args...)
		// pkg_a uses an absolute filename (as real ruff emits), pkg_b a
		// workspace-relative one, so batchWorkspacePath is exercised both ways.
		absA := jsonString(t, filepath.Join(root, "pkg_a", "src", "main.py"))
		stdout := `[
			{"code":"F401","filename":` + absA + `,"message":"unused import","severity":"error","location":{"row":1,"column":8}},
			{"code":"E501","filename":"pkg_b/src/main.py","message":"line too long","severity":"error","location":{"row":2,"column":3}}
		]`
		return &exec.Result{Success: false, ExitCode: 1, Stdout: stdout}, nil
	})

	status, data, err := runLintBatch(ctx, jsonl.New(), lintBatchCheck, true)
	if err != nil {
		t.Fatalf("runLintBatch: %v", err)
	}
	if status != "OK" {
		t.Fatalf("aggregate status = %q, want protocol success", status)
	}
	if calls != 1 {
		t.Fatalf("ruff calls = %d, want one", calls)
	}
	for _, want := range []string{"check", "--output-format=json", "--fix", "pkg_a", "pkg_b"} {
		if !slices.Contains(gotArgs, want) {
			t.Fatalf("batch args %v missing %q", gotArgs, want)
		}
	}

	results, ok := data["batchResults"].([]lintBatchProjectResult)
	if !ok || len(results) != 2 {
		t.Fatalf("batchResults = %#v", data["batchResults"])
	}
	if results[0].ProjectID != "/pkg_a" || results[0].Status != "FAILED" ||
		len(results[0].Diagnostics) != 1 ||
		results[0].Diagnostics[0].File != "pkg_a/src/main.py" ||
		results[0].Diagnostics[0].Category != "F401" ||
		results[0].Summary.Errors != 1 {
		t.Fatalf("project a result = %+v", results[0])
	}
	if results[1].ProjectID != "/pkg_b" || results[1].Status != "FAILED" ||
		len(results[1].Diagnostics) != 1 ||
		results[1].Diagnostics[0].File != "pkg_b/src/main.py" ||
		results[1].Summary.Errors != 1 {
		t.Fatalf("project b result = %+v", results[1])
	}
}

func TestRunLintBatch_CheckCleanProjectStaysOK(t *testing.T) {
	stubSyncWorkspace(t)
	ctx, root := makeLintBatchContext(t)

	stubRuffExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		absA := jsonString(t, filepath.Join(root, "pkg_a", "src", "main.py"))
		stdout := `[{"code":"F401","filename":` + absA + `,"message":"unused import","severity":"error","location":{"row":1,"column":8}}]`
		return &exec.Result{Success: false, ExitCode: 1, Stdout: stdout}, nil
	})

	_, data, err := runLintBatch(ctx, jsonl.New(), lintBatchCheck, true)
	if err != nil {
		t.Fatalf("runLintBatch: %v", err)
	}
	results := data["batchResults"].([]lintBatchProjectResult)
	// Only pkg_a owns the error; pkg_b must not inherit a sibling's failure.
	if results[0].Status != "FAILED" {
		t.Fatalf("pkg_a status = %q, want FAILED", results[0].Status)
	}
	if results[1].Status != "OK" || len(results[1].Diagnostics) != 0 {
		t.Fatalf("pkg_b must stay clean, got %+v", results[1])
	}
}

func TestRunLintBatch_CheckCrashFailsEveryProject(t *testing.T) {
	stubSyncWorkspace(t)
	ctx, _ := makeLintBatchContext(t)

	stubRuffExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		// Non-JSON stdout simulates a ruff crash: fail closed for everyone.
		return &exec.Result{Success: false, ExitCode: 2, Stderr: "panic: ruff exploded"}, nil
	})

	_, data, err := runLintBatch(ctx, jsonl.New(), lintBatchCheck, false)
	if err != nil {
		t.Fatalf("runLintBatch: %v", err)
	}
	results := data["batchResults"].([]lintBatchProjectResult)
	for _, r := range results {
		if r.Status != "FAILED" {
			t.Fatalf("project %s status = %q, want FAILED on crash", r.ProjectID, r.Status)
		}
		// The crash cause must be forwarded, not swallowed behind a bare FAILED.
		// The single-project path emits the ruff tail; the batch path must too.
		if len(r.Diagnostics) == 0 {
			t.Fatalf("project %s FAILED on crash but has no diagnostic explaining why", r.ProjectID)
		}
		if !strings.Contains(r.Diagnostics[0].Description, "ruff exploded") {
			t.Fatalf("project %s diagnostic %q dropped the ruff error tail", r.ProjectID, r.Diagnostics[0].Description)
		}
		if r.Summary.Errors == 0 {
			t.Fatalf("project %s crash diagnostic did not count as an error", r.ProjectID)
		}
	}
}

// TestLintBatchConfigFilesShareWorkspaceConfig guards a batch-grouping
// regression. Every UV workspace member owns a
// pyproject.toml, so if {projectRoot}/pyproject.toml were a batch-key config
// candidate it would resolve to a distinct path per project — giving each
// project a distinct batch key and making the batch path unreachable for
// ordinary workspaces. Sibling projects that inherit the workspace ruff config
// must instead resolve to the SAME config file so the scheduler folds them into
// one group.
func TestLintBatchConfigFilesShareWorkspaceConfig(t *testing.T) {
	// A UV workspace: two members, each with its own pyproject.toml (as every
	// member has), plus a shared workspace pyproject.toml; no ruff.toml anywhere.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[tool.ruff]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"a", "b"} {
		dir := filepath.Join(root, member)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname=\""+member+"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// All four halves of the ruff check/fix split keep the same batchable
	// config candidates: which of them the lint pipeline
	// schedules depends on --fix, but a check-only run must batch exactly as
	// well as a fixing one.
	for _, task := range []string{"lint-format-fix", "lint-format-readonly", "lint-check-fix", "lint-check-readonly"} {
		tool, configFiles := manifestBatchable(t, task)
		if tool != "ruff" {
			t.Fatalf("%s batchable tool = %q, want ruff", task, tool)
		}
		resolvedA := resolveFirstExistingConfig(configFiles, root, "a")
		resolvedB := resolveFirstExistingConfig(configFiles, root, "b")
		if resolvedA == "" {
			t.Fatalf("%s: no config candidate resolved for a UV member (would digest as \"none\")", task)
		}
		if resolvedA != resolvedB {
			t.Fatalf("%s: sibling members resolve to different config files (%q vs %q) — they will never batch", task, resolvedA, resolvedB)
		}
		if resolvedA != filepath.Join(root, "pyproject.toml") {
			t.Fatalf("%s: shared config resolved to %q, want the workspace pyproject.toml", task, resolvedA)
		}
	}
}

// manifestBatchable reads the shipped extension manifest and returns the
// batchable tool + configFiles declared for a task, so the test guards the real
// config rather than a Go copy of it.
func manifestBatchable(t *testing.T, task string) (string, []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Batchable struct {
				Tool        string   `json:"tool"`
				ConfigFiles []string `json:"configFiles"`
			} `json:"batchable"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	def, ok := manifest.Tasks[task]
	if !ok {
		t.Fatalf("manifest has no task %q", task)
	}
	return def.Batchable.Tool, def.Batchable.ConfigFiles
}

// resolveFirstExistingConfig mirrors the scheduler's batchConfigDigest candidate
// resolution: expand {projectRoot}/{workspaceRoot} and return the first existing
// regular file (the file whose path+bytes seed the batch key), or "" for none.
func resolveFirstExistingConfig(configFiles []string, wsRoot, projectPath string) string {
	projRoot := filepath.Join(wsRoot, projectPath)
	repl := strings.NewReplacer("{projectRoot}", projRoot, "{workspaceRoot}", wsRoot)
	for _, candidate := range configFiles {
		path := repl.Replace(candidate)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return filepath.Clean(path)
		}
	}
	return ""
}

func TestRunLintBatch_FormatFixWritesEveryProject(t *testing.T) {
	stubSyncWorkspace(t)
	ctx, root := makeLintBatchContext(t)
	aFile := filepath.Join(root, "pkg_a", "src", "main.py")
	bFile := filepath.Join(root, "pkg_b", "src", "main.py")
	for _, file := range []string{aFile, bFile} {
		if err := os.WriteFile(file, []byte("x=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var calls int
	stubRuffExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		if !slices.Contains(args, "format") || !slices.Contains(args, "pkg_a") || !slices.Contains(args, "pkg_b") {
			t.Fatalf("format batch args = %v", args)
		}
		if slices.Contains(args, "--check") {
			t.Fatalf("fix batch must not pass --check: %v", args)
		}
		for _, file := range []string{aFile, bFile} {
			if err := os.WriteFile(file, []byte("x = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, data, err := runLintBatch(ctx, jsonl.New(), lintBatchFormat, true)
	if err != nil || status != "OK" {
		t.Fatalf("format batch status=%q err=%v", status, err)
	}
	if calls != 1 {
		t.Fatalf("format calls = %d, want one", calls)
	}
	results := data["batchResults"].([]lintBatchProjectResult)
	if len(results) != 2 || results[0].Status != "OK" || results[1].Status != "OK" {
		t.Fatalf("format split results = %+v", results)
	}
	for _, file := range []string{aFile, bFile} {
		if content, err := os.ReadFile(file); err != nil || string(content) != "x = 1\n" {
			t.Fatalf("fix write %s = %q, %v", file, content, err)
		}
	}
}

func TestRunLintBatch_FormatCheckSplitsFindings(t *testing.T) {
	stubSyncWorkspace(t)
	ctx, _ := makeLintBatchContext(t)

	stubRuffExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if !slices.Contains(args, "--check") {
			t.Fatalf("no-fix format must pass --check: %v", args)
		}
		return &exec.Result{
			Success:  false,
			ExitCode: 1,
			Stderr:   "Would reformat: pkg_a/src/main.py\n1 file would be reformatted\n",
		}, nil
	})

	_, data, err := runLintBatch(ctx, jsonl.New(), lintBatchFormat, false)
	if err != nil {
		t.Fatalf("runLintBatch: %v", err)
	}
	results := data["batchResults"].([]lintBatchProjectResult)
	if results[0].Status != "FAILED" || len(results[0].Diagnostics) != 1 ||
		results[0].Diagnostics[0].Category != "format" {
		t.Fatalf("pkg_a should own the format finding, got %+v", results[0])
	}
	if results[1].Status != "OK" || len(results[1].Diagnostics) != 0 {
		t.Fatalf("pkg_b should be clean, got %+v", results[1])
	}
}

func TestRunLintBatch_NoSelectedProjectsFails(t *testing.T) {
	ctx := &pctx.Context{WorkspaceRoot: t.TempDir()}
	status, _, err := runLintBatch(ctx, jsonl.New(), lintBatchCheck, true)
	if err == nil || status != "FAILED" {
		t.Fatalf("empty selection = (%q, %v), want FAILED error", status, err)
	}
}

func TestRunLintBatch_SyncFailureFailsClosed(t *testing.T) {
	orig := syncWorkspace
	t.Cleanup(func() { syncWorkspace = orig })
	syncWorkspace = func(string, *jsonl.Emitter) bool { return false }

	ctx, _ := makeLintBatchContext(t)
	var calls int
	stubRuffExec(t, func(_ string, _ []string, _ ...exec.Option) (*exec.Result, error) {
		calls++
		return &exec.Result{Success: true}, nil
	})

	status, data, err := runLintBatch(ctx, jsonl.New(), lintBatchCheck, true)
	if err != nil {
		t.Fatalf("runLintBatch: %v", err)
	}
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED when sync fails", status)
	}
	if data != nil {
		t.Fatalf("data = %#v, want nil when sync fails", data)
	}
	if calls != 0 {
		t.Fatalf("ruff invoked %d times, want 0 when sync fails", calls)
	}
}

func TestLintFormat_BatchDispatch(t *testing.T) {
	stubSyncWorkspace(t)
	ctx, _ := makeLintBatchContext(t)
	var gotArgs []string
	stubRuffExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		gotArgs = append([]string(nil), args...)
		return &exec.Result{Success: true, ExitCode: 0}, nil
	})

	status, data, err := LintFormat(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("LintFormat batch status=%q err=%v", status, err)
	}
	if !slices.Contains(gotArgs, "format") {
		t.Fatalf("LintFormat did not dispatch to the ruff format batch: %v", gotArgs)
	}
	if _, ok := data["batchResults"]; !ok {
		t.Fatalf("LintFormat batch missing batchResults: %#v", data)
	}
}

func TestLintCheck_BatchDispatch(t *testing.T) {
	stubSyncWorkspace(t)
	ctx, _ := makeLintBatchContext(t)
	var gotArgs []string
	stubRuffExec(t, func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		gotArgs = append([]string(nil), args...)
		return &exec.Result{Success: true, ExitCode: 0, Stdout: "[]"}, nil
	})

	status, data, err := LintCheck(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("LintCheck batch status=%q err=%v", status, err)
	}
	if !slices.Contains(gotArgs, "check") {
		t.Fatalf("LintCheck did not dispatch to the ruff check batch: %v", gotArgs)
	}
	if _, ok := data["batchResults"]; !ok {
		t.Fatalf("LintCheck batch missing batchResults: %#v", data)
	}
}
