package completion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// ShellCompletionRefresh describes the result of refreshing an installed shell
// completion file.
type ShellCompletionRefresh struct {
	Shell         string
	Path          string
	ZshCacheHint  bool
	ZshDuplicates []string
	SkippedReason string
}

// RefreshShellCompletions regenerates the completion file for the user's
// current shell using binaryPath. It intentionally shells out to binaryPath
// rather than calling Completion* directly, so self-update writes completions
// from the newly installed CLI binary. The spawns run under ctx so a canceled
// upgrade aborts them.
func RefreshShellCompletions(ctx context.Context, binaryPath string) (ShellCompletionRefresh, error) {
	shell := shellNameFromEnv()
	result := ShellCompletionRefresh{Shell: shell}
	if shell == "" {
		result.SkippedReason = "SHELL is not set"
		return result, nil
	}
	if binaryPath == "" {
		return result, fmt.Errorf("binary path is required")
	}

	var target string
	switch shell {
	case "bash":
		target = bashCompletionPath()
	case "zsh":
		target = zshCompletionPath(ctx)
		result.ZshCacheHint = true
		result.ZshDuplicates = zshCompletionDuplicates(ctx)
	case "fish":
		target = fishCompletionPath()
	default:
		result.SkippedReason = fmt.Sprintf("unsupported shell %q", shell)
		return result, nil
	}
	if target == "" {
		result.SkippedReason = fmt.Sprintf("no completion path for %s", shell)
		return result, nil
	}
	result.Path = target

	if err := writeCompletionFromBinary(ctx, binaryPath, shell, target); err != nil {
		return result, err
	}
	return result, nil
}

func shellNameFromEnv() string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		return ""
	}
	return filepath.Base(shell)
}

// systemBashCompletionDir is the system-wide bash completion directory probed
// before the per-user fallback. It is a var, not a const, so tests can redirect
// the probe away from the real /etc and stay hermetic regardless of whether the
// suite runs as root (where /etc/bash_completion.d is writable). An empty value
// disables the system path entirely.
var systemBashCompletionDir = "/etc/bash_completion.d"

func bashCompletionPath() string {
	if systemBashCompletionDir != "" && isWritableDir(systemBashCompletionDir) {
		return filepath.Join(systemBashCompletionDir, "putnami")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "bash-completion", "completions", "putnami")
}

func zshCompletionPath(ctx context.Context) string {
	if zshDir := os.Getenv("ZSH"); zshDir != "" {
		if info, err := os.Stat(zshDir); err == nil && info.IsDir() {
			custom := os.Getenv("ZSH_CUSTOM")
			if custom == "" {
				custom = filepath.Join(zshDir, "custom")
			}
			return filepath.Join(custom, "completions", "_putnami")
		}
	}

	for _, path := range zshCompletionDuplicates(ctx) {
		if isWritableDir(filepath.Dir(path)) {
			return path
		}
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".zfunc", "_putnami")
}

func fishCompletionPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "fish", "completions", "putnami.fish")
}

// writeCompletionFromBinary writes the completion script binaryPath prints.
// The workspace's CLI may be a build of its source, so running it counts as
// repository code (runcredential.MarkRepositoryCodeStarted).
func writeCompletionFromBinary(ctx context.Context, binaryPath, shell, target string) error {
	runcredential.MarkRepositoryCodeStarted(filepath.Base(binaryPath) + " completion " + shell)
	cmd := exec.CommandContext(ctx, binaryPath, "completion", shell)
	output, err := cmd.Output()
	if err != nil {
		ee := &exec.ExitError{}
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return fmt.Errorf("generate %s completion: %w: %s", shell, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return fmt.Errorf("generate %s completion: %w", shell, err)
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return fmt.Errorf("generate %s completion: empty output", shell)
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create completion directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".putnami-completion-*")
	if err != nil {
		return fmt.Errorf("create temp completion file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(output); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp completion file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp completion file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("chmod temp completion file: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("install completion file: %w", err)
	}
	return nil
}

func zshCompletionDuplicates(ctx context.Context) []string {
	var paths []string
	if out, err := exec.CommandContext(ctx, "zsh", "-c", `for d in $fpath; do [[ -f "$d/_putnami" ]] && print -r -- "$d/_putnami"; done`).Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				paths = append(paths, line)
			}
		}
	}
	if zshDir := os.Getenv("ZSH"); zshDir != "" {
		custom := os.Getenv("ZSH_CUSTOM")
		if custom == "" {
			custom = filepath.Join(zshDir, "custom")
		}
		paths = append(paths, filepath.Join(custom, "completions", "_putnami"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".zfunc", "_putnami"))
	}

	seen := map[string]bool{}
	var existing []string
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			existing = append(existing, path)
		}
	}
	sort.Strings(existing)
	return existing
}

func isWritableDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	tmp, err := os.CreateTemp(path, ".putnami-write-test-*")
	if err != nil {
		return false
	}
	name := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(name)
	return true
}
