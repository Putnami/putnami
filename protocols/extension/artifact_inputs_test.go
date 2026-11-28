package extension

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// artifactInputsFixture is a test command whose test step folds two
// artifacts: the manifest file its own emit step writes, and the outDir the
// tsc step writes in each direct dependency. It is stamped at the
// artifact-inputs rung.
func artifactInputsFixture(t *testing.T) *Manifest {
	t.Helper()
	return &Manifest{
		Name:        "sample/artifact-inputs",
		Version:     "1.0.0",
		CLIContract: protocolcli.ArtifactInputsContract,
		Commands: map[string]CommandDefinition{
			"test": {
				Description: "Test against the artifacts the steps it binds produce",
				Run: []PipelineStep{
					{ID: "emit", Task: "emit"},
					{ID: "tsc", Task: "compile"},
					{ID: "test", Task: "run", With: map[string]InputBinding{
						"manifest": {FromStep: "emit", Output: "manifest"},
						"outDir":   {FromStep: "^tsc", Output: "outDir"},
					}},
				},
			},
		},
		Tasks: map[string]TaskDefinition{
			"emit": {
				Kind: "command", Command: "emit",
				Outputs: map[string]TaskOutputPort{"manifest": {Kind: OutputKindFile, Path: "emit-manifest.json"}},
				Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"manifest": {Kind: OutputKindFile, Root: OutputRootCommandOutput, Path: "emit-manifest.json"},
				}},
			},
			"compile": {
				Kind: "command", Command: "compile",
				Outputs: map[string]TaskOutputPort{"outDir": {}},
				Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"outDir": {Kind: OutputKindDirectory, PathFrom: "outDir", OptionalEmpty: true},
				}},
			},
			"run": {
				Kind: "command", Command: "run",
				Inputs: map[string]TaskInputPort{
					"manifest": {From: TaskInputFromTask, Fold: TaskInputFoldArtifact},
					"outDir":   {From: TaskInputFromTask, Fold: TaskInputFoldArtifact, Optional: true},
				},
			},
		},
	}
}

func TestArtifactInputs_AValidDeclarationValidates(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-artifact-input-binds-a-declared-output", "artifact-input-rules-report-their-codes")
	m := artifactInputsFixture(t)
	if diags := ValidateArtifactInputs(m); len(diags) != 0 {
		t.Fatalf("expected no diagnostics, got %v", diags)
	}
	if diags := FullValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("a manifest with artifact inputs must validate: %v", diags)
	}
}

// Each rule reports its own code on its own field, so a reader keys on the
// code rather than on the message.
func TestArtifactInputs_Rules(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-artifact-input-binds-a-declared-output", "artifact-input-rules-report-their-codes")
	bind := func(m *Manifest, port string, binding InputBinding) {
		step := m.Commands["test"].Run[2]
		step.With[port] = binding
	}
	cases := []struct {
		name  string
		edit  func(*Manifest)
		code  string
		field string
	}{
		{"a fold outside the vocabulary", func(m *Manifest) {
			m.Tasks["run"].Inputs["manifest"] = TaskInputPort{From: TaskInputFromTask, Fold: "key"}
		}, "invalid-enum", "tasks.run.inputs.manifest.fold"},
		{"a fold on a project port", func(m *Manifest) {
			m.Tasks["run"].Inputs["sources"] = TaskInputPort{From: TaskInputFromProject, Files: []string{"src/**"}, Fold: TaskInputFoldArtifact}
		}, "invalid-value", "tasks.run.inputs.sources.fold"},
		{"an upstream binding of a port that folds the key", func(m *Manifest) {
			m.Tasks["run"].Inputs["outDir"] = TaskInputPort{From: TaskInputFromTask, Optional: true}
		}, "invalid-upstream-binding", "commands.test.run[2].with.outDir.fromStep"},
		{"an artifact port bound by a value", func(m *Manifest) {
			bind(m, "manifest", InputBinding{Value: "x", HasValue: true})
		}, "invalid-artifact-binding", "commands.test.run[2].with.manifest"},
		{"an artifact binding without an output", func(m *Manifest) {
			bind(m, "manifest", InputBinding{FromStep: "emit"})
		}, "required-field", "commands.test.run[2].with.manifest.output"},
		{"an artifact binding of the step itself", func(m *Manifest) {
			bind(m, "manifest", InputBinding{FromStep: "test", Output: "manifest"})
		}, "invalid-artifact-binding", "commands.test.run[2].with.manifest.fromStep"},
		{"an artifact binding of a step the command does not run", func(m *Manifest) {
			bind(m, "manifest", InputBinding{FromStep: "pack", Output: "manifest"})
		}, "unknown-reference", "commands.test.run[2].with.manifest.fromStep"},
		{"an output the producer task does not declare as a port", func(m *Manifest) {
			bind(m, "manifest", InputBinding{FromStep: "emit", Output: "data"})
		}, "unknown-reference", "commands.test.run[2].with.manifest.output"},
		{"an output port that names no declared output", func(m *Manifest) {
			m.Tasks["emit"].Outputs["data"] = TaskOutputPort{}
			bind(m, "manifest", InputBinding{FromStep: "emit", Output: "data"})
		}, "undeclared-artifact", "commands.test.run[2].with.manifest.output"},
		{"an output port that names two declared outputs", func(m *Manifest) {
			m.Tasks["emit"].Declares.Outputs["copy"] = DeclaredOutput{Kind: OutputKindFile, PathFrom: "manifest"}
		}, "ambiguous-artifact", "commands.test.run[2].with.manifest.output"},
		{"an output port of another kind", func(m *Manifest) {
			m.Tasks["emit"].Outputs["manifest"] = TaskOutputPort{Kind: OutputKindDirectory}
		}, "invalid-artifact-output", "commands.test.run[2].with.manifest.output"},
		{"an output port at another path", func(m *Manifest) {
			m.Tasks["emit"].Outputs["manifest"] = TaskOutputPort{Kind: OutputKindFile, Path: "other.json"}
		}, "invalid-artifact-output", "commands.test.run[2].with.manifest.output"},
		{"an invocation-scoped output", func(m *Manifest) {
			m.Tasks["emit"].Declares.Outputs["manifest"] = DeclaredOutput{Kind: OutputKindFile, Scope: OutputScopeInvocation, Path: "emit-manifest.json"}
		}, "invalid-artifact-output", "commands.test.run[2].with.manifest.output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := artifactInputsFixture(t)
			tc.edit(m)
			diags := ValidateArtifactInputs(m)
			for _, d := range diags {
				if d.Code == tc.code && d.Field == tc.field {
					if !diag.HasErrors([]diag.Diagnostic{d}) {
						t.Fatalf("%s on %s is not an error", tc.code, tc.field)
					}
					return
				}
			}
			t.Fatalf("want %s on %s, got %v", tc.code, tc.field, diags)
		})
	}
}

