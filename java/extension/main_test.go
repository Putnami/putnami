package java

import (
	"os"
	"testing"

	"go.putnami.dev/sdk/extension/cpubudget"
)

// TestMain runs the tests with a CPU budget of 2, whatever the job that runs
// them was granted, so the steps they read expand {cpus} to 2.
func TestMain(m *testing.M) {
	_ = os.Setenv(cpubudget.Env, "2")
	os.Exit(m.Run())
}
