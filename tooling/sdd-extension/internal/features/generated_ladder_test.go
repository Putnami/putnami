package features

import (
	"testing"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspaceproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The full maturity ladder, proved end to end against a fixture rather than a
// real repository feature.
//
// Every current repository feature targets `modeled`, so promoting one to
// `coded` would be an acceptance counter dressed as a product decision. This
// fixture instead declares a feature that genuinely targets `coded`, runs the
// real producer resolver over an explicit mapping, and asserts the real
// assessment engine moves it from `modeled` to `coded` — and back again the
// moment that evidence stops being active.
//
// The evidence document is never hand-written: it is whatever
// features.BuildGeneratedEvidence emits, marshaled through the real
// marshaler. A fixture that hand-built the expected document could assert a
// shape no producer emits.

const ladderProject = "ladder-app"

func ladderBinding() string { return sourceBinding('c') }

func ladderManifest() featureproto.Manifest {
	return featureproto.Manifest{
		ProtocolVersion: featureproto.MinimumManifestProtocolVersion,
		Namespace:       "ladder",
		Features: []featureproto.Feature{{
			ID: "ladder/generated-proof", Type: featureproto.FeatureTypeFeature,
			Name:    "Generated proof",
			Outcome: "A build producer earns the coded stage from an exact technical contribution",
			Owner:   "ladder", Target: featureproto.MaturityCoded,
			Requirements: []featureproto.Requirement{{
				ID: "implementation", Stage: featureproto.MaturityCoded,
				EvidenceKinds: []featureproto.EvidenceKind{featureproto.EvidenceKindCapability},
			}},
		}},
	}
}

func ladderContribution() capabilityproto.ConfigDefinitionV2 {
	return capabilityproto.ConfigDefinitionV2{
		Identity: capabilityproto.ContributionIdentity{
			OwnerProject: ladderProject, Kind: capabilityproto.ContributionKindConfig, Key: "receiver",
		},
		Path: "receiver",
		Provenance: capabilityproto.ProvenanceV2{
			Project: ladderProject, SourceKind: capabilityproto.SourceKindFramework,
			Declaration: capabilityproto.DeclarationLocation{Root: capabilityproto.LocationRootProject, Path: "main.go"},
		},
	}
}

// producedLadderEvidence runs the real generated-evidence producer for the
// fixture's one explicit mapping and returns its canonical bytes.
func producedLadderEvidence(t *testing.T, binding string) []byte {
	t.Helper()
	contribution := ladderContribution()
	manifest := ladderManifest()
	document, findings := featureproto.BuildGeneratedEvidence(featureproto.GeneratedEvidenceInput{
		Issuer: featureproto.Issuer{Kind: featureproto.IssuerKindBuild, ID: "go.putnami.dev/app"},
		Source: featureproto.SourceSelector{
			Root: featureproto.LocationRootProject, OwnerProject: ladderProject, Binding: binding,
		},
		Authored:      &manifest,
		Contributions: []capabilityproto.ContributionIdentity{contribution.Identity},
		Mappings: []featureproto.GeneratedEvidenceMapping{{
			Feature: "ladder/generated-proof", Requirement: "implementation",
			Kind: capabilityproto.ContributionKindConfig, Key: "receiver",
			Provenance: featureproto.EvidenceProvenance{
				Root: featureproto.LocationRootProject, Path: "main.go", Symbol: "main.main",
			},
		}},
	})
	if document == nil || diag.HasErrors(findings) {
		t.Fatalf("producer refused the ladder mapping: %v", findings)
	}
	data, err := featureproto.MarshalEvidenceDocument(document)
	if err != nil {
		t.Fatalf("marshal produced evidence: %v", err)
	}
	return data
}

// ladderRequest assembles the fixture workspace. evidenceBinding is the binding
// the producer stamped into the record; treeBinding is what the tree reports
// now. They differ only when the test is exercising staleness.
func ladderRequest(t *testing.T, withEvidence bool, evidenceBinding, treeBinding string) Request {
	t.Helper()
	files := map[string][]byte{
		ladderProject + "/putnami.features.json": mustFeatureManifest(t, ladderManifest()),
		ladderProject + "/schema/capabilities.json": mustCapabilitiesV2(
			t, ladderProject, []capabilityproto.ConfigDefinitionV2{ladderContribution()},
		),
	}
	if withEvidence {
		files[ladderProject+"/schema/feature-evidence/go-framework.json"] = producedLadderEvidence(t, evidenceBinding)
	}
	ws := workspace.NewWorkspace("/workspace", &workspaceproto.Config{Name: "fixture"},
		[]*workspace.Project{{Name: ladderProject, Path: ladderProject, Version: "1.0.0"}})
	return Request{
		Workspace: ws,
		Reader:    &memoryReader{files: files, bindings: map[string]string{ladderProject: treeBinding}},
		Revision:  Revision{Kind: RevisionKindWorktree},
	}
}

func ladderAssessment(t *testing.T, request Request) (FeatureAssessment, Result) {
	t.Helper()
	result := Aggregate(request)
	if diag.HasErrors(result.Diagnostics) {
		t.Fatalf("Aggregate reported errors: %#v", result.Diagnostics)
	}
	if result.Snapshot == nil || len(result.Snapshot.Features) != 1 {
		t.Fatalf("Aggregate did not assess exactly one feature: %#v", result.Snapshot)
	}
	return result.Snapshot.Features[0], result
}

func TestGeneratedEvidenceAloneEarnsCoded(t *testing.T) {
	binding := ladderBinding()
	feature, result := ladderAssessment(t, ladderRequest(t, true, binding, binding))
	if feature.Current != featureproto.MaturityCoded {
		t.Fatalf("Current = %q, want coded from generated evidence alone", feature.Current)
	}
	requirement := feature.Requirements[0]
	if requirement.State != VerificationVerified || len(requirement.Evidence) != 1 {
		t.Fatalf("Requirement = %#v, want one active verifying record", requirement)
	}
	evidence := requirement.Evidence[0]
	if evidence.Issuer.Kind != featureproto.IssuerKindBuild {
		t.Fatalf("Issuer kind = %q, want build", evidence.Issuer.Kind)
	}
	if evidence.State != EvidenceActive || len(evidence.StaleReasons) != 0 {
		t.Fatalf("Evidence = %#v, want active", evidence)
	}
	if evidence.Contribution == nil || evidence.Contribution.Identity != ladderContribution().Identity {
		t.Fatalf("Evidence did not resolve to the mapped contribution: %#v", evidence.Contribution)
	}
	if got := len(result.Snapshot.Unclassified); got != 0 {
		t.Fatalf("Unclassified = %d, want the mapped contribution to be classified", got)
	}
}

func TestRemovingGeneratedEvidenceRegressesToModeled(t *testing.T) {
	binding := ladderBinding()
	feature, result := ladderAssessment(t, ladderRequest(t, false, binding, binding))
	if feature.Current != featureproto.MaturityModeled {
		t.Fatalf("Current = %q, want modeled without evidence", feature.Current)
	}
	if feature.Requirements[0].State != VerificationMissing {
		t.Fatalf("Requirement state = %q, want missing", feature.Requirements[0].State)
	}
	// The fact does not disappear with its proof: it becomes visibly unproven.
	if got := len(result.Snapshot.Unclassified); got != 1 {
		t.Fatalf("Unclassified = %d, want the contribution to be reported unclassified", got)
	}
}

func TestASourceChangeMakesGeneratedEvidenceStaleAndRegressesMaturity(t *testing.T) {
	// The record was produced against one source state; the tree has moved on.
	feature, _ := ladderAssessment(t, ladderRequest(t, true, ladderBinding(), sourceBinding('e')))
	if feature.Current != featureproto.MaturityModeled {
		t.Fatalf("Current = %q, want modeled once the binding no longer matches", feature.Current)
	}
	requirement := feature.Requirements[0]
	if requirement.State != VerificationStale {
		t.Fatalf("Requirement state = %q, want stale", requirement.State)
	}
	evidence := requirement.Evidence[0]
	if evidence.State != EvidenceStale || len(evidence.StaleReasons) == 0 {
		t.Fatalf("Evidence = %#v, want stale with a reason", evidence)
	}
}
