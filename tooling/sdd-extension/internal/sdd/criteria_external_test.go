package sdd

import (
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
)

func TestExternalSpecReachesTheGate(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "external-specs-share-one-discovery", "an-external-spec-reaches-the-gate")
	spec := featureengine.DiscoveredSpec{
		Path:   "app/openspec/invoice",
		Format: featureproto.SpecFormatOpenSpec,
		Spec: &featureproto.Spec{
			ProtocolVersion: featureproto.SpecProtocolVersion,
			Feature:         "billing/invoice-export",
			Outcomes:        []string{"O"},
			Requirements:    []featureproto.SpecRequirement{{ID: "export", Text: featureproto.SpecText{"Text."}}},
		},
	}
	repository := &specRepository{
		discovery: featureengine.SpecDiscoveryResult{Selected: []featureengine.DiscoveredSpec{spec}},
	}
	data, err := featureproto.MarshalSpecCriteriaProjection(criteriaProjectionFromRepository(repository))
	if err != nil {
		t.Fatal(err)
	}
	projection, findings := featureproto.ParseAndValidateSpecCriteriaProjection(data)
	if projection == nil || diag.HasErrors(findings) {
		t.Fatalf("the spec gate's strict reader refuses the projection: %v\n%s", findings, data)
	}
}

// TestCriteriaGroupsCarryTheAuthoredTarget pins that each criteria group names
// the target its feature's manifest entry declares, for a native and an
// external spec alike, and that a spec whose feature no manifest declares gives
// a group with no target.
func TestCriteriaGroupsCarryTheAuthoredTarget(t *testing.T) {
	spectest.Proves(t, "features/feature-contracts", "groups-name-the-feature-target",
		"the-sdd-extension-copies-the-authored-target-into-the-criteria-group")
	discovered := func(path, feature string, format featureproto.SpecFormat) featureengine.DiscoveredSpec {
		return featureengine.DiscoveredSpec{Path: path, Format: format, Spec: &featureproto.Spec{
			ProtocolVersion: featureproto.SpecProtocolVersion,
			Feature:         feature,
			Outcomes:        []string{"O"},
			Requirements:    []featureproto.SpecRequirement{{ID: "export", Text: featureproto.SpecText{"Text."}}},
		}}
	}
	authored := func(id string, target featureproto.MaturityStage) featureproto.Feature {
		return featureproto.Feature{ID: id, Type: featureproto.FeatureTypeFeature, Name: "N", Outcome: "O",
			Owner: "billing", Target: target}
	}
	repository := &specRepository{
		discovery: featureengine.SpecDiscoveryResult{Selected: []featureengine.DiscoveredSpec{
			discovered("app/specs/invoice-export.json", "billing/invoice-export", ""),
			discovered("app/openspec/refunds", "billing/refunds", featureproto.SpecFormatOpenSpec),
			discovered("app/specs/ledger.json", "billing/ledger", ""),
		}},
		manifests: []featureproto.ManifestSource{{Path: "app/putnami.features.json", Manifest: &featureproto.Manifest{
			ProtocolVersion: featureproto.ManifestProtocolVersion,
			Namespace:       "billing",
			Features: []featureproto.Feature{
				authored("billing/invoice-export", featureproto.MaturityWired),
				authored("billing/refunds", featureproto.MaturityLiveVerified),
			},
		}}},
	}
	data, err := featureproto.MarshalSpecCriteriaProjection(criteriaProjectionFromRepository(repository))
	if err != nil {
		t.Fatal(err)
	}
	projection, findings := featureproto.ParseAndValidateSpecCriteriaProjection(data)
	if projection == nil || diag.HasErrors(findings) {
		t.Fatalf("the spec gate's strict reader refuses the projection: %v\n%s", findings, data)
	}
	want := map[string]featureproto.MaturityStage{
		"billing/invoice-export": featureproto.MaturityWired,
		"billing/refunds":        featureproto.MaturityLiveVerified,
		"billing/ledger":         "",
	}
	if len(projection.Groups) != len(want) {
		t.Fatalf("groups = %+v, want one per spec", projection.Groups)
	}
	for _, group := range projection.Groups {
		if group.Target != want[group.Feature] {
			t.Errorf("%s: target = %q, want %q", group.Feature, group.Target, want[group.Feature])
		}
	}
}
