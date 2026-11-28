package main

import (
	"slices"
	"sort"
	"testing"

	testjob "go.putnami.dev/go/extension/internal/jobs/test"
	protocolcli "go.putnami.dev/protocol/cli"
	proto "go.putnami.dev/protocol/extension"
)

// These tests pin the wiring of ADR 0017: the test step keys on the identity
// of the binaries it tests, on what its own generate and describe steps
// wrote, and on its project's files, and on no key of a step that moves with
// the Go sources of the dependency closure.

// testSteps indexes the test command's steps by id.
func testSteps(t *testing.T, m *proto.Manifest) map[string]proto.PipelineStep {
	t.Helper()
	steps := map[string]proto.PipelineStep{}
	for _, step := range m.Commands["test"].Run {
		steps[step.ID] = step
	}
	return steps
}

func TestTestStepFoldsTheIdentityAndTheArtifactsOfItsProducers(t *testing.T) {
	m := loadExtensionManifest(t)
	if m.CLIContract < protocolcli.ArtifactInputsContract {
		t.Fatalf("cliContract = %d, want at least %d: test-exec declares artifact inputs", m.CLIContract, protocolcli.ArtifactInputsContract)
	}
	steps := testSteps(t, m)
	step := steps["test"]
	task := m.Tasks[step.Task]

	want := map[string]struct {
		step, output string
		optional     bool
	}{
		"testIdentity": {step: "test-identity", output: "manifest"},
		"gen":          {step: "generate", output: "gen"},
		// describe runs only in an app's closure (its activation), so its
		// artifacts are optional: without it the ports fold absent.
		"describeSchema":          {step: "describe", output: "schema", optional: true},
		"describeMigrationBundle": {step: "describe", output: "migrationBundle", optional: true},
		"describeMigrations":      {step: "describe", output: "migrations", optional: true},
		"describeDesign":          {step: "describe", output: "design", optional: true},
		"describeHttpRoutes":      {step: "describe", output: "httpRoutes", optional: true},
	}
	bindings := proto.StepArtifactBindings(step, task)
	if len(bindings) != len(want) {
		t.Fatalf("test step artifact bindings = %+v, want %d", bindings, len(want))
	}
	for _, binding := range bindings {
		expected, ok := want[binding.Port]
		if !ok || binding.Step != expected.step || binding.Output != expected.output || binding.Upstream || binding.Optional != expected.optional {
			t.Errorf("binding %+v, want %+v", binding, expected)
			continue
		}
		producer := m.Tasks[steps[binding.Step].Task]
		if _, _, err := proto.ArtifactOutputFor(producer, binding.Output); err != nil {
			t.Errorf("binding %s names output %q of %s, which is no artifact: %v", binding.Port, binding.Output, steps[binding.Step].Task, err)
		}
	}

	// Every other edge of the step folds its producer's key. The one left is
	// test-env, an invocation producer.
	edges := proto.WithArtifactEdges(m.Commands["test"].Run, m.Tasks)
	artifactProducers := map[string]bool{}
	for _, binding := range bindings {
		artifactProducers[binding.Step] = true
	}
	for _, s := range edges {
		if s.ID != "test" {
			continue
		}
		var keyEdges []string
		for _, edge := range s.DependsOn {
			if !artifactProducers[edge] {
				keyEdges = append(keyEdges, edge)
			}
		}
		if !slices.Equal(keyEdges, []string{"test-env"}) {
			t.Fatalf("test step key edges = %v, want [test-env]: an edge to generate, describe or the identity folds a key that moves with the closure's Go sources", keyEdges)
		}
	}
}

