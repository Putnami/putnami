package observe

import (
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/detect/swift"
)

func TestTheSwiftDemoIsExplained(t *testing.T) {
	spectest.Proves(t, "tooling/observe-swift", "swift-change-analysis", "the-swift-demo-is-explained")
	o := observeDemo(t, "swift")
	noUnplacedReports(t, o)
	noWarnings(t, o)
	ws, sw := evidence.SourceWorkspace, swift.Ecosystem
	for _, want := range []evidence.Component{
		{ID: "App", Name: "Shop", Ecosystem: swift.XcodeEcosystem, Source: evidence.SourceManifest, Impact: evidence.ImpactDependent, DependsOn: []string{"Packages/Pricing"}},
		{ID: "Packages/Networking", Name: "Networking", Ecosystem: sw, Source: ws, Impact: evidence.ImpactDependent, DependsOn: []string{"Packages/Networking/Sources/Networking"}},
		{ID: "Packages/Networking/Sources/Networking", Name: "Networking", Ecosystem: sw, Source: ws, Impact: evidence.ImpactChanged},
		{ID: "Packages/Networking/Tests/NetworkingTests", Name: "NetworkingTests", Ecosystem: sw, Source: ws, Impact: evidence.ImpactChanged, DependsOn: []string{"Packages/Networking/Sources/Networking"}},
		{ID: "Packages/Pricing", Name: "Pricing", Ecosystem: sw, Source: ws, Impact: evidence.ImpactDependent, DependsOn: []string{"Packages/Pricing/Sources/Pricing"}},
		{ID: "Packages/Pricing/Sources/Pricing", Name: "Pricing", Ecosystem: sw, Source: ws, Impact: evidence.ImpactDependent, DependsOn: []string{"Packages/Networking"}},
		{ID: "Packages/Pricing/Tests/PricingTests", Name: "PricingTests", Ecosystem: sw, Source: ws, Impact: evidence.ImpactDependent, DependsOn: []string{"Packages/Pricing/Sources/Pricing"}},
	} {
		if got := pantsComponents(o)[want.ID]; !reflect.DeepEqual(got, want) {
			t.Errorf("component %s = %+v, want %+v", want.ID, got, want)
		}
	}
	if len(o.Analysis.Components) != 7 {
		t.Errorf("components %+v", o.Analysis.Components)
	}
	for _, c := range o.Analysis.Coverage {
		if c.TotalLines == 0 || c.CoveredLines != c.TotalLines {
			t.Errorf("coverage of %s: %+v", c.Component, c)
		}
	}
	requireLines(t, summary(t, o),
		"  components  2 changed, 5 dependent, 0 unaffected\n",
		"    dependent App (Shop) [xcode, manifest], depends on Packages/Pricing\n",
		"    upgraded   Alamofire 5.9.1 -> 5.10.2, in Podfile.lock\n",
		"    upgraded   swift-log 1.6.1 -> 1.6.2, in Shop.xcworkspace/xcshareddata/swiftpm/Package.resolved\n",
	)
	static := 0
	for _, d := range o.Diagnostics {
		if d.Code == swift.CodeStaticRead && d.Severity == diag.Info {
			static++
			// Pricing has its export; only Networking is read statically.
			if !strings.Contains(d.Message, "1 Package.swift is read without swiftpm.json: Packages/Networking/Package.swift.") {
				t.Errorf("static read: %s", d.Message)
			}
		}
	}
	if static != 1 {
		t.Errorf("diagnostics %+v, want one %s", o.Diagnostics, swift.CodeStaticRead)
	}
	doc := validEvidence(t, o)
	if len(doc.DependencyChanges) != 2 {
		t.Errorf("dependency changes %+v", doc.DependencyChanges)
	}
	for _, c := range doc.DependencyChanges {
		if c.Ecosystem != map[string]string{"Alamofire": "cocoapods", "swift-log": "swift"}[c.Name] {
			t.Errorf("dependency change %+v", c)
		}
	}
}
