package abort

import (
	"context"
	"os"
	"sync/atomic"
	"time"
)

// supervisorPollInterval is how often WatchSupervisor reads the parent pid.
const supervisorPollInterval = 250 * time.Millisecond

// startParent is the parent pid NoteParent read, or 0 before it ran.
var startParent atomic.Int64

// NoteParent records the pid of the process that started this one. The CLI
// calls it first thing, so WatchSupervisor also sees a supervisor that exited
// while the run loaded its workspace. A Unix relaunch replaces the process
// image and keeps the parent, and the new image notes it again.
func NoteParent() { startParent.Store(int64(os.Getppid())) }

// WatchSupervisor returns a copy of ctx that is canceled once the process that
// started this one exits, and the function that ends the watch. A supervised
// serve run calls it, so its supervisor's exit, signaled or not, cancels the
// run as SIGTERM cancels it, and Source then reports SourceSignal. The cancel
// arms no forced exit: the scheduler's own stop budgets bound the unwind.
//
// The process that started this one is the one NoteParent recorded, else the
// parent at the call. A Unix process whose parent exits is adopted by init or
// a subreaper, so its parent pid changes: the watch polls it, and cancels at
// once when it already changed. A write to a stdout or stderr whose reader is
// gone fails instead of ending the process from the call on
// (keepWritesToClosedPipes). On Windows putnami's Job Object ends the
// supervisor's whole process tree, this run included: the watch only returns a
// cancelable copy of ctx.
func WatchSupervisor(ctx context.Context) (context.Context, context.CancelFunc) {
	if !supervisorWatched {
		return context.WithCancel(ctx)
	}
	keepWritesToClosedPipes()
	start := int(startParent.Load())
	if start == 0 {
		start = os.Getppid()
	}
	return watchSupervisor(ctx, supervisorPollInterval, start, os.Getppid)
}

// watchSupervisor is WatchSupervisor with its clock, the supervisor's pid and
// the parent pid reader passed in.
func watchSupervisor(ctx context.Context, interval time.Duration, supervisor int, getppid func() int) (context.Context, context.CancelFunc) {
	watched, cancel := context.WithCancel(ctx)
	lost := func() bool {
		if getppid() == supervisor {
			return false
		}
		name := SourceSignal
		source.CompareAndSwap(nil, &name)
		cancel()
		return true
	}
	if lost() {
		return watched, cancel
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-watched.Done():
				return
			case <-ticker.C:
				if lost() {
					return
				}
			}
		}
	}()
	return watched, cancel
}
