package sdd

import (
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// TestValidateWorkspaceRunsTheFeaturesStep pins the move of the feature
// validation (ADR 0006 of this extension): `validate` keeps no features step,
// `validate-workspace` runs it once, and the task declares the evidence
// fragment file the CLI's verdict reads as a required file of its command
// output.
func TestValidateWorkspaceRunsTheFeaturesStep(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "feature-findings-are-evidence-fragments",
		"validate-workspace-runs-the-features-step")
	m := committedManifest(t)

	for _, step := range m.Commands["validate"].Run {
		if step.ID == "features" || step.Task == "features-validate" {
			t.Errorf("validate still runs a per-project features step: %+v", step)
		}
	}
	// `validate` plans validate-workspace, so a narrowed and an impacted run
	// still validate features.
	if also := m.Commands["validate"].AlsoRuns; len(also) != 1 || also[0] != "validate-workspace" {
		t.Errorf("validate alsoRuns = %v, want validate-workspace", also)
	}
	workspace := m.Commands["validate-workspace"]
	if workspace.Activation != "workspace-once" {
		t.Fatalf("validate-workspace activation = %q, want workspace-once", workspace.Activation)
	}
	var features []proto.PipelineStep
	for _, step := range workspace.Run {
		if step.Task == "features-validate" {
			features = append(features, step)
		}
	}
	if len(features) != 1 || features[0].ID != "features" || len(features[0].DependsOn) != 0 {
		t.Fatalf("validate-workspace features steps = %+v, want one independent step `features`", features)
	}

	task := m.Tasks["features-validate"]
	if task.Cwd != "{workspaceRoot}" {
		t.Errorf("features-validate cwd = %q, want the workspace root", task.Cwd)
	}
	if task.Declares == nil {
		t.Fatal("features-validate declares nothing")
	}
	output, ok := task.Declares.Outputs["evidence"]
	if !ok || output.Kind != proto.OutputKindFile || output.EffectiveRoot() != proto.OutputRootCommandOutput ||
		output.Path != "putnami-evidence.jsonl" || output.PathFrom != "" || output.OptionalEmpty {
		t.Fatalf("features-validate evidence output = %+v, want the required command-output file putnami-evidence.jsonl", output)
	}
	// The task reads no selection, so it is given none and one cache entry
	// serves every selection.
	if !task.ReadsNoSelection() {
		t.Errorf("features-validate selection = %q, want %q", task.Selection, proto.TaskSelectionNone)
	}
	if !task.Cache.IsEnabled() || task.Cache.NoOutput {
		t.Errorf("features-validate cache = %+v, want enabled and keeping its declared file", task.Cache)
	}
	// The key is the workspace's candidate cut, an interim the description
	// states until the collector of ADR 0006 of protocols/evidence lands.
	if repository := task.Inputs["repository"]; repository.From != "workspace" || len(repository.Files) != 1 || repository.Files[0] != "git:**" {
		t.Errorf("features-validate repository input = %+v, want the workspace's git:**", repository)
	}
	if !strings.Contains(task.Description, "interim") || !strings.Contains(task.Description, "ADR 0006 of protocols/evidence") {
		t.Errorf("features-validate description does not state that its key is an interim: %q", task.Description)
	}
}
