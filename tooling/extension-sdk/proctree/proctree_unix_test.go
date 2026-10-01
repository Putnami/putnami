//go:build unix

package proctree

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const guardHelperEnv = "PROCTREE_GUARD_HELPER"

// waitGroupGone waits until no process of the group id runs. Members whose
// parent exited are reaped by init, so a killed group empties promptly.
func waitGroupGone(t *testing.T, id int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for GroupAlive(id) {
		if !time.Now().Before(deadline) {
			t.Fatalf("process group %d is still running", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartMakesTheRootTheLeaderOfItsOwnGroup(t *testing.T) {
	started := startHelperTree(t)
	pids := waitForLevels(t, started.dir)

	if started.tree.ID() != pids[0] {
		t.Fatalf("ID() = %d, want the root's pid %d", started.tree.ID(), pids[0])
	}
	if started.tree.ID() == syscall.Getpgrp() {
		t.Fatalf("the tree shares the test's process group %d", syscall.Getpgrp())
	}
	for level, pid := range pids {
		pgid, err := syscall.Getpgid(pid)
		if err != nil {
			t.Fatalf("getpgid of level %d: %v", level+1, err)
		}
		if pgid != started.tree.ID() {
			t.Fatalf("level %d is in group %d, want %d", level+1, pgid, started.tree.ID())
		}
	}
	if !GroupAlive(started.tree.ID()) {
		t.Fatalf("GroupAlive(%d) = false for a running tree", started.tree.ID())
	}
}

func TestTerminateSendsSIGTERMToEveryLevel(t *testing.T) {
	started := startHelperTree(t)
	waitForLevels(t, started.dir)

	if err := started.tree.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	for level := 1; level <= helperLevels; level++ {
		if !waitForFile(terminatedFile(started.dir, level), 30*time.Second) {
			t.Fatalf("level %d did not receive SIGTERM", level)
		}
	}
	started.waitForExit(t)
	if started.err != nil {
		t.Fatalf("the root exited with %v, want a clean exit after SIGTERM", started.err)
	}
	waitGroupGone(t, started.tree.ID())
}

func TestKillEndsTheGroupAfterItsRootExited(t *testing.T) {
	started := startHelperTree(t, helperRootExitsEnv+"=1")
	waitForLevels(t, started.dir)
	started.waitForExit(t)

	id := started.tree.ID()
	if !GroupAlive(id) {
		t.Fatalf("GroupAlive(%d) = false while the root's descendants run", id)
	}
	if err := started.tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitGroupGone(t, id)
	for level := 2; level <= helperLevels; level++ {
		if _, err := os.Stat(terminatedFile(started.dir, level)); err == nil {
			t.Fatalf("level %d handled a signal, want SIGKILL", level)
		}
	}
	// A group that is gone is not an error.
	if err := started.tree.Kill(); err != nil {
		t.Fatalf("Kill of a group that is gone: %v", err)
	}
	if err := started.tree.Terminate(); err != nil {
		t.Fatalf("Terminate of a group that is gone: %v", err)
	}
}

func TestKillGroupAndTerminateGroupReachATreeByItsID(t *testing.T) {
	started := startHelperTree(t)
	waitForLevels(t, started.dir)
	id := started.tree.ID()

	if err := TerminateGroup(id); err != nil {
		t.Fatalf("TerminateGroup: %v", err)
	}
	for level := 1; level <= helperLevels; level++ {
		if !waitForFile(terminatedFile(started.dir, level), 30*time.Second) {
			t.Fatalf("level %d did not receive SIGTERM", level)
		}
	}
	started.waitForExit(t)
	waitGroupGone(t, id)

	killed := startHelperTree(t, helperRootExitsEnv+"=1")
	waitForLevels(t, killed.dir)
	killed.waitForExit(t)
	if err := KillGroup(killed.tree.ID()); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}
	waitGroupGone(t, killed.tree.ID())
	if err := KillGroup(killed.tree.ID()); err != nil {
		t.Fatalf("KillGroup of a group that is gone: %v", err)
	}
	if err := TerminateGroup(killed.tree.ID()); err != nil {
		t.Fatalf("TerminateGroup of a group that is gone: %v", err)
	}
}

func TestCloseLeavesTheMembersRunningAndDisablesTheStops(t *testing.T) {
	started := startHelperTree(t)
	pids := waitForLevels(t, started.dir)
	id := started.tree.ID()

	if err := started.tree.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := started.tree.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := started.tree.Terminate(); err != nil {
		t.Fatalf("Terminate after Close: %v", err)
	}
	if err := started.tree.Kill(); err != nil {
		t.Fatalf("Kill after Close: %v", err)
	}
	if started.tree.ID() != id {
		t.Fatalf("ID() after Close = %d, want %d", started.tree.ID(), id)
	}
	time.Sleep(100 * time.Millisecond)
	for level, pid := range pids {
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("level %d stopped after Close: %v", level+1, err)
		}
		if _, err := os.Stat(terminatedFile(started.dir, level+1)); err == nil {
			t.Fatalf("level %d received a stop after Close", level+1)
		}
	}
}

func TestCancelThroughTerminateStopsTheTree(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessTreeHelper$")
	cmd.Env = append(os.Environ(), helperLevelEnv+"=1", helperDirEnv+"="+dir)
	tree := New(cmd)
	cmd.Cancel = tree.Terminate
	cmd.WaitDelay = 30 * time.Second
	if err := tree.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := tree.ID()
	t.Cleanup(func() { _ = KillGroup(id) })
	waitForLevels(t, dir)

	cancel()
	if err := cmd.Wait(); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after cancel = %v, want context.Canceled", err)
	}
	for level := 1; level <= helperLevels; level++ {
		if !waitForFile(terminatedFile(dir, level), 30*time.Second) {
			t.Fatalf("level %d did not receive SIGTERM", level)
		}
	}
	waitGroupGone(t, id)
	_ = tree.Close()
}

func TestRunClosesTheTreeAndReturnsTheExitError(t *testing.T) {
	tree := New(helperCommand(t.TempDir(), helperExitCodeEnv+"=7"))
	err := tree.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("Run = %v, want exit status 7", err)
	}
	tree.mu.Lock()
	closed := tree.closed
	tree.mu.Unlock()
	if !closed {
		t.Fatal("Run left the tree open")
	}
}

