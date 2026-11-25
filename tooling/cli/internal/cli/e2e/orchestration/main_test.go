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
	"time"

	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
)

// repoRoot is the root of this repository, absolute, since the CLI runs change
// the working directory.
var repoRoot string

func TestMain(m *testing.M) {
	clitest.AnswerRegistryTokenSeam()
	abs, err := filepath.Abs("../../../../../..")
	if err != nil {
		panic(err)
	}
	repoRoot = abs
	// The first run of a process reports the process since it started; this
	// clock takes that place, so every run a test counts reports its own share.
	scheduler.NewOrchestrationClock()
	// A run counts one seam call per host however long it takes.
	scheduler.HoldNativeCredentialsForTest(time.Hour)
	os.Exit(clitest.Main(m))
}
