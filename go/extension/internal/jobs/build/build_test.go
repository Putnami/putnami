package build

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// --- setEnv ---

func TestSetEnv_NewKey(t *testing.T) {
	env := []string{"A=1", "B=2"}
	result := setEnv(env, "C", "3")
	if len(result) != 3 {
		t.Fatalf("len = %d, want 3", len(result))
	}
	if result[2] != "C=3" {
		t.Errorf("result[2] = %q, want %q", result[2], "C=3")
	}
}

func TestSetEnv_ReplaceExistingKey(t *testing.T) {
	env := []string{"A=1", "B=2", "C=3"}
	result := setEnv(env, "B", "99")
	if len(result) != 3 {
		t.Fatalf("len = %d, want 3 (no new element)", len(result))
	}
	if result[1] != "B=99" {
		t.Errorf("result[1] = %q, want %q", result[1], "B=99")
	}
}

func TestSetEnv_EmptyEnv(t *testing.T) {
	result := setEnv(nil, "KEY", "value")
	if len(result) != 1 {
		t.Fatalf("len = %d, want 1", len(result))
	}
	if result[0] != "KEY=value" {
		t.Errorf("result[0] = %q, want %q", result[0], "KEY=value")
	}
}

func TestSetEnv_EmptyValue(t *testing.T) {
	env := []string{"A=1"}
	result := setEnv(env, "B", "")
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	if result[1] != "B=" {
		t.Errorf("result[1] = %q, want %q", result[1], "B=")
	}
}

func TestSetEnv_ReplaceFirst(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/home/user"}
	result := setEnv(env, "PATH", "/usr/local/bin")
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	if result[0] != "PATH=/usr/local/bin" {
		t.Errorf("result[0] = %q, want %q", result[0], "PATH=/usr/local/bin")
	}
}

// --- copyFile ---

func TestCopyFile_Success(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	content := []byte("hello world")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("dst content = %q, want %q", got, content)
	}
}

func TestCopyFile_DestPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows files carry no execute permission")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "binary")
	dst := filepath.Join(dir, "binary-copy")

	if err := os.WriteFile(src, []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	// copyFile opens with 0o755 so the result should be executable
	if info.Mode()&0o111 == 0 {
		t.Errorf("copied file should be executable, got mode %v", info.Mode())
	}
}

func TestCopyFile_SourceNotFound(t *testing.T) {
	dir := t.TempDir()
	err := copyFile(filepath.Join(dir, "nonexistent"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Error("expected error copying nonexistent source, got nil")
	}
}

func TestCopyFile_DestDirectoryNotFound(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := copyFile(src, filepath.Join(dir, "nonexistent-dir", "dst.txt"))
	if err == nil {
		t.Error("expected error when destination directory does not exist, got nil")
	}
}

func TestCopyFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty")
	dst := filepath.Join(dir, "empty-copy")

	if err := os.WriteFile(src, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile empty file: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty file, got %d bytes", len(got))
	}
}

func TestCopyFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")

	if err := os.WriteFile(dst, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content" {
		t.Errorf("dst = %q, want %q", got, "new content")
	}
}
