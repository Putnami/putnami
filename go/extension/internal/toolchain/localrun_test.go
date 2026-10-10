package toolchain

import (
	"testing"

	"go.putnami.dev/sdk/extension/localrun"
)

// Every test package of the Go extension that links a reader of the hosted-run variables
// runs as a job of a local run.
func TestTheGoExtensionTestPackagesRunAsALocalRun(t *testing.T) {
	localrun.CheckModule(t)
}
