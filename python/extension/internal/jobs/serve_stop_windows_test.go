//go:build windows

package jobs

import (
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/proctree"
)

// A server that Serve starts on Windows roots a process tree: the stop it
// relays on Ctrl-C sends nothing to a single process there, yet uv and the
// python it started both end, within the grace and the kill after it.
func TestWindowsStopProcess_EndsTheServerAndItsChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	proc := startStopHelper(t, stopHelperChildFileEnv+"="+pidFile)
	if proc.tree == nil {
		t.Fatal("the server roots no process tree on Windows")
	}
	child := waitForChild(t, pidFile)
	stopWithin(t, proc, 2*time.Second, 30*time.Second, proctree.Relay)
	waitForGone(t, child)
}
