package features_test

import (
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

const openSpecWithGaps = `# invoice-export Specification

## Purpose
Customers receive invoices, as [the design](./design.md) explains.

## Status
Draft

## Requirements
### Requirement: Issue an invoice
The system SHALL issue an invoice, per [ADR 0002](doc/adr/0002-invoices.md) and [the plan](plan.md).

#### Scenario: Issued
- **WHEN** an order closes
- **THEN** an invoice is issued
`

func gapFeature(format features.SpecFormat, path string, checks ...string) features.Feature {
	feature := features.Feature{ID: contractFeature, Spec: &features.SpecReference{Format: format, Path: path}}
	if len(checks) > 0 {
		feature.Requirements = []features.Requirement{{
			ID: "issue-an-invoice", Verification: &features.VerificationCriterion{Checks: checks},
		}}
	}
	return feature
}

func TestEachGapKindNamesItsPositionAndStep(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "native-gaps", "each-gap-kind-names-its-position-and-step")

	projection := readOpenSpec(t, openSpecWithGaps)
	if projection.Spec == nil {
		t.Fatalf("findings = %v", projection.Findings)
	}
	gaps := features.SpecNativeGaps(projection, gapFeature(features.SpecFormatOpenSpec, openSpecReference, "paid"))
	type got struct {
		kind  features.SpecGapKind
		field string
	}
	summary := make([]got, 0, len(gaps))
	for _, gap := range gaps {
		if gap.Step == "" || gap.Message == "" {
			t.Fatalf("gap without a message or a step: %+v", gap)
		}
		summary = append(summary, got{gap.Kind, gap.Field})
		if !strings.HasSuffix(gap.Step, "`putnami specs init "+contractFeature+" --from openspec:"+openSpecReference+"`") {
			t.Fatalf("a %s gap does not name the native rewrite: %q", gap.Kind, gap.Step)
		}
	}
	want := []got{
		{features.SpecGapContextLink, openSpecFile + "#L4"},
		{features.SpecGapDerivedSection, openSpecFile + "#L6"},
		{features.SpecGapContextLink, openSpecFile + "#L10"},
		{features.SpecGapScenarioMismatch, openSpecFile + "#L10"},
		{features.SpecGapScenarioMismatch, openSpecFile + "#L13"},
	}
	if !reflect.DeepEqual(summary, want) {
		t.Fatalf("gaps = %+v\nwant %+v", gaps, want)
	}
	if !strings.Contains(gaps[0].Message, `"./design.md"`) || !strings.Contains(gaps[2].Message, `"plan.md"`) {
		t.Fatalf("a context link gap names the wrong link: %+v", gaps)
	}

	specKit := readSpecKit(t, specKitMinimal)
	if specKit.Spec == nil {
		t.Fatalf("findings = %v", specKit.Findings)
	}
	gaps = features.SpecNativeGaps(specKit, gapFeature(features.SpecFormatSpecKit, specKitPath))
	if len(gaps) != 1 || gaps[0].Kind != features.SpecGapUnanchoredRequirement ||
		gaps[0].Field != specKitPath+"/spec.md#L5" || !strings.Contains(gaps[0].Step, "<!-- putnami:id fr-001 -->") || strings.Contains(gaps[0].Step, "specs init") {
		t.Fatalf("gaps = %+v, want the unanchored FR-001 at line 5 with its anchor", gaps)
	}
}

func TestANativeSpecHasNoGap(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "native-gaps", "a-native-spec-has-no-gap")
	projection := readOpenSpec(t, openSpecWithGaps)
	native := gapFeature(features.SpecFormatOpenSpec, openSpecReference, "paid")
	native.Spec = nil
	if gaps := features.SpecNativeGaps(projection, native); gaps != nil {
		t.Fatalf("a feature with no external reference has gaps: %+v", gaps)
	}
	if gaps := features.SpecNativeGaps(features.SpecProjection{}, gapFeature(features.SpecFormatOpenSpec, openSpecReference)); gaps != nil {
		t.Fatalf("a projection with no spec has gaps: %+v", gaps)
	}
}

func TestTheSameProjectionGivesTheSameGaps(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "features/feature-contracts", "native-gaps", "the-same-projection-gives-the-same-gaps")
	feature := gapFeature(features.SpecFormatOpenSpec, openSpecReference, "paid")
	first := features.SpecNativeGaps(readOpenSpec(t, openSpecWithGaps), feature)
	for range 5 {
		if again := features.SpecNativeGaps(readOpenSpec(t, openSpecWithGaps), feature); !reflect.DeepEqual(first, again) {
			t.Fatalf("gaps differ:\n%+v\n%+v", first, again)
		}
	}
}
