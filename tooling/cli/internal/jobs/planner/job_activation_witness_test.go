package planner

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestAJobThatGainsItsActivationFileIsPlannedInTheNextRun is the guard of
// the activation files a job declares, which a plan answers from the
// witnesses an earlier plan kept (CLI ADR 0132 §2): an unchanged tree reads
// the answer from its witnesses, and a file the plan read that changes gives
// the new plan on the next run.
func TestAJobThatGainsItsActivationFileIsPlannedInTheNextRun(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "lib", "src/lib.txt", "x\n")
	projects := []*workspace.Project{{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@test/proto"}}}
	ext := &extension.ExtensionDescription{
		Name: "@test/proto",
		Jobs: map[string]*extension.JobDefinition{
			"build": {ExtensionName: "@test/proto", Name: "build", Kind: "command", Command: "run", ActivationFiles: []string{"**/*.proto"}},
		},
	}
	plan := func(settled bool) bool {
		ws := workspace.NewWorkspace(root, nil, projects)
		ws.Name = "ws"
		planned, err := Plan(ws, []string{"build"}, projects, []*extension.ExtensionDescription{ext}, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		// The gap check matches as the planner does: on a settled tree it
		// reads the answers the plan just kept, and walks nothing of its own.
		// A file written within the record's margin keeps no answer.
		held := workspace.ActivationHolds()
		InactiveChannels(ws, []string{"build"}, projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
		if settled && workspace.ActivationHolds() == held {
			t.Error("the gap check read no activation answer the plan decided")
		}
		workspace.SaveActivationWitnesses(root, workspace.SnapshotWritePolicy{})
		return findScheduled(planned, "/lib:build") != nil
	}
	settle()
	if plan(true) {
		t.Fatal("/lib:build planned before its activation file existed")
	}
	held := workspace.ActivationHolds()
	if plan(true) {
		t.Fatal("/lib:build planned on an unchanged tree")
	}
	if workspace.ActivationHolds() == held {
		t.Fatal("the unchanged plan read no activation answer from its witnesses")
	}
	writeProjectFile(t, root, "lib", "src/api/service.proto", "syntax = \"proto3\";\n")
	if !plan(false) {
		t.Error("/lib:build not planned after its activation file appeared")
	}
	if err := os.Remove(filepath.Join(root, "lib", "src", "api", "service.proto")); err != nil {
		t.Fatal(err)
	}
	if plan(false) {
		t.Error("/lib:build still planned after its activation file was removed")
	}
}
