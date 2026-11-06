package analyze

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/components"
	"go.putnami.dev/tooling/observe/internal/gitbase"
	"go.putnami.dev/tooling/observe/internal/reports"
)

const feature = "tooling/observe-analyses"

type fake struct{ found components.Found }

func (fake) Name() string                     { return "fake" }
func (f fake) Detect(string) components.Found { return f.found }

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// repository is a pricing package, an api that depends on it, and a Go
// module whose import paths the fake detector locates.
func repository(t *testing.T) (string, components.Set) {
	t.Helper()
	root := t.TempDir()
	write(t, root, map[string]string{
		"packages/pricing/src/index.ts":      "export const a = 1\n",
		"packages/pricing/src/index.test.ts": "test\n",
		"apps/api/src/quote.ts":              "export const q = 1\n",
		"apps/api/test/quote.test.ts":        "test\n",
		"svc/calc.go":                        "package calc\n",
	})
	set := components.Detect(root, []components.Detector{fake{components.Found{
		Components: []components.Component{
			{ID: "packages/pricing", Ecosystem: "node", Source: evidence.SourceWorkspace},
			{ID: "apps/api", Ecosystem: "node", Source: evidence.SourceWorkspace, DependsOn: []string{"packages/pricing"}},
			{ID: "svc", Ecosystem: "go", Source: evidence.SourceManifest},
		},
		Locate: func(importPath string) (string, string, bool) {
			if rest, ok := strings.CutPrefix(importPath, "example.com/svc"); ok {
				return "svc" + rest, "svc", true
			}
			return "", "", false
		},
	}}})
	return root, set
}

func tests(r Result) map[string]evidence.ComponentTests {
	out := map[string]evidence.ComponentTests{}
	for _, entry := range r.Tests {
		out[entry.Component] = entry
	}
	return out
}

func TestATestGoesToTheComponentOfItsFileElseOfItsReport(t *testing.T) {
	spectest.Proves(t, feature, "tests-and-coverage", "a-test-goes-to-the-component-of-its-file-else-of-its-report")
	root, set := repository(t)
	write(t, root, map[string]string{
		// Vitest names the test file relative to the package as the class.
		"packages/pricing/reports/junit.xml": `<testsuites><testsuite name="src/index.test.ts">
			<testcase classname="src/index.test.ts" name="sums" time="0.5"/>
			<testcase classname="src/index.test.ts" name="rejects" time="0.25"><failure message="expected 1&#10;got 2">trace</failure></testcase>
			<testcase classname="src/index.test.ts" name="later"><skipped/></testcase>
		</testsuite></testsuites>`,
		// A root report whose cases name their file absolutely, from another checkout.
		"reports/junit.xml": `<testsuites><testsuite name="all">
			<testcase name="quotes" file="/builds/acme/shop/apps/api/test/quote.test.ts"/>
			<testcase name="orphan" classname="Some.Class"/>
		</testsuite>
		<testsuite name="example.com/svc"><testcase classname="example.com/svc" name="TestCalc" time="1"/></testsuite></testsuites>`,
		"reports/broken.xml": `<testsuites><testsuite`,
	})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "packages/pricing/reports/junit.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
		{Path: "reports/junit.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
		{Path: "reports/broken.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
	}})
	got := tests(r)
	pricing := got["packages/pricing"]
	if pricing.Passed != 1 || pricing.Failed != 1 || pricing.Skipped != 1 || pricing.DurationMs != 750 {
		t.Errorf("pricing tests %+v", pricing)
	}
	if len(pricing.FailingCases) != 1 || pricing.FailingCases[0].File != "packages/pricing/src/index.test.ts" || pricing.FailingCases[0].Message != "expected 1\ngot 2" || pricing.FailingCases[0].Suite != "src/index.test.ts" {
		t.Errorf("failing cases %+v", pricing.FailingCases)
	}
	if got["apps/api"].Passed != 1 || got["svc"].Passed != 1 {
		t.Errorf("tests %+v", r.Tests)
	}
	codes := make([]string, 0, len(r.Diagnostics))
	for _, d := range r.Diagnostics {
		codes = append(codes, d.Code)
	}
	slices.Sort(codes)
	if !slices.Equal(codes, []string{CodeReportUnread, CodeTestsUnattributed}) {
		t.Errorf("diagnostics %v", r.Diagnostics)
	}
	if len(r.Components) != 3 {
		t.Errorf("components %+v", r.Components)
	}
}

