package extension

import (
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

func providerMembersManifest(stamp int, members ...string) *Manifest {
	yes := true
	return &Manifest{
		Name:        "@test/x",
		Version:     "1.0.0",
		CLIContract: stamp,
		Providers: map[string]ProviderDeclaration{"tasks": {
			Version: 1,
			Command: "{extensionRuntime}",
			Operations: map[string]ProviderOperation{
				"get":  {Description: "Read one task.", OpenWorld: &yes, Members: members},
				"find": {Description: "Find tasks.", OpenWorld: &yes},
			},
		}},
	}
}

// Declaring members on a provider operation requires contract 24: a manifest
// stamped 24 loads with them, and one stamped below is refused with the
// rung's reason.
func TestProviderMembersRequireTheirContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "each-additive-vocabulary-requires-its-contract")
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "a-manifest-below-its-rung-does-not-load")
	m := providerMembersManifest(protocolcli.ProviderMembersContract, "blockedBy", "comments")
	if !DeclaresProviderMembers(m) {
		t.Fatal("DeclaresProviderMembers = false for an operation that declares members")
	}
	if got := RequiredCLIContract(m); got != protocolcli.ProviderMembersContract {
		t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.ProviderMembersContract)
	}
	loaded, err := NegotiateManifest("x.json", marshalManifest(t, m))
	if err != nil {
		t.Fatalf("a manifest stamped 24 must load: %v", err)
	}
	if got := loaded.Providers["tasks"].Operations["get"].Members; strings.Join(got, ",") != "blockedBy,comments" {
		t.Errorf("loaded members = %v, want [blockedBy comments]", got)
	}
	for _, stamp := range []int{protocolcli.ProviderContract, protocolcli.ProviderMembersContract - 1} {
		_, err := NegotiateManifest("x.json", marshalManifest(t, providerMembersManifest(stamp, "comments")))
		if err == nil {
			t.Fatalf("stamp %d loaded a manifest whose provider operation declares members", stamp)
		}
		for _, want := range []string{"requires 24 for the members of its provider operations", "putnami extensions update"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("stamp %d: error %q does not say %q", stamp, err, want)
			}
		}
	}
}

// A provider without members stays on the providers rung.
func TestProviderMembersRaiseTheFloorOnlyWhenDeclared(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "the-floor-rises-only-for-a-manifest-that-uses-the-vocabulary")
	m := providerMembersManifest(protocolcli.ProviderContract)
	if DeclaresProviderMembers(m) || DeclaresProviderMembers(nil) {
		t.Fatal("DeclaresProviderMembers = true for a manifest whose operations declare no member")
	}
	if got := RequiredCLIContract(m); got != protocolcli.ProviderContract {
		t.Errorf("RequiredCLIContract = %d, want %d", got, protocolcli.ProviderContract)
	}
}

// Strict validation refuses a member that is not a JSON member name, and a
// repeated one.
func TestValidateProviders_RefusesMalformedMembers(t *testing.T) {
	for name, tc := range map[string]struct {
		members []string
		field   string
	}{
		"not a member name": {[]string{"blocked-by"}, "providers.tasks.operations.get.members[0]"},
		"empty":             {[]string{""}, "providers.tasks.operations.get.members[0]"},
		"repeated":          {[]string{"comments", "comments"}, "providers.tasks.operations.get.members[1]"},
		"an empty list":     {[]string{}, "providers.tasks.operations.get.members"},
	} {
		t.Run(name, func(t *testing.T) {
			errs := diag.Errors(ValidateManifest(providerMembersManifest(protocolcli.ProviderMembersContract, tc.members...)))
			if len(errs) != 1 || errs[0].Code != "invalid-value" || errs[0].Field != tc.field {
				t.Fatalf("diagnostics = %v, want one invalid-value at %s", errs, tc.field)
			}
		})
	}
	if errs := diag.Errors(ValidateManifest(providerMembersManifest(protocolcli.ProviderMembersContract, "blockedBy", "comments"))); len(errs) != 0 {
		t.Fatalf("declared members must validate: %v", errs)
	}
}
