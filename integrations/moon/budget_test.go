package moon

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// From a CPU budget of 2, moon runs as many threads as the budget grants.
func TestTheToolsTakeTheCPUBudget(t *testing.T) {
	spectest.Proves(t, feature, "tools-take-the-cpu-budget", "the-tools-take-a-budget-of-2")
	for _, c := range []struct {
		variant, task, arg string
		env                map[string]string
	}{
		{variant: "", task: "build", arg: "", env: map[string]string{"MOON_CONCURRENCY": "2"}},
	} {
		steps, env := stackstest.BudgetOfTwo(t, stack(t).Profile, c.variant, c.task)
		if c.arg != "" && !slices.ContainsFunc(steps, func(step []string) bool { return slices.Contains(step, c.arg) }) {
			t.Errorf("%s %s: steps %q, want %s", c.variant, c.task, steps, c.arg)
		}
		for name, value := range c.env {
			if env[name] != value {
				t.Errorf("%s %s: %s = %q, want %s", c.variant, c.task, name, env[name], value)
			}
		}
	}
}
