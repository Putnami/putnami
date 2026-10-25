package fixtureproc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/scratch"
)

// cliPackage is the shipped putnami CLI, built by import path from the test's
// working directory, which is a package directory of this module.
const cliPackage = "go.putnami.dev/tooling/cli/cmd/putnami"

// builtCLI is the CLI binary CLI builds, once per test process.
var builtCLI struct {
	once sync.Once
	// dir holds the build. Keeping it reachable keeps its owner lock held.
	dir  *scratch.Dir
	path string
	err  error
}

// CLI returns the path of this module's putnami CLI, built once per test
// process without the race detector the test binary may carry. A test uses it
// when the code under test starts the CLI as a separate process, the way a
// user's shell does. The build runs go from PATH under the environment of the
// first call. Remove deletes the build.
func CLI(t testing.TB) string {
	t.Helper()
	builtCLI.once.Do(buildCLI)
	if builtCLI.err != nil {
		t.Fatalf("fixtureproc: %v", builtCLI.err)
	}
	return builtCLI.path
}

func buildCLI() {
	// scratch.New, not os.MkdirTemp: a test process that dies before Remove
	// leaves the build to the next run's sweep.
	dir, err := scratch.New("putnami-fixtureproc-cli-")
	if err != nil {
		builtCLI.err = err
		return
	}
	builtCLI.dir = dir
	builtCLI.path = executablePath(filepath.Join(dir.Path(), "putnami"))
	cmd := exec.Command("go", "build", "-o", builtCLI.path, cliPackage)
	if out, err := cmd.CombinedOutput(); err != nil {
		builtCLI.err = fmt.Errorf("build %s: %w\n%s", cliPackage, err, out)
	}
}

// removeCLI deletes the CLI build, if any.
func removeCLI() {
	if builtCLI.dir != nil {
		_ = builtCLI.dir.Remove()
	}
}

// CLIEnv returns this process's environment for a CLI that CLI built: every
// PUTNAMI_ variable is dropped but the store and artifact roots a TestMain
// points at its scratch directory, so the run a test starts never joins the
// session, job or host channel of a putnami run that started the test, and
// telemetry is off.
func CLIEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if reachesCLI(name) {
			env = append(env, entry)
		}
	}
	return append(env, "DO_NOT_TRACK=1")
}

// IsolateCLIEnv applies CLIEnv's rule to this process's own environment, for
// a test whose code under test starts the CLI with that environment, as a
// composition starts each member's serve run. It keeps spectest's fragment
// directory, which the test process itself writes its evidence to. A TestMain
// calls it before m.Run.
func IsolateCLIEnv() {
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !reachesCLI(name) && name != spectest.FragmentDirEnv {
			_ = os.Unsetenv(name)
		}
	}
	_ = os.Setenv("DO_NOT_TRACK", "1")
}

// reachesCLI reports whether the variable name reaches a CLI a test starts.
func reachesCLI(name string) bool {
	return !strings.HasPrefix(name, "PUTNAMI_") || name == "PUTNAMI_STORE_DIR" || name == "PUTNAMI_ARTIFACT_DIR"
}
