// Package admission is the T2 admission vertical of the runner protocol
// (ADR 0037): the portable composition of e2e/portable on a workspace
// whose tasks declare inputs the plain snapshot does not carry. Each scenario
// runs a whole CLI session with a provider child, so the package is its own
// test binary, which keeps it from holding internal/cli's other tests serial.
package admission

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.RunnerFixtureMain(m))
}
