//go:build windows

package clitest

import (
	"errors"
	"os/exec"
	"testing"
)

// errFixtureNeedsUnix is why the runner fixture refuses to supervise on
// Windows. Its cancellation asks a detached supervisor to stop through SIGTERM,
// and no stop request reaches a detached process on Windows, so a cancel could
// never be honored. Refusing the start keeps that failure visible.
var errFixtureNeedsUnix = errors.New("the runner fixture cancels a detached supervisor through SIGTERM, which Windows cannot deliver")

// RequireRunnerFixture skips the test where the fixture provider cannot
// supervise a submitted execution (errFixtureNeedsUnix).
func RequireRunnerFixture(t *testing.T) {
	t.Helper()
	t.Skip(errFixtureNeedsUnix)
}

func startDetachedSupervisor(*exec.Cmd) error { return errFixtureNeedsUnix }

func stopSupervisor(int) error { return errFixtureNeedsUnix }
