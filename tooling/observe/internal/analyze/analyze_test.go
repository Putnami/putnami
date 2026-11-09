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

func TestAMinitestCaseGoesToItsTestFile(t *testing.T) {
	spectest.Proves(t, "tooling/observe-ruby", "ruby-reports", "a-minitest-case-goes-to-its-test-file")
	root := t.TempDir()
	write(t, root, map[string]string{
		"app/models/order.rb":               "class Order; end\n",
		"test/models/order_test.rb":         "class OrderTest; end\n",
		"gems/pricing/lib/pricing.rb":       "module Pricing\n  def self.total(lines)\n    lines.sum\n  end\nend\n",
		"gems/pricing/test/pricing_test.rb": "class PricingTest; end\n",
	})
	set := components.Detect(root, []components.Detector{fake{components.Found{Components: []components.Component{
		{ID: ".", Ecosystem: "ruby", Source: evidence.SourceManifest},
		{ID: "gems/pricing", Ecosystem: "ruby", Source: evidence.SourceManifest},
	}}}})
	// minitest-reporters, run in the gem, names each test file from there,
	// and simplecov-lcov each covered file.
	write(t, root, map[string]string{
		"gems/pricing/test/reports/TEST-PricingTest.xml": `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="PricingTest" filepath="test/pricing_test.rb" skipped="0" failures="0" errors="0" tests="2" assertions="2" time="7.2e-05">
    <testcase name="test_total" lineno="4" classname="PricingTest" assertions="1" time="6.2e-05" file="test/pricing_test.rb">
    </testcase>
    <testcase name="test_empty" lineno="8" classname="PricingTest" assertions="1" time="1.0e-05" file="test/pricing_test.rb">
    </testcase>
  </testsuite>
</testsuites>
`,
		"gems/pricing/coverage/lcov/pricing.lcov": "SF:./lib/pricing.rb\nDA:1,1\nDA:2,1\nDA:3,4\nLF:3\nLH:3\nend_of_record\n",
		// The application's Rails run, from the root.
		"test/reports/TEST-OrderTest.xml": `<testsuites><testsuite name="OrderTest" filepath="test/models/order_test.rb">
			<testcase name="test_valid" classname="OrderTest" file="test/models/order_test.rb"/>
		</testsuite></testsuites>`,
	})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "gems/pricing/test/reports/TEST-PricingTest.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
		{Path: "gems/pricing/coverage/lcov/pricing.lcov", Kind: reports.KindCoverage, Format: reports.FormatLCOV},
		{Path: "test/reports/TEST-OrderTest.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
	}})
	got := tests(r)
	if got["gems/pricing"].Passed != 2 || got["."].Passed != 1 {
		t.Errorf("tests %+v", r.Tests)
	}
	if len(r.Coverage) != 1 || r.Coverage[0] != (evidence.ComponentCoverage{Component: "gems/pricing", CoveredLines: 3, TotalLines: 3}) {
		t.Errorf("coverage %+v", r.Coverage)
	}
	if len(r.Diagnostics) != 0 {
		t.Errorf("diagnostics %v", r.Diagnostics)
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
		// Coverlet on Linux: the source is "/", each file name the absolute
		// path without its leading slash, in this checkout or another one.
		"reports/coverlet/cobertura.xml": `<coverage><sources><source>/</source></sources><packages><package name="p"><classes>
			<class name="quote" filename="` + strings.TrimPrefix(filepath.ToSlash(filepath.Join(root, "apps/api/src/quote.ts")), "/") + `"><lines><line number="5" hits="1"/></lines></class>
			<class name="quote" filename="builds/acme/shop/apps/api/src/quote.ts"><lines><line number="6" hits="0"/></lines></class>
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
		{Path: "reports/coverlet/cobertura.xml", Kind: reports.KindCoverage, Format: reports.FormatCobertura},
		{Path: "reports/cover.out", Kind: reports.KindCoverage, Format: reports.FormatGoCover},
		{Path: "reports/bad.info", Kind: reports.KindCoverage, Format: reports.FormatLCOV},
	}})
	want := []evidence.ComponentCoverage{
		{Component: "apps/api", CoveredLines: 4, TotalLines: 6},
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

func TestADotnetCaseGoesToTheProjectOfItsTestAssembly(t *testing.T) {
	spectest.Proves(t, "tooling/observe-dotnet", "dotnet-test-assemblies", "a-case-goes-to-the-project-of-its-test-assembly")
	root := t.TempDir()
	write(t, root, map[string]string{
		"src/Calc/Calc.csproj": `<Project Sdk="Microsoft.NET.Sdk"/>`,
		"tests/Calc.Tests.xunit/Calc.Tests.xunit.csproj": `<Project Sdk="Microsoft.NET.Sdk"><ItemGroup>` +
			`<ProjectReference Include="..\..\src\Calc\Calc.csproj" /></ItemGroup></Project>`,
	})
	set := components.Detect(root, components.Default(components.Options{}))
	// dotnet test --logger trx and JunitXml.TestLogger wrote these reports
	// for the xUnit project, whose classes lie in the namespace Calc.Tests.
	for name, file := range map[string]string{"TestResults/xunit.trx": "xunit.trx", "TestResults/junit.xml": "junit-xunit-logger.xml"} {
		data, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		write(t, root, map[string]string{name: string(data)})
	}
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "TestResults/junit.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
		{Path: "TestResults/xunit.trx", Kind: reports.KindJUnit, Format: reports.FormatTRX},
	}})
	got := tests(r)
	project := got["tests/Calc.Tests.xunit"]
	if project.Passed != 4 || project.Failed != 2 || project.Skipped != 2 || len(project.FailingCases) != 2 {
		t.Errorf("tests of the test project %+v, want both reports' cases", project)
	}
	if _, onLibrary := got["src/Calc"]; onLibrary || len(r.Tests) != 1 {
		t.Errorf("tests %+v, want none on src/Calc, whose name the namespace starts with", r.Tests)
	}
	if len(r.Diagnostics) != 0 {
		t.Errorf("diagnostics %v", r.Diagnostics)
	}
}

func TestABazelTestLogGoesToThePackageOfItsTestTarget(t *testing.T) {
	spectest.Proves(t, "tooling/observe-bazel", "bazel-reports", "a-test-log-goes-to-the-package-of-its-test-target")
	root := t.TempDir()
	write(t, root, map[string]string{
		"MODULE.bazel":        "",
		"BUILD.bazel":         "",
		"pricing/BUILD.bazel": "",
		"pricing/pricing.go":  "package pricing\n",
		"api/BUILD.bazel":     "",
		"api/api.go":          "package api\n",
		"bazel-graph.json": `{"type":"RULE","rule":{"name":"//pricing:pricing_test","ruleInput":["//pricing:pricing.go"]}}
{"type":"RULE","rule":{"name":"//api:api_test","ruleInput":["//pricing:pricing_test"]}}
{"type":"RULE","rule":{"name":"//:all"}}
`,
		// rules_go names the target as the suite and no file; a case may
		// still name a file of another package.
		"bazel-testlogs/pricing/pricing_test/test.xml": `<testsuites><testsuite name="pricing/pricing_test">
			<testcase classname="pricing_test" name="TestQuote" time="1"/>
			<testcase classname="pricing_test" name="TestRound"><failure message="off by one"/></testcase>
		</testsuite></testsuites>`,
		"bazel-testlogs/api/api_test/test.xml": `<testsuites><testsuite name="api/api_test">
			<testcase name="TestServe" file="api/api.go"/>
			<testcase name="TestPrice" file="pricing/pricing.go"/>
		</testsuite></testsuites>`,
	})
	set := components.Detect(root, []components.Detector{components.BazelDetector{}})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "bazel-testlogs/api/api_test/test.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
		{Path: "bazel-testlogs/pricing/pricing_test/test.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
	}})
	if len(r.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v", r.Diagnostics)
	}
	got := tests(r)
	if pricing := got["pricing"]; pricing.Passed != 1 || pricing.Failed != 1 || pricing.FailingCases[0].Suite != "pricing/pricing_test" {
		t.Errorf("pricing tests %+v", pricing)
	}
	if api := got["api"]; api.Passed != 2 {
		t.Errorf("api tests %+v", api)
	}
	if _, rooted := got["."]; rooted || len(r.Tests) != 2 {
		t.Errorf("tests %+v", r.Tests)
	}
}

const jvmFeature = "tooling/observe-jvm"

// jvmRepository is a Gradle build: app, an Android library, and api depend
// on pricing; api and app, which do not depend on each other, both hold a
// com.acme.shared.Util.
func jvmRepository(t *testing.T) (string, components.Set) {
	t.Helper()
	root := t.TempDir()
	write(t, root, map[string]string{
		"pricing/src/main/kotlin/com/acme/pricing/Price.kt":     "package com.acme.pricing\n",
		"pricing/src/test/kotlin/com/acme/pricing/PriceTest.kt": "class PriceTest\n",
		"api/src/main/java/com/acme/api/Api.java":               "package com.acme.api;\n",
		"api/src/test/java/com/acme/api/ApiTest.java":           "class ApiTest {}\n",
		"api/src/main/kotlin/com/acme/shared/Util.kt":           "package com.acme.shared\n",
		"app/src/main/kotlin/com/acme/shared/Util.kt":           "package com.acme.shared\n",
		"app/src/androidTest/kotlin/com/acme/app/ScreenTest.kt": "class ScreenTest\n",
	})
	set := components.Detect(root, []components.Detector{fake{components.Found{Components: []components.Component{
		{ID: "pricing", Ecosystem: "gradle", Source: evidence.SourceWorkspace},
		{ID: "api", Ecosystem: "gradle", Source: evidence.SourceWorkspace, DependsOn: []string{"pricing"}},
		{ID: "app", Ecosystem: "gradle", Source: evidence.SourceWorkspace, DependsOn: []string{"pricing"}},
	}}}})
	return root, set
}

func TestAJVMClassNamePlacesItsCase(t *testing.T) {
	spectest.Proves(t, jvmFeature, "jvm-report-placement", "a-jvm-class-name-places-its-case")
	root, set := jvmRepository(t)
	write(t, root, map[string]string{
		// Reports gathered at the root, as a CI collects them.
		"reports/TEST-all.xml": `<testsuites>
			<testsuite name="com.acme.pricing.PriceTest">
				<testcase classname="com.acme.pricing.PriceTest" name="rounds"/>
				<testcase classname="com.acme.pricing.PriceTest$Discounts" name="nested"><failure message="expected 9"/></testcase>
			</testsuite>
			<testsuite name="com.acme.api.ApiTest"><testcase classname="com.acme.api.ApiTest" name="quotes"/></testsuite>
			<testsuite name="com.acme.app.ScreenTest"><testcase classname="com.acme.app.ScreenTest" name="renders"/></testsuite>
			<testsuite name="com.acme.gone.GoneTest"><testcase classname="com.acme.gone.GoneTest" name="orphan"/></testsuite>
		</testsuites>`,
	})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{{Path: "reports/TEST-all.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit}}})
	got := tests(r)
	if p := got["pricing"]; p.Passed != 1 || p.Failed != 1 || p.FailingCases[0].File != "pricing/src/test/kotlin/com/acme/pricing/PriceTest.kt" {
		t.Errorf("pricing tests %+v", p)
	}
	if got["api"].Passed != 1 || got["app"].Passed != 1 {
		t.Errorf("tests %+v", got)
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != CodeTestsUnattributed || !strings.HasPrefix(r.Diagnostics[0].Message, "1 test cases") {
		t.Errorf("diagnostics %v", r.Diagnostics)
	}
	for classname, want := range map[string][]string{
		"com.acme.PriceTest$Nested$Deeper": {"com/acme/PriceTest.java", "com/acme/PriceTest.kt"},
		"PriceTest":                        nil,
		"src/test/PriceTest.kt":            nil,
		"":                                 nil,
	} {
		if got := jvmClassFiles(classname); !slices.Equal(got, want) {
			t.Errorf("jvmClassFiles(%q) = %v, want %v", classname, got, want)
		}
	}
}

func TestAJaCoCoFileMapsThroughItsPackage(t *testing.T) {
	spectest.Proves(t, jvmFeature, "jvm-report-placement", "a-jacoco-file-maps-through-its-package")
	root, set := jvmRepository(t)
	report := func(packages string) string {
		return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><!DOCTYPE report PUBLIC "-//JACOCO//DTD Report 1.1//EN" "report.dtd"><report name="r">` + packages + `</report>`
	}
	write(t, root, map[string]string{
		// The app report: its own Util.kt wins over the one of api.
		"app/build/reports/jacoco/report.xml": report(`<package name="com/acme/shared"><sourcefile name="Util.kt"><line nr="1" mi="0" ci="2"/></sourcefile></package>`),
		// An aggregate report at the root, with a group per project.
		"build/reports/jacoco/aggregate.xml": report(`<group name="pricing"><package name="com/acme/pricing"><sourcefile name="Price.kt"><line nr="1" mi="0" ci="1"/><line nr="2" mi="3" ci="0"/></sourcefile></package></group>
			<group name="api"><package name="com/acme/api"><sourcefile name="Api.java"><line nr="1" mi="0" ci="1"/></sourcefile><sourcefile name="Gone.java"><line nr="1" mi="1" ci="0"/></sourcefile></package></group>`),
	})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "app/build/reports/jacoco/report.xml", Kind: reports.KindCoverage, Format: reports.FormatJaCoCo},
		{Path: "build/reports/jacoco/aggregate.xml", Kind: reports.KindCoverage, Format: reports.FormatJaCoCo},
	}})
	want := []evidence.ComponentCoverage{
		{Component: "api", CoveredLines: 1, TotalLines: 1},
		{Component: "app", CoveredLines: 1, TotalLines: 1},
		{Component: "pricing", CoveredLines: 1, TotalLines: 2},
	}
	if !slices.Equal(r.Coverage, want) {
		t.Errorf("coverage\n got %+v\nwant %+v", r.Coverage, want)
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != CodeCoverageUnmapped || !strings.HasPrefix(r.Diagnostics[0].Message, "1 of the 3 files") {
		t.Errorf("diagnostics %v", r.Diagnostics)
	}
}

