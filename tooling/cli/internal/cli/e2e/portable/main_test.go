// Package portable is the T3 conformance vertical of the runner protocol:
// portable execution through a real out-of-process provider, this
// test binary in its provider, supervisor and CLI roles
// (internal/cli/clitest). Every scenario runs a whole CLI session with a
// provider child, swapping the process streams and environment, so the
// package is its own test binary rather than holding internal/cli serial;
// the T4 lifecycle and T2 admission verticals are its siblings for
// the same reason.
package portable

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.RunnerFixtureMain(m))
}
