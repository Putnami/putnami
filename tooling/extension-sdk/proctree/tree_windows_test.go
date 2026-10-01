//go:build windows

package proctree

import (
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// joinJobWithoutBreakaway puts the calling process in a new job, nested in the
// job it runs in, that does not allow breakaway. The handle stays open for the
// life of the process.
func joinJobWithoutBreakaway() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	return windows.AssignProcessToJobObject(job, windows.CurrentProcess())
}

// killProcesses ends pids, which a test left outside every tree it closes.
func killProcesses(pids []int) {
	for _, pid := range pids {
		process, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			continue
		}
		_ = windows.TerminateProcess(process, killedExitCode)
		_ = windows.CloseHandle(process)
	}
}

// waitProcessesGone waits until none of pids runs.
func waitProcessesGone(t *testing.T, pids []int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for _, pid := range pids {
		for processRunning(uint32(pid)) {
			if !time.Now().Before(deadline) {
				t.Fatalf("process %d of the tree is still running", pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestWindowsKillEndsAThreeLevelTree(t *testing.T) {
	started := startHelperTree(t)
	pids := waitForLevels(t, started.dir)
	if started.tree.ID() != pids[0] {
		t.Fatalf("ID() = %d, want the root's pid %d", started.tree.ID(), pids[0])
	}
	if !GroupAlive(started.tree.ID()) {
		t.Fatalf("GroupAlive(%d) = false for a running tree", started.tree.ID())
	}

	if err := started.tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	started.waitForExit(t)
	waitProcessesGone(t, pids)
	if GroupAlive(started.tree.ID()) {
		t.Fatalf("GroupAlive(%d) = true after Kill", started.tree.ID())
	}
}

// Terminate either delivers CTRL_BREAK_EVENT, which every level handles by
// exiting, or ends the job when the test has no console: both end the tree.
// This test proves only that the tree ends;
// TestWindowsTerminateDeliversCtrlBreakToEveryLevel proves the delivery.
func TestWindowsTerminateEndsAThreeLevelTree(t *testing.T) {
	started := startHelperTree(t)
	pids := waitForLevels(t, started.dir)

	if err := started.tree.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	started.waitForExit(t)
	waitProcessesGone(t, pids)
}

// With a console the tree shares, Terminate asks every level to exit through
// CTRL_BREAK_EVENT: each level records the os.Interrupt it received before it
// exits, which a job that was ended instead would never let it do.
func TestWindowsTerminateDeliversCtrlBreakToEveryLevel(t *testing.T) {
	ensureConsole(t)
	started := startHelperTree(t)
	pids := waitForLevels(t, started.dir)

	if err := started.tree.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	started.waitForExit(t)
	waitProcessesGone(t, pids)
	for level := 1; level <= helperLevels; level++ {
		if _, err := os.Stat(terminatedFile(started.dir, level)); err != nil {
			t.Errorf("level %d exited without receiving CTRL_BREAK_EVENT: %v", level, err)
		}
	}
}

// ensureConsole attaches the test process to a console, which the trees it
// then starts share: a console control event reaches only the processes of
// the caller's console. A console the test allocated is freed when it ends.
func ensureConsole(t *testing.T) {
	t.Helper()
	if _, err := windows.GetConsoleCP(); err == nil {
		return
	}
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	if ok, _, err := kernel32.NewProc("AllocConsole").Call(); ok == 0 {
		t.Fatalf("allocate a console for the test: %v", err)
	}
	t.Cleanup(func() { _, _, _ = kernel32.NewProc("FreeConsole").Call() })
}

func TestWindowsCloseEndsAThreeLevelTree(t *testing.T) {
	started := startHelperTree(t)
	pids := waitForLevels(t, started.dir)

	if err := started.tree.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	started.waitForExit(t)
	waitProcessesGone(t, pids)
}

// The child already has the console event that asked the caller to stop, so
// Relay sends it nothing more.
func TestWindowsRelaySendsNothing(t *testing.T) {
	started := startHelperTree(t)
	waitForLevels(t, started.dir)

	if err := Relay(started.tree.cmd.Process); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	select {
	case <-started.exited:
		t.Fatal("the child exited after Relay")
	case <-time.After(500 * time.Millisecond):
	}
	if _, err := os.Stat(terminatedFile(started.dir, 1)); err == nil {
		t.Fatal("the child received a stop request from Relay")
	}
}

// A member leaves the job only when it asks to: silent breakaway would let
// every process a member starts escape the tree.
func TestWindowsJobLimitsAllowOnlyExplicitBreakaway(t *testing.T) {
	started := startHelperTree(t)
	waitForLevels(t, started.dir)

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	started.tree.mu.Lock()
	err := windows.QueryInformationJobObject(started.tree.sys.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	started.tree.mu.Unlock()
	if err != nil {
		t.Fatalf("query the job limits: %v", err)
	}
	want := uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK)
	if got := info.BasicLimitInformation.LimitFlags; got != want {
		t.Fatalf("job limit flags = %#x, want %#x", got, want)
	}
}

func TestWindowsStartDetachedLeavesTheJob(t *testing.T) {
	started := startHelperTree(t, helperDetachEnv+"=1")
	pids := waitForLevels(t, started.dir)
	t.Cleanup(func() { killProcesses(pids[1:]) })
	if _, err := os.Stat(startedInsideFile(started.dir)); err == nil {
		t.Fatal("StartDetached started level 2 inside the tree's job, which allows breakaway")
	}

	if err := started.tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	started.waitForExit(t)
	if err := started.tree.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	for level, pid := range pids[1:] {
		if !processRunning(uint32(pid)) {
			t.Fatalf("level %d ended with the tree it was detached from", level+2)
		}
	}
}

func TestWindowsStartDetachedStartsInsideAJobThatForbidsBreakaway(t *testing.T) {
	started := startHelperTree(t, helperDetachEnv+"=1", helperNoBreakawayEnv+"=1")
	pids := waitForLevels(t, started.dir)
	t.Cleanup(func() { killProcesses(pids[1:]) })
	if _, err := os.Stat(startedInsideFile(started.dir)); err != nil {
		t.Fatalf("StartDetached did not fall back to a start inside the job: %v", err)
	}

	if err := started.tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	started.waitForExit(t)
	waitProcessesGone(t, pids)
}

func TestWindowsKillGroupEndsTheTreeAfterItsRootExited(t *testing.T) {
	started := startHelperTree(t, helperRootExitsEnv+"=1")
	pids := waitForLevels(t, started.dir)
	started.waitForExit(t)

	id := started.tree.ID()
	if !GroupAlive(id) {
		t.Fatalf("GroupAlive(%d) = false while the root's descendants run", id)
	}
	if err := KillGroup(id); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}
	waitProcessesGone(t, pids[1:])
	if GroupAlive(id) {
		t.Fatalf("GroupAlive(%d) = true after KillGroup", id)
	}
}
