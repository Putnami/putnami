package planner

import (
	"slices"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// artifactExtension resolves, through the manifest conversion every loaded
// extension takes, a test command whose test step folds the manifest its own
// emit step writes and the outDir the tsc step writes in each direct
// dependency, and lists neither in dependsOn.
func artifactExtension(t *testing.T, edit func(*extension.Manifest)) *extension.ExtensionDescription {
	t.Helper()
	manifest := &extension.Manifest{
		Name:    "@test/ts",
		Version: "1.0.0",
		Commands: map[string]extension.CommandDefinition{
			"test": {Run: []extension.PipelineStep{
				{ID: "emit", Task: "emit"},
				{ID: "tsc", Task: "compile"},
				{ID: "test", Task: "run", With: map[string]extension.InputBinding{
					"manifest": {FromStep: "emit", Output: "manifest"},
					"outDir":   {FromStep: "^tsc", Output: "outDir"},
				}},
			}},
		},
		Tasks: map[string]extension.TaskDefinition{
			"emit": {
				Kind: "command", Command: "emit",
				Outputs: map[string]extension.TaskOutputPort{"manifest": {}},
				Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"manifest": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput, Path: "emit-manifest.json"},
				}},
			},
			"compile": {
				Kind: "command", Command: "compile",
				Outputs: map[string]extension.TaskOutputPort{"outDir": {}},
				Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"outDir": {Kind: extension.OutputKindDirectory, PathFrom: "outDir", OptionalEmpty: true},
				}},
			},
			"run": {
				Kind: "command", Command: "run",
				Inputs: map[string]extension.TaskInputPort{
					"manifest": {From: extension.TaskInputFromTask, Fold: extension.TaskInputFoldArtifact},
					"outDir":   {From: extension.TaskInputFromTask, Fold: extension.TaskInputFoldArtifact, Optional: true},
				},
			},
		},
	}
	if edit != nil {
		edit(manifest)
	}
	return extension.Resolve(manifest, "/ext")
}

// artifactWorkspace is app, which depends on lib and direct; lib depends on
// deep.
func artifactWorkspace() *workspace.Workspace {
	ts := []string{"@test/ts"}
	projects := []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app", Extensions: ts, Dependencies: []string{"/lib", "/direct"}},
		{ID: "/lib", Name: "lib", Path: "lib", Extensions: ts, Dependencies: []string{"/deep"}},
		{ID: "/direct", Name: "direct", Path: "direct", Extensions: ts},
		{ID: "/deep", Name: "deep", Path: "deep", Extensions: ts},
	}
	ws := workspace.NewWorkspace("", nil, projects)
	ws.Name = "artifact-inputs"
	return ws
}

func planArtifacts(t *testing.T, ws *workspace.Workspace, ext *extension.ExtensionDescription, selected ...string) ([]*jobmodel.ScheduledJob, error) {
	t.Helper()
	var projects []*workspace.Project
	for _, id := range selected {
		projects = append(projects, ws.ProjectByID(id))
	}
	return Plan(ws, []string{"test"}, projects, []*extension.ExtensionDescription{ext}, nil, nil, nil, nil)
}

