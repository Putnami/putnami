package main

import (
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/sdd/extension/internal/sdd"
)

// TestSpecsRenderTheSourceAndTheNativeGaps pins the human lines of specs list
// and specs inspect: an external spec names its format after its path, and
// inspect lists each gap with its position and step. A native spec prints no
// format and no gap.
func TestSpecsRenderTheSourceAndTheNativeGaps(t *testing.T) {
	var list strings.Builder
	printSpecCatalogHuman(&list, sdd.SpecCatalogReport{Specs: []sdd.SpecSummary{
		{Feature: "billing/invoicing", Path: "app/openspec/specs/invoice-export", Source: "openspec", Project: "billing-app", Outcomes: []string{}},
		{Feature: "billing/refunds", Path: "app/specs/refunds.json", Source: sdd.SpecSourceNative, Project: "billing-app", Outcomes: []string{}},
	}})
	for _, line := range []string{
		"    source: app/openspec/specs/invoice-export (billing-app) · openspec\n",
		"    source: app/specs/refunds.json (billing-app)\n",
	} {
		if !strings.Contains(list.String(), line) {
			t.Fatalf("specs list lacks %q:\n%s", line, list.String())
		}
	}

	var inspect strings.Builder
	printSpecContextHuman(&inspect, sdd.SpecContextReport{
		Feature: "billing/invoicing",
		Source:  sdd.SpecSourceRef{Path: "app/openspec/specs/invoice-export", Project: "billing-app", Format: featureproto.SpecFormatOpenSpec},
		Spec:    &featureproto.Spec{Outcomes: []string{"Customers receive invoices."}},
		NativeGaps: []featureproto.SpecGap{{
			Kind: featureproto.SpecGapDerivedSection, Field: "app/openspec/specs/invoice-export/spec.md#L6",
			Message: "section \"Status\" restates a fact Putnami derives: remove it", Step: "delete the section",
		}},
	})
	want := "  Spec: app/openspec/specs/invoice-export (billing-app) · openspec\n" +
		"  Outcomes:\n    Customers receive invoices.\n" +
		"  Requirements:\n    none authored yet\n" +
		"  Native gaps (1), information only:\n" +
		"    [derived_section] app/openspec/specs/invoice-export/spec.md#L6\n" +
		"      section \"Status\" restates a fact Putnami derives: remove it\n" +
		"      step: delete the section\n"
	if !strings.Contains(inspect.String(), want) {
		t.Fatalf("specs inspect =\n%s\nwant it to hold\n%s", inspect.String(), want)
	}

	var native strings.Builder
	printSpecContextHuman(&native, sdd.SpecContextReport{
		Feature: "billing/refunds",
		Source:  sdd.SpecSourceRef{Path: "app/specs/refunds.json", Project: "billing-app"},
		Spec:    &featureproto.Spec{Outcomes: []string{"Refunds."}},
	})
	if !strings.Contains(native.String(), "  Spec: app/specs/refunds.json (billing-app)\n") || strings.Contains(native.String(), "Native gaps") {
		t.Fatalf("a native spec's inspect output changed:\n%s", native.String())
	}
}
