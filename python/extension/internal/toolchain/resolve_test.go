package toolchain

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestUVDirEnv(t *testing.T) {
	state := filepath.Join("/ws", ".putnami", "cache", "extensions", "@putnami-python", "uv")
	wantPython := filepath.Join(state, "python")
	wantCache := filepath.Join(state, "cache")

	t.Run("points both directories at the workspace", func(t *testing.T) {
		got := UVDirEnv("/ws", []string{"HOME=/root", "PATH=/usr/bin"})
		if len(got) != 2 || got[UVPythonInstallDirEnv] != wantPython || got[UVCacheDirEnv] != wantCache {
			t.Fatalf("UVDirEnv = %v, want %s=%s and %s=%s", got, UVPythonInstallDirEnv, wantPython, UVCacheDirEnv, wantCache)
		}
	})

	t.Run("keeps an explicit uv setting", func(t *testing.T) {
		got := UVDirEnv("/ws", []string{"UV_CACHE_DIR=/shared/uv-cache"})
		if _, ok := got[UVCacheDirEnv]; ok {
			t.Errorf("UVDirEnv overrode an explicit %s: %v", UVCacheDirEnv, got)
		}
		if got[UVPythonInstallDirEnv] != wantPython {
			t.Errorf("%s = %q, want %q", UVPythonInstallDirEnv, got[UVPythonInstallDirEnv], wantPython)
		}
	})

	t.Run("a blank explicit setting counts as unset", func(t *testing.T) {
		got := UVDirEnv("/ws", []string{"UV_PYTHON_INSTALL_DIR=", "UV_CACHE_DIR=  "})
		if got[UVPythonInstallDirEnv] != wantPython || got[UVCacheDirEnv] != wantCache {
			t.Errorf("UVDirEnv = %v, want both workspace directories", got)
		}
	})

	t.Run("the last entry wins", func(t *testing.T) {
		got := UVDirEnv("/ws", []string{"UV_CACHE_DIR=/shared/uv-cache", "UV_CACHE_DIR="})
		if got[UVCacheDirEnv] != wantCache {
			t.Errorf("%s = %q, want %q: a later blank entry unsets the earlier one", UVCacheDirEnv, got[UVCacheDirEnv], wantCache)
		}
	})

	t.Run("a Windows environment matches names without regard to case", func(t *testing.T) {
		base := []string{"uv_cache_dir=/shared/uv-cache"}
		if !envSetFold(base, UVCacheDirEnv, true) {
			t.Errorf("folded lookup missed %q", base[0])
		}
		if envSetFold(base, UVCacheDirEnv, false) {
			t.Errorf("exact lookup matched %q", base[0])
		}
		if envSetFold([]string{"UV_CACHE_DIR_X=/x"}, UVCacheDirEnv, true) {
			t.Error("a longer name matched")
		}
	})

	t.Run("no workspace root sets nothing", func(t *testing.T) {
		if got := UVDirEnv("  ", nil); len(got) != 0 {
			t.Errorf("UVDirEnv without a workspace root = %v, want none", got)
		}
	})
}

