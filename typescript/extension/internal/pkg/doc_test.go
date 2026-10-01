package pkg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyDocFilesPrefersAIMD(t *testing.T) {
	projectRoot := t.TempDir()
	outputPath := t.TempDir()

	if err := os.WriteFile(filepath.Join(projectRoot, "AI.md"), []byte("new"), 0o644); err != nil {
		t.Fatalf("write AI.md: %v", err)
	}
	copyDocFiles(projectRoot, outputPath)

	data, err := os.ReadFile(filepath.Join(outputPath, "AI.md"))
	if err != nil {
		t.Fatalf("read output AI.md: %v", err)
	}
	if string(data) != "new" {
		t.Errorf("output AI.md = %q, want %q", string(data), "new")
	}
}

func TestCopyDocFilesIgnoresClaudeMD(t *testing.T) {
	projectRoot := t.TempDir()
	outputPath := t.TempDir()

	if err := os.WriteFile(filepath.Join(projectRoot, "CLAUDE.md"), []byte("unsupported docs"), 0o644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	copyDocFiles(projectRoot, outputPath)

	if _, err := os.Stat(filepath.Join(outputPath, "AI.md")); !os.IsNotExist(err) {
		t.Errorf("output AI.md error = %v, want not exist", err)
	}
}
