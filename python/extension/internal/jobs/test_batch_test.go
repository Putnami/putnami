package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// makeTestBatchWorkspace creates a workspace with two sibling Python projects
// that each hold one test file, and a context selecting both.
func makeTestBatchWorkspace(t *testing.T) *pctx.Context {
	t.Helper()
	tmp := t.TempDir()
	selected := make([]pctx.ProjectRef, 0, 2)
	for _, name := range []string{"a", "b"} {
		full := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Join(full, "tests"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, "pyproject.toml"),
			[]byte("[project]\nname = \"pkg_"+name+"\"\nversion = \"0.1.0\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, "tests", "test_x.py"),
			[]byte("def test_x():\n    assert True\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		selected = append(selected, pctx.ProjectRef{
			ID:       "/" + name,
			Name:     name,
			Path:     name,
			FullPath: full,
		})
	}
	return &pctx.Context{
		WorkspaceRoot:    tmp,
		Project:          pctx.Project{Name: "a", Path: "a", FullPath: filepath.Join(tmp, "a")},
		SelectedProjects: selected,
		Params:           pctx.Params{},
	}
}

// stubSync bypasses the real UV workspace sync so batch logic can be tested
// without a uv toolchain.
func stubSync(t *testing.T) {
	t.Helper()
	orig := syncWorkspaceFn
	t.Cleanup(func() { syncWorkspaceFn = orig })
	syncWorkspaceFn = func(string, *jsonl.Emitter) bool { return true }
}

// stubRunner replaces the pytest command runner and restores it after the test.
func stubRunner(t *testing.T, fn func(ctx context.Context, name string, args, envDir []string) (string, error)) {
	t.Helper()
	orig := pytestCommandRunner
	t.Cleanup(func() { pytestCommandRunner = orig })
	pytestCommandRunner = func(cmdCtx context.Context, name string, args []string, dir string, env []string) (string, error) {
		return fn(cmdCtx, name, args, []string{dir})
	}
}

// projectFromArgs returns the project path pytest was pointed at (the trailing
// positional argument), so a stub can vary its output per project.
func projectFromArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

func TestTestBatchSplitsPerProjectResults(t *testing.T) {
	stubSync(t)
	ctx := makeTestBatchWorkspace(t)
	stubRunner(t, func(_ context.Context, _ string, args, _ []string) (string, error) {
		if projectFromArgs(args) == "b" {
			return "1 failed in 0.01s", errors.New("exit status 1")
		}
		return "1 passed in 0.01s", nil
	})

	status, data, err := TestBatch(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("TestBatch status=%q err=%v, want OK/nil", status, err)
	}
	results := data["batchResults"].([]pyTestBatchProjectResult)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].ProjectID != "/a" || results[0].Status != "OK" {
		t.Fatalf("project a = %+v, want OK", results[0])
	}
	if results[1].ProjectID != "/b" || results[1].Status != "FAILED" {
		t.Fatalf("project b = %+v, want FAILED", results[1])
	}
	if ts := results[0].Data["testSummary"].(map[string]any); ts["passed"].(int) != 1 || ts["failed"].(int) != 0 {
		t.Fatalf("project a testSummary = %v", ts)
	}
	if ts := results[1].Data["testSummary"].(map[string]any); ts["failed"].(int) != 1 {
		t.Fatalf("project b testSummary = %v", ts)
	}
}

func TestTestBatchCrashIsolationRetriesAndDoesNotMaskPeers(t *testing.T) {
	stubSync(t)
	ctx := makeTestBatchWorkspace(t)
	var aCalls int
	stubRunner(t, func(_ context.Context, _ string, args, _ []string) (string, error) {
		if projectFromArgs(args) == "a" {
			aCalls++
			// Non-graceful crash: no parseable pytest output.
			return "Traceback: internal error", errors.New("exit status 2")
		}
		return "1 passed in 0.01s", nil
	})

	status, data, err := TestBatch(ctx, jsonl.New(), nil)
	if err != nil || status != "OK" {
		t.Fatalf("crash must not fail the whole batch: status=%q err=%v", status, err)
	}
	if aCalls != 2 {
		t.Fatalf("crashed suite ran %d times, want one respawn (2)", aCalls)
	}
	results := data["batchResults"].([]pyTestBatchProjectResult)
	if results[0].Status != "FAILED" {
		t.Fatalf("crashed project a = %+v, want FAILED", results[0])
	}
	if results[1].Status != "OK" {
		t.Fatalf("healthy project b = %+v, want OK (not masked by crash)", results[1])
	}
	if len(results[0].Diagnostics) == 0 {
		t.Fatal("crashed project must carry a diagnostic")
	}
}

func TestTestBatchHungSuiteRespectsTimeout(t *testing.T) {
	stubSync(t)
	origTimeout := batchSuiteTimeout
	t.Cleanup(func() { batchSuiteTimeout = origTimeout })
	batchSuiteTimeout = 40 * time.Millisecond

	ctx := makeTestBatchWorkspace(t)
	stubRunner(t, func(cmdCtx context.Context, _ string, args, _ []string) (string, error) {
		if projectFromArgs(args) == "a" {
			// Simulate a hung suite that only returns when the deadline fires.
			<-cmdCtx.Done()
			return "", cmdCtx.Err()
		}
		return "1 passed in 0.01s", nil
	})

	done := make(chan struct{})
	var status string
	var data map[string]any
	go func() {
		status, data, _ = TestBatch(ctx, jsonl.New(), nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("batch stalled on a hung suite; per-project timeout not honored")
	}

	if status != "OK" {
		t.Fatalf("status = %q, want OK (hung suite isolated, batch survives)", status)
	}
	results := data["batchResults"].([]pyTestBatchProjectResult)
	if results[0].Status != "FAILED" {
		t.Fatalf("hung project a = %+v, want FAILED", results[0])
	}
	if results[1].Status != "OK" {
		t.Fatalf("healthy project b = %+v, want OK", results[1])
	}
}

// TestTestRunManifestBatchableAvoidsPerProjectConfig pins a lesson for the
// Python manifest: batch config candidates must be workspace/extension-scoped
// shared files (not {projectRoot}/... always-present files), or sibling
// projects would each get a distinct digest and never batch.
func TestTestRunManifestBatchableAvoidsPerProjectConfig(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Batchable *struct {
				Tool        string   `json:"tool"`
				MaxWorkers  int      `json:"maxWorkers"`
				MaxProjects int      `json:"maxProjects"`
				ConfigFiles []string `json:"configFiles"`
			} `json:"batchable"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	task, ok := manifest.Tasks["test-run"]
	if !ok || task.Batchable == nil {
		t.Fatal("test-run task has no batchable policy")
	}
	batch := task.Batchable
	if batch.Tool != "pytest" {
		t.Fatalf("test-run batch tool = %q, want pytest", batch.Tool)
	}
	if batch.MaxWorkers <= 0 || batch.MaxProjects <= 0 {
		t.Fatalf("conservative gate missing: maxWorkers=%d maxProjects=%d", batch.MaxWorkers, batch.MaxProjects)
	}
	if len(batch.ConfigFiles) == 0 {
		t.Fatal("test-run batch declares no config files")
	}
	for _, cf := range batch.ConfigFiles {
		if strings.Contains(cf, "{projectRoot}") {
			t.Fatalf("config file %q is per-project; siblings would never share a batch key", cf)
		}
		if !strings.HasPrefix(cf, "{workspaceRoot}") && !strings.HasPrefix(cf, "{extensionRoot}") {
			t.Fatalf("config file %q is not a shared workspace/extension file", cf)
		}
	}
}
