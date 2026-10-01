package watch

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultServeRecoveryQuietFor is the quiet window required before we
	// re-arm watching after a failed serve iteration.
	DefaultServeRecoveryQuietFor = 300 * time.Millisecond
	// DefaultServeRecoveryMaxWait bounds how long we wait for the workspace
	// to settle after a failed serve iteration.
	DefaultServeRecoveryMaxWait = 5 * time.Second
	// DefaultServePortReleaseWait is how long we wait for the serve port to
	// become available before starting the next serve iteration.
	DefaultServePortReleaseWait = 2 * time.Second
	// DefaultServePortPollInterval is the poll cadence while waiting for port
	// release between serve restarts.
	DefaultServePortPollInterval = 50 * time.Millisecond
)

// runServeLoop runs the watch loop for serve mode with process lifecycle management.
// It starts the serve in a goroutine and watches for file changes concurrently.
// When changes are detected, it cancels the current iteration (sending SIGTERM
// to the serve process) and starts a new one.
func (s *Session) runServeLoop(ctx context.Context) int {
	type watchStream struct {
		changes <-chan []string
		stop    func()
	}

	closedChanges := func() <-chan []string {
		ch := make(chan []string)
		close(ch)
		return ch
	}

	newWatchStream := func(closeSignals ...<-chan struct{}) watchStream {
		watchDone := make(chan struct{})
		var once sync.Once
		stop := func() {
			once.Do(func() {
				close(watchDone)
			})
		}

		for _, sig := range closeSignals {
			if sig == nil {
				continue
			}
			go func(ch <-chan struct{}) {
				select {
				case <-ch:
					stop()
				case <-watchDone:
				}
			}(sig)
		}

		return watchStream{
			changes: s.watcher.Watch(watchDone),
			stop:    stop,
		}
	}

	// startIteration launches generate + serve in a goroutine and returns:
	// - a cancel function for the running iteration
	// - an iteration completion channel
	// - a watch stream that is armed only once serve reports readiness
	startIteration := func(changedFiles []string) (context.CancelFunc, <-chan struct{}, watchStream) {
		iterCtx, iterCancel := context.WithCancel(ctx) //nolint:gosec // G118: iterCancel is returned to caller
		iterDone := make(chan struct{})
		serveReady := s.watchRend.ResetServeReadySignal()

		go func() {
			s.runIteration(iterCtx, changedFiles)
			close(iterDone)
		}()

		// In serve mode, only arm the watcher after the server is actually
		// listening. If iteration exits before that, return a closed stream
		// and let the main loop switch to passive waiting.
		select {
		case <-ctx.Done():
			return iterCancel, iterDone, watchStream{changes: closedChanges(), stop: func() {}}
		case <-iterDone:
			return iterCancel, iterDone, watchStream{changes: closedChanges(), stop: func() {}}
		case <-serveReady:
		}

		_ = s.watcher.TakeSnapshot()
		return iterCancel, iterDone, newWatchStream(ctx.Done(), iterDone)
	}

	// Passive watch is used when serve is not running (failed/stopped) so we
	// wait for an explicit file change before attempting another restart.
	startPassiveWatch := func() watchStream {
		s.waitForWorkspaceQuiet(ctx, DefaultServeRecoveryQuietFor, DefaultServeRecoveryMaxWait)
		return newWatchStream(ctx.Done())
	}

	s.watchRend.IterationStart(0, nil)
	iterCancel, iterDone, watch := startIteration(nil)

	for {
		select {
		case <-ctx.Done():
			watch.stop()
			iterCancel()
			<-iterDone
			s.watchRend.Shutdown()
			return 0

		case changedFiles, ok := <-watch.changes:
			if !ok {
				select {
				case <-ctx.Done():
					watch.stop()
					iterCancel()
					<-iterDone
					s.watchRend.Shutdown()
					return 0
				default:
					// Iteration ended (startup failure or server exit).
					// Don't spin-restart; wait for the next real file change.
					watch = startPassiveWatch()
					continue
				}
			}

			s.iteration++

			affectedProjects := s.affectedProjects(changedFiles)
			if len(affectedProjects) == 0 {
				continue
			}

			watch.stop()

			// Cancel current iteration (sends SIGTERM to serve process).
			iterCancel()
			<-iterDone
			s.waitForServePortRelease(ctx)

			s.watchRend.ServeRestarting(changedFiles)

			// Start new iteration — watcher is armed only once serve is ready.
			iterCancel, iterDone, watch = startIteration(changedFiles)
		}
	}
}

func (s *Session) waitForServePortRelease(ctx context.Context) {
	port := DefaultPort
	if raw, ok := s.cfg.CommandParams["port"]; ok {
		if isEphemeralPort(raw) && s.cfg.BoundPort != nil {
			// Port 0 is not a listener: the port to wait on is the one the
			// previous iteration bound. None observed means nothing to release.
			port = s.cfg.BoundPort()
			if port <= 0 {
				return
			}
		} else {
			port = parsePortValue(raw, port)
		}
	}

	if IsPortAvailable(port) {
		return
	}

	deadline := time.Now().Add(DefaultServePortReleaseWait)
	for {
		if IsPortAvailable(port) {
			return
		}
		if time.Now().After(deadline) {
			return
		}

		timer := time.NewTimer(DefaultServePortPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// isEphemeralPort reports whether a "port" param asks the workload to pick its
// own port. parsePortValue cannot say so: it maps 0 to its fallback, like any
// other out-of-range value.
func isEphemeralPort(raw any) bool {
	switch v := raw.(type) {
	case int:
		return v == 0
	case int64:
		return v == 0
	case float64:
		return v == 0
	case string:
		return strings.TrimSpace(v) == "0"
	default:
		return false
	}
}

func parsePortValue(raw any, fallback int) int {
	switch v := raw.(type) {
	case int:
		return ParsePort(strconv.Itoa(v), fallback)
	case int64:
		return ParsePort(strconv.FormatInt(v, 10), fallback)
	case float64:
		return ParsePort(strconv.Itoa(int(v)), fallback)
	case string:
		return ParsePort(v, fallback)
	default:
		return fallback
	}
}

// waitForWorkspaceQuiet drains pending filesystem updates until no new changes
// are observed for quietFor, or until maxWait elapses.
func (s *Session) waitForWorkspaceQuiet(ctx context.Context, quietFor, maxWait time.Duration) {
	if quietFor <= 0 {
		return
	}
	if maxWait <= 0 {
		maxWait = quietFor
	}

	poll := s.watcher.cfg.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}

	deadline := time.Now().Add(maxWait)
	quietSince := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if len(s.watcher.detectChanges()) > 0 {
			quietSince = time.Now()
		} else if time.Since(quietSince) >= quietFor {
			return
		}

		if time.Now().After(deadline) {
			return
		}

		wait := poll
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait <= 0 {
			return
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
