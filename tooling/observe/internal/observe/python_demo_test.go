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

// observeDemoWithSBOMs observes a demo with the SBOMs of its sbom/ files,
// written outside the repository and named on the command line.
func observeDemoWithSBOMs(t *testing.T, name string) Observation {
	t.Helper()
	root, base := demoRepository(t, name)
	outside := t.TempDir()
	env := demos[name].env
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
	for _, f := range readDemo(t, name) {
		file, ok := strings.CutPrefix(f.name, "sbom/")
		if !ok {
			continue
		}
		writeDemoFile(t, outside, file, f.content)
		switch file {
		case "head.cdx.json":
			opts.SBOM = []string{filepath.Join(outside, file)}
		case "base.cdx.json":
			opts.BaseSBOM = []string{filepath.Join(outside, file)}
		}
	}
	return Run(context.Background(), opts)
}

func TestTheUVWorkspaceDemoIsExplained(t *testing.T) {
	spectest.Proves(t, "tooling/observe-python", "python-change-analysis", "the-uv-workspace-demo-is-explained")
	o := observeDemoWithSBOMs(t, "python")
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
		"api":    {ID: "api", Name: "api", Ecosystem: "python", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"shared"}},
		"worker": {ID: "worker", Name: "worker", Ecosystem: "python", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"shared"}},
		"shared": {ID: "shared", Name: "shared", Ecosystem: "python", Source: evidence.SourceWorkspace, Impact: evidence.ImpactChanged},
		"web":    {ID: "web", Name: "@demo/web", Ecosystem: "node", Source: evidence.SourceWorkspace, Impact: evidence.ImpactUnaffected},
	} {
		if got := byID[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("component %s = %+v, want %+v", id, got, want)
		}
	}
	if len(o.Analysis.Components) != 4 {
		t.Errorf("components %+v", o.Analysis.Components)
	}
	for _, c := range o.Analysis.Coverage {
		if c.Component != "web" && (c.TotalLines == 0 || c.CoveredLines != c.TotalLines) {
			t.Errorf("coverage of %s: %+v", c.Component, c)
		}
	}
	requireLines(t, summary(t, o),
		"  components  1 changed, 2 dependent, 1 unaffected\n",
		"    changed   shared [python, workspace]\n",
		"    dependent api [python, workspace], depends on shared\n",
		"    dependent worker [python, workspace], depends on shared\n",
		"    upgraded   fastapi 0.115.5 -> 0.115.6, in uv.lock\n",
	)
	validEvidence(t, o)
}

func TestAPyPIPackageURLMatchesAPythonLockfileChange(t *testing.T) {
	spectest.Proves(t, analysesFeature, "dependency-sbom-facts", "a-pypi-package-url-matches-a-python-lockfile-change")
	o := observeDemoWithSBOMs(t, "python")
	noWarnings(t, o)
	doc := validEvidence(t, o)
	fastapi := dependency(t, doc.DependencyChanges, "fastapi")
	if fastapi.Ecosystem != "pypi" || fastapi.Before == nil || fastapi.After == nil {
		t.Fatalf("fastapi %+v", fastapi)
	}
	if !reflect.DeepEqual(fastapi.Before.Licenses, []string{"MIT"}) || !reflect.DeepEqual(fastapi.After.Licenses, []string{"MIT"}) {
		t.Errorf("fastapi licenses %v -> %v", fastapi.Before.Licenses, fastapi.After.Licenses)
	}
	if len(doc.DependencyChanges) != 1 {
		t.Errorf("dependency changes %+v", doc.DependencyChanges)
	}
}