func TestAPytestCaseGoesToTheComponentOfItsModuleFile(t *testing.T) {
	spectest.Proves(t, feature, "tests-and-coverage", "a-pytest-case-goes-to-the-component-of-its-module-file")
	root := t.TempDir()
	write(t, root, map[string]string{
		"services/api/tests/test_api.py":  "def test_ok(): pass\n",
		"services/api/tests/__init__.py":  "",
		"libs/shared/tests/test_money.py": "def test_sum(): pass\n",
	})
	set := components.Detect(root, []components.Detector{fake{components.Found{Components: []components.Component{
		{ID: "services/api", Ecosystem: "python", Source: evidence.SourceWorkspace},
		{ID: "libs/shared", Ecosystem: "python", Source: evidence.SourceWorkspace},
	}}}})
	write(t, root, map[string]string{
		// pytest run at the root: the class names the module from the root,
		// with the test class when there is one.
		"junit.xml": `<testsuites><testsuite name="pytest">
			<testcase classname="services.api.tests.test_api.TestLogin" name="test_ok" time="0.5"/>
			<testcase classname="services.api.tests.test_api" name="test_module_level"/>
			<testcase classname="services.api.tests.test_api.TestLogin" name="test_bad"><failure message="assert 1 == 2"/></testcase>
			<testcase classname="services.api.tests.test_gone" name="test_orphan"/>
			<testcase classname="Some.Class" name="orphan"/>
		</testsuite></testsuites>`,
		// pytest run in a component: the class names the module from there.
		"libs/shared/junit.xml": `<testsuites><testsuite name="pytest">
			<testcase classname="tests.test_money" name="test_sum"/>
		</testsuite></testsuites>`,
	})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "junit.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
		{Path: "libs/shared/junit.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
	}})
	got := tests(r)
	api := got["services/api"]
	if api.Passed != 2 || api.Failed != 1 || len(api.FailingCases) != 1 || api.FailingCases[0].File != "services/api/tests/test_api.py" {
		t.Errorf("api tests %+v", api)
	}
	if got["libs/shared"].Passed != 1 {
		t.Errorf("shared tests %+v", got["libs/shared"])
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != CodeTestsUnattributed || !strings.HasPrefix(r.Diagnostics[0].Message, "2 test cases") {
		t.Errorf("diagnostics %v", r.Diagnostics)
	}
	for classname, want := range map[string][]string{
		"tests.test_api.TestLogin": {"tests/test_api/TestLogin.py", "tests/test_api.py", "tests.py"},
		"test_api":                 {"test_api.py"},
		"src/index.test.ts":        nil,
		"example.com/svc":          nil,
		"":                         nil,
		"a..b":                     nil,
	} {
		if got := moduleFiles(classname); !slices.Equal(got, want) {
			t.Errorf("moduleFiles(%q) = %v, want %v", classname, got, want)
		}
	}
}

