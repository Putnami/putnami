package extension

import "testing"

// An inactive step is idle, not unwanted: the steps it would have waited for
// stay as live as they were, so dropping it removes it alone (CLI ADR 0132).
func TestExpandPipeline_AnInactiveLeafKeepsWhatItWaitedFor(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@putnami/go",
		Tasks: map[string]TaskDefinition{
			"build-tidy":    {Kind: "command", Command: "go"},
			"build-compile": {Kind: "command", Command: "go"},
			"build-infra":   {Kind: "command", Command: "go"},
		},
	}
	steps := []PipelineStep{
		{ID: "tidy", Task: "build-tidy"},
		// compile is excluded by its condition, which no parameter sets:
		// tidy reaches no planned consumer through it.
		{ID: "compile", Task: "build-compile", DependsOn: []string{"tidy"}, If: "params.compile"},
		// infra is idle for this project, and it is the leaf that kept tidy.
		{ID: "infra", Task: "build-infra", DependsOn: []string{"tidy"},
			Activation: &StepActivation{ProjectTypes: []string{"application"}}},
	}
	opts := PipelineExpansionOptions{StepActive: func(step PipelineStep) bool { return step.ID != "infra" }}
	expanded, err := ExpandPipelineWithOptions("build", &JobDefinition{Name: "build"}, steps, ext,
		nil, opts)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	got := stepNames(expanded)
	if len(got) != 1 || got[0] != "build~tidy" {
		t.Fatalf("steps = %v, want tidy alone: the idle infra is dropped and what it waited for kept", got)
	}

	// With infra active the plan holds both, as before.
	opts.StepActive = func(PipelineStep) bool { return true }
	expanded, err = ExpandPipelineWithOptions("build", &JobDefinition{Name: "build"}, steps, ext,
		nil, opts)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got := stepNames(expanded); len(got) != 2 {
		t.Fatalf("steps = %v, want tidy and infra", got)
	}
}
