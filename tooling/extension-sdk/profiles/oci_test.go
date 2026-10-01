package profiles_test

import (
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/profiles"
)

// The oci profile is a builtin precisely because two extensions publish images
// through it. Resolving it beside their manifests must therefore succeed with
// both listing it under `uses`, and must fail the moment one of them declares
// it — that is the rule the builtin exists to enforce.
func TestOCIProfileResolvesWithExtensionManifests(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "ecosystem-profiles", "oci-profile-resolves-with-extension-manifests")

	publisher := func(name string, uses []string, ecosystems []extproto.EcosystemProfile) extproto.NamedManifest {
		return extproto.NamedManifest{Name: name, Manifest: &extproto.Manifest{
			Name:       name,
			Uses:       uses,
			Ecosystems: ecosystems,
			Commands:   map[string]extproto.CommandDefinition{"publish": {}},
		}}
	}

	registry, diagnostics := extproto.ResolveProfiles([]extproto.NamedManifest{
		publisher("@putnami/typescript", []string{"oci"}, nil),
		publisher("@putnami/go", []string{"oci"}, nil),
	}, profiles.Builtin())
	if len(diagnostics) != 0 {
		t.Fatalf("resolving the builtin oci profile reported %v", diagnostics)
	}
	profile, owner, ok := registry.Profile("oci")
	if !ok || owner != profiles.OCI.Owner || profile.Channel != extproto.ChannelNative {
		t.Fatalf("oci profile = %+v, owner %q, ok %v", profile, owner, ok)
	}
	if err := registry.ValidateCoordinate("oci", "putnami/web"); err != nil {
		t.Fatalf("valid repository path rejected: %v", err)
	}
	if err := registry.ValidateCoordinate("oci", "Putnami/Web"); err == nil {
		t.Fatal("an uppercase repository path was accepted")
	}
	if err := registry.ValidateVersion("oci", "0.0.0-5336de185"); err != nil {
		t.Fatalf("valid tag rejected: %v", err)
	}
	if !registry.HasNativeChannel("oci") {
		t.Fatal("oci must project channels natively: a channel name IS an OCI tag")
	}

	_, diagnostics = extproto.ResolveProfiles([]extproto.NamedManifest{
		publisher("@putnami/go", nil, []extproto.EcosystemProfile{profiles.OCI.Profile}),
	}, profiles.Builtin())
	if len(diagnostics) == 0 {
		t.Fatal("an extension redeclaring the builtin oci profile was accepted")
	}
}
