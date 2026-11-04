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
			Requirements:    []featureproto.SpecRequirement{{ID: "export", Text: "Text."}},
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