func TestANextestCaseGoesToTheCrateItsBinaryNames(t *testing.T) {
	spectest.Proves(t, "tooling/observe-rust", "rust-report-discovery", "a-nextest-case-goes-to-the-crate-its-binary-names")
	root := t.TempDir()
	write(t, root, map[string]string{
		"Cargo.toml":                "[workspace]\nmembers = [\"crates/*\"]\n",
		"crates/api/Cargo.toml":     "[package]\nname = \"api-server\"\n",
		"crates/pricing/Cargo.toml": "[package]\nname = \"pricing\"\n",
		// The virtual workspace root holds the report and is no component.
		"target/nextest/ci/junit.xml": `<testsuites name="nextest-run">
			<testsuite name="api-server"><testcase name="tests::quote" classname="api-server"/></testsuite>
			<testsuite name="pricing"><testcase name="tests::empty" classname="pricing"/></testsuite>
			<testsuite name="pricing::format"><testcase name="formats" classname="pricing::format"><failure message="left != right"/></testcase></testsuite>
			<testsuite name="api_server::bin/api"><testcase name="serves" classname="api_server::bin/api"/></testsuite>
			<testsuite name="gone"><testcase name="orphan" classname="gone"/></testsuite>
		</testsuites>`,
	})
	set := components.Detect(root, []components.Detector{components.CargoDetector{}})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "target/nextest/ci/junit.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
	}})
	got := tests(r)
	if got["crates/api"].Passed != 2 || got["crates/pricing"].Passed != 1 || got["crates/pricing"].Failed != 1 {
		t.Errorf("tests %+v", got)
	}
	if failing := got["crates/pricing"].FailingCases; len(failing) != 1 || failing[0].Suite != "pricing::format" {
		t.Errorf("failing cases %+v", failing)
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != CodeTestsUnattributed || !strings.HasPrefix(r.Diagnostics[0].Message, "1 test cases") {
		t.Errorf("diagnostics %v", r.Diagnostics)
	}
}

func TestAJestSuiteNamedLikeACrateStaysWithItsReport(t *testing.T) {
	spectest.Proves(t, "tooling/observe-rust", "rust-report-discovery", "a-nextest-case-goes-to-the-crate-its-binary-names")
	root := t.TempDir()
	write(t, root, map[string]string{
		"crates/pricing/Cargo.toml": "[package]\nname = \"pricing\"\n",
		// jest-junit writes the describe title as the suite name, and the
		// title and the test name as the class name.
		"web/junit.xml": `<testsuites name="jest tests">
			<testsuite name="pricing"><testcase classname="pricing adds up" name="pricing adds up"/></testsuite>
		</testsuites>`,
	})
	set := components.Detect(root, []components.Detector{components.CargoDetector{}, fake{components.Found{Components: []components.Component{
		{ID: "web", Ecosystem: "node", Source: evidence.SourceWorkspace},
	}}}})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "web/junit.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
	}})
	got := tests(r)
	if got["web"].Passed != 1 || got["crates/pricing"].Passed != 0 {
		t.Errorf("tests %+v", got)
	}
}

func TestCoverageGoesToTheComponentOfEachFile(t *testing.T) {
	spectest.Proves(t, feature, "tests-and-coverage", "coverage-goes-to-the-component-of-each-file")
	root, set := repository(t)
	write(t, root, map[string]string{
		// LCOV relative to the package that ran the tests.
		"packages/pricing/coverage/lcov.info": "TN:\nSF:src/index.ts\nDA:1,1\nDA:2,0\nLF:2\nLH:1\nend_of_record\nSF:src/missing.ts\nDA:1,1\nend_of_record\n",
		// Cobertura with an absolute source root under the repository.
		"apps/api/coverage/cobertura.xml": `<coverage><sources><source>` + filepath.ToSlash(filepath.Join(root, "apps/api")) + `</source></sources><packages><package name="p"><classes>
			<class name="quote" filename="src/quote.ts"><lines><line number="1" hits="3"/><line number="2" hits="1"/></lines></class>
		</classes></package></packages></coverage>`,
		// Clover from another checkout directory.
		"reports/clover.xml": `<coverage><project><file name="/builds/acme/shop/apps/api/src/quote.ts"><line num="2" type="stmt" count="0"/><line num="3" type="stmt" count="0"/></file></project></coverage>`,
		// Cobertura gathered in a root folder, its source from another
		// checkout directory; a source with ".." matches nothing.
		"reports/api/cobertura.xml": `<coverage><sources><source>/builds/acme/shop/apps/api</source><source>/x/../../apps</source></sources><packages><package name="p"><classes>
			<class name="quote" filename="src/quote.ts"><lines><line number="4" hits="1"/></lines></class>
		</classes></package></packages></coverage>`,
		// A Go cover profile names import paths.
		"reports/cover.out": "mode: set\nexample.com/svc/calc.go:1.1,2.2 1 1\nexample.com/svc/calc.go:3.1,3.5 1 0\n",
		"reports/bad.info":  "SF:x\nDA:0,1\n",
	})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "packages/pricing/coverage/lcov.info", Kind: reports.KindCoverage, Format: reports.FormatLCOV},
		{Path: "apps/api/coverage/cobertura.xml", Kind: reports.KindCoverage, Format: reports.FormatCobertura},
		{Path: "reports/clover.xml", Kind: reports.KindCoverage, Format: reports.FormatClover},
		{Path: "reports/api/cobertura.xml", Kind: reports.KindCoverage, Format: reports.FormatCobertura},
		{Path: "reports/cover.out", Kind: reports.KindCoverage, Format: reports.FormatGoCover},
		{Path: "reports/bad.info", Kind: reports.KindCoverage, Format: reports.FormatLCOV},
	}})
	want := []evidence.ComponentCoverage{
		{Component: "apps/api", CoveredLines: 3, TotalLines: 4},
		{Component: "packages/pricing", CoveredLines: 1, TotalLines: 2},
		{Component: "svc", CoveredLines: 2, TotalLines: 3},
	}
	if !slices.Equal(r.Coverage, want) {
		t.Errorf("coverage\n got %+v\nwant %+v", r.Coverage, want)
	}
	codes := make([]string, 0, len(r.Diagnostics))
	for _, d := range r.Diagnostics {
		codes = append(codes, d.Code)
	}
	slices.Sort(codes)
	if !slices.Equal(codes, []string{CodeCoverageUnmapped, CodeReportUnread}) {
		t.Errorf("diagnostics %v", r.Diagnostics)
	}
}