func TestTestExecKeysOnNoGoSourceOutsideItsProject(t *testing.T) {
	m := loadExtensionManifest(t)
	task := m.Tasks["test-exec"]
	for _, name := range sortedInputNames(task) {
		input := task.Inputs[name]
		switch input.From {
		case proto.TaskInputFromProject, proto.TaskInputFromParams, proto.TaskInputFromEnv, proto.TaskInputFromRuntime:
		case proto.TaskInputFromWorkspace:
			if !slices.Equal(input.Files, []string{"go.work", "go.work.sum"}) {
				t.Errorf("test-exec workspace input %s = %v, want the workspace module files alone", name, input.Files)
			}
		case proto.TaskInputFromTask:
			if !input.FoldsArtifact() {
				t.Errorf("test-exec task input %s folds its producer's key; it folds the artifact the step binds", name)
			}
		default:
			t.Errorf("test-exec input %s reads %q; the dependency closure reaches it through the identity", name, input.From)
		}
	}
	// The project's own Go sources stay: a test may read them as text, which
	// no content ID covers (a comment within a line).
	if sources := task.Inputs["sources"]; !slices.Contains(sources.Files, "**/*.go") || sources.From != proto.TaskInputFromProject {
		t.Errorf("test-exec sources = %+v, want the project's Go sources", sources)
	}
}

func TestTestIdentityStepNamesTheBinariesBeforeTheTestStep(t *testing.T) {
	m := loadExtensionManifest(t)
	var order []string
	for _, step := range m.Commands["test"].Run {
		order = append(order, step.ID)
	}
	identity, test := slices.Index(order, "test-identity"), slices.Index(order, "test")
	if identity < 0 || test < 0 || identity > test {
		t.Fatalf("test steps = %v, want test-identity before test", order)
	}
	step := testSteps(t, m)["test-identity"]
	if step.Task != "test-identity-exec" || !slices.Equal(step.DependsOn, []string{"generate"}) || step.ReadsCommandLayer() {
		t.Fatalf("test-identity step = %+v, want test-identity-exec after generate, reading no command option layer", step)
	}

	task := m.Tasks["test-identity-exec"]
	if len(task.Args) != 1 || task.Args[0] != "test-identity" || commandHandlers()["test-identity"] == nil {
		t.Fatalf("test-identity-exec args = %v, want the dispatched test-identity job", task.Args)
	}
	output := task.Declares.Outputs["manifest"]
	if output.Kind != proto.OutputKindFile || output.EffectiveRoot() != "command-output" || output.Path != testjob.IdentityManifestFile {
		t.Fatalf("test-identity-exec manifest = %+v, want the command-output file %s the job writes", output, testjob.IdentityManifestFile)
	}
	if !task.Cache.IsEnabled() {
		t.Fatal("test-identity-exec is not cacheable; its listing is cheap, but a hit is cheaper")
	}

	// Its key: the project's Go sources and embeds, its modules, the
	// workspace modules, the platform the binaries are built for, and the
	// parameters projectBinaryFlags derives the compile flags from. Its key
	// also folds generate's, which covers the closure: the identity moves
	// freely, test-exec does not.
	for _, name := range []string{"coverage", "coverprofile", "covermode", "coverhtml", "race"} {
		if input, ok := task.Inputs[name]; !ok || input.From != proto.TaskInputFromParams {
			t.Errorf("test-identity-exec input %s = %+v, want the compile parameter it derives the flags from", name, input)
		}
	}
	if input := task.Inputs["hostPlatform"]; input.From != proto.TaskInputFromRuntime {
		t.Errorf("test-identity-exec hostPlatform = %+v, want the runtime host platform: the binaries are built for it", input)
	}
	derived := proto.DeriveTaskCacheKey(task.Inputs)
	for _, pattern := range []string{"**/*.go", "go-embed:test", "go.mod", "go.sum"} {
		if !containsString(derived.Files, pattern) {
			t.Errorf("test-identity-exec derived key files = %v, missing %s", derived.Files, pattern)
		}
	}
}

func sortedInputNames(task proto.TaskDefinition) []string {
	names := make([]string, 0, len(task.Inputs))
	for name := range task.Inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
