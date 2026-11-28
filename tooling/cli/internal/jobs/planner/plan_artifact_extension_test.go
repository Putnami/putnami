package planner

import (
	"slices"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// otherTscExtension runs a test command whose tsc step has the id the
// artifact extension's ^tsc binding names, and declares no artifact.
func otherTscExtension() *extension.ExtensionDescription {
	return extension.Resolve(&extension.Manifest{
		Name:    "@test/other",
		Version: "1.0.0",
		Commands: map[string]extension.CommandDefinition{
			"test": {Run: []extension.PipelineStep{{ID: "tsc", Task: "check"}}},
		},
		Tasks: map[string]extension.TaskDefinition{
			"check": {Kind: "command", Command: "check"},
		},
	}, "/other")
}

// A ^step binding names a step of the manifest that declares it: a direct
// dependency whose step of that id another extension runs is no producer of
// the artifact, and plans no failure for a declaration it never made.
func TestPlan_AnUpstreamProducerIsAStepOfTheConsumersExtension(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "an-artifact-binding-plans-its-producers")
	projects := []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app", Extensions: []string{"@test/ts"}, Dependencies: []string{"/lib", "/direct"}},
		{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@test/ts"}},
		{ID: "/direct", Name: "direct", Path: "direct", Extensions: []string{"@test/other"}},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "artifact-extensions"
	planned, err := Plan(ws, []string{"test"}, []*workspace.Project{ws.ProjectByID("/app")},
		[]*extension.ExtensionDescription{artifactExtension(t, nil), otherTscExtension()}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	test := scheduledJobByKey(planned, "/app:test~test")
	if test == nil {
		t.Fatalf("planned = %v, want /app:test~test", jobKeys(planned))
	}
	var outDir *jobmodel.ArtifactInput
	for i := range test.ArtifactInputs {
		if test.ArtifactInputs[i].Port == "outDir" {
			outDir = &test.ArtifactInputs[i]
		}
	}
	want := []jobmodel.ArtifactProducer{{Key: "/lib:test~tsc", Project: "/lib"}}
	if outDir == nil || !slices.Equal(outDir.Producers, want) {
		t.Fatalf("outDir input = %+v, want the producer %v alone", outDir, want)
	}
	if direct := scheduledJobByKey(planned, "/direct:test~tsc"); direct != nil && len(direct.ArtifactOutputs) != 0 {
		t.Fatalf("another extension's step publishes %v", direct.ArtifactOutputs)
	}
}
