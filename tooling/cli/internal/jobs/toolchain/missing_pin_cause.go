package toolchain

// MissingPinCause is the signature of a function that explains why the
// workspace lock of workspaceRoot pins no release for the lock identity lock.
// No resolution of this package calls one: a missing pin's error ends with
// missingPinRemedy.
type MissingPinCause func(workspaceRoot, lock string) error

// InstallMissingPinCause accepts cause, which no resolution reads, and returns
// a restore function that does nothing.
func InstallMissingPinCause(MissingPinCause) (restore func()) {
	return func() {}
}