func TestChangedLinesAreMeasuredAgainstLineCoverage(t *testing.T) {
	spectest.Proves(t, feature, "changed-line-coverage", "changed-lines-are-measured-against-line-coverage")
	root, set := repository(t)
	write(t, root, map[string]string{
		"a.info": "SF:packages/pricing/src/index.ts\nDA:1,0\nDA:2,0\nDA:3,1\nDA:5,0\nend_of_record\n",
		// A second report covers line 2.
		"b.info": "SF:" + filepath.ToSlash(filepath.Join(root, "packages/pricing/src/index.ts")) + "\nDA:2,4\nend_of_record\nSF:apps/api/src/quote.ts\nDA:9,1\nend_of_record\n",
	})
	changed := []gitbase.ChangedFile{
		{Path: "packages/pricing/src/index.ts", Status: gitbase.StatusModified},
		{Path: "apps/api/src/quote.ts", Status: gitbase.StatusModified},
		{Path: "apps/api/src/old.ts", Status: gitbase.StatusDeleted},
		{Path: "README.md", Status: gitbase.StatusAdded},
	}
	in := Input{
		Root: root, Set: set, ChangedFiles: changed,
		Reports: []reports.Report{
			{Path: "a.info", Kind: reports.KindCoverage, Format: reports.FormatLCOV},
			{Path: "b.info", Kind: reports.KindCoverage, Format: reports.FormatLCOV},
		},
		AddedLines: map[string][]int{
			"packages/pricing/src/index.ts": {1, 2, 3, 4, 5},
			"apps/api/src/quote.ts":         {1},
			"README.md":                     {1},
		},
	}
	r := Run(in)
	want := &evidence.ChangedLineCoverage{CoveredLines: 2, TotalLines: 4, Files: []evidence.ChangedLineCoverageFile{
		{Path: "apps/api/src/quote.ts", CoveredLines: 0, TotalLines: 0},
		{Path: "packages/pricing/src/index.ts", CoveredLines: 2, TotalLines: 4, UncoveredLines: []int{1, 5}},
	}}
	got := r.ChangedLineCoverage
	if got == nil || got.CoveredLines != want.CoveredLines || got.TotalLines != want.TotalLines || len(got.Files) != 2 ||
		got.Files[1].Path != want.Files[1].Path || !slices.Equal(got.Files[1].UncoveredLines, want.Files[1].UncoveredLines) || got.Files[0].Path != want.Files[0].Path {
		t.Errorf("changed-line coverage\n got %+v\nwant %+v", got, want)
	}
	if r.Owners["README.md"] != "" || r.Owners["packages/pricing/src/index.ts"] != "packages/pricing" {
		t.Errorf("owners %v", r.Owners)
	}
	impacts := map[string]string{}
	for _, c := range r.Components {
		impacts[c.ID] = c.Impact
	}
	if impacts["packages/pricing"] != evidence.ImpactChanged || impacts["apps/api"] != evidence.ImpactChanged || impacts["svc"] != evidence.ImpactUnaffected {
		t.Errorf("impacts %v", impacts)
	}
	in.AddedLines = nil
	if Run(in).ChangedLineCoverage != nil {
		t.Error("changed-line coverage without a base")
	}
	in.AddedLines = map[string][]int{}
	in.Reports = nil
	if Run(in).ChangedLineCoverage != nil {
		t.Error("changed-line coverage without a coverage report")
	}
}