func TestUVRunArgs(t *testing.T) {
	tests := []struct {
		name          string
		packageName   string
		workspaceRoot string
		extra         []string
		want          []string
	}{
		{
			name:          "basic args without extra",
			packageName:   "mypackage",
			workspaceRoot: "/workspace",
			extra:         nil,
			want:          []string{"uv", "run", "--package", "mypackage", "--directory", "/workspace"},
		},
		{
			name:          "with single extra arg",
			packageName:   "mypackage",
			workspaceRoot: "/workspace",
			extra:         []string{"pytest"},
			want:          []string{"uv", "run", "--package", "mypackage", "--directory", "/workspace", "pytest"},
		},
		{
			name:          "with multiple extra args",
			packageName:   "mypackage",
			workspaceRoot: "/home/user/project",
			extra:         []string{"pytest", "-v", "--tb=short"},
			want:          []string{"uv", "run", "--package", "mypackage", "--directory", "/home/user/project", "pytest", "-v", "--tb=short"},
		},
		{
			name:          "package name with dashes",
			packageName:   "my-cool-package",
			workspaceRoot: "/ws",
			extra:         []string{},
			want:          []string{"uv", "run", "--package", "my-cool-package", "--directory", "/ws"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UVRunArgs(tt.packageName, tt.workspaceRoot, tt.extra...)
			if len(got) != len(tt.want) {
				t.Fatalf("UVRunArgs() len=%d, want len=%d\ngot:  %v\nwant: %v", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("UVRunArgs()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestUVRunWithToolArgs(t *testing.T) {
	tests := []struct {
		name          string
		tool          string
		packageName   string
		workspaceRoot string
		toolArgs      []string
		want          []string
	}{
		{
			name:          "ruff format check",
			tool:          "ruff",
			packageName:   "mypackage",
			workspaceRoot: "/workspace",
			toolArgs:      []string{"format", "--check"},
			want:          []string{"uv", "run", "--with", "ruff", "--package", "mypackage", "--directory", "/workspace", "ruff", "format", "--check"},
		},
		{
			name:          "ruff check with fix",
			tool:          "ruff",
			packageName:   "mypkg",
			workspaceRoot: "/ws",
			toolArgs:      []string{"check", "--fix"},
			want:          []string{"uv", "run", "--with", "ruff", "--package", "mypkg", "--directory", "/ws", "ruff", "check", "--fix"},
		},
		{
			name:          "tool without extra args",
			tool:          "black",
			packageName:   "pkg",
			workspaceRoot: "/root",
			toolArgs:      nil,
			want:          []string{"uv", "run", "--with", "black", "--package", "pkg", "--directory", "/root", "black"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UVRunWithToolArgs(tt.tool, tt.packageName, tt.workspaceRoot, tt.toolArgs...)
			if len(got) != len(tt.want) {
				t.Fatalf("UVRunWithToolArgs() len=%d, want len=%d\ngot:  %v\nwant: %v", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("UVRunWithToolArgs()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTailText(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		stdout string
		lines  int
		want   string
	}{
		{
			name:   "both empty returns unknown error",
			stderr: "",
			stdout: "",
			lines:  5,
			want:   "Unknown error",
		},
		{
			name:   "stderr preferred over stdout",
			stderr: "error from stderr",
			stdout: "output from stdout",
			lines:  5,
			want:   "error from stderr",
		},
		{
			name:   "falls back to stdout when stderr empty",
			stderr: "",
			stdout: "fallback output",
			lines:  5,
			want:   "fallback output",
		},
		{
			name:   "returns full text when fewer lines than limit",
			stderr: "line1\nline2\nline3",
			stdout: "",
			lines:  5,
			want:   "line1\nline2\nline3",
		},
		{
			name:   "returns last N lines when output exceeds limit",
			stderr: "line1\nline2\nline3\nline4\nline5",
			stdout: "",
			lines:  3,
			want:   "line3\nline4\nline5",
		},
		{
			name:   "returns last 1 line",
			stderr: "line1\nline2\nline3",
			stdout: "",
			lines:  1,
			want:   "line3",
		},
		{
			name:   "trims leading/trailing whitespace from stderr",
			stderr: "  \nerror line\n  ",
			stdout: "",
			lines:  5,
			want:   "error line",
		},
		{
			name:   "whitespace-only stderr falls back to stdout",
			stderr: "   ",
			stdout: "actual output",
			lines:  5,
			want:   "actual output",
		},
		{
			name:   "exact number of lines equals limit returns full text",
			stderr: "line1\nline2\nline3",
			stdout: "",
			lines:  3,
			want:   "line1\nline2\nline3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TailText(tt.stderr, tt.stdout, tt.lines)
			if got != tt.want {
				t.Errorf("TailText(%q, %q, %d) = %q, want %q", tt.stderr, tt.stdout, tt.lines, got, tt.want)
			}
		})
	}
}

func TestResolveUV_NotInPath(t *testing.T) {
	// Temporarily set PATH to an empty temp dir so uv won't be found
	tmpDir := t.TempDir()
	originalPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", originalPath) })
	os.Setenv("PATH", tmpDir)

	_, err := ResolveUV()
	if err == nil {
		t.Error("ResolveUV() expected error when uv not in PATH, got nil")
	}
}

func TestResolveUV_InPath(t *testing.T) {
	// Create a fake uv binary in a temp dir, under the name PATH lookup finds
	// on this host: Windows looks only for the extensions PATHEXT lists.
	tmpDir := t.TempDir()
	name := "uv"
	if runtime.GOOS == "windows" {
		name = "uv.exe"
	}
	uvPath := filepath.Join(tmpDir, name)
	if err := os.WriteFile(uvPath, []byte("#!/bin/sh\necho uv"), 0755); err != nil {
		t.Fatal(err)
	}

	originalPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", originalPath) })
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+originalPath)

	got, err := ResolveUV()
	if err != nil {
		t.Fatalf("ResolveUV() unexpected error: %v", err)
	}
	if got != uvPath {
		t.Errorf("ResolveUV() = %q, want the uv first on PATH %q", got, uvPath)
	}
}
