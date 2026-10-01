// Package initchannel drives `putnami init` through the real CLI and the real
// engine against a registry and a module proxy that serve two release sets:
// the one `latest` names and the one a candidate channel names. It reads what
// init asked each of them and the lock it left. Its scenarios set the
// environment, change the working directory and swap the process streams, so
// they run in their own test binary rather than holding internal/cli serial.
package initchannel

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// TestMain runs the tests, or, when the runtime of a fixture language
// extension execs the test binary, its workspace-install job.
func TestMain(m *testing.M) {
	if os.Getenv(fixtureRuntimeEnv) != "" {
		os.Exit(runFixtureRuntime(os.Args[1:]))
	}
	os.Exit(clitest.Main(m))
}
