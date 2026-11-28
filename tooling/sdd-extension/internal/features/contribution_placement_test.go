package features

import (
	"slices"
	"testing"

	capabilityproto "go.putnami.dev/protocol/capabilities"
)

// TestAContributionFindingIsStatedAtEveryManifestThatDeclaresIt pins where a
// contribution that does not resolve is reported: at each capability manifest
// that declares the identity, and at the evidence record that references it,
// so the finding counts for every project involved, not only the project of
// the first manifest read.
func TestAContributionFindingIsStatedAtEveryManifestThatDeclaresIt(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		result := Aggregate(dependencyFixture(t, reverse, true))
		var fields []string
		for _, finding := range result.Diagnostics {
			if finding.Code == capabilityproto.ErrorCodeConflictingContributionCopy {
				fields = append(fields, finding.Field)
			}
		}
		slices.Sort(fields)
		want := []string{
			"app-a/schema/capabilities.json#contribution",
			"app-a/schema/feature-evidence/a.json#evidence[0].subject.contribution",
			"app-b/schema/capabilities.json#contribution",
			"dep/schema/capabilities.json#contribution",
		}
		if !slices.Equal(fields, want) {
			t.Errorf("reverse=%v: conflicting-copy findings at %v, want %v", reverse, fields, want)
		}
	}
}
