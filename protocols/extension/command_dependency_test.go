package extension

import (
	"fmt"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// stepDependencyManifest is a manifest stamped `stamp` whose clientgen command
// depends on dependsOn.
func stepDependencyManifest(stamp int, dependsOn ...string) *Manifest {
	return &Manifest{
		Name:        "@test/x",
		Version:     "1.0.0",
		CLIContract: stamp,
		Commands: map[string]CommandDefinition{"clientgen": {
			DependsOn: dependsOn,
			Run:       []PipelineStep{{ID: "generate", Task: "t"}},
		}},
		Tasks: map[string]TaskDefinition{"t": {Kind: "command", Command: "echo"}},
	}
}

// Each reader takes the command an entry names from one parse: a bare name, a
// step of a command, and a session barrier.
func TestParseCommandDependency(t *testing.T) {
	cases := map[string]CommandDependency{
		"build":                 {Command: "build"},
		"build~describe":        {Command: "build", Step: "describe"},
		"!publish":              {Command: "publish", Session: true},
		"!build~describe":       {Command: "build", Step: "describe", Session: true},
		"build~describe~schema": {Command: "build", Step: "describe~schema"},
		"":                      {},
	}
	for entry, want := range cases {
		if got := ParseCommandDependency(entry); got != want {
			t.Errorf("ParseCommandDependency(%q) = %+v, want %+v", entry, got, want)
		}
	}
}

// A dependency on one step requires rung 18 from every origin, and a manifest
// stamped 18 loads it intact.
func TestStepDependencyRequiresItsContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "each-additive-vocabulary-requires-its-contract")
	m := stepDependencyManifest(protocolcli.StepDependencyContract, "build~generate", "build~describe")
	if !DeclaresStepDependency(m) {
		t.Fatal("DeclaresStepDependency = false for a command that depends on build~describe")
	}
	if got := RequiredLoadContract(m); got != protocolcli.StepDependencyContract {
		t.Errorf("RequiredLoadContract = %d, want %d", got, protocolcli.StepDependencyContract)
	}
	if got := RequiredCLIContract(m); got != protocolcli.StepDependencyContract {
		t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.StepDependencyContract)
	}
	loaded, err := NegotiateManifest("x.json", marshalManifest(t, m))
	if err != nil {
		t.Fatalf("a manifest stamped 18 must load: %v", err)
	}
	if got := loaded.Commands["clientgen"].DependsOn; len(got) != 2 || got[1] != "build~describe" {
		t.Errorf("loaded dependsOn = %v, want [build~generate build~describe]", got)
	}
}

// Bare names and session barriers declare nothing: the manifest keeps the base
// stamp.
func TestNoStepDependencyKeepsTheStamp(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "the-floor-rises-only-for-a-manifest-that-uses-the-vocabulary")
	m := stepDependencyManifest(protocolcli.CurrentContract, "build", "!lint")
	if DeclaresStepDependency(m) {
		t.Fatal("DeclaresStepDependency = true for bare names and a session barrier")
	}
	if got := RequiredCLIContract(m); got != protocolcli.CurrentContract {
		t.Errorf("RequiredCLIContract = %d, want the base %d", got, protocolcli.CurrentContract)
	}
	if DeclaresStepDependency(nil) {
		t.Error("DeclaresStepDependency(nil) = true")
	}
}

// Stamped below 18, the manifest is refused for its step dependency with the
// remedy that moves it: an older reader would plan nothing for the entry and
// run the dependent before the step it reads. A malformed entry that holds the
// separator is refused the same way.
func TestStepDependencyBelowItsContractIsRefused(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "a-manifest-below-its-rung-does-not-load")
	for _, entry := range []string{"build~describe", "!build~describe", "build~"} {
		for _, stamp := range []int{protocolcli.CurrentContract, protocolcli.StepDependencyContract - 1} {
			_, err := NegotiateManifest("x.json", marshalManifest(t, stepDependencyManifest(stamp, entry)))
			if err == nil {
				t.Fatalf("stamp %d loaded a manifest that depends on %q", stamp, entry)
			}
			for _, want := range []string{"requires 18 for a dependency on one step of another command", "putnami extensions update"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("stamp %d, %q: error %q does not say %q", stamp, entry, err, want)
				}
			}
		}
	}
}

// Stamped 18, a manifest speaks the step form, so an entry the planner does
// not read is refused at load instead of planning nothing.
func TestMalformedStepDependencyAtItsContractIsRefused(t *testing.T) {
	for _, entry := range []string{"~describe", "build~", "build~describe~schema", "!build~describe", "build~*describe"} {
		_, err := NegotiateManifest("x.json", marshalManifest(t, stepDependencyManifest(protocolcli.StepDependencyContract, entry)))
		if err == nil {
			t.Fatalf("a manifest stamped 18 loaded the dependency %q", entry)
		}
		if !strings.Contains(err.Error(), "commands.clientgen.dependsOn[0]") {
			t.Errorf("%q: error %q does not name the entry", entry, err)
		}
	}
}

// Strict validation accepts the three forms and refuses every other spelling
// at any stamp, with the entry's field.
func TestValidateManifest_StepDependencyForms(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "strict-manifests-and-a-fixture-corpus", "each-counter-example-reports-its-exact-codes")
	m := stepDependencyManifest(protocolcli.StepDependencyContract, "build", "build~describe", "!lint")
	if diags := ValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("command, command~step and !command must validate: %v", diags)
	}
	invalid := []string{"~describe", "build~", "build~describe~schema", "!build~describe", "build~^describe", "^build~describe", "/build~describe"}
	errors := diag.Errors(ValidateManifest(stepDependencyManifest(protocolcli.StepDependencyContract, invalid...)))
	if len(errors) != len(invalid) {
		t.Fatalf("errors = %v, want one per invalid entry %v", errors, invalid)
	}
	for i, d := range errors {
		if d.Code != "invalid-command-dependency" || d.Field != fmt.Sprintf("commands.clientgen.dependsOn[%d]", i) {
			t.Errorf("error %d = %s %s, want invalid-command-dependency at dependsOn[%d]", i, d.Code, d.Field, i)
		}
	}
}
