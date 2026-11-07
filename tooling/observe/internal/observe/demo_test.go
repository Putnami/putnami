package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/observe/internal/analyze"
)

const analysesFeature = "tooling/observe-analyses"

// demoFile is one file of a demo fixture: base/ files are the target
// branch, head/ files the ones the change rewrites, ci/ files the reports the
// CI wrote.
type demoFile struct {
	name, content string
}

// readDemo reads testdata/demos/<name>.txtar: a comment line, then files,
// each after a "-- <path> --" line. A delete/<path> file names a base file the
// change deletes.
func readDemo(t *testing.T, name string) []demoFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "demos", name+".txtar"))
	if err != nil {
		t.Fatal(err)
	}
	var files []demoFile
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if header, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "-- "); ok && strings.HasSuffix(header, " --") {
			files = append(files, demoFile{name: strings.TrimSuffix(header, " --")})
			continue
		}
		if len(files) > 0 {
			files[len(files)-1].content += line
		}
	}
	return files
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := gittest.Command(dir, args...)
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeDemoFile(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// demoRepository builds the demo as a repository: the base commit, the
// change on top of it, then the reports in the working tree. It returns the
// root and the base commit.
func demoRepository(t *testing.T, name string) (string, string) {
	t.Helper()
	return demoRepositoryWithout(t, name, func(string) bool { return false })
}

func TestDemoRepositoryIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		root, _ := demoRepository(t, "go")
		return root
	})
}

type demoExpectation struct {
	env        map[string]string
	changed    []string
	dependent  []string
	tests      map[string]int
	covered    []string
	lineFile   string
	dependency evidence.DependencyChange
}

