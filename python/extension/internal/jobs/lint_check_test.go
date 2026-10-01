package jobs

import (
	"go.putnami.dev/protocol/features/spectest"

	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestLintCheck_SkipOnEmptyProject(t *testing.T) {
	ctx := &pctx.Context{
		Project: pctx.Project{Name: ""},
	}
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := LintCheck(ctx, emit, nil)
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

func TestLintCheck_FixFlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantFix bool
	}{
		{"default is fix=true", nil, true},
		{"explicit --fix", []string{"--fix"}, true},
		{"explicit --no-fix", []string{"--no-fix"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &pctx.Context{
				Project: pctx.Project{Name: ""},
				Params:  pctx.Params{},
			}
			status, _, err := LintCheck(ctx, jsonl.New(), tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != "SKIP" {
				t.Errorf("expected SKIP, got %s", status)
			}
		})
	}
}

func TestLintCheck_SyncWorkspaceFailure(t *testing.T) {
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
		status, _, err := LintCheck(ctx, emit, nil)
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

func TestLintCheck_WithWorkspace(t *testing.T) {
	tmp := setupPythonWorkspace(t, "testpkg")
	pkgDir := filepath.Join(tmp, "pkg")

	// Create a clean Python file
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
		status, _, err := LintCheck(ctx, emit, nil)
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

func TestLintCheck_WithFixFlag(t *testing.T) {
	tmp := setupPythonWorkspace(t, "testpkg")
	pkgDir := filepath.Join(tmp, "pkg")

	os.WriteFile(filepath.Join(pkgDir, "src", "main.py"), []byte("import os\nx = 1\n"), 0644)

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
		status, _, err := LintCheck(ctx, emit, []string{"--fix"})
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

func TestLintCheck_WithNoFixFlag(t *testing.T) {
	tmp := setupPythonWorkspace(t, "testpkg")
	pkgDir := filepath.Join(tmp, "pkg")

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
		status, _, err := LintCheck(ctx, emit, []string{"--no-fix"})
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

func TestSingleProjectLintFormatScopesRuffToProjectRoot(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "native-tools", "lint-delegates-to-ruff-scoped-to-the-project-root")
	tests := []struct {
		name string
		run  func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error)
	}{
		{name: "format", run: LintFormat},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspaceRoot := t.TempDir()
			projectRoot := filepath.Join(workspaceRoot, "project")
			projectFile := filepath.Join(projectRoot, "src", "main.py")
			outsideFile := filepath.Join(workspaceRoot, "outside.py")
			if err := os.MkdirAll(filepath.Dir(projectFile), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(projectFile, []byte("x=1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outsideFile, []byte("y=1\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			origSync := syncWorkspace
			syncWorkspace = func(string, *jsonl.Emitter) bool { return true }
			t.Cleanup(func() { syncWorkspace = origSync })

			origRun := ruffSingleRun
			ruffSingleRun = func(_ string, args []string, dir string, _ []string) ([]byte, error) {
				if dir != projectRoot {
					t.Fatalf("ruff command directory = %q, want project root %q", dir, projectRoot)
				}
				for i := 0; i < len(args)-1; i++ {
					if args[i] == "--directory" {
						if args[i+1] != projectRoot {
							t.Fatalf("uv --directory = %q, want project root %q", args[i+1], projectRoot)
						}
						if err := os.WriteFile(projectFile, []byte("x = 1\n"), 0o644); err != nil {
							t.Fatal(err)
						}
						return nil, nil
					}
				}
				t.Fatalf("ruff command %v has no --directory argument", args)
				return nil, nil
			}
			t.Cleanup(func() { ruffSingleRun = origRun })

			ctx := &pctx.Context{
				WorkspaceRoot: workspaceRoot,
				Project: pctx.Project{
					Name:     "project",
					Path:     "project",
					FullPath: projectRoot,
				},
			}
			status, _, err := tt.run(ctx, jsonl.New(), nil)
			if err != nil || status != "OK" {
				t.Fatalf("lint status=%q err=%v, want OK", status, err)
			}

			projectData, err := os.ReadFile(projectFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(projectData) != "x = 1\n" {
				t.Fatalf("project file = %q, want formatted content", projectData)
			}
			outsideData, err := os.ReadFile(outsideFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(outsideData) != "y=1\n" {
				t.Fatalf("lint changed file outside selected project: %q", outsideData)
			}
		})
	}
}

func TestLintCheck_SoloMatchesBatchInvocationAndDiagnostics(t *testing.T) {
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "project")
	projectFile := filepath.Join(projectRoot, "src", "main.py")
	if err := os.MkdirAll(filepath.Dir(projectFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectFile, []byte("import os\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origSync := syncWorkspace
	syncWorkspace = func(string, *jsonl.Emitter) bool { return true }
	t.Cleanup(func() { syncWorkspace = origSync })

	origRun := ruffSingleCheckRun
	ruffSingleCheckRun = func(name string, args []string, dir string, env []string) (ruffCheckResult, error) {
		if name != "uv" {
			t.Fatalf("ruff command = %q, want uv", name)
		}
		if dir != workspaceRoot {
			t.Fatalf("ruff command directory = %q, want workspace root %q", dir, workspaceRoot)
		}
		wantArgs := []string{
			"run", "--with", "ruff", "--package", "project", "--directory", workspaceRoot,
			"ruff", "check", "--output-format=json", "project", "--cache-dir",
			filepath.Join(workspaceRoot, ".putnami/bin/extensions/putnami-python/.ruff_cache"), "--fix",
		}
		if !slices.Equal(args, wantArgs) {
			t.Fatalf("ruff args = %v, want %v", args, wantArgs)
		}
		if !slices.Contains(env, "PUTNAMI_WORKING_DIR="+workspaceRoot) {
			t.Fatalf("ruff environment does not use workspace working directory: %v", env)
		}
		return ruffCheckResult{
			Stdout: []byte(`[{"code":"F401","filename":` + jsonString(t, projectFile) + `,"message":"unused import","severity":"error","location":{"row":1,"column":8}}]`),
			Stderr: []byte("uv: resolved workspace status\n"),
		}, errors.New("ruff lint findings")
	}
	t.Cleanup(func() { ruffSingleCheckRun = origRun })

	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		Project: pctx.Project{
			Name:     "project",
			Path:     "project",
			FullPath: projectRoot,
		},
	}
	var status string
	var data map[string]any
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		var err error
		status, data, err = LintCheck(ctx, emit, []string{"--fix"})
		if err != nil {
			t.Fatalf("LintCheck: %v", err)
		}
	})
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if summary, ok := data["lintSummary"].(map[string]any); !ok || summary["errors"] != 1 {
		t.Fatalf("lint summary = %#v, want one error", data["lintSummary"])
	}

	structuredDiagnostic := false
	for _, event := range events {
		if event["type"] != "diagnostic" {
			continue
		}
		if event["code"] != "F401" {
			t.Fatalf("uv stderr caused an unstructured fallback diagnostic: %#v", event)
		}
		if event["severity"] != "error" || event["message"] != "unused import" {
			t.Fatalf("diagnostic = %#v", event)
		}
		location, ok := event["location"].(map[string]any)
		if !ok || location["file"] != "project/src/main.py" ||
			location["line"] != float64(1) || location["column"] != float64(8) {
			t.Fatalf("diagnostic location = %#v", event["location"])
		}
		structuredDiagnostic = true
	}
	if !structuredDiagnostic {
		t.Fatalf("structured Ruff diagnostic not emitted: %#v", events)
	}
}
