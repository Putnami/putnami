package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

func commandLayerManifest(stamp int, commandLayerInputs *bool) *Manifest {
	return &Manifest{
		Name:        "@test/x",
		Version:     "1.0.0",
		CLIContract: stamp,
		Commands: map[string]CommandDefinition{"test": {
			Run: []PipelineStep{
				{ID: "generate", Task: "t", CommandLayerInputs: commandLayerInputs},
				{ID: "test", Task: "t", DependsOn: []string{"generate"}},
			},
		}},
		Tasks: map[string]TaskDefinition{"t": {Kind: "command", Command: "echo"}},
	}
}

// A step reads its command layers unless it declares commandLayerInputs false.
func TestPipelineStepReadsCommandLayer(t *testing.T) {
	no, yes := false, true
	cases := []struct {
		declared *bool
		want     bool
	}{{nil, true}, {&yes, true}, {&no, false}}
	for _, tc := range cases {
		if got := (PipelineStep{CommandLayerInputs: tc.declared}).ReadsCommandLayer(); got != tc.want {
			t.Errorf("ReadsCommandLayer with commandLayerInputs %v = %v, want %v", tc.declared, got, tc.want)
		}
	}
}

// Declaring commandLayerInputs, either value, requires contract 23: a manifest
// stamped 23 loads with the field, and one stamped below is refused with the
// rung's reason.
func TestCommandLayerInputsRequiresItsContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "each-additive-vocabulary-requires-its-contract")
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "a-manifest-below-its-rung-does-not-load")
	no, yes := false, true
	for _, declared := range []*bool{&no, &yes} {
		m := commandLayerManifest(protocolcli.CommandLayerInputsContract, declared)
		if !DeclaresCommandLayerInputs(m) {
			t.Fatalf("DeclaresCommandLayerInputs = false for a step that declares %v", *declared)
		}
		if got := RequiredCLIContract(m); got != protocolcli.CommandLayerInputsContract {
			t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.CommandLayerInputsContract)
		}
		loaded, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err != nil {
			t.Fatalf("a manifest stamped 23 must load: %v", err)
		}
		if got := loaded.Commands["test"].Run[0].CommandLayerInputs; got == nil || *got != *declared {
			t.Errorf("loaded commandLayerInputs = %v, want %v", got, *declared)
		}
	}
	if DeclaresCommandLayerInputs(commandLayerManifest(protocolcli.CurrentContract, nil)) || DeclaresCommandLayerInputs(nil) {
		t.Error("DeclaresCommandLayerInputs = true for a manifest whose steps leave the field out")
	}
	for _, stamp := range []int{protocolcli.CurrentContract, protocolcli.CommandLayerInputsContract - 1} {
		_, err := NegotiateManifest("x.json", marshalManifest(t, commandLayerManifest(stamp, &no)))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest whose step declares commandLayerInputs", stamp)
		}
		for _, want := range []string{"requires 23 for a step that reads no command option layer", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}
