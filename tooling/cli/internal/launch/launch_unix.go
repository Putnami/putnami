//go:build !windows

package launch

import "go.putnami.dev/tooling/cli/internal/runcredential"

// Delegated reports whether this process has handed the invocation to a
// relaunched child and only waits for it. It is always false here: reexec
// replaces the process image, so no parent is left to wait.
func Delegated() bool {
	return false
}

// reexec replaces the current process image with path (argv[0] must be path,
// env is the full environment). On success it does not return; the new image
// re-parses the same arguments and runs to completion in our place. When this
// process holds the run credential, the new image receives it on a fresh
// descriptor that --credential-fd names in argv (runcredential.Exec).
func reexec(path string, argv []string, env []string) error {
	return runcredential.Exec(path, argv, env)
}