var demos = map[string]demoExpectation{
	"turbo": {
		env:        map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-turbo", "GITHUB_REPOSITORY_ID": "1104", "GITHUB_RUN_ID": "41335501311", "GITHUB_JOB": "test", "GITHUB_REF": "refs/pull/1/merge"},
		changed:    []string{"packages/pricing"},
		dependent:  []string{"apps/api"},
		tests:      map[string]int{"packages/pricing": 6, "apps/api": 3},
		covered:    []string{"apps/api", "packages/pricing"},
		lineFile:   "packages/pricing/src/index.ts",
		dependency: evidence.DependencyChange{Ecosystem: "npm", Lockfile: "pnpm-lock.yaml", Name: "zod", Change: evidence.DependencyUpgraded, From: "3.23.8", To: "3.24.1"},
	},
	"go": {
		env:     map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-go", "GITHUB_REPOSITORY_ID": "1105", "GITHUB_RUN_ID": "41335469595", "GITHUB_JOB": "test", "GITHUB_REF": "refs/pull/1/merge"},
		changed: []string{"api", "pricing"},
		tests:   map[string]int{"pricing": 5, "api": 3},
		covered: []string{"api", "pricing"},
		// The bump changes api/go.mod too: Go records the indirect
		// dependency in every module that builds pricing.
		lineFile:   "pricing/pricing.go",
		dependency: evidence.DependencyChange{Ecosystem: "go", Lockfile: "pricing/go.mod", Name: "golang.org/x/text", Change: evidence.DependencyUpgraded, From: "v0.20.0", To: "v0.21.0", Component: "pricing"},
	},
	"nx": {
		env:        map[string]string{"GITLAB_CI": "true", "CI_SERVER_HOST": "gitlab.com", "CI_PROJECT_ID": "88373083", "CI_PROJECT_PATH": "putnami/demo-observe-nx", "CI_PIPELINE_ID": "3015178231", "CI_JOB_ID": "17647015420", "CI_MERGE_REQUEST_IID": "1"},
		changed:    []string{"packages/pricing"},
		dependent:  []string{"apps/api"},
		tests:      map[string]int{"packages/pricing": 5, "apps/api": 2},
		covered:    []string{"apps/api", "packages/pricing"},
		lineFile:   "packages/pricing/src/index.ts",
		dependency: evidence.DependencyChange{Ecosystem: "npm", Lockfile: "package-lock.json", Name: "decimal.js", Change: evidence.DependencyUpgraded, From: "10.4.3", To: "10.5.0"},
	},
	"python": {
		env:       map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-python", "GITHUB_REPOSITORY_ID": "1107", "GITHUB_RUN_ID": "41335512001", "GITHUB_JOB": "test", "GITHUB_REF": "refs/pull/1/merge"},
		changed:   []string{"shared"},
		dependent: []string{"api", "worker"},
		tests:     map[string]int{"api": 2, "shared": 4, "worker": 1},
		covered:   []string{"api", "shared", "worker"},
		// uv.lock lies in the virtual workspace root, which is no component.
		lineFile:   "shared/src/shared/money.py",
		dependency: evidence.DependencyChange{Ecosystem: "pypi", Lockfile: "uv.lock", Name: "fastapi", Change: evidence.DependencyUpgraded, From: "0.115.5", To: "0.115.6"},
	},
	"pants": {
		env:       map[string]string{"GITLAB_CI": "true", "CI_SERVER_HOST": "gitlab.com", "CI_PROJECT_ID": "88373291", "CI_PROJECT_PATH": "putnami/demo-observe-pants", "CI_PIPELINE_ID": "3015178544", "CI_JOB_ID": "17647016102", "CI_MERGE_REQUEST_IID": "1"},
		changed:   []string{"src/python/pricing"},
		dependent: []string{"src/python/api"},
		tests:     map[string]int{"src/python/api": 2, "src/python/pricing": 4, "src/python/web": 1},
		covered:   []string{"src/python/api", "src/python/pricing", "src/python/web"},
		// The resolve lockfile lies in 3rdparty/python, which holds only
		// third-party targets and is no component.
		lineFile:   "src/python/pricing/pricing.py",
		dependency: evidence.DependencyChange{Ecosystem: "pypi", Lockfile: "3rdparty/python/default.lock", Name: "fastapi", Change: evidence.DependencyUpgraded, From: "0.115.5", To: "0.115.6"},
	},
	"gradle": {
		env:       map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-gradle", "GITHUB_REPOSITORY_ID": "1108", "GITHUB_RUN_ID": "41335530117", "GITHUB_JOB": "test", "GITHUB_REF": "refs/pull/1/merge"},
		changed:   []string{"pricing"},
		dependent: []string{"api", "app"},
		tests:     map[string]int{"api": 2, "app": 1, "pricing": 5},
		covered:   []string{"api", "app", "pricing"},
		lineFile:  "pricing/src/main/kotlin/com/acme/pricing/Pricing.kt",
		// The catalog lies in the gradle directory of the root project,
		// which holds no source and is no component.
		dependency: evidence.DependencyChange{Ecosystem: "maven", Lockfile: "pricing/gradle.lockfile", Name: "org.apache.commons:commons-lang3", Change: evidence.DependencyUpgraded, From: "3.17.0", To: "3.18.0", Component: "pricing"},
	},
	"swift": {
		env:       map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-swift", "GITHUB_REPOSITORY_ID": "1109", "GITHUB_RUN_ID": "41335533107", "GITHUB_JOB": "test", "GITHUB_REF": "refs/pull/1/merge"},
		changed:   []string{"Packages/Networking/Sources/Networking", "Packages/Networking/Tests/NetworkingTests"},
		dependent: []string{"App", "Packages/Networking", "Packages/Pricing", "Packages/Pricing/Sources/Pricing", "Packages/Pricing/Tests/PricingTests"},
		tests:     map[string]int{"App": 2, "Packages/Pricing/Tests/PricingTests": 3, "Packages/Networking/Tests/NetworkingTests": 2},
		covered:   []string{"App", "Packages/Networking/Sources/Networking", "Packages/Pricing/Sources/Pricing"},
		// The CocoaPods workspace at the root holds the lockfiles, and the
		// root is no component.
		lineFile:   "Packages/Networking/Sources/Networking/Client.swift",
		dependency: evidence.DependencyChange{Ecosystem: "swift", Lockfile: "Shop.xcworkspace/xcshareddata/swiftpm/Package.resolved", Name: "swift-log", Change: evidence.DependencyUpgraded, From: "1.6.1", To: "1.6.2"},
	},
	"dart": {
		env:       map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-dart", "GITHUB_REPOSITORY_ID": "1109", "GITHUB_RUN_ID": "41335530417", "GITHUB_JOB": "test", "GITHUB_REF": "refs/pull/1/merge"},
		changed:   []string{"packages/pricing"},
		dependent: []string{"apps/mobile"},
		tests:     map[string]int{"apps/mobile": 1, "packages/pricing": 4, "packages/ui": 1},
		covered:   []string{"apps/mobile", "packages/pricing", "packages/ui"},
		// pubspec.lock lies in the pub workspace root, which is no component.
		lineFile:   "packages/pricing/lib/src/price.dart",
		dependency: evidence.DependencyChange{Ecosystem: "pub", Lockfile: "pubspec.lock", Name: "intl", Change: evidence.DependencyUpgraded, From: "0.19.0", To: "0.20.2"},
	},

	"dotnet": {
		env:       map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-dotnet", "GITHUB_REPOSITORY_ID": "1109", "GITHUB_RUN_ID": "41335527412", "GITHUB_JOB": "test", "GITHUB_REF": "refs/pull/1/merge"},
		changed:   []string{"src/Pricing", "tools/Migrate"},
		dependent: []string{"src/Api", "src/Worker", "tests/Pricing.Tests"},
		tests:     map[string]int{"tests/Pricing.Tests": 4},
		covered:   []string{"src/Pricing"},
		// The bump changes the lock file of tools/Migrate, the only project
		// that uses the package.
		lineFile:   "src/Pricing/PriceCalculator.cs",
		dependency: evidence.DependencyChange{Ecosystem: "nuget", Lockfile: "tools/Migrate/packages.lock.json", Name: "Humanizer.Core", Change: evidence.DependencyUpgraded, From: "2.13.14", To: "2.14.1", Component: "tools/Migrate"},
	},
	"rails": {
		env: map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-rails", "GITHUB_REPOSITORY_ID": "1108", "GITHUB_RUN_ID": "41335527004", "GITHUB_JOB": "test", "GITHUB_REF": "refs/pull/1/merge"},
		// Gemfile.lock lies in the application at the root, which the
		// change therefore also touches.
		changed:    []string{".", "packs/pricing"},
		dependent:  []string{"packs/checkout"},
		tests:      map[string]int{".": 1, "engines/admin": 1, "packs/checkout": 2, "packs/pricing": 4},
		covered:    []string{".", "engines/admin", "packs/checkout", "packs/pricing"},
		lineFile:   "packs/pricing/app/models/pricing/calculator.rb",
		dependency: evidence.DependencyChange{Ecosystem: "gem", Lockfile: "Gemfile.lock", Name: "money", Change: evidence.DependencyUpgraded, From: "6.16.0", To: "6.19.0", Component: "."},
	},
	"rust": {
		env:       map[string]string{"GITLAB_CI": "true", "CI_SERVER_HOST": "gitlab.com", "CI_PROJECT_ID": "88373402", "CI_PROJECT_PATH": "putnami/demo-observe-rust", "CI_PIPELINE_ID": "3015178713", "CI_JOB_ID": "17647016530", "CI_MERGE_REQUEST_IID": "1"},
		changed:   []string{"crates/pricing"},
		dependent: []string{"crates/api", "crates/worker"},
		tests:     map[string]int{"crates/api": 2, "crates/pricing": 4, "crates/worker": 1},
		covered:   []string{"crates/api", "crates/pricing", "crates/worker"},
		// Cargo.lock lies in the virtual workspace root, which is no
		// component.
		lineFile:   "crates/pricing/src/lib.rs",
		dependency: evidence.DependencyChange{Ecosystem: "cargo", Lockfile: "Cargo.lock", Name: "itoa", Change: evidence.DependencyUpgraded, From: "1.0.14", To: "1.0.15"},
	},
	"symfony": {
		env: map[string]string{"GITLAB_CI": "true", "CI_SERVER_HOST": "gitlab.com", "CI_PROJECT_ID": "88373184", "CI_PROJECT_PATH": "putnami/demo-observe-symfony", "CI_PIPELINE_ID": "3015177948", "CI_JOB_ID": "17647015000", "CI_MERGE_REQUEST_IID": "1"},
		// composer.lock lies in the application at the root, which the
		// change therefore also touches.
		changed:    []string{".", "packages/pricing"},
		tests:      map[string]int{"packages/pricing": 6, ".": 3},
		covered:    []string{".", "packages/pricing"},
		lineFile:   "packages/pricing/src/Calculator.php",
		dependency: evidence.DependencyChange{Ecosystem: "composer", Lockfile: "composer.lock", Name: "brick/math", Change: evidence.DependencyUpgraded, From: "0.13.0", To: "0.13.1", Component: "."},
	},
}

