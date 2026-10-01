// Package toolchain provides UV binary resolution and command building
// for the Putnami Python extension.
package toolchain

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/envkeys"
)

// ResolveUV finds the uv binary in PATH.
func ResolveUV() (string, error) {
	path, err := exec.LookPath("uv")
	if err != nil {
		return "", fmt.Errorf("'uv' is required but was not found in PATH")
	}
	return path, nil
}

// The variables uv reads for the two directories it writes outside the
// project: managed Python installations and the package cache.
const (
	UVPythonInstallDirEnv = "UV_PYTHON_INSTALL_DIR"
	UVCacheDirEnv         = "UV_CACHE_DIR"
)

// UVStateDir returns the workspace directory that holds uv's managed Pythons
// (python/) and cache (cache/). It is the per-worktree root the extension
// cache contract (go.putnami.dev/protocol/extension MachineCacheRoot) gives
// @putnami/python when no home directory is usable. Project discovery,
// snapshots, and the uv workspace sync all skip .putnami.
func UVStateDir(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".putnami", "cache", "extensions", "@putnami-python", "uv")
}

// UVDirEnv returns the uv directory variables to add to a subprocess
// environment built on base.
//
// By default uv installs managed Pythons under ~/.local/share/uv/python and
// caches under ~/.cache/uv. When HOME is read-only for the task user, as on
// the Putnami CI runner, every uv command that has to download a Python
// fails. Pointing both directories at the workspace keeps uv's writes inside
// the checkout. Every job and project of the worktree shares them, so uv
// downloads a Python once per worktree, not once per run.
//
// A variable that base already sets is kept, because an explicit uv setting
// wins over this default. An empty workspaceRoot returns nil, so no job
// points uv at a relative path.
func UVDirEnv(workspaceRoot string, base []string) map[string]string {
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil
	}
	root := UVStateDir(workspaceRoot)
	out := map[string]string{}
	if !envSet(base, UVPythonInstallDirEnv) {
		out[UVPythonInstallDirEnv] = filepath.Join(root, "python")
	}
	if !envSet(base, UVCacheDirEnv) {
		out[UVCacheDirEnv] = filepath.Join(root, "cache")
	}
	return out
}

// envSet reports whether base gives name a non-blank value. The last entry
// wins, as it does for os/exec, and names match the way this platform's process
// environment does (envkeys.Host).
func envSet(base []string, name string) bool {
	return envSetFold(base, name, envkeys.Host.Fold)
}

// envSetFold is envSet with an explicit name comparison: exact, or without
// regard to case when fold is set.
func envSetFold(base []string, name string, fold bool) bool {
	return strings.TrimSpace(envkeys.Keys{Fold: fold}.Last(base, name)) != ""
}

// UVRunArgs builds a uv run command with standard arguments.
// Returns: ["uv", "run", "--package", name, "--directory", workspaceRoot, extra...]
func UVRunArgs(packageName, workspaceRoot string, extra ...string) []string {
	args := make([]string, 0, 6+len(extra))
	args = append(args, "uv", "run", "--package", packageName, "--directory", workspaceRoot)
	args = append(args, extra...)
	return args
}

// UVRunWithToolArgs builds a uv run command that invokes a tool in directory.
// Returns: ["uv", "run", "--with", tool, "--package", name, "--directory", directory, tool, toolArgs...]
func UVRunWithToolArgs(tool, packageName, directory string, toolArgs ...string) []string {
	args := make([]string, 0, 9+len(toolArgs))
	args = append(args, "uv", "run", "--with", tool, "--package", packageName, "--directory", directory, tool)
	args = append(args, toolArgs...)
	return args
}

// TailText returns the last n lines of the output for error diagnostics.
// Falls back to "Unknown error" if output is empty.
func TailText(stderr, stdout string, lines int) string {
	merged := strings.TrimSpace(stderr)
	if merged == "" {
		merged = strings.TrimSpace(stdout)
	}
	if merged == "" {
		return "Unknown error"
	}
	parts := strings.Split(merged, "\n")
	if len(parts) <= lines {
		return merged
	}
	return strings.Join(parts[len(parts)-lines:], "\n")
}
