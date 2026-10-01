// Package lifecycle is the T4 durable-lifecycle vertical of the runner
// protocol: the portable composition of e2e/portable under the
// failures a remote placement actually meets. Each scenario runs a whole CLI
// session with a provider child, so the package is its own test binary,
// which keeps it from holding internal/cli's other tests serial.
package lifecycle

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.RunnerFixtureMain(m))
}