func observeDemo(t *testing.T, name string) Observation {
	t.Helper()
	root, base := demoRepository(t, name)
	env := demos[name].env
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

func impacts(o Observation, impact string) []string {
	var out []string
	for _, c := range o.Analysis.Components {
		if c.Impact == impact {
			out = append(out, c.ID)
		}
	}
	return out
}

func TestEachDemoChangeIsExplained(t *testing.T) {
	spectest.Proves(t, analysesFeature, "summary", "the-demo-evidence-documents-validate")
	spectest.Proves(t, analysesFeature, "components", "changed-files-give-changed-and-dependent-components")
	for name, want := range demos {
		t.Run(name, func(t *testing.T) {
			o := observeDemo(t, name)
			a := o.Analysis
			if got := strings.Join(impacts(o, evidence.ImpactChanged), ","); got != strings.Join(want.changed, ",") {
				t.Errorf("changed %s, want %v", got, want.changed)
			}
			if got := strings.Join(impacts(o, evidence.ImpactDependent), ","); got != strings.Join(want.dependent, ",") {
				t.Errorf("dependent %s, want %v", got, want.dependent)
			}
			for component, passed := range want.tests {
				found := false
				for _, entry := range a.Tests {
					if entry.Component == component {
						found = entry.Passed == passed && entry.Failed == 0
					}
				}
				if !found {
					t.Errorf("tests of %s: %+v, want %d passed", component, a.Tests, passed)
				}
			}
			var covered []string
			for _, c := range a.Coverage {
				if c.TotalLines > 0 {
					covered = append(covered, c.Component)
				}
			}
			if strings.Join(covered, ",") != strings.Join(want.covered, ",") {
				t.Errorf("coverage %+v, want %v", a.Coverage, want.covered)
			}
			if a.ChangedLineCoverage == nil || a.ChangedLineCoverage.TotalLines == 0 {
				t.Fatalf("changed-line coverage %+v", a.ChangedLineCoverage)
			}
			lineFound := false
			for _, f := range a.ChangedLineCoverage.Files {
				lineFound = lineFound || (f.Path == want.lineFile && f.TotalLines > 0 && f.CoveredLines == f.TotalLines)
			}
			if !lineFound {
				t.Errorf("changed lines %+v, want %s covered", a.ChangedLineCoverage.Files, want.lineFile)
			}
			depFound := false
			for _, d := range a.DependencyChanges {
				depFound = depFound || d == want.dependency
			}
			if !depFound {
				t.Errorf("dependency changes %+v, want %+v", a.DependencyChanges, want.dependency)
			}
			for _, d := range o.Diagnostics {
				if d.Severity != diag.Info && !strings.HasPrefix(d.Code, "observe.report_") {
					t.Errorf("diagnostic %s: %s", d.Code, d.Message)
				}
			}
			doc := Evidence(o, now)
			if problems := evidence.ValidateDocument(doc); diag.HasErrors(problems) {
				t.Fatalf("the evidence document does not validate: %v", problems)
			}
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if _, problems := evidence.ParseAndValidate(data); diag.HasErrors(problems) {
				t.Fatalf("the written evidence document does not parse back: %v", problems)
			}
			if doc.Subject.ChangeRequest == nil || doc.Subject.ChangeRequest.Number != 1 || doc.Subject.ProjectID != want.env[projectIDVariable(want.env)] {
				t.Errorf("subject %+v", doc.Subject)
			}
		})
	}
}

func projectIDVariable(env map[string]string) string {
	if env["GITLAB_CI"] == "true" {
		return "CI_PROJECT_ID"
	}
	return "GITHUB_REPOSITORY_ID"
}

func TestTheSummaryShowsTheAnalyses(t *testing.T) {
	spectest.Proves(t, analysesFeature, "summary", "the-summary-shows-the-analyses")
	o := observeDemo(t, "turbo")
	var text bytes.Buffer
	if err := WriteDetails(&text, o); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"  components  1 changed, 1 dependent, 0 unaffected\n",
		"    changed   packages/pricing (@demo/pricing) [node, workspace]\n",
		"    dependent apps/api (@demo/api) [node, workspace], depends on packages/pricing\n",
		"    packages/pricing  6 passed, 0 failed, 0 skipped, coverage 19 of 19 lines (100.0%)\n",
		"  changed lines  5 of 5 lines (100.0%) covered\n",
		"    upgraded   zod 3.23.8 -> 3.24.1, in pnpm-lock.yaml\n",
	} {
		if !strings.Contains(text.String(), line) {
			t.Errorf("the summary lacks %q:\n%s", line, text.String())
		}
	}
	var out bytes.Buffer
	if err := WriteJSON(&out, o); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Analysis struct {
			Components          []evidence.Component          `json:"components"`
			ChangedLineCoverage *evidence.ChangedLineCoverage `json:"changedLineCoverage"`
			DependencyChanges   []evidence.DependencyChange   `json:"dependencyChanges"`
		} `json:"analysis"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Analysis.Components) != 2 || decoded.Analysis.ChangedLineCoverage == nil || len(decoded.Analysis.DependencyChanges) != 1 {
		t.Fatalf("the JSON output lacks the analysis: %s", out.String())
	}
}

func TestAChangedComponentNamesTheChangedComponentsItDependsOn(t *testing.T) {
	spectest.Proves(t, analysesFeature, "summary", "a-changed-component-names-the-changed-components-it-depends-on")
	for name, want := range map[string]struct {
		component, dependency string
		lines                 []string
	}{
		"go": {"api", "pricing", []string{
			"  components  2 changed, 0 dependent, 0 unaffected\n",
			"    changed   api (github.com/Putnami/demo-observe-go/api) [go, workspace], depends on pricing\n",
			"    changed   pricing (github.com/Putnami/demo-observe-go/pricing) [go, workspace]\n",
		}},
		"symfony": {".", "packages/pricing", []string{
			"  components  2 changed, 0 dependent, 0 unaffected\n",
			"    changed   . [composer, manifest], depends on packages/pricing\n",
			"    changed   packages/pricing (demo/pricing) [composer, manifest]\n",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			o := observeDemo(t, name)
			var text bytes.Buffer
			if err := WriteDetails(&text, o); err != nil {
				t.Fatal(err)
			}
			for _, line := range want.lines {
				if !strings.Contains(text.String(), line) {
					t.Errorf("the summary lacks %q:\n%s", line, text.String())
				}
			}
			// The evidence document keeps the edge of a changed component.
			found := false
			for _, c := range Evidence(o, now).Components {
				found = found || (c.ID == want.component && c.Impact == evidence.ImpactChanged && slices.Contains(c.DependsOn, want.dependency))
			}
			if !found {
				t.Errorf("the evidence document lacks the edge %s -> %s", want.component, want.dependency)
			}
		})
	}
}

func TestAChangedComponentNamesAChangedComponentItReachesThroughAnother(t *testing.T) {
	spectest.Proves(t, analysesFeature, "summary", "a-changed-component-names-the-changed-components-it-depends-on")
	a := analyze.Result{Components: []evidence.Component{
		{ID: "api", Ecosystem: "go", Source: "workspace", Impact: evidence.ImpactChanged, DependsOn: []string{"lib"}},
		{ID: "cli", Ecosystem: "go", Source: "workspace", Impact: evidence.ImpactChanged, DependsOn: []string{"tools"}},
		{ID: "core", Ecosystem: "go", Source: "workspace", Impact: evidence.ImpactChanged},
		{ID: "lib", Ecosystem: "go", Source: "workspace", Impact: evidence.ImpactDependent, DependsOn: []string{"api", "pricing"}},
		{ID: "pricing", Ecosystem: "go", Source: "workspace", Impact: evidence.ImpactChanged, DependsOn: []string{"core"}},
		{ID: "tools", Ecosystem: "go", Source: "workspace", Impact: evidence.ImpactUnaffected},
	}}
	var b strings.Builder
	writeAnalysis(&b, a)
	for _, line := range []string{
		"  components  4 changed, 1 dependent, 1 unaffected\n",
		"    changed   api [go, workspace], depends on core, pricing\n",
		"    changed   cli [go, workspace]\n",
		"    changed   core [go, workspace]\n",
		"    changed   pricing [go, workspace], depends on core\n",
		"    dependent lib [go, workspace], depends on api, pricing\n",
	} {
		if !strings.Contains(b.String(), line) {
			t.Errorf("the summary lacks %q:\n%s", line, b.String())
		}
	}
}

func TestADependsOnListNamesTenComponents(t *testing.T) {
	spectest.Proves(t, analysesFeature, "summary", "a-changed-component-names-the-changed-components-it-depends-on")
	a := analyze.Result{}
	all := make([]string, 0, 12)
	for i := range 12 {
		id := fmt.Sprintf("m%02d", i)
		all = append(all, id)
		a.Components = append(a.Components, evidence.Component{ID: id, Ecosystem: "go", Source: "workspace", Impact: evidence.ImpactChanged})
	}
	a.Components = append(a.Components,
		evidence.Component{ID: "top", Ecosystem: "go", Source: "workspace", Impact: evidence.ImpactChanged, DependsOn: all},
		evidence.Component{ID: "user", Ecosystem: "go", Source: "workspace", Impact: evidence.ImpactDependent, DependsOn: all})
	var b strings.Builder
	writeAnalysis(&b, a)
	names := strings.Join(all[:10], ", ") + " and 2 more\n"
	for _, line := range []string{
		"    changed   top [go, workspace], depends on " + names,
		"    dependent user [go, workspace], depends on " + names,
	} {
		if !strings.Contains(b.String(), line) {
			t.Errorf("the summary lacks %q:\n%s", line, b.String())
		}
	}
}

func TestTheRushDemoChangeIsExplained(t *testing.T) {
	spectest.Proves(t, "tooling/observe-rush", "rush-change-analysis", "the-rush-demo-change-is-explained")
	root, base := demoRepository(t, "rush")
	env := map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "Putnami/demo-observe-rush", "GITHUB_REPOSITORY_ID": "1108", "GITHUB_RUN_ID": "41335520001", "GITHUB_JOB": "test"}
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	o := Run(context.Background(), opts)

	var components []string
	for _, c := range o.Analysis.Components {
		components = append(components, c.ID+" "+c.Name+" "+c.Ecosystem+" "+c.Source+" "+c.Impact+" "+strings.Join(c.DependsOn, ","))
	}
	want := []string{
		"apps/api @demo/api node orchestrator dependent libs/pricing",
		"apps/web @demo/web node orchestrator unaffected ",
		"libs/pricing @demo/pricing node orchestrator changed ",
	}
	if !slices.Equal(components, want) {
		t.Errorf("components\n%s\nwant\n%s", strings.Join(components, "\n"), strings.Join(want, "\n"))
	}
	var text bytes.Buffer
	if err := WriteDetails(&text, o); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"  components  1 changed, 1 dependent, 1 unaffected\n",
		"    changed   libs/pricing (@demo/pricing) [node, orchestrator]\n",
		"    dependent apps/api (@demo/api) [node, orchestrator], depends on libs/pricing\n",
		"    upgraded   zod 3.23.8 -> 3.24.1, in common/config/rush/pnpm-lock.yaml\n",
	} {
		if !strings.Contains(text.String(), line) {
			t.Errorf("the summary lacks %q:\n%s", line, text.String())
		}
	}
	wantDependency := evidence.DependencyChange{Ecosystem: "npm", Lockfile: "common/config/rush/pnpm-lock.yaml", Name: "zod", Change: evidence.DependencyUpgraded, From: "3.23.8", To: "3.24.1"}
	if len(o.Analysis.DependencyChanges) != 1 || o.Analysis.DependencyChanges[0] != wantDependency {
		t.Errorf("dependency changes %+v, want %+v", o.Analysis.DependencyChanges, wantDependency)
	}
	for _, d := range o.Diagnostics {
		if d.Severity != diag.Info && !strings.HasPrefix(d.Code, "observe.report_") {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
	}
	if problems := evidence.ValidateDocument(Evidence(o, now)); diag.HasErrors(problems) {
		t.Fatalf("the evidence document does not validate: %v", problems)
	}
}

// demoRepositoryWithout builds the demo as demoRepository does, without the
// files skip names.
func demoRepositoryWithout(t *testing.T, name string, skip func(string) bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	git(t, root, "init", "--quiet", "--initial-branch=main")
	files := slices.DeleteFunc(readDemo(t, name), func(f demoFile) bool { return skip(f.name) })
	for _, stage := range []string{"base/", "head/"} {
		for _, f := range files {
			if rest, ok := strings.CutPrefix(f.name, stage); ok {
				writeDemoFile(t, root, rest, f.content)
			}
			if rest, ok := strings.CutPrefix(f.name, "delete/"); ok && stage == "head/" {
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(rest))); err != nil {
					t.Fatal(err)
				}
			}
		}
		git(t, root, "add", "--all")
		git(t, root, "commit", "--quiet", "-m", stage)
	}
	for _, f := range files {
		if rest, ok := strings.CutPrefix(f.name, "ci/"); ok {
			writeDemoFile(t, root, rest, f.content)
		}
	}
	return root, git(t, root, "rev-parse", "HEAD~1")
}
