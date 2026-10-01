//go:build unix

package serve

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestClassifyServeExit_RealUnixSignalExitIsSuccess(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	cmd := exec.Command("sh", "-c", "sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("signal: %v", err)
	}

	ok, err := classifyServeExit(cmd.Wait(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected success for a process terminated by SIGTERM")
	}
}

// TestServeCommand_KeepsBunInTheJobProcessGroup pins that bun joins the
// group its launcher records and signals, instead of leading a group of its
// own that nothing records.
func TestServeCommand_KeepsBunInTheJobProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	cmd := serveCommand("sleep", []string{"30"}, t.TempDir(), os.Environ())
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if pgid != syscall.Getpgrp() {
		t.Errorf("bun runs in process group %d, want the extension's %d", pgid, syscall.Getpgrp())
	}
}