// A binding is an edge: the consumer waits for its own project's emit step and
// for the tsc step of each direct dependency, never a transitive one, though
// its step lists neither, and the plan stamps both the producers each port
// reads and the output ports each producer publishes.
func TestPlan_AnArtifactBindingPlansItsProducers(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "an-artifact-binding-plans-its-producers")
	planned, err := planArtifacts(t, artifactWorkspace(), artifactExtension(t, nil), "/app")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	test := scheduledJobByKey(planned, "/app:test~test")
	if test == nil {
		t.Fatalf("planned = %v, want /app:test~test", jobKeys(planned))
	}
	wantDeps := []string{"/app:test~emit", "/direct:test~tsc", "/lib:test~tsc"}
	if !slices.Equal(test.DependsOn, wantDeps) {
		t.Fatalf("test deps = %v, want %v", test.DependsOn, wantDeps)
	}
	if deep := scheduledJobByKey(planned, "/deep:test~tsc"); deep != nil {
		t.Fatal("a transitive dependency's tsc was planned for the consumer")
	}
	want := []jobmodel.ArtifactInput{
		{Port: "manifest", Output: "manifest", Producers: []jobmodel.ArtifactProducer{{Key: "/app:test~emit", Project: "/app"}}},
		{Port: "outDir", Output: "outDir", Upstream: true, Optional: true, Producers: []jobmodel.ArtifactProducer{
			{Key: "/direct:test~tsc", Project: "/direct"}, {Key: "/lib:test~tsc", Project: "/lib"},
		}},
	}
	if len(test.ArtifactInputs) != len(want) {
		t.Fatalf("artifact inputs = %+v, want %+v", test.ArtifactInputs, want)
	}
	for i := range want {
		got := test.ArtifactInputs[i]
		if got.Port != want[i].Port || got.Output != want[i].Output || got.Upstream != want[i].Upstream ||
			got.Optional != want[i].Optional || !slices.Equal(got.Producers, want[i].Producers) {
			t.Errorf("artifact input %d = %+v, want %+v", i, got, want[i])
		}
	}
	for key, ports := range map[string][]string{
		"/app:test~emit": {"manifest"}, "/lib:test~tsc": {"outDir"}, "/direct:test~tsc": {"outDir"},
	} {
		if got := scheduledJobByKey(planned, key).ArtifactOutputs; !slices.Equal(got, ports) {
			t.Errorf("%s publishes %v, want %v", key, got, ports)
		}
	}
	if got := scheduledJobByKey(planned, "/app:test~tsc").ArtifactOutputs; len(got) != 0 {
		t.Errorf("the consumer's own tsc publishes %v, which no consumer binds", got)
	}
}

// A required artifact input without a producer fails planning, as any
// required task input does: an emit step an `if` removed, or a ^step no
// direct dependency runs. An optional one plans, with no producer.
func TestPlan_ARequiredArtifactProducerThatIsMissingFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "an-artifact-binding-plans-its-producers")
	pruned := artifactExtension(t, func(m *extension.Manifest) {
		m.Commands["test"].Run[0].If = "params.emit"
	})
	_, err := planArtifacts(t, artifactWorkspace(), pruned, "/app")
	if err == nil || !strings.Contains(err.Error(), `"manifest"`) || !strings.Contains(err.Error(), `"emit"`) {
		t.Fatalf("error = %v, want the missing emit producer of the manifest port", err)
	}

	required := artifactExtension(t, func(m *extension.Manifest) {
		m.Tasks["run"].Inputs["outDir"] = extension.TaskInputPort{From: extension.TaskInputFromTask, Fold: extension.TaskInputFoldArtifact}
	})
	_, err = planArtifacts(t, artifactWorkspace(), required, "/direct")
	if err == nil || !strings.Contains(err.Error(), `"outDir"`) || !strings.Contains(err.Error(), "no direct dependency") {
		t.Fatalf("error = %v, want the ^tsc producer no direct dependency runs", err)
	}

	planned, err := planArtifacts(t, artifactWorkspace(), artifactExtension(t, nil), "/direct")
	if err != nil {
		t.Fatalf("an optional artifact input without a producer must plan: %v", err)
	}
	test := scheduledJobByKey(planned, "/direct:test~test")
	if input := test.ArtifactInputs[1]; input.Port != "outDir" || len(input.Producers) != 0 {
		t.Fatalf("outDir input = %+v, want no producer", input)
	}
}

// A producer that declares no artifact for the bound output port fails
// planning, naming the consumer, the port and the producer.
func TestPlan_AProducerWithoutTheArtifactFails(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "an-artifact-binding-plans-its-producers")
	ext := artifactExtension(t, func(m *extension.Manifest) {
		compile := m.Tasks["compile"]
		compile.Declares = nil
		m.Tasks["compile"] = compile
	})
	_, err := planArtifacts(t, artifactWorkspace(), ext, "/app")
	if err == nil {
		t.Fatal("a producer without the artifact planned")
	}
	for _, want := range []string{"/app:test~test", `"outDir"`, "/lib:test~tsc", "undeclared"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}
