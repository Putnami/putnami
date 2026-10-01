package features

import (
	"bytes"
	"strings"
	"testing"

	capabilityproto "go.putnami.dev/protocol/capabilities"
	featureproto "go.putnami.dev/protocol/features"
)

func TestCompareSnapshotsReportsEveryAcceptedCategoryDeterministically(t *testing.T) {
	base := &Snapshot{
		Compatibility: SnapshotCompatibility,
		Revision:      Revision{Kind: RevisionKindGit, Commit: "git:" + strings.Repeat("a", 40)},
		Features: []FeatureAssessment{
			{ID: "z/removed", Name: "Removed", Current: featureproto.MaturityModeled, Target: featureproto.MaturityModeled},
			{ID: "b/regressed", Name: "Regressed", Current: featureproto.MaturityCoded, Target: featureproto.MaturityCoded},
			{ID: "a/promoted", Name: "Promoted", Current: featureproto.MaturityModeled, Target: featureproto.MaturityCoded},
			{ID: "d/contradicted", Name: "Contradicted", Current: featureproto.MaturityModeled, Target: featureproto.MaturityCoded, Requirements: []RequirementAssessment{{ID: "coded", Stage: featureproto.MaturityCoded, State: VerificationMissing}}},
			{ID: "c/stale", Name: "Stale", Current: featureproto.MaturityCoded, Target: featureproto.MaturityCoded, Requirements: []RequirementAssessment{{ID: "coded", Stage: featureproto.MaturityCoded, State: VerificationVerified}}},
		},
		Unclassified: []ContributionAssessment{{Identity: capabilityproto.ContributionIdentity{OwnerProject: "old", Kind: capabilityproto.ContributionKindConfig, Key: "old"}}},
	}
	head := &Snapshot{
		Compatibility: SnapshotCompatibility,
		Revision:      Revision{Kind: RevisionKindGit, Commit: "git:" + strings.Repeat("b", 40)},
		Features: []FeatureAssessment{
			{ID: "e/added", Name: "Added", Current: featureproto.MaturityModeled, Target: featureproto.MaturityModeled},
			{ID: "d/contradicted", Name: "Contradicted", Current: featureproto.MaturityModeled, Target: featureproto.MaturityCoded, Requirements: []RequirementAssessment{{ID: "coded", Stage: featureproto.MaturityCoded, State: VerificationContradicted}}},
			{ID: "c/stale", Name: "Stale", Current: featureproto.MaturityModeled, Target: featureproto.MaturityCoded, Requirements: []RequirementAssessment{{ID: "coded", Stage: featureproto.MaturityCoded, State: VerificationStale}}},
			{ID: "b/regressed", Name: "Regressed", Current: featureproto.MaturityModeled, Target: featureproto.MaturityCoded},
			{ID: "a/promoted", Name: "Promoted", Current: featureproto.MaturityCoded, Target: featureproto.MaturityCoded},
		},
		Unclassified: []ContributionAssessment{
			{Identity: capabilityproto.ContributionIdentity{OwnerProject: "new", Kind: capabilityproto.ContributionKindSchema, Key: "new"}},
			{Identity: capabilityproto.ContributionIdentity{OwnerProject: "old", Kind: capabilityproto.ContributionKindConfig, Key: "old"}},
		},
	}

	delta := CompareSnapshots(base, head)
	if delta.Compatibility != SnapshotCompatibility || delta.Base != base.Revision || delta.Head != head.Revision {
		t.Fatalf("delta metadata = %+v", delta)
	}
	if len(delta.Added) != 1 || delta.Added[0].ID != "e/added" || len(delta.Removed) != 1 || delta.Removed[0].ID != "z/removed" {
		t.Fatalf("added/removed = %+v / %+v", delta.Added, delta.Removed)
	}
	if len(delta.Promoted) != 1 || delta.Promoted[0].FeatureID != "a/promoted" || len(delta.Regressed) != 2 {
		t.Fatalf("promoted/regressed = %+v / %+v", delta.Promoted, delta.Regressed)
	}
	if delta.Regressed[0].FeatureID != "b/regressed" || delta.Regressed[1].FeatureID != "c/stale" {
		t.Fatalf("regression order = %+v", delta.Regressed)
	}
	if len(delta.Stale) != 1 || delta.Stale[0].FeatureID != "c/stale" || delta.Stale[0].From != VerificationVerified {
		t.Fatalf("stale = %+v", delta.Stale)
	}
	if len(delta.Contradicted) != 1 || delta.Contradicted[0].FeatureID != "d/contradicted" || delta.Contradicted[0].From != VerificationMissing {
		t.Fatalf("contradicted = %+v", delta.Contradicted)
	}
	if len(delta.NewlyUnclassified) != 1 || delta.NewlyUnclassified[0].Identity.OwnerProject != "new" {
		t.Fatalf("newly unclassified = %+v", delta.NewlyUnclassified)
	}
	if base.Features[0].ID != "z/removed" || head.Features[0].ID != "e/added" {
		t.Fatal("comparison mutated caller snapshot order")
	}

	first, err := MarshalDelta(delta)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalDelta(delta)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("delta bytes are not deterministic: %v", err)
	}
	for _, want := range []string{`"compatibility": "provisional"`, `"promoted": [`, `"newlyUnclassified": [`} {
		if !bytes.Contains(first, []byte(want)) {
			t.Errorf("delta output missing %s:\n%s", want, first)
		}
	}
}

func TestCompareSnapshotsKeepsEmptyCollectionsNonNull(t *testing.T) {
	revision := Revision{Kind: RevisionKindGit, Commit: "git:" + strings.Repeat("c", 40)}
	delta := CompareSnapshots(&Snapshot{Revision: revision}, &Snapshot{Revision: revision})
	data, err := MarshalDelta(delta)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("null")) {
		t.Fatalf("empty delta contains null collections:\n%s", data)
	}
}
