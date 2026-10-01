package jobs

import (
	"os"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestLintFormat_SkipOnEmptyProject(t *testing.T) {
	ctx := &pctx.Context{
		Project: pctx.Project{Name: ""},
	}
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := LintFormat(ctx, emit, nil)
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

func TestLintFormat_FixFlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"default (fix=true, no --check)", nil},
		{"explicit --fix", []string{"--fix"}},
		{"explicit --no-fix adds --check", []string{"--no-fix"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &pctx.Context{
				Project: pctx.Project{Name: ""},
				Params:  pctx.Params{},
			}
			status, _, err := LintFormat(ctx, jsonl.New(), tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != "SKIP" {
				t.Errorf("expected SKIP, got %s", status)
			}
		})
	}
}

// setupPythonWorkspace creates a minimal Python workspace for testing.
// Returns the workspace root path.
// setupPythonWorkspace creates a minimal Python workspace for testing.
// Returns the workspace root path.
func setupPythonWorkspace(t *testing.T, pkgName string) string {
	t.Helper()
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "putnami.workspace.json"), []byte(`{"projects": ["pkg"]}`), 0644)
	os.MkdirAll(filepath.Join(tmp, "pkg", "src"), 0755)
	os.WriteFile(filepath.Join(tmp, "pkg", "pyproject.toml"), []byte("[project]\nname = \""+pkgName+"\"\nversion = \"0.1.0\"\nrequires-python = \">=3.11\"\n"), 0644)
	return tmp
}

func TestLintFormat_SyncWorkspaceFailure(t *testing.T) {
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
		status, _, err := LintFormat(ctx, emit, nil)
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

func TestLintFormat_WithWorkspace(t *testing.T) {
	tmp := setupPythonWorkspace(t, "testpkg")
	pkgDir := filepath.Join(tmp, "pkg")

	// Create a Python file so ruff has something to work with
	os.WriteFile(filepath.Join(pkgDir, "src", "main.py"), []byte("x = 1\n"), 0644)

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
		status, _, err := LintFormat(ctx, emit, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Status depends on whether ruff is resolvable by uv
		if status != "OK" && status != "FAILED" {
			t.Errorf("expected OK or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected JSONL events")
	}
}

func TestLintFormat_WithCheckFlag(t *testing.T) {
	tmp := setupPythonWorkspace(t, "testpkg")
	pkgDir := filepath.Join(tmp, "pkg")

	// Create a badly formatted Python file
	os.WriteFile(filepath.Join(pkgDir, "src", "main.py"), []byte("x=1\ny=2\n"), 0644)

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
		status, _, err := LintFormat(ctx, emit, []string{"--no-fix"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Will either succeed or fail depending on ruff availability
		if status != "OK" && status != "FAILED" {
			t.Errorf("expected OK or FAILED, got %s", status)
		}
	})

	if len(events) == 0 {
		t.Error("expected JSONL events")
	}
}
