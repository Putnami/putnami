package abort

import (
	"context"
	"os"
	"time"
)

// supervisorPollInterval is how often WatchSupervisor reads the parent pid.
const supervisorPollInterval = 250 * time.Millisecond

// WatchSupervisor returns a copy of ctx that is canceled once the process that
// started this one exits, and the function that ends the watch. A supervised
// serve run calls it: its supervisor, such as compose, stops it with SIGTERM,
// but a supervisor killed outright signals nothing, and the run would keep its
// serve step running. The loss stops the run as SIGTERM does, and Source then
// reports SourceSignal.
//
// A Unix process whose parent exits is adopted by init or a subreaper, so its
// parent pid changes: the watch polls it. A write to the stdout or stderr the
// lost supervisor read then fails instead of ending the process, from the call
// on (keepWritesToClosedPipes), so the run lives to stop its serve step. On
// Windows putnami's Job Object ends the supervisor's whole process tree, this
// run included: the watch only returns a cancelable copy of ctx.
func WatchSupervisor(ctx context.Context) (context.Context, context.CancelFunc) {
	if !supervisorWatched {
		return context.WithCancel(ctx)
	}
	keepWritesToClosedPipes()
	return watchSupervisor(ctx, supervisorPollInterval, os.Getppid)
}

// watchSupervisor is WatchSupervisor with its clock and its parent pid reader
// passed in. The parent pid it compares against is the one at the call.
func watchSupervisor(ctx context.Context, interval time.Duration, getppid func() int) (context.Context, context.CancelFunc) {
	watched, cancel := context.WithCancel(ctx)
	start := getppid()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-watched.Done():
				return
			case <-ticker.C:
				if getppid() != start {
					name := SourceSignal
					source.CompareAndSwap(nil, &name)
					cancel()
					return
				}
			}
		}
	}()
	return watched, cancel
}
