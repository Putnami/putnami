package observe

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/detect/bazel"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

const bazelFeature = "tooling/observe-bazel"

// observeBazelDemo observes the bazel demo. The outputs of bazel test and
// bazel coverage lie outside the repository, as in Bazel's output base, behind
// the bazel-testlogs and bazel-out links of the root. export replaces the
// query export the CI wrote, or removes it when it is nil.
func observeBazelDemo(t *testing.T, export *string) Observation {
	t.Helper()
	root, base := demoRepository(t, "bazel")
	outputBase := t.TempDir()
	for _, f := range readDemo(t, "bazel") {
		if name, ok := strings.CutPrefix(f.name, "bazel/"); ok {
			writeDemoFile(t, outputBase, name, f.content)
		}
	}
	for link, target := range map[string]string{"bazel-testlogs": "testlogs", "bazel-out": "out"} {
		if err := os.Symlink(filepath.Join(outputBase, target), filepath.Join(root, link)); err != nil {
			t.Skipf("symbolic links: %v", err)
		}
	}
	graph := filepath.Join(root, bazel.GraphFile)
	if export == nil {
		if err := os.Remove(graph); err != nil {
			t.Fatal(err)
		}
	} else if *export != "" {
		writeDemoFile(t, root, bazel.GraphFile, *export)
	}
	env := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-bazel", "GITHUB_REPOSITORY_ID": "1108", "GITHUB_RUN_ID": "41335530117", "GITHUB_JOB": "test", "GITHUB_REF": "refs/heads/discount"}
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	return Run(context.Background(), opts)
}

func bazelComponents(o Observation) map[string]evidence.Component {
	out := map[string]evidence.Component{}
	for _, c := range o.Analysis.Components {
		out[c.ID] = c
	}
	return out
}

func TestTheBazelDemoIsExplained(t *testing.T) {
	spectest.Proves(t, bazelFeature, "bazel-change-analysis", "the-bazel-demo-is-explained")
	asWritten := ""
	o := observeBazelDemo(t, &asWritten)
	noWarnings(t, o)
	for _, d := range o.Diagnostics {
		if d.Code == analyze.CodeTestsUnattributed || d.Code == analyze.CodeCoverageUnmapped || d.Code == bazel.CodeGraphMissing {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
	}
	want := map[string]evidence.Component{
		"api":     {ID: "api", Name: "//api", Ecosystem: "bazel", Source: evidence.SourceOrchestrator, Impact: evidence.ImpactDependent, DependsOn: []string{"pricing"}},
		"pricing": {ID: "pricing", Name: "//pricing", Ecosystem: "bazel", Source: evidence.SourceOrchestrator, Impact: evidence.ImpactChanged},
		"web":     {ID: "web", Name: "//web", Ecosystem: "bazel", Source: evidence.SourceOrchestrator, Impact: evidence.ImpactUnaffected},
	}
	if got := bazelComponents(o); !reflect.DeepEqual(got, want) {
		t.Errorf("components %+v, want %+v", got, want)
	}
	tests := map[string]int{}
	for _, entry := range o.Analysis.Tests {
		tests[entry.Component] = entry.Passed
	}
	if !reflect.DeepEqual(tests, map[string]int{"api": 2, "pricing": 3}) {
		t.Errorf("tests %+v", o.Analysis.Tests)
	}
	coverage := map[string]evidence.ComponentCoverage{}
	for _, c := range o.Analysis.Coverage {
		coverage[c.Component] = c
	}
	if coverage["api"].TotalLines != 1 || coverage["pricing"].TotalLines != 4 || coverage["pricing"].CoveredLines != 4 || len(coverage) != 2 {
		t.Errorf("coverage %+v", o.Analysis.Coverage)
	}
	lines := o.Analysis.ChangedLineCoverage
	if lines == nil || len(lines.Files) != 1 || lines.Files[0].Path != "pricing/pricing.go" || lines.Files[0].TotalLines != 2 || lines.Files[0].CoveredLines != 2 {
		t.Errorf("changed lines %+v", lines)
	}
	requireLines(t, summary(t, o),
		"  components  1 changed, 1 dependent, 1 unaffected\n",
		"    changed   pricing (//pricing) [bazel, orchestrator]\n",
		"    dependent api (//api) [bazel, orchestrator], depends on pricing\n",
		"    junit     bazel-testlogs/api/api_test/test.xml\n",
		"    lcov      bazel-out/_coverage/_coverage_report.dat\n",
	)
	doc := validEvidence(t, o)
	if len(doc.Components) != 3 {
		t.Errorf("evidence components %+v", doc.Components)
	}
}

func TestWithoutAnExportTheBazelDemoListsItsPackages(t *testing.T) {
	spectest.Proves(t, bazelFeature, "bazel-without-export", "without-an-export-the-build-files-give-the-packages")
	o := observeBazelDemo(t, nil)
	noWarnings(t, o)
	got := bazelComponents(o)
	if len(got) != 3 {
		t.Fatalf("components %+v", got)
	}
	for id, c := range got {
		if len(c.DependsOn) != 0 || c.Name != "//"+id || c.Source != evidence.SourceManifest {
			t.Errorf("component %+v", c)
		}
	}
	if got["web"].Impact != evidence.ImpactUnaffected || got["api"].Impact != evidence.ImpactUnaffected || got["pricing"].Impact != evidence.ImpactChanged {
		t.Errorf("impacts %+v", got)
	}
	var info *diag.Diagnostic
	for i, d := range o.Diagnostics {
		if d.Code == bazel.CodeGraphMissing {
			info = &o.Diagnostics[i]
		}
	}
	if info == nil || info.Severity != diag.Info || !strings.Contains(info.Message, "bazel query 'deps(//...)' --output=streamed_jsonproto") {
		t.Fatalf("diagnostics %v", o.Diagnostics)
	}
	if len(o.Analysis.Tests) != 2 || hasCode(o, analyze.CodeTestsUnattributed) || hasCode(o, analyze.CodeCoverageUnmapped) {
		t.Errorf("tests %+v, diagnostics %v", o.Analysis.Tests, o.Diagnostics)
	}
	validEvidence(t, o)
}

func TestAMalformedExportIsAWarningNeverAFailedRun(t *testing.T) {
	spectest.Proves(t, bazelFeature, "bazel-without-export", "a-malformed-export-is-a-warning")
	malformed := `{"type":"RULE","rule":{"name":"//pricing:pricing",`
	o := observeBazelDemo(t, &malformed)
	var codes []string
	for _, d := range o.Diagnostics {
		if d.Severity != diag.Info && !strings.HasPrefix(d.Code, "observe.report_missing") {
			codes = append(codes, string(d.Severity)+" "+d.Code)
		}
	}
	if !reflect.DeepEqual(codes, []string{"warning " + bazel.CodeGraphUnreadable}) {
		t.Errorf("diagnostics %v", o.Diagnostics)
	}
	if got := bazelComponents(o); len(got) != 3 || len(got["api"].DependsOn) != 0 {
		t.Errorf("components %+v", got)
	}
	validEvidence(t, o)
}
