package gitread

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Timeout bounds one git call: Run, RunCapture and RunCaptureEnv kill a call
// still running after it.
const Timeout = 30 * time.Second

// Run runs git with args in repoRoot and returns its standard output. A failed
// call is an error that names the git verb, repoRoot and what git wrote on
// standard error.
func Run(repoRoot string, args ...string) (string, error) {
	stdout, stderr, err := RunCapture(repoRoot, args...)
	if err != nil {
		return "", fmt.Errorf("git %s (in %s): %w: %s", args[0], repoRoot, err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

// RunCapture runs git with args in repoRoot and returns its standard output
// and its standard error.
func RunCapture(repoRoot string, args ...string) (string, string, error) {
	return RunCaptureEnv(repoRoot, nil, args...)
}

// RunCaptureEnv is RunCapture with env as git's environment; nil is this
// process's environment. RepositoryEnvironment filters it either way. A call
// still running after 30 seconds is killed and fails with "timed out after
// 30s".
func RunCaptureEnv(repoRoot string, env []string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, Executable(), args...)
	cmd.Dir = repoRoot
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = RepositoryEnvironment(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", stderr.String(), fmt.Errorf("timed out after %s", Timeout)
		}
		return "", stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}

// RepositoryEnvironment removes from environ the variables that could
// redirect a git command away from its working directory: every GIT_CONFIG*
// variable and the repository, worktree, index, object and discovery
// selectors. Transport credentials and tracing (including GIT_SSH_COMMAND and
// GIT_ASKPASS) pass through unchanged.
func RepositoryEnvironment(environ []string) []string {
	cleaned := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_CONFIG") {
			continue
		}
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX", "GIT_CEILING_DIRECTORIES",
			"GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_NAMESPACE":
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}
