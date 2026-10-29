package extension

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// providersFixture is a manifest stamped 19 that declares a memory provider.
var providersFixture = filepath.Join("testdata", "providers", ManifestFilename)

// readProvidersFixture parses and validates the fixture without negotiating
// it, so a test can restamp or mutate it.
func readProvidersFixture(t *testing.T) *Manifest {
	t.Helper()
	data, err := os.ReadFile(providersFixture)
	if err != nil {
		t.Fatal(err)
	}
	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("fixture fails strict validation: %v", diags)
	}
	return m
}

// A providers section requires rung 19 from every origin, it is a contract
// surface on its own, and the stamped fixture loads with every operation.
func TestProvidersRequireTheirContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "each-additive-vocabulary-requires-its-contract")
	m := readProvidersFixture(t)
	if !DeclaresProviders(m) {
		t.Fatal("DeclaresProviders = false for a manifest with a providers section")
	}
	if !DeclaresContractSurface(m) {
		t.Fatal("a provider-only manifest must be a contract surface")
	}
	if got := RequiredLoadContract(m); got != protocolcli.ProviderContract {
		t.Errorf("RequiredLoadContract = %d, want %d", got, protocolcli.ProviderContract)
	}
	if got := RequiredCLIContract(m); got != protocolcli.ProviderContract {
		t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.ProviderContract)
	}
	if diags := FullValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("a provider-only manifest must validate: %v", diags)
	}
	loaded, err := LoadManifest(providersFixture)
	if err != nil {
		t.Fatalf("a manifest stamped 19 must load: %v", err)
	}
	memory := loaded.Providers["memory"]
	if memory.Version != 1 || memory.Command != "{extensionRuntime}" || !slices.Equal(memory.Args, []string{"provider"}) {
		t.Errorf("loaded provider = %+v", memory)
	}
	if got := sortedKeys(memory.Operations); !slices.Equal(got, []string{"checkpoint", "context", "mission"}) {
		t.Errorf("loaded operations = %v", got)
	}
	if op := memory.Operations["checkpoint"]; op.Preconditions != ProviderPreconditionsAtomic || op.OpenWorld == nil || !*op.OpenWorld {
		t.Errorf("checkpoint = %+v", op)
	}
	if !memory.Operations["context"].WorkspaceSelection {
		t.Error("context lost its workspaceSelection")
	}
}

// Stamped below 19, a provider manifest is refused for its providers, with the
// remedy that moves it.
func TestProvidersBelowTheirContractAreRefused(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "a-manifest-below-its-rung-does-not-load")
	for _, stamp := range []int{0, protocolcli.CurrentContract, protocolcli.ProviderContract - 1} {
		m := readProvidersFixture(t)
		m.CLIContract = stamp
		loaded, err := NegotiateManifest("x.json", marshalManifest(t, m))
		if err == nil || loaded != nil {
			t.Fatalf("stamp %d loaded a manifest that declares providers", stamp)
		}
		for _, want := range []string{"requires 19 for its providers", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}

// A manifest without providers, or with an empty section, keeps its stamp.
func TestManifestsWithoutProvidersKeepTheStamp(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "the-floor-rises-only-for-a-manifest-that-uses-the-vocabulary")
	commands := map[string]CommandDefinition{"deploy": {Run: []PipelineStep{{ID: "d", Task: "t"}}}}
	for name, providers := range map[string]map[string]ProviderDeclaration{
		"absent": nil,
		"empty":  {},
	} {
		m := &Manifest{Name: "@test/x", Commands: commands, Providers: providers}
		if DeclaresProviders(m) {
			t.Errorf("%s: DeclaresProviders = true", name)
		}
		if got := RequiredCLIContract(m); got != protocolcli.CurrentContract {
			t.Errorf("%s: RequiredCLIContract = %d, want the base %d", name, got, protocolcli.CurrentContract)
		}
		if diags := ValidateProviders(m); diags != nil {
			t.Errorf("%s: the provider validator spoke about a manifest without providers: %v", name, diags)
		}
	}
	if DeclaresProviders(nil) {
		t.Error("DeclaresProviders(nil) = true")
	}
}

