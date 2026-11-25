package stackstest

import (
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/cpubudget"
	"go.putnami.dev/tooling/stacks"
)

// BudgetOfTwo answers what the steps of a task receive from a CPU budget of 2,
// through the core's Profile.CPUBudget, for a component that configures no
// width of its own: its steps with {cpus} expanded, and the variables the
// profile's cpuEnv binds. variant names a variant of the profile; empty reads
// the profile itself.
func BudgetOfTwo(t testing.TB, profile stacks.Profile, variant, task string) (steps [][]string, env map[string]string) {
	t.Helper()
	if variant != "" {
		var err error
		if profile, err = profile.Variant(variant); err != nil {
			t.Fatalf("variant %s: %v", variant, err)
		}
	}
	spec, ok := profile.Tasks[task]
	if !ok {
		t.Fatalf("the %s profile declares no %s task", profile.ID, task)
	}
	dir := t.TempDir()
	bound, cpus := profile.CPUBudget([]string{cpubudget.Env + "=2"}, dir, dir)
	env = map[string]string{}
	for _, entry := range bound[1:] {
		name, value, _ := strings.Cut(entry, "=")
		env[name] = value
	}
	for _, step := range spec.Steps {
		expanded := make([]string, len(step))
		for i, arg := range step {
			expanded[i] = strings.ReplaceAll(arg, stacks.CPUsPlaceholder, cpus)
		}
		steps = append(steps, expanded)
	}
	return steps, env
}
