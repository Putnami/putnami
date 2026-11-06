package observe

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
	"go.putnami.dev/tooling/observe/internal/detect/pants"
)

const pantsFeature = "tooling/observe-pants"

func pantsComponents(o Observation) map[string]evidence.Component {
	byID := map[string]evidence.Component{}
	for _, c := range o.Analysis.Components {
		byID[c.ID] = c
	}
	return byID
}

func noUnplacedReports(t *testing.T, o Observation) {
	t.Helper()
	for _, d := range o.Diagnostics {
		if d.Code == analyze.CodeTestsUnattributed || d.Code == analyze.CodeCoverageUnmapped {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
	}
}

func TestThePantsDemoIsExplained(t *testing.T) {
	spectest.Proves(t, pantsFeature, "pants-change-analysis", "the-pants-demo-is-explained")
	o := observeDemo(t, "pants")
	noUnplacedReports(t, o)
	noWarnings(t, o)
	for id, want := range map[string]evidence.Component{
		"src/python/api":     {ID: "src/python/api", Ecosystem: "pants", Source: evidence.SourceOrchestrator, Impact: evidence.ImpactDependent, DependsOn: []string{"src/python/pricing"}},
		"src/python/pricing": {ID: "src/python/pricing", Ecosystem: "pants", Source: evidence.SourceOrchestrator, Impact: evidence.ImpactChanged},
		"src/python/web":     {ID: "src/python/web", Ecosystem: "pants", Source: evidence.SourceOrchestrator, Impact: evidence.ImpactUnaffected},
	} {
		if got := pantsComponents(o)[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("component %s = %+v, want %+v", id, got, want)
		}
	}
	if len(o.Analysis.Components) != 3 {
		t.Errorf("components %+v", o.Analysis.Components)
	}
	for _, c := range o.Analysis.Coverage {
		if c.TotalLines == 0 || c.CoveredLines != c.TotalLines {
			t.Errorf("coverage of %s: %+v", c.Component, c)
		}
	}
	requireLines(t, summary(t, o),
		"  components  1 changed, 1 dependent, 1 unaffected\n",
		"    changed   src/python/pricing [pants, orchestrator]\n",
		"    dependent src/python/api [pants, orchestrator], depends on src/python/pricing\n",
		"    upgraded   fastapi 0.115.5 -> 0.115.6, in 3rdparty/python/default.lock\n",
	)
	if doc := validEvidence(t, o); len(doc.DependencyChanges) != 1 {
		t.Errorf("dependency changes %+v", doc.DependencyChanges)
	}
}

func TestThePantsDemoWithoutAnExportListsItsDirectories(t *testing.T) {
	spectest.Proves(t, pantsFeature, "pants-change-analysis-without-an-export", "the-pants-demo-without-an-export-lists-its-directories")
	root, base := demoRepository(t, "pants")
	if err := os.Remove(filepath.Join(root, pants.DefaultGraphFile)); err != nil {
		t.Fatal(err)
	}
	env := demos["pants"].env
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	o := Run(t.Context(), opts)
	noUnplacedReports(t, o)
	for id, impact := range map[string]string{
		"src/python/api":     evidence.ImpactUnaffected,
		"src/python/pricing": evidence.ImpactChanged,
		"src/python/web":     evidence.ImpactUnaffected,
	} {
		want := evidence.Component{ID: id, Ecosystem: "pants", Source: evidence.SourceWorkspace, Impact: impact}
		if got := pantsComponents(o)[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("component %s = %+v, want %+v", id, got, want)
		}
	}
	if len(o.Analysis.Components) != 3 {
		t.Errorf("components %+v", o.Analysis.Components)
	}
	missing := 0
	for _, d := range o.Diagnostics {
		if d.Code == pants.CodeGraphMissing && d.Severity == diag.Info {
			missing++
			requireLines(t, d.Message, pants.ExportCommand)
		}
	}
	if missing != 1 {
		t.Errorf("diagnostics %+v, want one %s", o.Diagnostics, pants.CodeGraphMissing)
	}
	validEvidence(t, o)
}
