package cpubudget

import (
	"runtime"
	"slices"
	"testing"
)

func TestParseReadsAPositiveInteger(t *testing.T) {
	for _, c := range []struct {
		value string
		want  int
	}{{"2", 2}, {" 3 ", 3}, {"0", 0}, {"-1", 0}, {"2.5", 0}, {"", 0}, {"x", 0}} {
		value, want := c.value, c.want
		if got := Parse(value); got != want {
			t.Errorf("Parse(%q) = %d, want %d", value, got, want)
		}
	}
}

// A budget of 2 reaches every name the environment does not set; a name the
// operator set keeps its value.
func TestBindSetsTheBudgetUnlessTheOperatorSetTheName(t *testing.T) {
	env := []string{"PATH=/bin", Env + "=2", "RAYON_NUM_THREADS=7"}
	got := Bind(env, "RAYON_NUM_THREADS", "CARGO_BUILD_JOBS", "NEXTEST_TEST_THREADS")
	want := []string{"PATH=/bin", Env + "=2", "RAYON_NUM_THREADS=7", "CARGO_BUILD_JOBS=2", "NEXTEST_TEST_THREADS=2"}
	if !slices.Equal(got, want) {
		t.Fatalf("Bind = %v, want %v", got, want)
	}
	if len(env) != 3 {
		t.Fatalf("Bind modified its input: %v", env)
	}
}

func TestBindWithoutABudgetSetsNothing(t *testing.T) {
	env := []string{"PATH=/bin", Env + "=0"}
	if got := Bind(env, "CARGO_BUILD_JOBS"); !slices.Equal(got, env) {
		t.Fatalf("Bind = %v, want %v", got, env)
	}
}

func TestThreadsFallsBackToTheHostCPUs(t *testing.T) {
	if got := ThreadsIn([]string{Env + "=2"}); got != 2 {
		t.Fatalf("ThreadsIn(budget 2) = %d", got)
	}
	if got := ThreadsIn(nil); got != runtime.GOMAXPROCS(0) {
		t.Fatalf("ThreadsIn(no budget) = %d, want %d", got, runtime.GOMAXPROCS(0))
	}
	t.Setenv(Env, "2")
	if got := Threads(); got != 2 {
		t.Fatalf("Threads() = %d", got)
	}
}

func TestVarsLeavesTheOperatorsVariables(t *testing.T) {
	t.Setenv(Env, "2")
	t.Setenv("RAYON_NUM_THREADS", "5")
	got := Vars("RAYON_NUM_THREADS", "BUN_JSC_numberOfGCMarkers")
	if len(got) != 1 || got["BUN_JSC_numberOfGCMarkers"] != "2" {
		t.Fatalf("Vars = %v", got)
	}
	t.Setenv(Env, "")
	if got := Vars("CARGO_BUILD_JOBS"); got != nil {
		t.Fatalf("Vars without a budget = %v", got)
	}
}