func TestAMavenPackageURLMatchesAGradleChange(t *testing.T) {
	spectest.Proves(t, jvmFeature, "gradle-dependency-changes", "a-maven-package-url-matches-a-gradle-change")
	root, set := jvmRepository(t)
	write(t, root, map[string]string{
		"bom.cdx.json": `{"bomFormat": "CycloneDX", "specVersion": "1.5", "version": 1,
			"components": [{"bom-ref": "g", "type": "library", "group": "com.google.guava", "name": "guava", "version": "33.4.0-jre",
				"purl": "pkg:maven/com.google.guava/guava@33.4.0-jre", "licenses": [{"license": {"id": "Apache-2.0"}}]}]}`,
	})
	sides := map[Revision]string{
		Base: "com.google.guava:guava:33.3.1-jre=compileClasspath\n",
		Head: "com.google.guava:guava:33.4.0-jre=compileClasspath\n",
	}
	r := Run(Input{Root: root, Set: set,
		ChangedFiles: []gitbase.ChangedFile{{Path: "pricing/gradle.lockfile", Status: gitbase.StatusModified}},
		Reports:      []reports.Report{{Path: "bom.cdx.json", Kind: reports.KindSBOM, Format: reports.FormatCycloneDX}},
		Show: func(side Revision, file string) ([]byte, bool, error) {
			return []byte(sides[side]), true, nil
		},
	})
	if len(r.DependencyChanges) != 1 {
		t.Fatalf("dependency changes %+v", r.DependencyChanges)
	}
	change := r.DependencyChanges[0]
	if change.Ecosystem != "maven" || change.Name != "com.google.guava:guava" || change.Component != "pricing" || change.Change != evidence.DependencyUpgraded {
		t.Errorf("change %+v", change)
	}
	if change.After == nil || !slices.Equal(change.After.Licenses, []string{"Apache-2.0"}) {
		t.Errorf("after %+v, want the SBOM's license", change.After)
	}
}

