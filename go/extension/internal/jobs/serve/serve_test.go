package serve

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- computeWatchSnapshot ---

func TestComputeWatchSnapshot_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	snap := computeWatchSnapshot(dir)
	if snap == "" {
		t.Error("computeWatchSnapshot should return a non-empty hash even for empty dir")
	}
}

func TestComputeWatchSnapshot_WithGoFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := computeWatchSnapshot(dir)
	if snap == "" {
		t.Error("computeWatchSnapshot should return non-empty hash")
	}
}

func TestComputeWatchSnapshot_Deterministic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module foo"), 0o644)

	snap1 := computeWatchSnapshot(dir)
	snap2 := computeWatchSnapshot(dir)
	if snap1 != snap2 {
		t.Errorf("computeWatchSnapshot is not deterministic: %q != %q", snap1, snap2)
	}
}

func TestComputeWatchSnapshot_ChangesOnFileModification(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "main.go")
	os.WriteFile(goFile, []byte("package main"), 0o644)

	snap1 := computeWatchSnapshot(dir)

	// Ensure different mtime by sleeping briefly and re-writing
	time.Sleep(10 * time.Millisecond)
	if err := os.Chtimes(goFile, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	snap2 := computeWatchSnapshot(dir)
	if snap1 == snap2 {
		t.Error("computeWatchSnapshot should change when file mtime changes")
	}
}

func TestComputeWatchSnapshot_IgnoresNonGoFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)

	snap1 := computeWatchSnapshot(dir)

	// Add a non-Go file — snapshot should not change
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# readme"), 0o644)

	snap2 := computeWatchSnapshot(dir)
	if snap1 != snap2 {
		t.Error("computeWatchSnapshot should not change when only non-Go files are added")
	}
}

func TestComputeWatchSnapshot_IgnoresGitDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	snap1 := computeWatchSnapshot(dir)

	// Add a file inside .git — should be skipped
	gitDir := filepath.Join(dir, ".git")
	os.MkdirAll(gitDir, 0o755)
	os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main"), 0o644)

	snap2 := computeWatchSnapshot(dir)
	if snap1 != snap2 {
		t.Error("computeWatchSnapshot should ignore .git directory")
	}
}

func TestComputeWatchSnapshot_IgnoresPutnamiDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	snap1 := computeWatchSnapshot(dir)

	// Add a .go file inside .putnami — should be skipped
	putnamiDir := filepath.Join(dir, ".putnami")
	os.MkdirAll(putnamiDir, 0o755)
	os.WriteFile(filepath.Join(putnamiDir, "cache.go"), []byte("package cache"), 0o644)

	snap2 := computeWatchSnapshot(dir)
	if snap1 != snap2 {
		t.Error("computeWatchSnapshot should ignore .putnami directory")
	}
}

func TestComputeWatchSnapshot_TracksGoMod(t *testing.T) {
	dir := t.TempDir()
	goModPath := filepath.Join(dir, "go.mod")
	os.WriteFile(goModPath, []byte("module foo\n\ngo 1.21\n"), 0o644)

	snap1 := computeWatchSnapshot(dir)

	time.Sleep(10 * time.Millisecond)
	os.Chtimes(goModPath, time.Now(), time.Now().Add(time.Second))

	snap2 := computeWatchSnapshot(dir)
	if snap1 == snap2 {
		t.Error("computeWatchSnapshot should detect go.mod changes")
	}
}

func TestComputeWatchSnapshot_DifferentDirs(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	os.WriteFile(filepath.Join(dir1, "main.go"), []byte("package main\n// dir1"), 0o644)
	os.WriteFile(filepath.Join(dir2, "main.go"), []byte("package main\n// dir2"), 0o644)

	snap1 := computeWatchSnapshot(dir1)
	snap2 := computeWatchSnapshot(dir2)

	// Snapshots include file paths so they differ even with similar content
	if snap1 == snap2 {
		t.Error("computeWatchSnapshot should differ for different directory paths")
	}
}
