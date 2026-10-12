package jobs

import "sync/atomic"

// missingPinRemedy is what the error of a required runtime toolchain the
// workspace lock does not pin tells the user to do, unless a MissingPinCause
// explains the missing pin.
const missingPinRemedy = "add a project that declares it, or run `putnami install` to record the declared one"

// MissingPinCause explains why the workspace lock of workspaceRoot pins no
// release for the lock identity lock: an error that names the declaration to
// write when the workspace declaration that pin derives from names no release
// the CLI can pin, and nil otherwise. `putnami install` alone pins nothing
// then, so the error names the declaration instead.
type MissingPinCause func(workspaceRoot, lock string) error

// missingPinCause is the installed MissingPinCause, or nil.
var missingPinCause atomic.Pointer[MissingPinCause]

// InstallMissingPinCause makes cause the explanation of a missing pin, and
// returns the function that restores the previous one. A nil cause removes
// it. The code that derives pins from workspace declarations installs it, so
// this package names no toolchain and no declaration file.
func InstallMissingPinCause(cause MissingPinCause) (restore func()) {
	var next *MissingPinCause
	if cause != nil {
		next = &cause
	}
	previous := missingPinCause.Swap(next)
	return func() { missingPinCause.Store(previous) }
}

// missingPinAdvice is the end of the error of a required runtime toolchain
// whose lock identity lock the workspace lock does not pin: the installed
// MissingPinCause and `putnami install` when it explains the missing pin,
// else missingPinRemedy.
func missingPinAdvice(workspaceRoot, lock string) string {
	if cause := missingPinCause.Load(); cause != nil {
		if err := (*cause)(workspaceRoot, lock); err != nil {
			return err.Error() + ", then run `putnami install`"
		}
	}
	return missingPinRemedy
}
