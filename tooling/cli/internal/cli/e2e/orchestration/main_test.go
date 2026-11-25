// Package orchestration holds the exact orchestration counts of this
// repository: what a run of the real CLI starts, read from its
// run.orchestration report, for the engine and for each extension built here.
// These are tests of this repository only: they run when Putnami changes and
// never in a consumer workspace.
//
// A pull request that changes a count changes its number here, in the same
// pull request, and a number only goes down. Wall times are not tested: they
// are read from the tooling bench.
//
// The tests swap the process streams and the working directory, so they run
// in their own test binary, one after another.
package orchestration

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
)

// repoRoot is the root of this repository, absolute, since the CLI runs change
// the working directory.
var repoRoot string

func TestMain(m *testing.M) {
	// A job that resolves a registry credential starts the CLI that spawned
	// it, which is this test binary: it answers as a CLI signed in to no cloud,
	// with no token, so the start is counted and nothing leaves the host.
	if len(os.Args) > 2 && os.Args[1] == "cloud" && os.Args[2] == "registry-token" {
		os.Exit(1)
	}
	abs, err := filepath.Abs("../../../../../..")
	if err != nil {
		panic(err)
	}
	repoRoot = abs
	// The first run of a process reports the process since it started; this
	// clock takes that place, so every run a test counts reports its own share.
	scheduler.NewOrchestrationClock()
	os.Exit(clitest.Main(m))
}
