package observe

import (
	"os"
	"path"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const dotnetFeature = "tooling/observe-dotnet"

func TestTheDotnetDemoIsExplained(t *testing.T) {
	spectest.Proves(t, dotnetFeature, "dotnet-change-analysis", "the-dotnet-demo-is-explained")
	o := observeDemo(t, "dotnet")
	noUnplacedReports(t, o)
	noWarnings(t, o)
	for id, want := range map[string]evidence.Component{
		"src/Api":             {ID: "src/Api", Name: "Api", Ecosystem: "dotnet", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"src/Pricing"}},
		"src/Pricing":         {ID: "src/Pricing", Name: "Pricing", Ecosystem: "dotnet", Source: evidence.SourceWorkspace, Impact: evidence.ImpactChanged},
		"src/Worker":          {ID: "src/Worker", Name: "Worker", Ecosystem: "dotnet", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"src/Pricing"}},
		"tests/Pricing.Tests": {ID: "tests/Pricing.Tests", Name: "Pricing.Tests", Ecosystem: "dotnet", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"src/Pricing"}},
		"tools/Migrate":       {ID: "tools/Migrate", Name: "Migrate", Ecosystem: "dotnet", Source: evidence.SourceManifest, Impact: evidence.ImpactChanged},
	} {
		if got := pantsComponents(o)[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("component %s = %+v, want %+v", id, got, want)
		}
	}
	if len(o.Analysis.Components) != 5 {
		t.Errorf("components %+v", o.Analysis.Components)
	}
	if len(o.Analysis.Tests) != 1 || o.Analysis.Tests[0].Component != "tests/Pricing.Tests" || o.Analysis.Tests[0].Passed != 4 {
		t.Errorf("tests %+v, want the four cases of the TRX report on tests/Pricing.Tests", o.Analysis.Tests)
	}
	requireLines(t, summary(t, o),
		"  components  2 changed, 3 dependent, 0 unaffected\n",
		"    changed   src/Pricing (Pricing) [dotnet, workspace]\n",
		"    dependent src/Api (Api) [dotnet, workspace], depends on src/Pricing\n",
		"    changed   tools/Migrate (Migrate) [dotnet, manifest]\n",
		"    dependent tests/Pricing.Tests (Pricing.Tests) [dotnet, workspace], depends on src/Pricing\n",
		"    upgraded   Humanizer.Core 2.13.14 -> 2.14.1, in tools/Migrate/packages.lock.json\n",
	)
	if doc := validEvidence(t, o); len(doc.DependencyChanges) != 1 {
		t.Errorf("dependency changes %+v, want only the lock file's: the repository has lock files", doc.DependencyChanges)
	}
}

func TestTheDotnetDemoWithoutLockFilesReadsItsCentralVersions(t *testing.T) {
	spectest.Proves(t, dotnetFeature, "dotnet-lockfiles", "central-package-versions-are-read-without-lock-files")
	root, base := demoRepositoryWithout(t, "dotnet", func(name string) bool {
		return path.Base(name) == "packages.lock.json"
	})
	env := demos["dotnet"].env
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	o := Run(t.Context(), opts)
	want := evidence.DependencyChange{Ecosystem: "nuget", Lockfile: "Directory.Packages.props", Name: "Humanizer.Core", Change: evidence.DependencyUpgraded, From: "2.13.14", To: "2.14.1"}
	if !reflect.DeepEqual(o.Analysis.DependencyChanges, []evidence.DependencyChange{want}) {
		t.Errorf("dependency changes %+v, want %+v", o.Analysis.DependencyChanges, want)
	}
	validEvidence(t, o)
}