// Strict validation refuses each malformed part of a declaration, in sorted
// order, and the field names what to fix.
func TestValidateProviders_RefusesMalformedDeclarations(t *testing.T) {
	yes := true
	valid := func() ProviderDeclaration {
		return ProviderDeclaration{
			Version: 1,
			Command: "{extensionRuntime}",
			Operations: map[string]ProviderOperation{
				"find": {Description: "Find tasks.", OpenWorld: &yes},
			},
		}
	}
	cases := []struct {
		name     string
		contract string
		mutate   func(*ProviderDeclaration)
		code     string
		field    string
	}{
		{"versionless", "tasks", func(p *ProviderDeclaration) { p.Version = 0 }, "invalid-value", "providers.tasks.version"},
		{"no command", "tasks", func(p *ProviderDeclaration) { p.Command = " " }, "required-field", "providers.tasks.command"},
		{"negative timeout", "tasks", func(p *ProviderDeclaration) { p.TimeoutMs = -1 }, "invalid-value", "providers.tasks.timeoutMs"},
		{"no operation", "tasks", func(p *ProviderDeclaration) { p.Operations = nil }, "required-field", "providers.tasks.operations"},
		{"contract name", "Tasks", func(*ProviderDeclaration) {}, "invalid-provider-name", "providers.Tasks"},
		{"operation name", "tasks", func(p *ProviderDeclaration) {
			p.Operations["Find.All"] = p.Operations["find"]
		}, "invalid-provider-name", "providers.tasks.operations.Find.All"},
		{"no description", "tasks", func(p *ProviderDeclaration) {
			p.Operations["find"] = ProviderOperation{OpenWorld: &yes}
		}, "required-field", "providers.tasks.operations.find.description"},
		{"openWorld unsaid", "tasks", func(p *ProviderDeclaration) {
			p.Operations["find"] = ProviderOperation{Description: "Find tasks."}
		}, "required-field", "providers.tasks.operations.find.openWorld"},
		{"unknown precondition", "tasks", func(p *ProviderDeclaration) {
			p.Operations["find"] = ProviderOperation{Description: "Find tasks.", OpenWorld: &yes, Preconditions: "eventual"}
		}, "invalid-enum", "providers.tasks.operations.find.preconditions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := valid()
			tc.mutate(&provider)
			m := &Manifest{Name: "@test/x", Providers: map[string]ProviderDeclaration{tc.contract: provider}}
			diags := ValidateManifest(m)
			errs := diag.Errors(diags)
			if len(errs) != 1 || errs[0].Code != tc.code || errs[0].Field != tc.field {
				t.Fatalf("diagnostics = %v, want one %s at %s", diags, tc.code, tc.field)
			}
		})
	}

	// The control: the unmutated declaration is a complete manifest on its own.
	m := &Manifest{Name: "@test/x", Providers: map[string]ProviderDeclaration{"tasks": valid()}}
	if diags := ValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("a provider-only manifest must validate: %v", diags)
	}
}

// The tools section is gone with no fallback: the strict parser refuses it as
// an unknown member, and the lenient loader reads it as nothing, so the
// manifest offers no provider.
func TestTheToolsSectionIsNotAProvider(t *testing.T) {
	data := []byte(`{
  "name": "@test/x",
  "cliContract": 19,
  "commands": {"deploy": {"run": [{"id": "d", "task": "t"}]}},
  "tasks": {"t": {"kind": "command", "command": "echo"}},
  "tools": {"tasks.find": {"description": "Find tasks.", "command": "{extensionRuntime}"}}
}`)
	if _, diags := ParseManifest(data); !diag.HasErrors(diags) {
		t.Fatal("the strict parser accepted a tools section")
	}
	m, err := NegotiateManifest("x.json", data)
	if err != nil {
		t.Fatalf("the lenient loader must read a manifest that still carries tools: %v", err)
	}
	if DeclaresProviders(m) {
		t.Fatal("a tools section became a provider")
	}
}
