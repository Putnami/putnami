package cli

import (
	"go.putnami.dev/tooling/cli/internal/commands/versioncmd"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// init lets the error of a missing runtime toolchain pin name the workspace
// declaration the pin derives from when that declaration is the cause
// (jobs.MissingPinCause). The job layer names no toolchain; versioncmd, which
// pins the lock from those declarations, explains why one gives no pin.
func init() {
	jobs.InstallMissingPinCause(versioncmd.MissingPinCause)
}
