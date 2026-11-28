package main

import (
	"slices"
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

// The test-env step waits for describe, which converges the committed
// infra/requirements.json its task keys through the requirements closure
// input. The test step folds test-env's action and what reaches it, so the
// edge to describe binds describe's declared outputs as artifacts: the engine
// then relays their digests on that edge, not describe's key, which moves with
// every non-test Go source of the closure (ADR 0017).
func TestTestEnvStepFoldsWhatDescribeWroteNotItsKey(t *testing.T) {
	m := loadExtensionManifest(t)
	steps := testSteps(t, m)
	env, test := steps["test-env"], steps["test"]
	if env.Task != "test-env-up" || !slices.Contains(env.DependsOn, "describe") {
		t.Fatalf("test-env step = %+v, want test-env-up after describe", env)
	}

	describeBindings := func(step proto.PipelineStep) []proto.ArtifactBinding {
		var bindings []proto.ArtifactBinding
		for _, binding := range proto.StepArtifactBindings(step, m.Tasks[step.Task]) {
			if binding.Step == "describe" {
				bindings = append(bindings, binding)
			}
		}
		return bindings
	}
	envBindings, testBindings := describeBindings(env), describeBindings(test)
	if len(envBindings) == 0 {
		t.Fatal("test-env binds no output of describe, so the edge folds describe's key")
	}
	// The same outputs the test step binds, optional because describe runs
	// only in an app's closure: the relay adds no byte the test step's key
	// does not read already.
	if !slices.Equal(envBindings, testBindings) {
		t.Errorf("test-env binds %+v of describe, want the outputs the test step binds %+v", envBindings, testBindings)
	}
	producer := m.Tasks[steps["describe"].Task]
	for _, binding := range envBindings {
		if !binding.Optional || binding.Upstream {
			t.Errorf("binding %+v, want an optional binding of this project's describe", binding)
		}
		if _, _, err := proto.ArtifactOutputFor(producer, binding.Output); err != nil {
			t.Errorf("binding %s names output %q of describe, which is no artifact: %v", binding.Port, binding.Output, err)
		}
	}

	// No edge of test-env folds a key: each one binds an artifact.
	bound := map[string]bool{}
	for _, binding := range proto.StepArtifactBindings(env, m.Tasks[env.Task]) {
		bound[binding.Step] = true
	}
	for _, step := range proto.WithArtifactEdges(m.Commands["test"].Run, m.Tasks) {
		if step.ID != "test-env" {
			continue
		}
		for _, edge := range step.DependsOn {
			if !bound[edge] {
				t.Errorf("test-env edge %q binds no artifact, so it folds the key of %s", edge, edge)
			}
		}
	}

	// What the task reads stays an input of its own: the closure's committed
	// requirements.
	if port := m.Tasks["test-env-up"].Inputs["requirements"]; port.From != proto.TaskInputFromClosure ||
		!slices.Equal(port.Files, []string{"infra/requirements.json"}) {
		t.Errorf("test-env-up requirements = %+v, want the closure's infra/requirements.json", port)
	}
}
