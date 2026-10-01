//go:build windows

package installscript

import (
	"os/exec"
	"testing"
)

// withoutControllingTerminal has no Windows form: install.sh runs under bash,
// and requireBash skips its tests on Windows before any command is built.
// Reaching this is a test bug, so it fails instead of running with a terminal.
func withoutControllingTerminal(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	t.Fatalf("install.sh run-mode tests do not run on Windows: %s", cmd.Path)
}
