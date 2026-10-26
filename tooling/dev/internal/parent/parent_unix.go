//go:build unix

package parent

import (
	"os/signal"
	"syscall"
)

// supported reports whether Watch watches the parent on this platform.
const supported = true

// ignoreBrokenPipes makes a write to a closed pipe fail with EPIPE instead of
// ending the process: Go ends a process that writes to a closed stdout or
// stderr unless SIGPIPE is ignored or notified.
func ignoreBrokenPipes() { signal.Ignore(syscall.SIGPIPE) }
