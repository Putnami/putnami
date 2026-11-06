package observe

import (
	"reflect"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

func TestTheRailsDemoIsExplained(t *testing.T) {
	spectest.Proves(t, "tooling/observe-ruby", "ruby-change-analysis", "the-rails-demo-is-explained")
	o := observeDemo(t, "rails")
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
		".":              {ID: ".", Name: ".", Ecosystem: "ruby", Source: evidence.SourceWorkspace, Impact: evidence.ImpactChanged, DependsOn: []string{"engines/admin"}},
		"engines/admin":  {ID: "engines/admin", Name: "admin", Ecosystem: "ruby", Source: evidence.SourceManifest, Impact: evidence.ImpactUnaffected},
		"packs/checkout": {ID: "packs/checkout", Name: "packs/checkout", Ecosystem: "ruby", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"packs/pricing"}},
		"packs/pricing":  {ID: "packs/pricing", Name: "packs/pricing", Ecosystem: "ruby", Source: evidence.SourceWorkspace, Impact: evidence.ImpactChanged},
	} {
		if got := byID[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("component %s = %+v, want %+v", id, got, want)
		}
	}
	if len(o.Analysis.Components) != 4 {
		t.Errorf("components %+v", o.Analysis.Components)
	}
	for _, c := range o.Analysis.Coverage {
		if c.TotalLines == 0 || c.CoveredLines != c.TotalLines {
			t.Errorf("coverage of %s: %+v", c.Component, c)
		}
	}
	requireLines(t, summary(t, o),
		"  components  2 changed, 1 dependent, 1 unaffected\n",
		"    changed   . [ruby, workspace]\n",
		"    changed   packs/pricing [ruby, workspace]\n",
		"    dependent packs/checkout [ruby, workspace], depends on packs/pricing\n",
		"    upgraded   money 6.16.0 -> 6.19.0, in Gemfile.lock\n",
	)
	doc := validEvidence(t, o)
	if len(doc.DependencyChanges) != 1 {
		t.Errorf("dependency changes %+v", doc.DependencyChanges)
	}
}
