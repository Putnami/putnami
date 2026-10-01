package jobs

import (
	"bytes"
	"os/exec"
)

// ruffSingleRun executes a solo ruff invocation. Keeping the command boundary
// replaceable lets tests verify its scope without requiring uv or ruff.
var ruffSingleRun = runRuffSingle

// ruffCheckResult keeps Ruff's JSON output separate from uv's status output.
// Ruff writes machine-readable findings to stdout while uv may write messages
// to stderr, so combining them makes otherwise valid JSON unparsable.
type ruffCheckResult struct {
	Stdout []byte
	Stderr []byte
}

// ruffSingleCheckRun executes a solo Ruff check and preserves its output streams so
// callers can parse the JSON stdout independently from diagnostic stderr.
var ruffSingleCheckRun = runRuffSingleCheck

func runRuffSingle(name string, args []string, dir string, env []string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

func runRuffSingleCheck(name string, args []string, dir string, env []string) (ruffCheckResult, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return ruffCheckResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}
