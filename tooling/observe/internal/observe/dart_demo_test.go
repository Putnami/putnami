package observe

import (
	"reflect"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

func TestTheMelosDemoIsExplained(t *testing.T) {
	spectest.Proves(t, "tooling/observe-dart", "dart-change-analysis", "the-melos-demo-is-explained")
	o := observeDemoWithSBOMs(t, "dart")
	for _, d := range o.Diagnostics {
		if d.Code == analyze.CodeTestsUnattributed || d.Code == analyze.CodeCoverageUnmapped {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
	}
	byID := map[string]evidence.Component{}
	for _, c := range o.Analysis.Components {
		byID[c.ID] = c
	}
	for id, want := range map[string]evidence.Component{
		"apps/mobile":      {ID: "apps/mobile", Name: "mobile", Ecosystem: "dart", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"packages/pricing", "packages/ui"}},
		"packages/pricing": {ID: "packages/pricing", Name: "pricing", Ecosystem: "dart", Source: evidence.SourceWorkspace, Impact: evidence.ImpactChanged},
		"packages/ui":      {ID: "packages/ui", Name: "ui", Ecosystem: "dart", Source: evidence.SourceWorkspace, Impact: evidence.ImpactUnaffected},
	} {
		if got := byID[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("component %s = %+v, want %+v", id, got, want)
		}
	}
	if len(o.Analysis.Components) != 3 {
		t.Errorf("components %+v", o.Analysis.Components)
	}
	requireLines(t, summary(t, o),
		"  components  1 changed, 1 dependent, 1 unaffected\n",
		"    changed   packages/pricing (pricing) [dart, workspace]\n",
		"    dependent apps/mobile (mobile) [dart, workspace], depends on packages/pricing, packages/ui\n",
		"    upgraded   intl 0.19.0 -> 0.20.2, in pubspec.lock\n",
	)
	validEvidence(t, o)
}

func TestAPubPackageURLMatchesAPubspecLockChange(t *testing.T) {
	spectest.Proves(t, analysesFeature, "dependency-sbom-facts", "a-pub-package-url-matches-a-pubspec-lock-change")
	o := observeDemoWithSBOMs(t, "dart")
	noWarnings(t, o)
	doc := validEvidence(t, o)
	intl := dependency(t, doc.DependencyChanges, "intl")
	if intl.Ecosystem != "pub" || intl.Before == nil || intl.After == nil {
		t.Fatalf("intl %+v", intl)
	}
	if !reflect.DeepEqual(intl.Before.Licenses, []string{"BSD-3-Clause"}) || !reflect.DeepEqual(intl.After.Licenses, []string{"BSD-3-Clause"}) {
		t.Errorf("intl licenses %v -> %v", intl.Before.Licenses, intl.After.Licenses)
	}
	if len(doc.DependencyChanges) != 1 {
		t.Errorf("dependency changes %+v", doc.DependencyChanges)
	}
}
