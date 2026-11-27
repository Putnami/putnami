package credentialcustody

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// engineDeadline bounds one engine run of a scenario. A run passes in seconds
// alone and in under twenty seconds beside four busy loops and other race
// test packages on four CPUs; the bound stays well under the package's test
// timeout, so a run that hangs fails its own test with the process tree it was
// waiting on instead of ending the whole package.
const engineDeadline = 3 * time.Minute

// runReserve is what a run's bound leaves of the test's own deadline: the
// dumps and kill of a hung tree, and the report of the failure.
const runReserve = engineDumpGrace + engineDumpPause + 20*time.Second

// runFloor is the shortest bound a run gets, so a run near the test's deadline
// still has time to start. A run that hangs at that bound also takes the dumps'
// grace before it fails, so it reports only when the reserve was not already
// spent.
const runFloor = 10 * time.Second

// runBound is the bound of one run that starts at now: ceiling, or less when
// the test's deadline would come first. A test that runs several scenarios
// then fails its own run with the tree it was waiting on, even after earlier
// runs took most of the package's timeout, instead of ending the whole
// package. hasDeadline is false when the test has no deadline (-timeout 0).
func runBound(deadline time.Time, hasDeadline bool, now time.Time, ceiling time.Duration) time.Duration {
	if !hasDeadline {
		return ceiling
	}
	return min(ceiling, max(deadline.Sub(now)-runReserve, runFloor))
}

// engineDumpGrace is how long a hung engine tree gets to write its goroutine
// dumps before it is killed.
const engineDumpGrace = 10 * time.Second

// treeProcess is one process of a hung engine's tree.
type treeProcess struct {
	pid, ppid          int
	state, wchan, role string
	args               string
}

// engineDumpPause is how long the engine's descendants get to write their
// dumps through it before the engine is asked for its own.
const engineDumpPause = time.Second

// waitEngine waits for cmd, started, within deadline. A run that
// overstays it is described, asked for its goroutine dumps, and killed with
// every process of its tree; the error then names the deadline and every
// process the run was waiting on, and output holds the dumps.
func waitEngine(cmd *exec.Cmd, deadline time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(deadline):
	}
	tree := engineTree(cmd.Process.Pid)
	// Each process is held as soon as the tree is read: on Linux with pidfd
	// support a handle holds a pidfd, so the kill below, up to
	// engineDumpGrace later, reaches the process held, never one that reused
	// its pid. A pid reused between the read of /proc and this hold, or a
	// kernel without pidfd, where the handle is a plain pid, is not covered.
	held := make([]*os.Process, 0, len(tree))
	for _, process := range tree {
		if found, err := os.FindProcess(process.pid); err == nil {
			held = append(held, found)
		}
	}
	defer func() {
		for _, process := range held {
			_ = process.Release()
		}
	}()
	select {
	case err := <-done:
		// The run ended as the deadline did: it did not hang.
		return err
	default:
	}
	lines := make([]string, 0, len(tree))
	for _, process := range tree {
		lines = append(lines, process.String())
	}
	dumpTree(tree)
	ended := false
	select {
	case <-done:
		ended = true
	case <-time.After(engineDumpGrace):
	}
	// A process the engine started outlives it once the engine is reaped, so
	// the kill takes the processes held before the dumps, not a new tree.
	for _, process := range held {
		_ = process.Kill()
	}
	if !ended {
		_ = cmd.Process.Kill()
		<-done
	}
	return &engineHangError{deadline: deadline, tree: lines}
}

// engineHangError is an engine run that overstayed its deadline, with the
// process tree it left.
type engineHangError struct {
	deadline time.Duration
	tree     []string
}

func (e *engineHangError) Error() string {
	return fmt.Sprintf("the engine did not finish within %s; its process tree was:\n  %s", e.deadline, strings.Join(e.tree, "\n  "))
}
