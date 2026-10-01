//go:build windows

package proctree

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isProcessInJob is IsProcessInJob, which x/sys/windows does not declare.
var isProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func inJob(t *testing.T, process, job windows.Handle) bool {
	t.Helper()
	var result int32
	if ok, _, err := isProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&result))); ok == 0 {
		t.Fatalf("IsProcessInJob: %v", err)
	}
	return result != 0
}

// resume runs a process created suspended: its only thread is the one
// CreateProcess left suspended, and NtResumeProcess reaches it through the
// process handle.
func TestWindowsResumeRunsAProcessCreatedSuspended(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(), helperLevelEnv+"=1", helperExitCodeEnv+"=7")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	select {
	case <-exited:
		t.Fatalf("the process ran before it was resumed: %v", waitErr)
	case <-time.After(300 * time.Millisecond):
	}

	process, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if err := resume(process); err != nil {
		t.Fatalf("resume: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(exitTimeout):
		t.Fatalf("the resumed process did not exit within %v", exitTimeout)
	}
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("the resumed process exited with %v, want exit code 7", waitErr)
	}
}

// Start resumes the root only once it is in its job, so every level of the
// tree, the root included, is a member.
func TestWindowsStartPutsEveryLevelInTheJob(t *testing.T) {
	started := startHelperTree(t)
	pids := waitForLevels(t, started.dir)

	started.tree.mu.Lock()
	job := started.tree.sys.job
	started.tree.mu.Unlock()
	for level, pid := range pids {
		process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			t.Fatalf("open level %d: %v", level+1, err)
		}
		member := inJob(t, process, job)
		_ = windows.CloseHandle(process)
		if !member {
			t.Errorf("level %d (pid %d) is not in the tree's job", level+1, pid)
		}
	}
}
