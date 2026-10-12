package toolchain

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// testUVProgram is the uv program the command builders are given. A command
// that starts with a bare "uv" would run whichever uv PATH holds first, not
// the release the workspace pins.
const testUVProgram = "/home/user/.putnami/toolchains/uv/uv-0.11.32/uv"

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
			want:          []string{testUVProgram, "run", "--package", "mypackage", "--directory", "/workspace"},
		},
		{
			name:          "with single extra arg",
			packageName:   "mypackage",
			workspaceRoot: "/workspace",
			extra:         []string{"pytest"},
			want:          []string{testUVProgram, "run", "--package", "mypackage", "--directory", "/workspace", "pytest"},
		},
		{
			name:          "with multiple extra args",
			packageName:   "mypackage",
			workspaceRoot: "/home/user/project",
			extra:         []string{"pytest", "-v", "--tb=short"},
			want:          []string{testUVProgram, "run", "--package", "mypackage", "--directory", "/home/user/project", "pytest", "-v", "--tb=short"},
		},
		{
			name:          "package name with dashes",
			packageName:   "my-cool-package",
			workspaceRoot: "/ws",
			extra:         []string{},
			want:          []string{testUVProgram, "run", "--package", "my-cool-package", "--directory", "/ws"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UVRunArgs(testUVProgram, tt.packageName, tt.workspaceRoot, tt.extra...)
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
			want:          []string{testUVProgram, "run", "--with", "ruff", "--package", "mypackage", "--directory", "/workspace", "ruff", "format", "--check"},
		},
		{
			name:          "ruff check with fix",
			tool:          "ruff",
			packageName:   "mypkg",
			workspaceRoot: "/ws",
			toolArgs:      []string{"check", "--fix"},
			want:          []string{testUVProgram, "run", "--with", "ruff", "--package", "mypkg", "--directory", "/ws", "ruff", "check", "--fix"},
		},
		{
			name:          "tool without extra args",
			tool:          "black",
			packageName:   "pkg",
			workspaceRoot: "/root",
			toolArgs:      nil,
			want:          []string{testUVProgram, "run", "--with", "black", "--package", "pkg", "--directory", "/root", "black"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UVRunWithToolArgs(testUVProgram, tt.tool, tt.packageName, tt.workspaceRoot, tt.toolArgs...)
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

// writeUVScript puts on a new PATH a uv that answers the version probe with
// release, and fails on any other command line.
func writeUVScript(t *testing.T, release string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script is not a program on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "uv")
	script := "#!/bin/sh\n" +
		"if [ \"$*\" = \"self version --short --color never\" ]; then echo " + release + "; exit 0; fi\n" +
		"echo \"unexpected: $*\" >&2\nexit 2\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	return path
}

// A uv on PATH is not the workspace's uv because PATH holds it: only a uv that
// reports the wanted release is.
func TestResolveUV_RefusesAUVOfAnotherReleaseOnPath(t *testing.T) {
	path := writeUVScript(t, "0.5.18")
	got, err := ResolveUV(t.TempDir())
	if err == nil {
		t.Fatalf("ResolveUV() = %q, want an error for the uv 0.5.18 on PATH", got)
	}
	if !strings.Contains(err.Error(), path+" (uv 0.5.18)") || !strings.Contains(err.Error(), "`putnami install`") {
		t.Errorf("the error does not name the uv it passed over and `putnami install`: %v", err)
	}
}

func TestResolveUV_FindsTheWantedReleaseOnPath(t *testing.T) {
	path := writeUVScript(t, DefaultUVVersion)
	got, err := ResolveUV(t.TempDir())
	if err != nil {
		t.Fatalf("ResolveUV() unexpected error: %v", err)
	}
	if got != path {
		t.Errorf("ResolveUV() = %q, want the uv on PATH %q", got, path)
	}
}

func TestResolveUV_NoUV(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PUTNAMI_HOME", t.TempDir())
	if got, err := ResolveUV(t.TempDir()); err == nil {
		t.Fatalf("ResolveUV() = %q, want an error when no uv exists", got)
	}
}
