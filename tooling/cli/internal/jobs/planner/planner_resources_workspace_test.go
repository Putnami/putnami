package planner

import (
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// workspaceOnceReader is a step of a workspace-once command, such as the
// feature validation of validate-workspace: it runs under the synthetic
// workspace project and carries the projects the run selected.
func workspaceOnceReader(name string, deps []string, selected ...*workspace.Project) *jobmodel.ScheduledJob {
	job := resourceJob("putnami", name, deps, nil, proj("sources"))
	job.Project = &workspace.Project{ID: "putnami", Name: "putnami", Path: "."}
	if len(selected) == 0 {
		selected = []*workspace.Project{{ID: "/a", Name: "a", Path: "a"}}
	}
	job.SelectedProjects = selected
	return job
}

// TestSerializeWriteResources_AWorkspaceReadIsOrderedAgainstEveryProjectsWriters
// is the plan of `lint,validate --projects /typescript/framework/analytics`:
// the project's format and check steps rewrite its sources, and the feature
// validation, once for the workspace, reads them. Before it ran once, each
// project's own features step read its sources and the writers waited for it.
// The workspace step must keep that order with every project's writers, or it
// can read a manifest mid-rewrite.
func TestSerializeWriteResources_AWorkspaceReadIsOrderedAgainstEveryProjectsWriters(t *testing.T) {
	t.Parallel()
	analytics := &workspace.Project{ID: "/typescript/framework/analytics", Name: "analytics", Path: "typescript/framework/analytics"}
	format := resourceJob(analytics.ID, "lint~format", nil, proj("sources"), nil)
	check := resourceJob(analytics.ID, "lint~check", nil, proj("sources"), nil)
	otherFormat := resourceJob("/go/framework/errors", "lint~format", nil, proj("sources"), nil)
	features := workspaceOnceReader("validate-workspace~features", nil, analytics)
	plan := []*jobmodel.ScheduledJob{format, check, otherFormat, features}

	serializeWriteResources(plan, nil, nil)

	for _, writer := range []*jobmodel.ScheduledJob{format, check, otherFormat} {
		if !containsStr(writer.SerializeAfter, features.Key()) {
			t.Errorf("%s serializeAfter = %v, want it after %s", writer.Key(), writer.SerializeAfter, features.Key())
		}
	}
	if len(features.SerializeAfter) != 0 {
		t.Errorf("the workspace reader derives from no writer and reads the tree first, got %v", features.SerializeAfter)
	}
	if err := ValidatePlanDAG(plan); err != nil {
		t.Fatalf("the plan must stay schedulable: %v", err)
	}

	// A project's reader still conflicts only with its own project's writers.
	reader := resourceJob(analytics.ID, "build~compile", nil, nil, proj("sources"))
	foreign := resourceJob("/go/framework/errors", "lint~format", nil, proj("sources"), nil)
	serializeWriteResources([]*jobmodel.ScheduledJob{reader, foreign}, nil, nil)
	if len(reader.SerializeAfter) != 0 || len(foreign.SerializeAfter) != 0 {
		t.Errorf("a project's read reached another project's writer: reader=%v writer=%v", reader.SerializeAfter, foreign.SerializeAfter)
	}
}

// TestSerializeWriteResources_AWorkspaceReadAfterAWriterClosesNoCycle covers
// the two functional shapes: a writer that depends on the workspace step, and
// a workspace step that depends on one project's writer. Neither closes a
// cycle, and a writer of a project the step does not derive from still waits
// for it.
func TestSerializeWriteResources_AWorkspaceReadAfterAWriterClosesNoCycle(t *testing.T) {
	t.Parallel()
	features := workspaceOnceReader("validate-workspace~features", nil)
	dependentWriter := resourceJob("/a", "lint~format", []string{features.Key()}, proj("sources"), nil)
	plan := []*jobmodel.ScheduledJob{features, dependentWriter}
	serializeWriteResources(plan, nil, nil)
	if err := ValidatePlanDAG(plan); err != nil {
		t.Fatalf("a writer that depends on the workspace step: %v", err)
	}
	if len(features.SerializeAfter) != 0 {
		t.Errorf("the workspace step must not wait for its own dependent, got %v", features.SerializeAfter)
	}

	generate := resourceJob("/a", "build~generate", nil, proj("sources"), nil)
	derived := workspaceOnceReader("validate-workspace~features", []string{generate.Key()})
	laterFormat := resourceJob("/b", "lint~format", nil, proj("sources"), nil)
	plan = []*jobmodel.ScheduledJob{generate, derived, laterFormat}
	serializeWriteResources(plan, nil, nil)
	if err := ValidatePlanDAG(plan); err != nil {
		t.Fatalf("a workspace step that depends on a writer: %v", err)
	}
	if containsStr(generate.SerializeAfter, derived.Key()) {
		t.Errorf("the writer the step derives from must not wait for it, got %v", generate.SerializeAfter)
	}
	if !containsStr(laterFormat.SerializeAfter, derived.Key()) {
		t.Errorf("/b:lint~format serializeAfter = %v, want it after the workspace step", laterFormat.SerializeAfter)
	}
}
