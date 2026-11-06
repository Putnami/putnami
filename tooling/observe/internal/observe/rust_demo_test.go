package observe

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

// observeRustDemo observes the rust demo with the SBOMs of its sbom/ files,
// written outside the repository and named on the command line.
func observeRustDemo(t *testing.T) Observation {
	t.Helper()
	root, base := demoRepository(t, "rust")
	outside := t.TempDir()
	env := demos["rust"].env
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve: func(p string) string {
			if filepath.IsAbs(p) {
				return p
			}
			return filepath.Join(root, p)
		},
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	for _, f := range readDemo(t, "rust") {
		name, ok := strings.CutPrefix(f.name, "sbom/")
		if !ok {
			continue
		}
		writeDemoFile(t, outside, name, f.content)
		switch name {
		case "head.cdx.json":
			opts.SBOM = []string{filepath.Join(outside, name)}
		case "base.cdx.json":
			opts.BaseSBOM = []string{filepath.Join(outside, name)}
		}
	}
	return Run(context.Background(), opts)
}

func TestTheCargoWorkspaceDemoIsExplained(t *testing.T) {
	spectest.Proves(t, "tooling/observe-rust", "rust-change-analysis", "the-cargo-workspace-demo-is-explained")
	o := observeRustDemo(t)
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
		"crates/api":     {ID: "crates/api", Name: "api", Ecosystem: "rust", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"crates/pricing"}},
		"crates/worker":  {ID: "crates/worker", Name: "worker", Ecosystem: "rust", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"crates/pricing"}},
		"crates/pricing": {ID: "crates/pricing", Name: "pricing", Ecosystem: "rust", Source: evidence.SourceWorkspace, Impact: evidence.ImpactChanged},
	} {
		if got := byID[id]; !reflect.DeepEqual(got, want) {
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
		"  components  1 changed, 2 dependent, 0 unaffected\n",
		"    changed   crates/pricing (pricing) [rust, workspace]\n",
		"    dependent crates/api (api) [rust, workspace], depends on crates/pricing\n",
		"    dependent crates/worker (worker) [rust, workspace], depends on crates/pricing\n",
		"    upgraded   itoa 1.0.14 -> 1.0.15, in Cargo.lock\n",
	)
	validEvidence(t, o)
}

func TestACargoPackageURLMatchesACargoLockChange(t *testing.T) {
	spectest.Proves(t, analysesFeature, "dependency-sbom-facts", "a-cargo-package-url-matches-a-cargo-lock-change")
	o := observeRustDemo(t)
	noWarnings(t, o)
	doc := validEvidence(t, o)
	itoa := dependency(t, doc.DependencyChanges, "itoa")
	if itoa.Ecosystem != "cargo" || itoa.Before == nil || itoa.After == nil {
		t.Fatalf("itoa %+v", itoa)
	}
	want := []string{"MIT OR Apache-2.0"}
	if !reflect.DeepEqual(itoa.Before.Licenses, want) || !reflect.DeepEqual(itoa.After.Licenses, want) {
		t.Errorf("itoa licenses %v -> %v", itoa.Before.Licenses, itoa.After.Licenses)
	}
	if len(doc.DependencyChanges) != 1 {
		t.Errorf("dependency changes %+v", doc.DependencyChanges)
	}
}
