//go:build windows

package proctree

import (
	"testing"

	"golang.org/x/sys/windows"
)

// openLevels opens a SYNCHRONIZE handle on each level of a helper tree, so the
// test reads each level's exit even after its pid is reused.
func openLevels(t *testing.T, pids []int) []windows.Handle {
	t.Helper()
	handles := make([]windows.Handle, 0, len(pids))
	for level, pid := range pids {
		process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			t.Fatalf("open level %d (pid %d): %v", level+1, pid, err)
		}
		handles = append(handles, process)
	}
	t.Cleanup(func() {
		for _, process := range handles {
			_ = windows.CloseHandle(process)
		}
	})
	return handles
}

// assertLevelsExited fails for each level that has not exited, without waiting.
func assertLevelsExited(t *testing.T, handles []windows.Handle, pids []int) {
	t.Helper()
	for level, process := range handles {
		if event, err := windows.WaitForSingleObject(process, 0); err != nil || event != windows.WAIT_OBJECT_0 {
			t.Errorf("level %d (pid %d) still runs after Close returned", level+1, pids[level])
		}
	}
}

// Windows ends a job's processes asynchronously: TerminateJobObject, and
// closing a job with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, return before the
// processes exit. Close waits for them, so a caller that frees a port or
// removes a directory right after Close never meets one. The window was 0 to
// 2.35 ms on the QA VM, so the test repeats to make a regression visible.
func TestWindowsCloseReturnsOnceEveryProcessExited(t *testing.T) {
	for range 10 {
		started := startHelperTree(t)
		pids := waitForLevels(t, started.dir)
		handles := openLevels(t, pids)

		if err := started.tree.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		assertLevelsExited(t, handles, pids)
		started.waitForExit(t)
	}
}

// The cancel path of a job: Kill, the root's exit, then Close. The root's exit
// says nothing about the other levels; Close returns once they exited too.
func TestWindowsCloseAfterKillReturnsOnceEveryProcessExited(t *testing.T) {
	for range 10 {
		started := startHelperTree(t)
		pids := waitForLevels(t, started.dir)
		handles := openLevels(t, pids)

		if err := started.tree.Kill(); err != nil {
			t.Fatalf("Kill: %v", err)
		}
		started.waitForExit(t)
		if err := started.tree.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		assertLevelsExited(t, handles, pids)
	}
}

// The graceful stop of a job: Terminate delivers CTRL_BREAK_EVENT, every level
// exits on its own, the root's exit is seen, then Close. A level that is still
// exiting has already left the job's count, so Close waits on the handle the
// stop request took.
func TestWindowsCloseAfterTerminateReturnsOnceEveryProcessExited(t *testing.T) {
	ensureConsole(t)
	for range 10 {
		started := startHelperTree(t)
		pids := waitForLevels(t, started.dir)
		handles := openLevels(t, pids)

		if err := started.tree.Terminate(); err != nil {
			t.Fatalf("Terminate: %v", err)
		}
		started.waitForExit(t)
		if err := started.tree.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		assertLevelsExited(t, handles, pids)
	}
}

// Kill and Terminate watch every member before they stop it: once a member
// left the job, a handle taken earlier is the only way Close can wait for it.
func TestWindowsStopRequestsWatchEveryMember(t *testing.T) {
	for _, stop := range []struct {
		name string
		run  func(*Tree) error
	}{
		{"Kill", (*Tree).Kill},
		{"Terminate", (*Tree).Terminate},
	} {
		t.Run(stop.name, func(t *testing.T) {
			started := startHelperTree(t)
			pids := waitForLevels(t, started.dir)

			if err := stop.run(started.tree); err != nil {
				t.Fatalf("%s: %v", stop.name, err)
			}
			started.tree.mu.Lock()
			watched := make(map[uint32]bool, len(started.tree.sys.members))
			for pid := range started.tree.sys.members {
				watched[pid] = true
			}
			started.tree.mu.Unlock()
			for level, pid := range pids {
				if !watched[uint32(pid)] {
					t.Errorf("%s did not watch level %d (pid %d)", stop.name, level+1, pid)
				}
			}
		})
	}
}