func TestStartReturnsTheStartErrorUnchanged(t *testing.T) {
	missing := "/nonexistent/proctree-missing-binary"
	want := exec.Command(missing).Start()

	tree := New(exec.Command(missing))
	err := tree.Start()
	if err == nil || !errors.Is(err, fs.ErrNotExist) || err.Error() != want.Error() {
		t.Fatalf("Start = %v, want %v", err, want)
	}
	if tree.ID() != 0 {
		t.Fatalf("ID() after a failed Start = %d, want 0", tree.ID())
	}
	for name, stop := range map[string]func() error{"Terminate": tree.Terminate, "Kill": tree.Kill, "Close": tree.Close} {
		if err := stop(); err != nil {
			t.Fatalf("%s after a failed Start: %v", name, err)
		}
	}
}

func TestATreeThatNeverStartedIsANoOp(t *testing.T) {
	var unset *Tree
	unstarted := New(exec.Command("true"))
	for name, tree := range map[string]*Tree{"nil": unset, "unstarted": unstarted} {
		if tree.ID() != 0 {
			t.Fatalf("%s tree: ID() = %d, want 0", name, tree.ID())
		}
		if err := tree.Terminate(); err != nil {
			t.Fatalf("%s tree: Terminate: %v", name, err)
		}
		if err := tree.Kill(); err != nil {
			t.Fatalf("%s tree: Kill: %v", name, err)
		}
		if err := tree.Close(); err != nil {
			t.Fatalf("%s tree: Close: %v", name, err)
		}
	}
}

