//go:build unix

package abort

import (
	"os"
	"os/signal"
	"syscall"
)

// supervisorWatched reports whether WatchSupervisor watches the parent here.
const supervisorWatched = true

// keepWritesToClosedPipes makes a write to a closed pipe fail with EPIPE
// instead of ending the process: Go ends a process that writes to a closed
// stdout or stderr unless SIGPIPE is ignored or notified. Notify, unlike
// Ignore, leaves the processes this one starts with the default SIGPIPE, as an
// unsupervised run does.
func keepWritesToClosedPipes() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
}
