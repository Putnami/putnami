//go:build !unix

package abort

// supervisorWatched reports whether WatchSupervisor watches the parent here.
// A Windows process keeps the parent pid it started with after its parent
// exits, and putnami's Job Object ends the supervisor's tree with putnami.
const supervisorWatched = false

// keepWritesToClosedPipes does nothing: only Unix raises SIGPIPE.
func keepWritesToClosedPipes() {}
