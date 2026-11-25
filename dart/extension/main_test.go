package dart

import (
	"os"
	"testing"

	"go.putnami.dev/sdk/extension/cpubudget"
	"go.putnami.dev/sdk/extension/dockertest"
)

// TestMain keeps the tests from the developer's Docker configuration, and runs
// them with a CPU budget of 2, whatever the job that runs them was granted,
// so the steps they read expand {cpus} to 2.
func TestMain(m *testing.M) {
	_ = os.Setenv(cpubudget.Env, "2")
	restore := dockertest.IsolateConfig()
	code := m.Run()
	restore()
	os.Exit(code)
}
