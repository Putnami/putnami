package cachekey

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/profiles"
	"go.putnami.dev/sdk/extension/releaseset"
)

// testProfiles is the ecosystem registry the release-set tests resolve
// against: npm owned by "npm-owner", go owned by "go-owner", plus the SDK's
// builtin oci. The CLI names no ecosystem itself, so every test that reaches
// the coordinator has to supply one of these.
var testProfiles = sync.OnceValue(func() *extensionproto.ProfileRegistry {
	owner := func(name, id, versionPattern string) extensionproto.NamedManifest {
		return extensionproto.NamedManifest{Name: name, Manifest: &extensionproto.Manifest{
			Name:     name,
			Commands: map[string]extensionproto.CommandDefinition{"package": {}, "publish": {}},
			Ecosystems: []extensionproto.EcosystemProfile{{
				ID:         id,
				Coordinate: extensionproto.PatternRule{Pattern: `^[A-Za-z0-9@._/-]+$`},
				Version:    extensionproto.VersionRule{Pattern: versionPattern, Ordering: extensionproto.OrderingSemver},
				Channel:    extensionproto.ChannelNative,
				Registries: json.RawMessage(`{"type":"object"}`),
				Publish:    "publish",
			}},
		}}
	}
	registry, diagnostics := extensionproto.ResolveProfiles([]extensionproto.NamedManifest{
		owner("npm-owner", "npm", `^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)*$`),
		owner("go-owner", "go", `^v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)*$`),
	}, profiles.Builtin())
	if len(diagnostics) != 0 {
		panic(fmt.Sprintf("test ecosystem profiles are invalid: %v", diagnostics))
	}
	return registry
})

// releaseSetProjectMetadata is what an extension probe writes: one member
// declaration per (ecosystem, coordinate) this project contributes.
func releaseSetProjectMetadata(t *testing.T, ecosystem distribution.Ecosystem, coordinate string, dependencies ...string) map[string]json.RawMessage {
	t.Helper()
	return releaseSetProjectMembers(t, releaseset.MemberDeclaration{
		Ecosystem: ecosystem, Coordinate: coordinate, PackageStep: string(ecosystem), PublishStep: string(ecosystem), Dependencies: sortedCopy(dependencies),
	})
}

func releaseSetProjectMembers(t *testing.T, declarations ...releaseset.MemberDeclaration) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		ReleaseSet releaseset.ProjectMetadata `json:"releaseSet"`
	}{ReleaseSet: releaseset.ProjectMetadata{Ecosystems: declarations}})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]json.RawMessage{"test-provider": raw}
}

func sortedCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return copied
}