// An output port names one declared output, by id or as its pathFrom, and two
// spellings of the path it states name the same output.
func TestArtifactInputs_AnOutputPortNamesOneDeclaredOutput(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-artifact-input-binds-a-declared-output", "an-output-port-names-one-declared-output")
	m := artifactInputsFixture(t)

	id, output, err := ArtifactOutputFor(m.Tasks["emit"], "manifest")
	if err != nil || id != "manifest" || output.Path != "emit-manifest.json" {
		t.Fatalf("by id: %q %+v %v", id, output, err)
	}
	id, output, err = ArtifactOutputFor(m.Tasks["compile"], "outDir")
	if err != nil || id != "outDir" || output.PathFrom != "outDir" {
		t.Fatalf("by pathFrom: %q %+v %v", id, output, err)
	}

	compile := m.Tasks["compile"]
	compile.Declares.Outputs = map[string]DeclaredOutput{
		"dist": {Kind: OutputKindDirectory, PathFrom: "outDir", OptionalEmpty: true},
	}
	if id, _, err := ArtifactOutputFor(compile, "outDir"); err != nil || id != "dist" {
		t.Fatalf("a pathFrom output under another id: %q %v", id, err)
	}

	emit := m.Tasks["emit"]
	emit.Outputs["manifest"] = TaskOutputPort{Kind: OutputKindFile, Path: "./emit-manifest.json"}
	if id, _, err := ArtifactOutputFor(emit, "manifest"); err != nil || id != "manifest" {
		t.Fatalf("a port path spelled ./: %q %v", id, err)
	}
}

// A binding implies the edge to its producer: the step of the same command,
// or the ^step reference of an upstream binding, unless the step already
// lists it. The input steps are not modified.
func TestArtifactInputs_ABindingIsAnEdge(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-artifact-input-binds-a-declared-output", "artifact-input-rules-report-their-codes")
	m := artifactInputsFixture(t)
	steps := m.Commands["test"].Run
	edged := WithArtifactEdges(steps, m.Tasks)
	if got := strings.Join(edged[2].DependsOn, ","); got != "emit,^tsc" {
		t.Fatalf("test dependsOn = %q, want emit,^tsc", got)
	}
	if len(steps[2].DependsOn) != 0 {
		t.Fatalf("the input step was modified: %v", steps[2].DependsOn)
	}
	listed := slices.Clone(steps)
	listed[2].DependsOn = []string{"^tsc", "emit"}
	if got := WithArtifactEdges(listed, m.Tasks); strings.Join(got[2].DependsOn, ",") != "^tsc,emit" {
		t.Fatalf("listed edges were repeated: %v", got[2].DependsOn)
	}
	plain := []PipelineStep{{ID: "emit", Task: "emit"}}
	if got := WithArtifactEdges(plain, m.Tasks); &got[0] != &plain[0] {
		t.Fatal("steps without artifact bindings were copied")
	}
}

