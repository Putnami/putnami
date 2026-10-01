// Package toolchainlock drives `putnami install` and `putnami init` through
// the real engine in a synthetic workspace and reads the toolchain pins they
// leave in the lock. It also drives the fetch that a hosted install runs
// first. Its scenarios set the environment and swap the process
// streams, so they run in their own test binary rather than holding
// internal/cli serial.
package toolchainlock

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// TestMain runs the tests, or, when the fixture Go extension's runtime execs
// the test binary, that runtime.
func TestMain(m *testing.M) {
	if os.Getenv(goExtensionFixtureEnv) != "" {
		os.Exit(runGoExtensionFixture(os.Args[1:]))
	}
	os.Exit(clitest.Main(m))
}
