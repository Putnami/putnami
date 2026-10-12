// Package hostedbootstrap drives a whole CLI session whose first-use install
// fails, on a hosted run and on a local one. Each scenario changes directory,
// sets the environment, holds a run credential and stubs the install for the
// whole process, so the package is its own test binary rather than holding
// internal/cli serial.
package hostedbootstrap

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}
