package toolchain

import (
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/jobs/procdeadline"
)

// writeExecutable writes a fixture program a test then starts directly.
//
// A fork in another parallel test inherits every descriptor open at that
// moment, until its child calls exec; while that copy of the write descriptor
// lives, starting the script fails with "text file busy".
// Go takes syscall.ForkLock for writing around each fork, so holding it for
// reading from open to close keeps the descriptor out of every child. The jobs
// package's TestExecutableFixturesAreWrittenUnderTheForkLock scans this
// package's tests too.
func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, []byte(contents), 0o755) //nolint:gosec // an executable test fixture
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// findJobsRepoRoot locates the workspace root, so a test can read the real
// first-party extension manifests.
func findJobsRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("could not find the Putnami repo root from %s", start)
		}
		dir = parent
	}
}

// elapsesNever is a deadline or bound no test outlives.
const elapsesNever = time.Duration(math.MaxInt64)

// neverRunning is a procdeadline.ProcessProgress under which a process is
// never seen running, as if the host held it before its first instruction.
func neverRunning(*os.Process) func() procdeadline.ProcessSample {
	return func() procdeadline.ProcessSample { return procdeadline.ProcessSample{Held: true} }
}

// alwaysWaitingForCPU is a procdeadline.ProcessProgress under which a process
// runs at once and then waits for a CPU all the time. Each sample reads a wait
// an hour longer than the last, which the deadline keeps to the time elapsed.
func alwaysWaitingForCPU(*os.Process) func() procdeadline.ProcessSample {
	var waited time.Duration
	return func() procdeadline.ProcessSample {
		waited += time.Hour
		return procdeadline.ProcessSample{WaitedForCPU: waited, Measured: true}
	}
}
