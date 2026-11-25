package toolchain

import (
	"os"
	"testing"

	"go.putnami.dev/sdk/extension/cpubudget"
)

// A bun process of a job with a budget of 2 runs two garbage-collector
// markers; the variables the job set itself stay.
func TestBunEnvBindsTheCPUBudget(t *testing.T) {
	t.Setenv(cpubudget.Env, "2")
	unsetEnvForBudgetTest(t, "BUN_JSC_numberOfGCMarkers")
	got := BunEnv(map[string]string{"NODE_ENV": "production"})
	if got["BUN_JSC_numberOfGCMarkers"] != "2" || got["NODE_ENV"] != "production" {
		t.Fatalf("BunEnv = %v", got)
	}
}

func TestBunEnvKeepsTheOperatorsValue(t *testing.T) {
	t.Setenv(cpubudget.Env, "2")
	t.Setenv("BUN_JSC_numberOfGCMarkers", "1")
	if got := BunEnv(nil); got != nil {
		t.Fatalf("BunEnv = %v, want the inherited operator value alone", got)
	}
}

func TestBunEnvWithoutABudgetAddsNothing(t *testing.T) {
	t.Setenv(cpubudget.Env, "")
	if got := BunEnv(map[string]string{"A": "1"}); len(got) != 1 {
		t.Fatalf("BunEnv = %v", got)
	}
}

func unsetEnvForBudgetTest(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}
