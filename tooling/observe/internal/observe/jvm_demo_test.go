package observe

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const jvmFeature = "tooling/observe-jvm"

func TestTheGradleDemoIsExplained(t *testing.T) {
	spectest.Proves(t, jvmFeature, "gradle-change-analysis", "the-gradle-demo-is-explained")
	o := observeDemo(t, "gradle")
	noUnplacedReports(t, o)
	noWarnings(t, o)
	for id, want := range map[string]evidence.Component{
		"api":     {ID: "api", Name: ":api", Ecosystem: "gradle", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"pricing"}},
		"app":     {ID: "app", Name: ":app", Ecosystem: "gradle", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"pricing"}},
		"pricing": {ID: "pricing", Name: ":pricing", Ecosystem: "gradle", Source: evidence.SourceWorkspace, Impact: evidence.ImpactChanged},
	} {
		if got := componentsByID(o)[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("component %s = %+v, want %+v", id, got, want)
		}
	}
	if len(o.Analysis.Components) != 3 {
		t.Errorf("components %+v", o.Analysis.Components)
	}
	requireLines(t, summary(t, o),
		"  components  1 changed, 2 dependent, 0 unaffected\n",
		"    changed   pricing (:pricing) [gradle, workspace]\n",
		"    dependent api (:api) [gradle, workspace], depends on pricing\n",
		"    dependent app (:app) [gradle, workspace], depends on pricing\n",
		"    upgraded   org.apache.commons:commons-lang3 3.17.0 -> 3.18.0, in gradle/libs.versions.toml\n",
		"    upgraded   org.apache.commons:commons-lang3 3.17.0 -> 3.18.0, in pricing/gradle.lockfile\n",
	)
	if doc := validEvidence(t, o); len(doc.DependencyChanges) != 2 {
		t.Errorf("dependency changes %+v", doc.DependencyChanges)
	}
}

func TestTheMavenDemoIsExplained(t *testing.T) {
	spectest.Proves(t, jvmFeature, "maven-change-analysis", "the-maven-demo-is-explained")
	root, base := demoRepository(t, "maven")
	env := map[string]string{"GITLAB_CI": "true", "CI_SERVER_HOST": "gitlab.com", "CI_PROJECT_ID": "88373402", "CI_PROJECT_PATH": "putnami/demo-observe-maven", "CI_PIPELINE_ID": "3015178870", "CI_JOB_ID": "17647016533", "CI_MERGE_REQUEST_IID": "1"}
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	o := Run(context.Background(), opts)
	noUnplacedReports(t, o)
	noWarnings(t, o)
	for id, want := range map[string]evidence.Component{
		"api":     {ID: "api", Name: "com.acme:api", Ecosystem: "maven", Source: evidence.SourceWorkspace, Impact: evidence.ImpactDependent, DependsOn: []string{"pricing"}},
		"pricing": {ID: "pricing", Name: "com.acme:pricing", Ecosystem: "maven", Source: evidence.SourceWorkspace, Impact: evidence.ImpactChanged},
	} {
		if got := componentsByID(o)[id]; !reflect.DeepEqual(got, want) {
			t.Errorf("component %s = %+v, want %+v", id, got, want)
		}
	}
	if len(o.Analysis.Components) != 2 {
		t.Errorf("components %+v", o.Analysis.Components)
	}
	tests := map[string]int{}
	for _, entry := range o.Analysis.Tests {
		tests[entry.Component] = entry.Passed
	}
	if !reflect.DeepEqual(tests, map[string]int{"api": 2, "pricing": 5}) {
		t.Errorf("tests %+v", o.Analysis.Tests)
	}
	covered := map[string]bool{}
	for _, c := range o.Analysis.Coverage {
		covered[c.Component] = c.TotalLines > 0
	}
	if !covered["api"] || !covered["pricing"] {
		t.Errorf("coverage %+v", o.Analysis.Coverage)
	}
	if lines := o.Analysis.ChangedLineCoverage; lines == nil || lines.TotalLines == 0 || lines.CoveredLines != lines.TotalLines {
		t.Errorf("changed-line coverage %+v", lines)
	}
	requireLines(t, summary(t, o),
		"  components  1 changed, 1 dependent, 0 unaffected\n",
		"    changed   pricing (com.acme:pricing) [maven, workspace]\n",
		"    dependent api (com.acme:api) [maven, workspace], depends on pricing\n",
	)
	if doc := validEvidence(t, o); len(doc.DependencyChanges) != 0 {
		t.Errorf("dependency changes %+v", doc.DependencyChanges)
	}
}
