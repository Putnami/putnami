package toolchain

import (
	"testing"

	"go.putnami.dev/sdk/extension/localrun"
)

// Every test package of the TypeScript extension that links a reader of the hosted-run variables
// runs as a job of a local run.
func TestTheTypeScriptExtensionTestPackagesRunAsALocalRun(t *testing.T) {
	localrun.CheckModule(t)
}
