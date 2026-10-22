// Package toolchainlock drives `putnami install` and `putnami init` through
// the real engine in a synthetic workspace, with fixture extensions that
// answer the toolchain pins, and reads the pins they leave in the lock. It
// also drives the fetch that a hosted install runs first. Its scenarios set
// the environment and swap the process streams, so they run in their own
// test binary rather than holding internal/cli serial.
package toolchainlock

import (
	"os"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

// TestMain runs the tests, with the releases of the fixture pins served
// (toolchainReleases), or, when a fixture extension's runtime execs the test
// binary, that runtime.
func TestMain(m *testing.M) {
	if os.Getenv(goExtensionFixtureEnv) != "" {
		os.Exit(runGoExtensionFixture(os.Args[1:]))
	}
	if dir := os.Getenv(pinsExtensionFixtureEnv); dir != "" {
		os.Exit(runPinsExtensionFixture(dir, os.Args[1:]))
	}
	toolchainReleases = serveToolchainReleases()
	code := clitest.Main(m)
	toolchainReleases.Close()
	os.Exit(code)
}

// isolateHomes points the user home and the Putnami home of t at directories
// t owns. The CLI names PUTNAMI_HOME in the environment of the jobs it runs,
// so a test run by `putnami test` inherits the real one, and a toolchain the
// test installs would land there.
func isolateHomes(t *testing.T) {
	t.Helper()
	hometest.Temp(t)
	t.Setenv("PUTNAMI_HOME", t.TempDir())
}