// dartRepository is a melos repository: a Flutter app and a Dart package,
// each with a test file, and a root lib/ that no package holds.
func dartRepository(t *testing.T) (string, components.Set) {
	t.Helper()
	root := t.TempDir()
	write(t, root, map[string]string{
		"apps/mobile/lib/main.dart":                 "void main() {}\n",
		"apps/mobile/test/widget_test.dart":         "void main() {}\n",
		"packages/pricing/lib/src/price.dart":       "int price() => 1;\n",
		"packages/pricing/test/src/price_test.dart": "void main() {}\n",
		"packages/pricing/test/helpers.dart":        "void helper() {}\n",
		"lib/src/price.dart":                        "int price() => 2;\n",
	})
	set := components.Detect(root, []components.Detector{fake{components.Found{Components: []components.Component{
		{ID: "apps/mobile", Ecosystem: "dart", Source: evidence.SourceWorkspace},
		{ID: "packages/pricing", Ecosystem: "dart", Source: evidence.SourceWorkspace},
	}}}})
	return root, set
}

func TestATojunitCaseGoesToTheComponentOfItsTestFile(t *testing.T) {
	spectest.Proves(t, feature, "tests-and-coverage", "a-tojunit-case-goes-to-the-component-of-its-test-file")
	spectest.Proves(t, "tooling/observe-dart", "dart-package-reports", "a-tojunit-class-name-names-its-test-file")
	root, set := dartRepository(t)
	write(t, root, map[string]string{
		// One report at the root: flutter test --machine names absolute
		// paths, which tojunit writes with a leading dot; dart test names
		// paths relative to the package, here with a --package prefix.
		"test-results.xml": `<testsuites>
			<testsuite name=".home.runner.work.shop.shop.apps.mobile.test.widget" tests="2">
				<testcase classname=".home.runner.work.shop.shop.apps.mobile.test.widget" name="renders" time="0.25"/>
				<testcase classname=".home.runner.work.shop.shop.apps.mobile.test.widget" name="taps" time="0.25"><failure message="expected 1"/></testcase>
			</testsuite>
			<testsuite name="shop.packages.pricing.test.src.price">
				<testcase classname="shop.packages.pricing.test.src.price" name="adds tax" time="0.1"/>
			</testsuite>
			<testsuite name="test.unknown">
				<testcase classname="test.unknown" name="orphan"/>
			</testsuite>
		</testsuites>`,
		// A report in a package: the class name is relative to it, and a
		// helper file without _test.dart is named whole.
		"packages/pricing/test-results.xml": `<testsuites><testsuite name="test.src.price">
			<testcase classname="test.src.price" name="rounds"/>
			<testcase classname="test.helpers" name="helps"/>
			<testcase classname="test.gone" name="still in pricing"/>
		</testsuite></testsuites>`,
	})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "test-results.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
		{Path: "packages/pricing/test-results.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit},
	}})
	got := tests(r)
	mobile := got["apps/mobile"]
	if mobile.Passed != 1 || mobile.Failed != 1 || len(mobile.FailingCases) != 1 || mobile.FailingCases[0].File != "apps/mobile/test/widget_test.dart" {
		t.Errorf("mobile tests %+v", mobile)
	}
	if got["packages/pricing"].Passed != 4 {
		t.Errorf("pricing tests %+v", got["packages/pricing"])
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != CodeTestsUnattributed || !strings.HasPrefix(r.Diagnostics[0].Message, "1 test cases") {
		t.Errorf("diagnostics %v", r.Diagnostics)
	}
	for classname, want := range map[string][]string{
		"test.price":    {"test/price_test.dart", "test/price.dart", "price_test.dart", "price.dart"},
		".a.test.b":     {"a/test/b_test.dart", "a/test/b.dart", "test/b_test.dart", "test/b.dart", "b_test.dart", "b.dart"},
		"src/a.test.ts": nil,
		"a-b.c":         nil,
		"":              nil,
		"a..b":          nil,
	} {
		if got := dartTestFiles(classname); !slices.Equal(got, want) {
			t.Errorf("dartTestFiles(%q) = %v, want %v", classname, got, want)
		}
	}
}

