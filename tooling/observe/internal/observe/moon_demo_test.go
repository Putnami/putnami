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
	"go.putnami.dev/tooling/observe/detect/moon"
)

// moonDemo reads testdata/demos/moon.txtar: its base files, its change and
// the export its CI wrote.
func moonDemo(t *testing.T) (base, head map[string]string, graph string) {
	t.Helper()
	base, head = map[string]string{}, map[string]string{}
	for _, f := range readDemo(t, "moon") {
		switch prefix, rel, _ := strings.Cut(f.name, "/"); prefix {
		case "base":
			base[rel] = f.content
		case "head":
			head[rel] = f.content
		case "ci":
			graph = f.content
		}
	}
	return base, head, graph
}

// observeMoon commits the demo, then a change to pricing, and observes it
// with the extra working-tree files and the named export.
func observeMoon(t *testing.T, base map[string]string, extra map[string]string, moonGraphFile string) Observation {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "--quiet", "--initial-branch=main")
	for name, content := range base {
		writeDemoFile(t, root, name, content)
	}
	git(t, root, "add", "--all")
	git(t, root, "commit", "--quiet", "-m", "base")
	_, head, _ := moonDemo(t)
	for name, content := range head {
		writeDemoFile(t, root, name, content)
	}
	git(t, root, "commit", "--quiet", "-am", "head")
	for name, content := range extra {
		writeDemoFile(t, root, name, content)
	}
	env := map[string]string{"CI": "true"}
	opts := Options{
		Dir:       root,
		Env:       func(key string) string { return env[key] },
		ReadFile:  func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:   func(p string) string { return filepath.Join(root, p) },
		Producer:  Producer{Name: "@putnami/observe", Version: "1.0.0"},
		MoonGraph: moonGraphFile,
	}
	opts.Overrides.Base = git(t, root, "rev-parse", "HEAD~1")
	opts.Overrides.Head = "HEAD"
	return Run(context.Background(), opts)
}

func TestTheMoonWorkspaceIsExplainedWithAndWithoutAnExport(t *testing.T) {
	spectest.Proves(t, "tooling/observe-moon", "moon-change-analysis", "the-moon-workspace-is-explained-with-and-without-an-export")
	moonBase, _, moonGraph := moonDemo(t)
	named := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(named, []byte(moonGraph), 0o644); err != nil {
		t.Fatal(err)
	}
	exported := map[string]string{"moon-graph.json": moonGraph}
	for name, c := range map[string]struct {
		run    func() Observation
		source string
	}{
		"without an export":                 {func() Observation { return observeMoon(t, moonBase, nil, "") }, evidence.SourceWorkspace},
		"with moon-graph.json":              {func() Observation { return observeMoon(t, moonBase, exported, "") }, evidence.SourceOrchestrator},
		"with an export --moon-graph names": {func() Observation { return observeMoon(t, moonBase, nil, named) }, evidence.SourceOrchestrator},
	} {
		t.Run(name, func(t *testing.T) {
			o := c.run()
			noWarnings(t, o)
			want := []evidence.Component{
				{ID: "apps/api", Name: "api", Ecosystem: "moon", Source: c.source, Impact: evidence.ImpactDependent, DependsOn: []string{"packages/pricing"}},
				{ID: "apps/web", Name: "web", Ecosystem: "moon", Source: c.source, Impact: evidence.ImpactUnaffected},
				{ID: "packages/pricing", Name: "pricing", Ecosystem: "moon", Source: c.source, Impact: evidence.ImpactChanged},
			}
			if !reflect.DeepEqual(o.Analysis.Components, want) {
				t.Fatalf("components %+v, want %+v", o.Analysis.Components, want)
			}
			requireLines(t, summary(t, o),
				"  components  1 changed, 1 dependent, 1 unaffected\n",
				"    changed   packages/pricing (pricing) [moon, "+c.source+"]\n",
				"    dependent apps/api (api) [moon, "+c.source+"], depends on packages/pricing\n",
			)
			validEvidence(t, o)
		})
	}
}

func TestAMalformedWorkspaceYMLLeavesTheRepositoryToTheOtherDetectors(t *testing.T) {
	spectest.Proves(t, "tooling/observe-moon", "moon-projects", "a-malformed-workspace-yml-leaves-the-repository-to-the-other-detectors")
	moonBase, _, _ := moonDemo(t)
	base := map[string]string{}
	for name, content := range moonBase {
		base[name] = content
	}
	base[".moon/workspace.yml"] = "projects: ['apps/*'\n"
	o := observeMoon(t, base, nil, "")
	if !hasCode(o, moon.CodeWorkspaceUnreadable) {
		t.Fatalf("diagnostics %+v", o.Diagnostics)
	}
	var got []string
	for _, c := range o.Analysis.Components {
		got = append(got, c.ID+" "+c.Ecosystem)
	}
	// The Node workspace answers for the apps, and the Cargo detector for
	// pricing, a Cargo package.
	if want := []string{"apps/api node", "apps/web node", "packages/pricing rust"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("components %v, want %v", got, want)
	}
}