func TestNewKeepsTheOtherFieldsOfSysProcAttr(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Noctty: true}
	New(cmd)
	if !cmd.SysProcAttr.Noctty || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %+v, want Noctty and Setpgid", *cmd.SysProcAttr)
	}

	bare := exec.Command("true")
	New(bare)
	if bare.SysProcAttr == nil || !bare.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %+v, want Setpgid", bare.SysProcAttr)
	}
}

func TestStartAppliesTheTreeSettingsAfterSysProcAttrIsReplaced(t *testing.T) {
	dir := t.TempDir()
	cmd := helperCommand(dir)
	tree := New(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	if err := tree.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = tree.Kill()
		_ = cmd.Wait()
		_ = tree.Close()
	})
	pids := waitForLevels(t, dir)
	pgid, err := syscall.Getpgid(pids[0])
	if err != nil || pgid != pids[0] {
		t.Fatalf("the root is in group %d (%v), want its own group %d", pgid, err, pids[0])
	}
}

// On Unix StartDetached is cmd.Start: the detached level stays in the root's
// group, and killing the tree ends it.
func TestStartDetachedStartsInTheCallersGroup(t *testing.T) {
	started := startHelperTree(t, helperDetachEnv+"=1")
	pids := waitForLevels(t, started.dir)
	for level, pid := range pids {
		pgid, err := syscall.Getpgid(pid)
		if err != nil || pgid != started.tree.ID() {
			t.Fatalf("level %d is in group %d (%v), want the tree's group %d", level+1, pgid, err, started.tree.ID())
		}
	}
	if _, err := os.Stat(startedInsideFile(started.dir)); err == nil {
		t.Fatal("StartDetached returned a copy of the command; want the command itself")
	}
	if err := started.tree.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitGroupGone(t, started.tree.ID())
}

// joinJobWithoutBreakaway is Windows-only; no Unix test sets
// helperNoBreakawayEnv.
func joinJobWithoutBreakaway() error { return errors.ErrUnsupported }

func TestRelaySendsTheChildSIGTERM(t *testing.T) {
	started := startHelperTree(t)
	waitForLevels(t, started.dir)

	if err := Relay(started.tree.cmd.Process); err != nil {
		t.Fatalf("Relay: %v", err)
	}
	if !waitForFile(terminatedFile(started.dir, 1), 30*time.Second) {
		t.Fatal("the child did not receive SIGTERM")
	}
	started.waitForExit(t)
}

func TestGroupAliveIsFalseForNonPositiveIDs(t *testing.T) {
	for _, id := range []int{0, -1, -os.Getpid()} {
		if GroupAlive(id) {
			t.Fatalf("GroupAlive(%d) = true, want false", id)
		}
	}
}

// TestNonPositiveIDsAreNeverSignaled runs the calls in a tree of their own:
// kill(2) reads 0 as the caller's group and -pid as the single process pid, so
// a broken guard ends that tree, and never the test binary or its caller.
func TestNonPositiveIDsAreNeverSignaled(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestNonPositiveIDGuardHelper$")
	cmd.Env = append(os.Environ(), guardHelperEnv+"=1")
	var output strings.Builder
	cmd.Stdout = &output
	tree := New(cmd)
	err := tree.Run()
	if err != nil || !strings.Contains(output.String(), "guard held") {
		t.Fatalf("the guard helper = %v, output %q; want it to survive every call", err, output.String())
	}
}

// TestNonPositiveIDGuardHelper is not a test: it sends every stop to
// non-positive ids and reports that it survived, when a test started it.
func TestNonPositiveIDGuardHelper(t *testing.T) {
	if os.Getenv(guardHelperEnv) != "1" {
		return
	}
	self := -os.Getpid()
	for _, call := range []func() error{
		func() error { return TerminateGroup(0) },
		func() error { return KillGroup(0) },
		func() error { return TerminateGroup(self) },
		func() error { return KillGroup(self) },
	} {
		if err := call(); err != nil {
			os.Exit(2)
		}
	}
	os.Stdout.WriteString("guard held " + strconv.Itoa(os.Getpid()) + "\n")
	os.Exit(0)
}
