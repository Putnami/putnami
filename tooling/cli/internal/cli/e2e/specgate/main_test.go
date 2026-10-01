// Package specgate drives the executable-spec verification gate end to end
// through the real CLI, in synthetic workspaces. Its scenarios swap the
// process streams and working directory, so they run in their own test
// binary rather than holding internal/cli serial.
package specgate

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}
