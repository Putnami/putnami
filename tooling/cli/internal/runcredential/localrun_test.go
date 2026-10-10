package runcredential

import (
	"testing"

	"go.putnami.dev/sdk/extension/localrun"
)

// Every test package of the CLI that links a reader of the hosted-run variables
// runs as a job of a local run.
func TestTheCLITestPackagesRunAsALocalRun(t *testing.T) {
	localrun.CheckModule(t)
}
