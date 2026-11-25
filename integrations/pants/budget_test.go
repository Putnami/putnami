package pants

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// From a CPU budget of 2, no Pants command sets a Pants option, and no step of the profile names the budget.
func TestAPantsCommandTakesNoBudget(t *testing.T) {
	spectest.Proves(t, feature, "pants-keeps-its-own-width", "a-pants-command-takes-no-budget")
	profile := stack(t).Profile
	if len(profile.CPUEnv) != 0 || len(profile.CPUConfig) != 0 {
		t.Fatalf("the Pants profile binds the budget: cpuEnv %q, cpuConfig %v", profile.CPUEnv, profile.CPUConfig)
	}
	if strings.Contains(string(profileJSON), stacks.CPUsPlaceholder) {
		t.Errorf("profile.json names %s: a step, a batch step or a report step would take the budget", stacks.CPUsPlaceholder)
	}
	for name := range profile.Tasks {
		_, env := stackstest.BudgetOfTwo(t, profile, "", name)
		for variable, value := range env {
			if strings.HasPrefix(variable, "PANTS_") {
				t.Errorf("task %s: a budget of 2 sets %s = %q", name, variable, value)
			}
		}
	}
}
