package extension

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// defaultPositionalsFixture is a manifest stamped 15 whose `stage` group
// defaults to `up`, which takes a required `project` positional.
var defaultPositionalsFixture = filepath.Join("testdata", "default-positionals", ManifestFilename)

// readDefaultPositionalsFixture parses the fixture without negotiating it, so
// a test can restamp it.
func readDefaultPositionalsFixture(t *testing.T) *Manifest {
	t.Helper()
	data, err := os.ReadFile(defaultPositionalsFixture)
	if err != nil {
		t.Fatal(err)
	}
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("fixture fails strict validation: %v", diags)
	}
	return m
}

// A group default that declares positionals requires rung 15 from every
// origin, and the stamped fixture loads.
func TestDefaultPositionalsRequireTheirContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "each-additive-vocabulary-requires-its-contract")
	m := readDefaultPositionalsFixture(t)
	if !DeclaresDefaultPositionals(m) {
		t.Fatal("DeclaresDefaultPositionals = false for a default that declares a positional")
	}
	if got := RequiredLoadContract(m); got != protocolcli.DefaultPositionalsContract {
		t.Errorf("RequiredLoadContract = %d, want %d", got, protocolcli.DefaultPositionalsContract)
	}
	if got := RequiredCLIContract(m); got != protocolcli.DefaultPositionalsContract {
		t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.DefaultPositionalsContract)
	}
	loaded, err := LoadManifest(defaultPositionalsFixture)
	if err != nil {
		t.Fatalf("a manifest stamped 15 must load: %v", err)
	}
	if loaded.CommandGroups["stage"].Default != "up" {
		t.Errorf("loaded default = %q, want up", loaded.CommandGroups["stage"].Default)
	}
}

// Stamped below 15, the manifest is refused for its positional default, with
// the remedy that moves it.
func TestDefaultPositionalsBelowTheirContractAreRefused(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "a-manifest-below-its-rung-does-not-load")
	for _, stamp := range []int{protocolcli.CurrentContract, protocolcli.DefaultPositionalsContract - 1} {
		m := readDefaultPositionalsFixture(t)
		m.CLIContract = stamp
		_, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest whose group default takes positionals", stamp)
		}
		for _, want := range []string{"requires 15 for a default subcommand that takes positionals", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}

// Positionals move the floor only on the subcommand a group defaults to: a
// default without positionals, positionals on another subcommand, and a default
// that names no subcommand keep the base stamp. The first case is the shape of
// @putnami/dev's `version` group, which defaults to `get` and gives `tag` its
// positional.
func TestPositionalsOffTheDefaultKeepTheStamp(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "the-floor-rises-only-for-a-manifest-that-uses-the-vocabulary")
	tagged := []PositionalDefinition{{Name: "tag", Required: true}}
	cases := map[string]CommandGroupDefinition{
		"default without positionals": {Default: "get", Subcommands: map[string]SubcommandDefinition{
			"get": {Command: "version-get"},
			"tag": {Command: "version-get", Positionals: tagged},
		}},
		"no default": {Subcommands: map[string]SubcommandDefinition{
			"tag": {Command: "version-get", Positionals: tagged},
		}},
		"default names no subcommand": {Default: "missing", Subcommands: map[string]SubcommandDefinition{
			"tag": {Command: "version-get", Positionals: tagged},
		}},
	}
	for name, group := range cases {
		m := &Manifest{
			Name:          "@test/x",
			Commands:      map[string]CommandDefinition{"version-get": {Run: []PipelineStep{{ID: "get", Task: "t"}}}},
			CommandGroups: map[string]CommandGroupDefinition{"version": group},
		}
		if DeclaresDefaultPositionals(m) {
			t.Errorf("%s: DeclaresDefaultPositionals = true", name)
		}
		if got := RequiredCLIContract(m); got != protocolcli.CurrentContract {
			t.Errorf("%s: RequiredCLIContract = %d, want the base %d", name, got, protocolcli.CurrentContract)
		}
	}
	if DeclaresDefaultPositionals(nil) {
		t.Error("DeclaresDefaultPositionals(nil) = true")
	}
}
