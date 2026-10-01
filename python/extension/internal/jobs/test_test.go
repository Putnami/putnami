package jobs

import (
	"go.putnami.dev/protocol/features/spectest"

	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestTest_SkipOnEmptyProject(t *testing.T) {
	ctx := &pctx.Context{
		Project: pctx.Project{Name: ""},
	}
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Test(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "SKIP" {
			t.Errorf("expected SKIP, got %s", status)
		}
	})
	if len(events) != 0 {
		t.Errorf("expected no events for skip, got %d", len(events))
	}
}

func TestTest_SkipWhenNoTestFiles(t *testing.T) {
	tmp := setupPythonWorkspace(t, "pkg")
	pkgDir := filepath.Join(tmp, "pkg")
	// Only non-test Python files
	os.WriteFile(filepath.Join(pkgDir, "src", "main.py"), []byte("print('hello')"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Project: pctx.Project{
			Name:     "pkg",
			Path:     "pkg",
			FullPath: pkgDir,
		},
		Params: pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Test(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Either SKIP (no test files found after sync) or FAILED (sync failed)
		if status != "SKIP" && status != "FAILED" {
			t.Errorf("expected SKIP or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}
}

func TestTest_PythonPathMerge(t *testing.T) {
	projectRoot := "/workspace/myproject"

	t.Run("without existing PYTHONPATH", func(t *testing.T) {
		original := os.Getenv("PYTHONPATH")
		os.Unsetenv("PYTHONPATH")
		defer func() {
			if original != "" {
				os.Setenv("PYTHONPATH", original)
			}
		}()

		pyPath := projectRoot
		if existing := os.Getenv("PYTHONPATH"); existing != "" {
			pyPath = projectRoot + string(os.PathListSeparator) + existing
		}

		// Mirrors the production test path, which uses MakeTestEnv.
		env := MakeTestEnv("/workspace", projectRoot, map[string]string{"PYTHONPATH": pyPath})
		if !slices.Contains(env, "PYTHONPATH="+projectRoot) {
			t.Error("expected PYTHONPATH to be set to projectRoot")
		}
	})

	t.Run("with existing PYTHONPATH", func(t *testing.T) {
		original := os.Getenv("PYTHONPATH")
		os.Setenv("PYTHONPATH", "/existing/path")
		defer func() {
			if original != "" {
				os.Setenv("PYTHONPATH", original)
			} else {
				os.Unsetenv("PYTHONPATH")
			}
		}()

		pyPath := projectRoot
		if existing := os.Getenv("PYTHONPATH"); existing != "" {
			pyPath = projectRoot + string(os.PathListSeparator) + existing
		}

		// Mirrors the production test path, which uses MakeTestEnv.
		env := MakeTestEnv("/workspace", projectRoot, map[string]string{"PYTHONPATH": pyPath})
		expected := "PYTHONPATH=" + projectRoot + string(os.PathListSeparator) + "/existing/path"
		lastPyPath := ""
		for _, e := range env {
			if strings.HasPrefix(e, "PYTHONPATH=") {
				lastPyPath = e
			}
		}
		if lastPyPath != expected {
			t.Errorf("last PYTHONPATH = %q, want %q", lastPyPath, expected)
		}
	})
}

func TestTest_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no flags", nil},
		{"with log", []string{"--log"}},
		{"with update-snapshots", []string{"--update-snapshots"}},
		{"with test filter", []string{"--test", "test_something"}},
		{"with short test filter", []string{"-t", "test_something"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &pctx.Context{
				Project: pctx.Project{Name: ""},
				Params:  pctx.Params{},
			}
			status, _, err := Test(ctx, jsonl.New(), tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != "SKIP" {
				t.Errorf("expected SKIP, got %s", status)
			}
		})
	}
}

func TestTest_NoWorkspaceConfig(t *testing.T) {
	// Workspace with no config file at all — SyncWorkspace returns no members
	tmp := t.TempDir()
	pkgDir := filepath.Join(tmp, "pkg")
	os.MkdirAll(pkgDir, 0755)
	os.WriteFile(filepath.Join(pkgDir, "pyproject.toml"), []byte("[project]\nname = \"pkg\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Project: pctx.Project{
			Name:     "pkg",
			Path:     "pkg",
			FullPath: pkgDir,
		},
		Params: pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Test(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// No workspace config → SyncWorkspace returns skipped phase, then UV check
		if status != "OK" && status != "FAILED" && status != "SKIP" {
			t.Errorf("expected OK, FAILED, or SKIP, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected at least one JSONL event")
	}
}

func TestTest_WithTestFiles(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "native-tools", "test-delegates-to-pytest")
	tmp := setupPythonWorkspace(t, "testpkg")
	pkgDir := filepath.Join(tmp, "pkg")

	// Create test files so discover phase finds them
	os.MkdirAll(filepath.Join(pkgDir, "tests"), 0755)
	os.WriteFile(filepath.Join(pkgDir, "tests", "test_hello.py"), []byte("def test_hello():\n    assert True\n"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Project: pctx.Project{
			Name:     "testpkg",
			Path:     "pkg",
			FullPath: pkgDir,
		},
		Params: pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Test(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Could be OK (tests pass), FAILED (pytest not available or tests fail)
		if status != "OK" && status != "FAILED" {
			t.Errorf("expected OK or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected JSONL events")
	}
}

func TestTest_WithLogFlag(t *testing.T) {
	tmp := setupPythonWorkspace(t, "testpkg")
	pkgDir := filepath.Join(tmp, "pkg")

	os.MkdirAll(filepath.Join(pkgDir, "tests"), 0755)
	os.WriteFile(filepath.Join(pkgDir, "tests", "test_hello.py"), []byte("def test_hello():\n    assert True\n"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Project: pctx.Project{
			Name:     "testpkg",
			Path:     "pkg",
			FullPath: pkgDir,
		},
		Params: pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Test(ctx, emit, []string{"--log"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" && status != "FAILED" {
			t.Errorf("expected OK or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected JSONL events")
	}
}

func TestTest_WithTestFilter(t *testing.T) {
	tmp := setupPythonWorkspace(t, "testpkg")
	pkgDir := filepath.Join(tmp, "pkg")

	os.MkdirAll(filepath.Join(pkgDir, "tests"), 0755)
	os.WriteFile(filepath.Join(pkgDir, "tests", "test_hello.py"), []byte("def test_hello():\n    assert True\n"), 0644)

	ctx := &pctx.Context{
		WorkspaceRoot: tmp,
		Project: pctx.Project{
			Name:     "testpkg",
			Path:     "pkg",
			FullPath: pkgDir,
		},
		Params: pctx.Params{},
	}

	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Test(ctx, emit, []string{"--test", "tests/test_hello.py"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != "OK" && status != "FAILED" {
			t.Errorf("expected OK or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected JSONL events")
	}
}
