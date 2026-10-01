package proctree

import "time"

// Stop ends a child within a bound and returns what done delivered. It asks the
// child to exit with terminate and waits up to grace for done; then it forces
// the child out with kill and waits for done again. When terminate fails, Stop
// calls kill at once: a request that was not delivered ends nothing, so
// waiting on it would be bounded by nothing. On Windows, Tree.Terminate and
// Relay succeed, so Stop waits the grace after them: Tree.Terminate delivers
// CTRL_BREAK_EVENT to the tree's process group, or ends the tree when no event
// can be delivered. os.Process.Signal with a signal other than os.Kill fails
// on Windows, so a stop that terminates with it kills at once.
//
// done must deliver, or be closed, once the child exited.
func Stop[T any](done <-chan T, grace time.Duration, terminate, kill func() error) T {
	if err := terminate(); err == nil {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case value := <-done:
			return value
		case <-timer.C:
		}
	}
	_ = kill()
	return <-done
}
