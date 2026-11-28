package extension

import (
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// plainUpstreamFixture is the artifact-inputs fixture without a fold: its test
// step still binds ^tsc, a step result binding contracts below 32 accept, on a
// port that folds the producer's key. It is stamped below 32.
func plainUpstreamFixture(t *testing.T) *Manifest {
	t.Helper()
	m := artifactInputsFixture(t)
	m.CLIContract = protocolcli.ArtifactInputsContract - 1
	m.Tasks["run"].Inputs["manifest"] = TaskInputPort{From: TaskInputFromTask}
	m.Tasks["run"].Inputs["outDir"] = TaskInputPort{From: TaskInputFromTask, Optional: true}
	return m
}

// A ^step binding of a port that folds no artifact is what contracts below 32
// accept: it raises no floor, strict validation leaves it alone, and a
// manifest stamped below 32 that holds one loads.
func TestArtifactInputs_APlainUpstreamBindingLoadsBelowTheirContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "artifact-inputs-require-contract-32")
	m := plainUpstreamFixture(t)
	if DeclaresArtifactInputs(m) {
		t.Fatal("DeclaresArtifactInputs = true for a ^step binding of a port that folds the key")
	}
	if got := RequiredLoadContract(m); got >= protocolcli.ArtifactInputsContract {
		t.Fatalf("RequiredLoadContract = %d, want below %d", got, protocolcli.ArtifactInputsContract)
	}
	if diags := ValidateArtifactInputs(m); len(diags) != 0 {
		t.Fatalf("ValidateArtifactInputs = %v, want none for a manifest that declares no fold", diags)
	}
	if diags := FullValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("strict validation refused a plain ^step binding: %v", diags)
	}
	for _, stamp := range []int{protocolcli.ArtifactInputsContract - 1, protocolcli.ArtifactInputsContract} {
		m.CLIContract = stamp
		loaded, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err != nil {
			t.Fatalf("stamp %d: a manifest with a plain ^step binding does not load: %v", stamp, err)
		}
		if loaded.Commands["test"].Run[2].With["outDir"].FromStep != "^tsc" {
			t.Fatalf("stamp %d: loaded binding = %+v", stamp, loaded.Commands["test"].Run[2].With["outDir"])
		}
	}
}

// artifactExcludes is contract-32 vocabulary on a port that names a directory
// artifact: it raises the floor alone, and each other use reports its code on
// its field.
func TestArtifactInputs_ArtifactExcludesNameSubpathsOfADirectoryArtifact(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-artifact-input-binds-a-declared-output", "artifact-input-rules-report-their-codes")
	withExcludes := func(task, port string, excludes ...string) *Manifest {
		m := artifactInputsFixture(t)
		definition := m.Tasks[task]
		output := definition.Outputs[port]
		output.ArtifactExcludes = excludes
		definition.Outputs[port] = output
		m.Tasks[task] = definition
		return m
	}

	valid := withExcludes("compile", "outDir", "records", "nested/evidence.json")
	if diags := ValidateArtifactInputs(valid); len(diags) != 0 {
		t.Fatalf("valid artifactExcludes reported %v", diags)
	}
	if got := ArtifactExcludes(valid.Tasks["compile"], "outDir"); !slices.Equal(got, []string{"nested/evidence.json", "records"}) {
		t.Fatalf("ArtifactExcludes = %v, want the sorted entries", got)
	}
	if got := ArtifactExcludes(valid.Tasks["emit"], "manifest"); got != nil {
		t.Fatalf("ArtifactExcludes of a port that declares none = %v", got)
	}

	alone := plainUpstreamFixture(t)
	compile := alone.Tasks["compile"]
	compile.Outputs["outDir"] = TaskOutputPort{ArtifactExcludes: []string{"records"}}
	if !DeclaresArtifactInputs(alone) || RequiredLoadContract(alone) != protocolcli.ArtifactInputsContract {
		t.Fatalf("artifactExcludes alone: DeclaresArtifactInputs = %v, RequiredLoadContract = %d, want true and %d",
			DeclaresArtifactInputs(alone), RequiredLoadContract(alone), protocolcli.ArtifactInputsContract)
	}

	cases := []struct {
		name  string
		m     *Manifest
		code  string
		field string
	}{
		{"on a file artifact", withExcludes("emit", "manifest", "x"),
			"invalid-artifact-exclude", "tasks.emit.outputs.manifest.artifactExcludes"},
		{"on a port that names no declared output", func() *Manifest {
			m := withExcludes("emit", "manifest")
			m.Tasks["emit"].Outputs["data"] = TaskOutputPort{ArtifactExcludes: []string{"x"}}
			return m
		}(), "undeclared-artifact", "tasks.emit.outputs.data.artifactExcludes"},
		{"a path that is not canonical", withExcludes("compile", "outDir", "./records"),
			"invalid-output-path", "tasks.compile.outputs.outDir.artifactExcludes[0]"},
		{"a path outside the output", withExcludes("compile", "outDir", "../records"),
			"invalid-output-path", "tasks.compile.outputs.outDir.artifactExcludes[0]"},
		{"an entry declared twice", withExcludes("compile", "outDir", "records", "records"),
			"duplicate-artifact-exclude", "tasks.compile.outputs.outDir.artifactExcludes[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := ValidateArtifactInputs(tc.m)
			for _, d := range diags {
				if d.Code == tc.code && d.Field == tc.field && diag.HasErrors([]diag.Diagnostic{d}) {
					return
				}
			}
			t.Fatalf("want the error %s on %s, got %v", tc.code, tc.field, diags)
		})
	}

	broken := withExcludes("compile", "outDir", "./records")
	if _, err := NegotiateManifest("x.json", marshalManifest(t, broken)); err == nil ||
		!strings.Contains(err.Error(), "tasks.compile.outputs.outDir.artifactExcludes[0]") {
		t.Fatalf("a manifest stamped 32 with an invalid artifact exclude loaded or named another field: %v", err)
	}
}

func TestDrift_TaskOutputPortArtifactExcludes(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "the-schema-and-the-go-types-agree", "every-wire-struct-matches-the-schema")
	assertFieldParity(t, "TaskOutputPort", schemaDefProperties(t, "taskOutputPort"), goTypeJSONFields(t, TaskOutputPort{}))
}

// Strict validation follows the edges a binding implies: a cycle that only a
// binding closes is refused with the manifest, not at plan time.
func TestValidatePipelineDAG_ABindingEdgeClosesACycle(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-artifact-input-binds-a-declared-output", "artifact-input-rules-report-their-codes")
	m := artifactInputsFixture(t)
	if diags := ValidatePipelineDAG(m); len(diags) != 0 {
		t.Fatalf("an acyclic pipeline reported %v", diags)
	}
	steps := m.Commands["test"].Run
	steps[0].DependsOn = []string{"test"}
	command := m.Commands["test"]
	command.Run = steps
	m.Commands["test"] = command
	diags := ValidatePipelineDAG(m)
	if len(diags) != 1 || diags[0].Code != "pipeline-cycle" || diags[0].Field != "commands.test.run" {
		t.Fatalf("a cycle closed by the binding of emit's manifest = %v, want one pipeline-cycle", diags)
	}
}
