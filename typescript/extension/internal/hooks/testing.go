package hooks

import "go.putnami.dev/sdk/extension/exec"

// SetExecRunForTesting replaces the exec.Run function used by hook functions.
// Returns a cleanup function that restores the original.
func SetExecRunForTesting(fn func(string, []string, ...exec.Option) (*exec.Result, error)) func() {
	orig := execRunFunc
	execRunFunc = fn
	return func() { execRunFunc = orig }
}