func TestChangedLockfilesGiveDependencyChanges(t *testing.T) {
	spectest.Proves(t, feature, "dependency-changes", "each-lockfile-format-gives-its-package-changes")
	root, set := repository(t)
	sides := map[Revision]map[string]string{
		Base: {
			"packages/pricing/composer.lock": `{"packages":[{"name":"brick/math","version":"0.13.0"}]}`,
			"old/yarn.lock":                  "a@^1:\n  version \"1.0.0\"\n",
			"broken/package-lock.json":       `{"packages":{}}`,
		},
		Head: {
			"packages/pricing/composer.lock": `{"packages":[{"name":"brick/math","version":"0.13.1"}]}`,
			"new/yarn.lock":                  "b@^2:\n  version \"2.0.0\"\n",
			"broken/package-lock.json":       `{`,
		},
	}
	in := Input{Root: root, Set: set,
		ChangedFiles: []gitbase.ChangedFile{
			{Path: "packages/pricing/composer.lock", Status: gitbase.StatusModified},
			{Path: "new/yarn.lock", Status: gitbase.StatusRenamed, PreviousPath: "old/yarn.lock"},
			{Path: "gone/pnpm-lock.yaml", Status: gitbase.StatusDeleted},
			{Path: "broken/package-lock.json", Status: gitbase.StatusModified},
			{Path: "failing/composer.lock", Status: gitbase.StatusModified},
			{Path: "packages/pricing/src/index.ts", Status: gitbase.StatusModified},
		},
		Show: func(side Revision, file string) ([]byte, bool, error) {
			if strings.HasPrefix(file, "failing/") {
				return nil, false, errors.New("git cat-file: bad object")
			}
			content, ok := sides[side][file]
			return []byte(content), ok, nil
		},
	}
	r := Run(in)
	want := []evidence.DependencyChange{
		{Ecosystem: "composer", Lockfile: "packages/pricing/composer.lock", Name: "brick/math", Change: evidence.DependencyUpgraded, From: "0.13.0", To: "0.13.1", Component: "packages/pricing"},
		{Ecosystem: "npm", Lockfile: "new/yarn.lock", Name: "a", Change: evidence.DependencyRemoved, From: "1.0.0"},
		{Ecosystem: "npm", Lockfile: "new/yarn.lock", Name: "b", Change: evidence.DependencyAdded, To: "2.0.0"},
	}
	if !slices.Equal(r.DependencyChanges, want) {
		t.Errorf("dependency changes\n got %+v\nwant %+v", r.DependencyChanges, want)
	}
	unread := 0
	for _, d := range r.Diagnostics {
		if d.Code == CodeLockfileUnread {
			unread++
		}
	}
	if unread != 2 {
		t.Errorf("diagnostics %v", r.Diagnostics)
	}
	if bounded(strings.Repeat("é", evidence.MaxStringBytes)) == strings.Repeat("é", evidence.MaxStringBytes) {
		t.Error("a long string is not bounded")
	}
}
