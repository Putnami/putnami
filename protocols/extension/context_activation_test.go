package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// contextActivationFixture is the emptied-output fixture whose describe step
// gates on its type or on alternatives, stamped 28.
func contextActivationFixture(t *testing.T, gate StepActivation) *Manifest {
	t.Helper()
	m := loadFixtureManifest(t, "fixtures/valid/declared-output-emptied.json")
	m.CLIContract = protocolcli.ContextActivationContract
	for name, command := range m.Commands {
		if len(command.Run) == 0 {
			continue
		}
		g := gate
		command.Run[0].Activation = &g
		m.Commands[name] = command
		return m
	}
	t.Fatal("the fixture has no pipeline step")
	return nil
}

func contextGates() map[string]StepActivation {
	return map[string]StepActivation{
		"projectTypes": {ProjectTypes: []string{"application", ""}},
		"anyOf":        {AnyOf: []StepActivation{{Files: []string{"infra/requirements.json"}}, {Contains: map[string]string{"go.mod": "go.putnami.dev/app"}}}},
	}
}

// A step activation on the project's type or on alternatives
// requires rung 28 from every origin, and the stamped manifest validates and
// loads with its gate.
func TestContextActivation_RequiresItsContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "context-activation-requires-contract-28")
	for name, gate := range contextGates() {
		m := contextActivationFixture(t, gate)
		if diags := FullValidateManifest(m); diag.HasErrors(diags) {
			t.Fatalf("%s: a context gate must validate: %v", name, diags)
		}
		if !DeclaresContextActivation(m) {
			t.Fatalf("%s: DeclaresContextActivation = false", name)
		}
		if got := RequiredLoadContract(m); got != protocolcli.ContextActivationContract {
			t.Errorf("%s: RequiredLoadContract = %d, want %d", name, got, protocolcli.ContextActivationContract)
		}
		if got := RequiredCLIContract(m); got != protocolcli.ContextActivationContract {
			t.Errorf("%s: RequiredCLIContract = %d, want %d", name, got, protocolcli.ContextActivationContract)
		}
		if _, err := NegotiateManifest("x.json", marshalManifest(t, m)); err != nil {
			t.Fatalf("%s: a manifest stamped 28 must load: %v", name, err)
		}
	}
	// The earlier gates keep their rung.
	plain := contextActivationFixture(t, StepActivation{Files: []string{"go.mod"}, ClosureFiles: []string{"conf/.env.yaml"}})
	if DeclaresContextActivation(plain) {
		t.Fatal("DeclaresContextActivation = true for files and closureFiles")
	}
}

// Stamped below 28, a manifest with such a gate is refused for it, with the
// remedy that moves it.
func TestContextActivation_BelowItsContractIsRefused(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "later-rungs-raise-the-contract-floor", "context-activation-below-contract-28-does-not-load")
	for name, gate := range contextGates() {
		for _, stamp := range []int{protocolcli.CurrentContract, protocolcli.ContextActivationContract - 1} {
			m := contextActivationFixture(t, gate)
			m.CLIContract = stamp
			_, err := NegotiateManifest("x.json", marshalManifest(t, m))
			if err == nil {
				t.Fatalf("%s: stamp %d loaded a manifest with a context gate", name, stamp)
			}
			for _, want := range []string{"requires 28 for a step activation", "putnami extensions update"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: stamp %d: error %q does not say %q", name, stamp, err, want)
				}
			}
		}
	}
}
