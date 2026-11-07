package observe

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
	"go.putnami.dev/tooling/observe/internal/components"
)

// observeFallbackDemo observes the codeowners demo, without the files skip
// names.
func observeFallbackDemo(t *testing.T, skip func(string) bool) Observation {
	t.Helper()
	root, base := demoRepositoryWithout(t, "codeowners", skip)
	opts := Options{
		Dir:      root,
		Env:      func(string) string { return "" },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	return Run(context.Background(), opts)
}

func requireFallbackSplit(t *testing.T, o Observation) {
	t.Helper()
	for _, d := range o.Diagnostics {
		switch d.Code {
		case components.CodeFallbackSplit:
			return
		case analyze.CodeTestsUnattributed, analyze.CodeCoverageUnmapped:
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
	}
	t.Errorf("no %s diagnostic in %+v", components.CodeFallbackSplit, o.Diagnostics)
}

func TestTheCodeownersDemoIsSplitByItsCodeowners(t *testing.T) {
	spectest.Proves(t, analysesFeature, "fallback-split", "codeowners-directories-are-components")
	spectest.Proves(t, analysesFeature, "fallback-split", "a-fallback-change-touches-only-its-own-components")
	o := observeFallbackDemo(t, func(string) bool { return false })
	requireFallbackSplit(t, o)
	want := []evidence.Component{
		{ID: "billing", Name: "billing", Ecosystem: "unknown", Source: evidence.SourceFallbackCodeowners, Impact: evidence.ImpactChanged},
		{ID: "catalog", Name: "catalog", Ecosystem: "unknown", Source: evidence.SourceFallbackCodeowners, Impact: evidence.ImpactUnaffected},
		{ID: "web", Name: "web", Ecosystem: "unknown", Source: evidence.SourceFallbackCodeowners, Impact: evidence.ImpactUnaffected},
	}
	if !reflect.DeepEqual(o.Analysis.Components, want) {
		t.Fatalf("components %+v, want %+v", o.Analysis.Components, want)
	}
	if len(o.Analysis.Tests) != 1 || o.Analysis.Tests[0].Component != "billing" || o.Analysis.Tests[0].Passed != 1 {
		t.Errorf("tests %+v", o.Analysis.Tests)
	}
	if len(o.Analysis.Coverage) != 1 || o.Analysis.Coverage[0].Component != "billing" || o.Analysis.Coverage[0].CoveredLines != 2 {
		t.Errorf("coverage %+v", o.Analysis.Coverage)
	}
	requireLines(t, summary(t, o),
		"  components  1 changed, 0 dependent, 2 unaffected\n",
		"    changed   billing [unknown, fallback-codeowners]\n",
	)
	doc := validEvidence(t, o)
	if len(doc.Components) != 3 {
		t.Errorf("evidence components %+v", doc.Components)
	}
}

func TestWithoutCodeownersTheDemoIsSplitByItsTopLevelDirectories(t *testing.T) {
	spectest.Proves(t, analysesFeature, "fallback-split", "top-level-source-directories-are-components")
	o := observeFallbackDemo(t, func(name string) bool { return name == "base/.github/CODEOWNERS" })
	requireFallbackSplit(t, o)
	var ids []string
	for _, c := range o.Analysis.Components {
		ids = append(ids, c.ID)
		if c.Source != evidence.SourceFallbackDirectories {
			t.Errorf("component %+v", c)
		}
	}
	if want := []string{"billing", "catalog", "web"}; !slices.Equal(ids, want) {
		t.Fatalf("components %v, want %v", ids, want)
	}
	requireLines(t, summary(t, o),
		"  components  1 changed, 0 dependent, 2 unaffected\n",
		"    changed   billing [unknown, fallback-directories]\n",
	)
	validEvidence(t, o)
}
