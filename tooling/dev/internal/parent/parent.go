// Package parent ends a command whose putnami parent is gone.
//
// putnami launches compose and qualify as interactive subcommands. A terminal
// signal reaches the command through putnami, which signals it and waits for
// it to tear its composition down. A putnami killed outright, by SIGKILL or an
// out-of-memory kill, signals nothing: the command would keep its members
// running until the next composition reaps them. So the command watches its
// parent itself, and its parent's loss cancels it the way a terminal signal
// does.
package parent

import (
	"context"
	"errors"
	"os"
	"time"
)

// ErrGone is the cause of a context Watch canceled: the process that started
// this one exited.
var ErrGone = errors.New("the putnami process that launched this command exited")

// pollInterval is how often Watch reads the parent's pid.
const pollInterval = 250 * time.Millisecond

// Watch returns a copy of ctx that is canceled, with ErrGone as its cause, once
// the process that started this one exits, and the function that ends the
// watch. A Unix process whose parent exits is adopted by init or a subreaper,
// so its parent pid changes: Watch polls it. Before it cancels, Watch ignores
// SIGPIPE, because the reader of the stdout and stderr the command shares with
// its parent is often gone too, and the teardown must outlive a write to them.
//
// On Windows putnami runs the command in a Job Object that ends it, and every
// process it started, with putnami, so the command has no teardown to run:
// Watch returns a cancelable copy of ctx, and the next composition reaps the
// lease the killed one left.
func Watch(ctx context.Context) (context.Context, context.CancelFunc) {
	if !supported {
		return context.WithCancel(ctx)
	}
	return watch(ctx, pollInterval, os.Getppid, ignoreBrokenPipes)
}

// watch is Watch with its clock, its parent pid reader and its loss hook
// passed in. The parent pid it compares against is the one at the call.
func watch(ctx context.Context, interval time.Duration, getppid func() int, onGone func()) (context.Context, context.CancelFunc) {
	watched, cancel := context.WithCancelCause(ctx)
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
					onGone()
					cancel(ErrGone)
					return
				}
			}
		}
	}()
	return watched, func() { cancel(nil) }
}
