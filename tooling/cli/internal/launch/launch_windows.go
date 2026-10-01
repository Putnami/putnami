//go:build windows

package launch

import (
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
)

// delegated is set once a relaunched child is running.
var delegated atomic.Bool

// Delegated reports whether this process has handed the invocation to a
// relaunched child and only waits for it. The child shares the console, so it
// receives every Ctrl-C itself and runs its own bounded shutdown; this process
// must not exit before it, or the shell would get its prompt back while the
// child still writes to the console.
func Delegated() bool {
	return delegated.Load()
}

// reexec spawns path as a child with inherited stdio (Windows has no exec(2)),
// waits for it, and exits with the child's status so the relaunch is
// transparent to whoever invoked us. It returns only when the child could not
// be started, letting the caller fall open to the current binary.
func reexec(path string, argv []string, env []string) error {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return err // could not start the child → caller falls open
	}
	delegated.Store(true)
	err := cmd.Wait()
	if err == nil {
		os.Exit(0)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	delegated.Store(false)
	return err // the child's status is unknown → caller falls open
}
