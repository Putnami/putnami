package pkg

import (
	"os"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestStageFrameworkDocsWritesAIMD(t *testing.T) {
	workspaceRoot := t.TempDir()
	stageDir := t.TempDir()

	docPath := filepath.Join(workspaceRoot, "go", "framework", "app")
	if err := os.MkdirAll(docPath, 0o755); err != nil {
		t.Fatalf("mkdir doc path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docPath, "AI.md"), []byte("app docs"), 0o644); err != nil {
		t.Fatalf("write AI.md: %v", err)
	}

	stageFrameworkDocs(workspaceRoot, stageDir, jsonl.New())

	data, err := os.ReadFile(filepath.Join(stageDir, "framework-docs", "app", "AI.md"))
	if err != nil {
		t.Fatalf("read staged AI.md: %v", err)
	}
	if string(data) != "app docs" {
		t.Errorf("staged AI.md = %q, want %q", string(data), "app docs")
	}
}

func TestStageFrameworkDocsIgnoresClaudeMD(t *testing.T) {
	workspaceRoot := t.TempDir()
	stageDir := t.TempDir()

	docPath := filepath.Join(workspaceRoot, "go", "framework", "app")
	if err := os.MkdirAll(docPath, 0o755); err != nil {
		t.Fatalf("mkdir doc path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docPath, "CLAUDE.md"), []byte("unsupported docs"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	stageFrameworkDocs(workspaceRoot, stageDir, jsonl.New())

	if _, err := os.Stat(filepath.Join(stageDir, "framework-docs", "app", "AI.md")); !os.IsNotExist(err) {
		t.Errorf("staged AI.md error = %v, want not exist", err)
	}
}

func TestPrepareGoModuleIgnoresClaudeMD(t *testing.T) {
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()

	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"), []byte("module example.com/test\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "main.go"), []byte("package test\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "CLAUDE.md"), []byte("unsupported docs"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "test",
			Path:     "test",
			FullPath: projectRoot,
		},
	}

	if ok := prepareGoModule(ctx, jsonl.New(), "1.2.3", outputRoot, false); !ok {
		t.Fatal("prepareGoModule failed")
	}

	stageDir := filepath.Join(outputRoot, "go", "source")
	if _, err := os.Stat(filepath.Join(stageDir, "AI.md")); !os.IsNotExist(err) {
		t.Errorf("staged AI.md error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(stageDir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("staged CLAUDE.md error = %v, want not exist", err)
	}
}