// A port that declares fold, bound by a step of the command or a ^step
// binding, requires rung 32, and a manifest stamped there loads with them.
func TestArtifactInputs_RequireTheirContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "artifact-inputs-require-contract-32")
	edits := map[string]func(*Manifest){
		"a fold and an upstream binding": func(*Manifest) {},
		"a fold alone": func(m *Manifest) {
			delete(m.Commands["test"].Run[2].With, "outDir")
			delete(m.Tasks["run"].Inputs, "outDir")
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			m := artifactInputsFixture(t)
			edit(m)
			if !DeclaresArtifactInputs(m) {
				t.Fatal("DeclaresArtifactInputs = false")
			}
			if got := RequiredCLIContract(m); got != protocolcli.ArtifactInputsContract {
				t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.ArtifactInputsContract)
			}
			if got := RequiredLoadContract(m); got != protocolcli.ArtifactInputsContract {
				t.Errorf("RequiredLoadContract = %d, want %d", got, protocolcli.ArtifactInputsContract)
			}
		})
	}
	loaded, err := NegotiateManifest("x.json", marshalManifest(t, artifactInputsFixture(t)))
	if err != nil {
		t.Fatalf("a manifest stamped 32 must load: %v", err)
	}
	if loaded.Tasks["run"].Inputs["manifest"].Fold != TaskInputFoldArtifact || loaded.Commands["test"].Run[2].With["outDir"].FromStep != "^tsc" {
		t.Fatalf("loaded test step = %+v, task = %+v", loaded.Commands["test"].Run[2], loaded.Tasks["run"])
	}

	plain := artifactInputsFixture(t)
	plain.Tasks["run"].Inputs["manifest"] = TaskInputPort{From: TaskInputFromTask}
	delete(plain.Tasks["run"].Inputs, "outDir")
	delete(plain.Commands["test"].Run[2].With, "outDir")
	if DeclaresArtifactInputs(plain) {
		t.Fatal("DeclaresArtifactInputs = true for a step result binding of a port that folds the key")
	}
}

// Stamped below 32, a manifest with artifact inputs is refused for them, with
// the remedy that moves it.
func TestArtifactInputs_BelowTheirContractAreRefused(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "artifact-inputs-below-contract-32-do-not-load")
	for _, stamp := range []int{protocolcli.CurrentContract, protocolcli.ArtifactInputsContract - 1} {
		m := artifactInputsFixture(t)
		m.CLIContract = stamp
		_, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest with artifact inputs", stamp)
		}
		for _, want := range []string{"requires 32 for its artifact inputs", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}

// Stamped at 32, a manifest whose artifact input binds no declared output does
// not load, and the error names the binding.
func TestArtifactInputs_AnInvalidBindingAtTheirContractDoesNotLoad(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-artifact-input-binds-a-declared-output", "an-invalid-artifact-input-at-contract-32-does-not-load")
	m := artifactInputsFixture(t)
	m.Commands["test"].Run[2].With["manifest"] = InputBinding{FromStep: "emit", Output: "data"}
	_, err := NegotiateManifest("x.json", marshalManifest(t, m))
	if err == nil || !strings.Contains(err.Error(), "commands.test.run[2].with.manifest.output") {
		t.Fatalf("error = %v, want one naming the binding's output", err)
	}
}

// Fold is task contract: an entry written for a port that folds the key never
// serves a task whose port folds an artifact.
func TestArtifactInputs_TheFoldIsTaskContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "task-digests-move-only-with-the-contract", "only-contract-members-move-the-digest")
	task := artifactInputsFixture(t).Tasks["run"]
	withFold := TaskContractDigest(task)
	task.Inputs = map[string]TaskInputPort{
		"manifest": {From: TaskInputFromTask},
		"outDir":   {From: TaskInputFromTask, Optional: true},
	}
	if TaskContractDigest(task) == withFold {
		t.Fatal("removing the fold kept the task contract digest")
	}
}

func TestDrift_TaskInputPortFold(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "the-schema-and-the-go-types-agree", "every-wire-struct-matches-the-schema")
	assertFieldParity(t, "TaskInputPort", schemaDefProperties(t, "taskInputPort"), goTypeJSONFields(t, TaskInputPort{}))
	var schema struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema %s: %v", schemaPath, err)
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse schema %s: %v", schemaPath, err)
	}
	if got := schema.Defs["taskInputPort"].Properties["fold"].Enum; !slices.Equal(got, ValidTaskInputFolds) {
		t.Fatalf("schema fold enum = %v, want %v", got, ValidTaskInputFolds)
	}
}
