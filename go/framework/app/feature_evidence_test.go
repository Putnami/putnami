package app

import (
	"slices"
	"testing"

	protocaps "go.putnami.dev/protocol/capabilities"
)

func TestPublishedContributionIdentitiesIncludeDomainAccess(t *testing.T) {
	access := protocaps.ContributionIdentity{
		OwnerProject: "go.putnami.dev/example",
		Kind:         protocaps.ContributionKindDomainAccess,
		Subkind:      "reference",
		Key:          "example.protocol-contracts.v1",
	}
	lifecycle := protocaps.ContributionIdentity{
		OwnerProject: "go.putnami.dev/example",
		Kind:         protocaps.ContributionKindLifecycle,
		Subkind:      "starter",
		Key:          "http",
	}
	manifest := &protocaps.ManifestV2{
		LifecycleHooks: []protocaps.LifecycleHookV2{{Identity: lifecycle}},
		DomainAccess:   []protocaps.DomainAccessV2{{Identity: access}},
	}
	got := publishedContributionIdentities(manifest)
	for _, want := range []protocaps.ContributionIdentity{lifecycle, access} {
		if !slices.Contains(got, want) {
			t.Errorf("published identities %v lack %v: a proof could not name it", got, want)
		}
	}
}
