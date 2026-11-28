package machineproc

import (
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// becomeSubreaper makes the processes a worker's run started, once their
// worker is gone, children of the supervisor rather than of init: the
// supervisor can then end them (endOrphans).
func becomeSubreaper() {
	_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// orphans are the supervisor's children that are not its workers: processes
// a run started whose worker ended, reparented here. workers is read once the
// children are listed: a worker is registered as it is started, under the
// lock workers takes, so every worker the listing found is in the set read
// after it, and none is taken for an orphan, killed or reaped in place of the
// wait of its own.
func orphans(workers func() map[int]bool) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var children []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// The parent's pid is the fourth field, after the command's ")".
		rest := string(data)
		if i := strings.LastIndexByte(rest, ')'); i >= 0 {
			rest = rest[i+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) >= 2 {
			if ppid, err := strconv.Atoi(fields[1]); err == nil && ppid == self {
				children = append(children, pid)
			}
		}
	}
	live := workers()
	var pids []int
	for _, pid := range children {
		if !live[pid] {
			pids = append(pids, pid)
		}
	}
	return pids
}

// liveWorkers is the set of worker pids, read by orphans.
var liveWorkers = func() map[int]bool { return nil }

// endOrphans kills every orphan and its process group, and reaps it.
func endOrphans() {
	for _, pid := range orphans(liveWorkers) {
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid > 1 && pgid != syscall.Getpgrp() {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &status, 0, nil)
	}
}

// reapOrphans reaps orphans that already exited, so none stays a zombie.
func reapOrphans() {
	for _, pid := range orphans(liveWorkers) {
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	}
}

// reapOnExit reaps an orphan as soon as it exits: an orphan a run left, such as
// a member whose command was killed, ends when it sees its parent gone, and a
// zombie would still answer to a signal as if it were alive.
func reapOnExit() {
	exits := make(chan os.Signal, 1)
	signal.Notify(exits, syscall.SIGCHLD)
	reapOrphans()
	go func() {
		for range exits {
			reapOrphans()
		}
	}()
}
