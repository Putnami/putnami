package jobs

import (
	"go.putnami.dev/protocol/features/spectest"

	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestRun_SkipOnEmptyProject(t *testing.T) {
	ctx := &pctx.Context{Project: pctx.Project{Name: ""}}
	status, data, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != "SKIP" {
		t.Errorf("expected SKIP, got %s", status)
	}
	if data != nil {
		t.Errorf("expected nil data on skip, got %v", data)
	}
}

func TestRun_SyncWorkspaceFailure(t *testing.T) {
	ctx := &pctx.Context{
		WorkspaceRoot: "/nonexistent/path",
		Project: pctx.Project{
			Name:     "mypkg",
			Path:     "pkg",
			FullPath: "/nonexistent/path/pkg",
		},
		Params: pctx.Params{},
	}
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Run(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "FAILED" {
			t.Errorf("expected FAILED, got %s", status)
		}
	})
	if len(events) == 0 {
		t.Error("expected events from failed sync")
	}
}

func TestRun_ForwardsExitCodeInData(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "native-tools", "the-application-entrypoint-result-is-exposed-through-the-job")
	// Without uv the workspace sync fails before the workload starts, so the
	// job reports FAILED with no result data and there is no exit code to
	// forward.
	if _, err := osexec.LookPath("uv"); err != nil {
		t.Skipf("uv unavailable: %v", err)
	}
	tmp := setupPythonWorkspace(t, "runpkg")
	pkgDir := filepath.Join(tmp, "pkg")

	// A workload that exits non-zero must surface its exact code in result data
	// so the CLI can forward it.
	os.WriteFile(filepath.Join(pkgDir, "src", "main.py"), []byte("import sys\nsys.exit(7)\n"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Project: pctx.Project{
			Name:     "runpkg",
			Path:     "pkg",
			FullPath: pkgDir,
		},
		Params: pctx.Params{},
	}

	status, data, err := Run(ctx, jsonl.New(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The workload exits 7, so a FAILED job carries a non-zero exit code.
	if status == "FAILED" {
		if code, ok := data[runExitCodeKey]; !ok {
			t.Errorf("expected %q in result data, got %v", runExitCodeKey, data)
		} else if code == 0 {
			t.Errorf("expected non-zero exit code in data, got %v", code)
		}
	}
}