func TestAPackageLCOVReadsFromThePackageDirectory(t *testing.T) {
	spectest.Proves(t, "tooling/observe-dart", "dart-package-reports", "a-package-lcov-reads-from-the-package-directory")
	root, set := dartRepository(t)
	write(t, root, map[string]string{
		// flutter test --coverage writes paths relative to the package; the
		// root holds a file of the same path, which the package's wins over.
		"packages/pricing/coverage/lcov.info": "SF:lib/src/price.dart\nDA:1,1\nLF:1\nLH:1\nend_of_record\n",
		"apps/mobile/coverage/lcov.info":      "SF:lib/main.dart\nDA:1,0\nLF:1\nLH:0\nend_of_record\n",
	})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{
		{Path: "packages/pricing/coverage/lcov.info", Kind: reports.KindCoverage, Format: reports.FormatLCOV},
		{Path: "apps/mobile/coverage/lcov.info", Kind: reports.KindCoverage, Format: reports.FormatLCOV},
	}})
	want := []evidence.ComponentCoverage{
		{Component: "apps/mobile", CoveredLines: 0, TotalLines: 1},
		{Component: "packages/pricing", CoveredLines: 1, TotalLines: 1},
	}
	if !slices.Equal(r.Coverage, want) || len(r.Diagnostics) != 0 {
		t.Errorf("coverage %+v, diagnostics %v", r.Coverage, r.Diagnostics)
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

func TestAGoJUnitReportV1CaseGoesToThePackageItsSuiteNames(t *testing.T) {
	spectest.Proves(t, feature, "tests-and-coverage", "a-test-goes-to-the-component-of-its-file-else-of-its-report")
	root, set := repository(t)
	// go-junit-report v1 writes the last element of the import path as the
	// class name and the whole path as the suite name.
	write(t, root, map[string]string{"junit.xml": `<testsuites>
		<testsuite name="example.com/svc"><testcase classname="svc" name="TestCalc"/></testsuite>
	</testsuites>`})
	r := Run(Input{Root: root, Set: set, Reports: []reports.Report{{Path: "junit.xml", Kind: reports.KindJUnit, Format: reports.FormatJUnit}}})
	if got := tests(r); got["svc"].Passed != 1 || len(r.Diagnostics) != 0 {
		t.Errorf("tests %+v, diagnostics %v", got, r.Diagnostics)
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
