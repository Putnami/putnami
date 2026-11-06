package reports

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe"

const (
	junitXML     = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<testsuites name="jest tests" tests="2"><testsuite name="a"/></testsuites>`
	phpunitXML   = "\xef\xbb\xbf<?xml version=\"1.0\"?>\n<!-- generated -->\n<testsuite name=\"App\" tests=\"1\"></testsuite>"
	coberturaXML = `<?xml version="1.0" ?>` + "\n" + `<!DOCTYPE coverage SYSTEM "http://cobertura.sourceforge.net/xml/coverage-04.dtd">` + "\n" + `<coverage lines-valid="10" line-rate="0.5"><sources/></coverage>`
	cloverXML    = `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<coverage generated="1700000000"><project timestamp="1700000000"/></coverage>`
	lcovTrace    = "TN:\nSF:src/a.ts\nDA:1,1\nend_of_record\n"
	goProfile    = "mode: atomic\nexample.com/m/a.go:3.1,4.2 1 1\n"
	configXML    = `<?xml version="1.0"?><phpunit bootstrap="vendor/autoload.php"/>`
)

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func resolver(dir string) func(string) string {
	return func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
}

func codes(diags []diag.Diagnostic) []string {
	if len(diags) == 0 {
		return nil
	}
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

func TestReportsAreFoundByTheirContent(t *testing.T) {
	spectest.Proves(t, feature, "report-discovery", "reports-are-found-by-their-content")
	root := tree(t, map[string]string{
		"packages/web/junit.xml":            junitXML,
		"build/logs/report.xml":             phpunitXML,
		"coverage/cobertura-coverage.xml":   coberturaXML,
		"build/logs/clover.xml":             cloverXML,
		"packages/web/coverage/lcov.info":   lcovTrace,
		"coverage.out":                      goProfile,
		"phpunit.xml":                       configXML,
		"junit-looking-name.xml":            "<html/>",
		"node_modules/dep/junit.xml":        junitXML,
		"vendor/lib/clover.xml":             cloverXML,
		"docs/notes.txt":                    lcovTrace,
		"coverage-go.txt":                   goProfile,
		"packages/web/src/__snapshots__/x":  junitXML,
		"packages/api/test-results/out.xml": "  \n" + junitXML,
	})
	reports, diags := Collect(Request{Root: root, Resolve: resolver(root)})
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	want := []Report{
		{Path: "build/logs/clover.xml", Kind: KindCoverage, Format: FormatClover, Source: SourceDiscovered},
		{Path: "build/logs/report.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceDiscovered},
		{Path: "coverage-go.txt", Kind: KindCoverage, Format: FormatGoCover, Source: SourceDiscovered},
		{Path: "coverage.out", Kind: KindCoverage, Format: FormatGoCover, Source: SourceDiscovered},
		{Path: "coverage/cobertura-coverage.xml", Kind: KindCoverage, Format: FormatCobertura, Source: SourceDiscovered},
		{Path: "packages/api/test-results/out.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceDiscovered},
		{Path: "packages/web/coverage/lcov.info", Kind: KindCoverage, Format: FormatLCOV, Source: SourceDiscovered},
		{Path: "packages/web/junit.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceDiscovered},
	}
	if !reflect.DeepEqual(reports, want) {
		t.Fatalf("reports:\n got %+v\nwant %+v", reports, want)
	}
}

func TestSniffRecognizesEachFormat(t *testing.T) {
	spectest.Proves(t, feature, "report-discovery", "reports-are-found-by-their-content")
	for content, want := range map[string]string{
		junitXML:     FormatJUnit,
		phpunitXML:   FormatJUnit,
		coberturaXML: FormatCobertura,
		`<coverage version="1"><packages/></coverage>`: FormatCobertura,
		cloverXML:                         FormatClover,
		`<coverage><project/></coverage>`: FormatClover,
		lcovTrace:                         FormatLCOV,
		"SF:a.js\nend_of_record\n":        FormatLCOV,
		goProfile:                         FormatGoCover,
		configXML:                         "",
		`<report name="m"><group name="api"/></report>`: FormatJaCoCo,
		`<report><title/></report>`:                     "",
		`<coverage><unknown/></coverage>`:               "",
		"mode: fast\n":                                  "",
		"":                                              "",
		"<":                                             "",
	} {
		if got := Sniff([]byte(content)); got != want {
			t.Errorf("Sniff(%q) = %q, want %q", content, got, want)
		}
	}
}

func TestExplicitReportPathsReplaceDiscovery(t *testing.T) {
	spectest.Proves(t, feature, "report-discovery", "explicit-report-paths-replace-discovery")
	root := tree(t, map[string]string{
		"reports/unit/junit.xml":         junitXML,
		"reports/e2e/deep/junit.xml":     junitXML,
		"other/junit.xml":                junitXML,
		"app/coverage/lcov.info":         lcovTrace,
		"coverage/cobertura.xml":         coberturaXML,
		"reports/node_modules/junit.xml": junitXML,
	})
	invocation := filepath.Join(root, "app")
	reports, diags := Collect(Request{
		Root:     root,
		JUnit:    SplitList(" ../reports/**/*.xml , "),
		Coverage: SplitList("coverage/lcov.info"),
		Resolve:  resolver(invocation),
	})
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	want := []Report{
		{Path: "app/coverage/lcov.info", Kind: KindCoverage, Format: FormatLCOV, Source: SourceNamed},
		{Path: "reports/e2e/deep/junit.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceNamed},
		{Path: "reports/unit/junit.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceNamed},
	}
	if !reflect.DeepEqual(reports, want) {
		t.Fatalf("reports:\n got %+v\nwant %+v", reports, want)
	}

	// Naming one kind leaves the other discovered.
	reports, diags = Collect(Request{Root: root, Coverage: []string{"coverage/cobertura.xml"}, Resolve: resolver(root)})
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	var discoveredJUnit, namedCoverage int
	for _, r := range reports {
		switch {
		case r.Kind == KindJUnit && r.Source == SourceDiscovered:
			discoveredJUnit++
		case r.Kind == KindCoverage && r.Source == SourceNamed:
			namedCoverage++
		default:
			t.Fatalf("unexpected report %+v", r)
		}
	}
	if discoveredJUnit != 3 || namedCoverage != 1 {
		t.Fatalf("reports %+v", reports)
	}
}

func TestAMissingOrMalformedReportIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "report-discovery", "a-missing-or-malformed-report-is-a-diagnostic")
	root := tree(t, map[string]string{
		"junit.xml":     junitXML,
		"broken.xml":    "not xml at all",
		"lcov.info":     lcovTrace,
		"truncated.xml": "<?xml version=\"1.0\"?>\n<!-- cut",
	})
	for name, tc := range map[string]struct {
		req   Request
		codes []string
	}{
		"a named file that does not exist": {
			req:   Request{JUnit: []string{"missing.xml"}, Coverage: []string{"lcov.info"}},
			codes: []string{CodeReportMissing},
		},
		"a pattern that matches nothing": {
			req:   Request{JUnit: []string{"out/**/*.xml"}, Coverage: []string{"lcov.info"}},
			codes: []string{CodeReportMissing},
		},
		"a named file that is not a report": {
			req:   Request{JUnit: []string{"broken.xml", "truncated.xml"}, Coverage: []string{"lcov.info"}},
			codes: []string{CodeReportUnrecognized, CodeReportUnrecognized},
		},
		"a report of the other kind": {
			req:   Request{JUnit: []string{"junit.xml"}, Coverage: []string{"junit.xml"}},
			codes: []string{CodeReportUnrecognized},
		},
		"an invalid pattern": {
			req:   Request{JUnit: []string{"[.xml"}, Coverage: []string{"lcov.info"}},
			codes: []string{CodePatternInvalid},
		},
		"a directory": {
			req:   Request{JUnit: []string{"."}, Coverage: []string{"lcov.info"}},
			codes: []string{CodeReportMissing},
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := tc.req
			req.Root, req.Resolve = root, resolver(root)
			_, diags := Collect(req)
			if got := codes(diags); !reflect.DeepEqual(got, tc.codes) {
				t.Fatalf("codes %v, want %v: %v", got, tc.codes, diags)
			}
			for _, d := range diags {
				if d.Severity != diag.Warning {
					t.Fatalf("%s is %s, not a warning", d.Code, d.Severity)
				}
			}
		})
	}

	empty := tree(t, map[string]string{"README.md": "no reports\n"})
	reports, diags := Collect(Request{Root: empty, Resolve: resolver(empty)})
	if len(reports) != 0 || !reflect.DeepEqual(codes(diags), []string{CodeReportMissing, CodeReportMissing}) {
		t.Fatalf("an empty repository gives %+v %v", reports, diags)
	}
}

func TestAnUnreadableReportIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "report-discovery", "a-missing-or-malformed-report-is-a-diagnostic")
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	root := tree(t, map[string]string{"junit.xml": junitXML, "lcov.info": lcovTrace})
	if err := os.Chmod(filepath.Join(root, "junit.xml"), 0); err != nil {
		t.Fatal(err)
	}
	_, diags := Collect(Request{Root: root, JUnit: []string{"junit.xml"}, Coverage: []string{"lcov.info"}, Resolve: resolver(root)})
	if got := codes(diags); !reflect.DeepEqual(got, []string{CodeReportUnreadable}) {
		t.Fatalf("codes %v", got)
	}
}

func TestMatchSegments(t *testing.T) {
	spectest.Proves(t, feature, "report-discovery", "explicit-report-paths-replace-discovery")
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.xml", "a.xml", true},
		{"**/*.xml", "a/b/c.xml", true},
		{"*/junit.xml", "a/junit.xml", true},
		{"*/junit.xml", "a/b/junit.xml", false},
		{"a/**/b/*.xml", "a/b/x.xml", true},
		{"a/**/b/*.xml", "a/x/y/b/x.xml", true},
		{"a/**", "a/x/y", true},
		{"*.xml", "a/x.xml", false},
	} {
		if got := matchSegments(splitSlash(tc.pattern), splitSlash(tc.path)); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func splitSlash(s string) []string {
	return strings.Split(s, "/")
}

const (
	cycloneDXJSON = `{"$schema":"http://cyclonedx.org/schema/bom-1.5.schema.json","bomFormat": "CycloneDX","specVersion":"1.5","components":[]}`
	spdxJSON      = "{\n  \"spdxVersion\" : \"SPDX-2.3\",\n  \"SPDXID\": \"SPDXRef-DOCUMENT\"\n}"
)

func TestAnSBOMIsFoundByItsContentOrNamed(t *testing.T) {
	spectest.Proves(t, "tooling/observe-analyses", "sbom-documents", "an-sbom-is-found-by-its-content-or-named")
	for content, want := range map[string]string{
		cycloneDXJSON:                      FormatCycloneDX,
		spdxJSON:                           FormatSPDX,
		`{"bomFormat":"SomethingElse"}`:    "",
		`{"name":"bom","version":"1.0.0"}`: "",
		`[{"bomFormat":"CycloneDX"}]`:      "",
	} {
		if got := Sniff([]byte(content)); got != want {
			t.Errorf("Sniff(%q) = %q, want %q", content, got, want)
		}
	}

	t.Run("discovered by content among the json files an SBOM name marks", func(t *testing.T) {
		root := tree(t, map[string]string{
			"sbom.cdx.json":              cycloneDXJSON,
			"package.json":               cycloneDXJSON,
			"bom-notes.json":             `{"name":"bom"}`,
			"node_modules/dep/bom.json":  cycloneDXJSON,
			"reports/junit.xml":          junitXML,
			"coverage/lcov.info":         lcovTrace,
			"build/trivy-cyclonedx.json": "not json",
		})
		reports, diags := Collect(Request{Root: root, Resolve: resolver(root)})
		if len(diags) != 0 {
			t.Fatalf("diagnostics %v", diags)
		}
		var sboms []Report
		for _, r := range reports {
			if r.Kind == KindSBOM || r.Kind == KindBaseSBOM {
				sboms = append(sboms, r)
			}
		}
		want := []Report{{Path: "sbom.cdx.json", Kind: KindSBOM, Format: FormatCycloneDX, Source: SourceDiscovered}}
		if !reflect.DeepEqual(sboms, want) {
			t.Fatalf("sboms:\n got %+v\nwant %+v", sboms, want)
		}
	})

	t.Run("a named base SBOM is never the commit's, and is never discovered", func(t *testing.T) {
		root := tree(t, map[string]string{
			"sbom.cdx.json": cycloneDXJSON,
			"base.cdx.json": cycloneDXJSON,
		})
		reports, diags := Collect(Request{Root: root, Resolve: resolver(root), BaseSBOM: []string{"base.cdx.json"}, JUnit: []string{"none.xml"}, Coverage: []string{"none.info"}})
		if got := codes(diags); !reflect.DeepEqual(got, []string{CodeReportMissing, CodeReportMissing}) {
			t.Fatalf("codes %v", got)
		}
		want := []Report{
			{Path: "base.cdx.json", Kind: KindBaseSBOM, Format: FormatCycloneDX, Source: SourceNamed},
			{Path: "sbom.cdx.json", Kind: KindSBOM, Format: FormatCycloneDX, Source: SourceDiscovered},
		}
		if !reflect.DeepEqual(reports, want) {
			t.Fatalf("reports:\n got %+v\nwant %+v", reports, want)
		}
	})

	t.Run("a named SBOM outside the repository replaces discovery", func(t *testing.T) {
		root := tree(t, map[string]string{"sbom.cdx.json": cycloneDXJSON})
		outside := tree(t, map[string]string{"head.spdx.json": spdxJSON})
		named := filepath.Join(outside, "head.spdx.json")
		reports, _ := Collect(Request{Root: root, Resolve: resolver(root), SBOM: []string{named}})
		want := []Report{{Path: filepath.ToSlash(named), Kind: KindSBOM, Format: FormatSPDX, Source: SourceNamed}}
		var sboms []Report
		for _, r := range reports {
			if r.Kind == KindSBOM {
				sboms = append(sboms, r)
			}
		}
		if !reflect.DeepEqual(sboms, want) {
			t.Fatalf("sboms:\n got %+v\nwant %+v", sboms, want)
		}
	})

	t.Run("several documents for one side leave it unknown", func(t *testing.T) {
		root := tree(t, map[string]string{
			"a/sbom.cdx.json":  cycloneDXJSON,
			"b/sbom.spdx.json": spdxJSON,
			"base1.cdx.json":   cycloneDXJSON,
			"base2.cdx.json":   cycloneDXJSON,
		})
		reports, diags := Collect(Request{Root: root, Resolve: resolver(root), BaseSBOM: []string{"base*.cdx.json"}, JUnit: []string{"none.xml"}, Coverage: []string{"none.info"}})
		var ambiguous []diag.Diagnostic
		for _, d := range diags {
			if d.Code == CodeSBOMAmbiguous {
				ambiguous = append(ambiguous, d)
			}
		}
		if len(ambiguous) != 2 || ambiguous[0].Field != KindSBOM || ambiguous[1].Field != KindBaseSBOM {
			t.Fatalf("ambiguous %+v", ambiguous)
		}
		if !strings.Contains(ambiguous[0].Message, "a/sbom.cdx.json, b/sbom.spdx.json") || !strings.Contains(ambiguous[1].Message, "--base-sbom") {
			t.Fatalf("messages %q, %q", ambiguous[0].Message, ambiguous[1].Message)
		}
		if len(reports) != 0 {
			t.Fatalf("reports %+v", reports)
		}
	})

	t.Run("a named file that is not an SBOM is a diagnostic", func(t *testing.T) {
		root := tree(t, map[string]string{"package.json": `{"name":"app"}`, "junit.xml": junitXML})
		reports, diags := Collect(Request{Root: root, Resolve: resolver(root), SBOM: []string{"package.json"}, BaseSBOM: []string{"junit.xml"}, JUnit: []string{"junit.xml"}, Coverage: []string{"none.info"}})
		if got := codes(diags); !reflect.DeepEqual(got, []string{CodeReportMissing, CodeReportUnrecognized, CodeReportUnrecognized}) {
			t.Fatalf("codes %v", got)
		}
		if !strings.Contains(diags[2].Message, "junit.xml is not a base SBOM report (it reads as junit)") {
			t.Fatalf("message %q", diags[2].Message)
		}
		if len(reports) != 1 || reports[0].Kind != KindJUnit {
			t.Fatalf("reports %+v", reports)
		}
	})
}

// bazelOutputs writes the outputs of bazel test and bazel coverage outside
// the repository, and the convenience links Bazel leaves at its root.
func bazelOutputs(t *testing.T, root string) {
	t.Helper()
	outputBase := tree(t, map[string]string{
		"testlogs/pricing/pricing_test/test.xml":           junitXML,
		"testlogs/pricing/pricing_test/test.log":           "PASS\n",
		"testlogs/api/api_test/shard_1_of_2/test.xml":      junitXML,
		"testlogs/api/api_test/test.outputs/outputs.xml":   junitXML,
		"testlogs/api/api_test/test.outputs/test.xml":      junitXML,
		"testlogs/api/api_test/test_attempts/test.xml":     junitXML,
		"out/_coverage/_coverage_report.dat":               lcovTrace,
		"out/k8-fastbuild/testlogs/pricing/other/test.xml": junitXML,
	})
	for link, target := range map[string]string{"bazel-testlogs": "testlogs", "bazel-out": "out"} {
		if err := os.Symlink(filepath.Join(outputBase, target), filepath.Join(root, link)); err != nil {
			t.Skipf("symbolic links: %v", err)
		}
	}
}

func TestBazelReportsAreDiscoveredThroughTheConvenienceLinks(t *testing.T) {
	spectest.Proves(t, "tooling/observe-bazel", "bazel-reports", "bazel-reports-are-discovered-through-the-convenience-links")
	root := tree(t, map[string]string{"MODULE.bazel": "", "pricing/BUILD.bazel": ""})
	bazelOutputs(t, root)
	found, diags := Collect(Request{Root: root, Resolve: resolver(root)})
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	want := []Report{
		{Path: "bazel-out/_coverage/_coverage_report.dat", Kind: KindCoverage, Format: FormatLCOV, Source: SourceDiscovered},
		{Path: "bazel-testlogs/api/api_test/shard_1_of_2/test.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceDiscovered},
		{Path: "bazel-testlogs/pricing/pricing_test/test.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceDiscovered},
	}
	if !reflect.DeepEqual(found, want) {
		t.Errorf("reports %+v, want %+v", found, want)
	}

	// Patterns through the link find the same logs, named through it.
	named, diags := Collect(Request{Root: root, JUnit: []string{"bazel-testlogs/*/*/test.xml", "bazel-testlogs/*/*/shard_*/test.xml"}, Coverage: []string{"bazel-out/_coverage/_coverage_report.dat"}, Resolve: resolver(root)})
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	for i := range want {
		want[i].Source = SourceNamed
	}
	if !reflect.DeepEqual(named, want) {
		t.Errorf("named reports %+v, want %+v", named, want)
	}
}

func TestJVMReportsAreDiscovered(t *testing.T) {
	spectest.Proves(t, "tooling/observe-jvm", "jvm-report-discovery", "jvm-reports-are-discovered")
	jacocoXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><!DOCTYPE report PUBLIC "-//JACOCO//DTD Report 1.1//EN" "report.dtd"><report name="calc"><sessioninfo id="vm" start="1" dump="2"/></report>`
	surefireXML := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<testsuite xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" name="com.acme.PriceTest" tests="1"><testcase name="t" classname="com.acme.PriceTest"/></testsuite>`
	root := tree(t, map[string]string{
		"pricing/build/test-results/test/TEST-com.acme.PriceTest.xml":          surefireXML,
		"app/build/outputs/androidTest-results/connected/TEST-device-app-.xml": surefireXML,
		"pricing/build/reports/jacoco/test/jacocoTestReport.xml":               jacocoXML,
		"api/target/surefire-reports/TEST-com.acme.ApiTest.xml":                surefireXML,
		"api/target/failsafe-reports/TEST-com.acme.ApiIT.xml":                  surefireXML,
		"api/target/site/jacoco/jacoco.xml":                                    jacocoXML,
		"pricing/build/kover/report.xml":                                       jacocoXML,
		"app/build/intermediates/merged_test_results/TEST-copy.xml":            surefireXML,
		"app/build/generated/report.xml":                                       jacocoXML,
		"app/build/tmp/kotlin-classes/TEST-x.xml":                              surefireXML,
		"api/target/classes/TEST-resource.xml":                                 surefireXML,
		"api/target/test-classes/jacoco.xml":                                   jacocoXML,
		"api/target/generated-sources/annotations/x.xml":                       surefireXML,
		"docs/report.xml": `<report><title>not JaCoCo</title></report>`,
	})
	reports, diags := Collect(Request{Root: root, Resolve: resolver(root)})
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	want := []Report{
		{Path: "api/target/failsafe-reports/TEST-com.acme.ApiIT.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceDiscovered},
		{Path: "api/target/site/jacoco/jacoco.xml", Kind: KindCoverage, Format: FormatJaCoCo, Source: SourceDiscovered},
		{Path: "api/target/surefire-reports/TEST-com.acme.ApiTest.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceDiscovered},
		{Path: "app/build/outputs/androidTest-results/connected/TEST-device-app-.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceDiscovered},
		{Path: "pricing/build/kover/report.xml", Kind: KindCoverage, Format: FormatJaCoCo, Source: SourceDiscovered},
		{Path: "pricing/build/reports/jacoco/test/jacocoTestReport.xml", Kind: KindCoverage, Format: FormatJaCoCo, Source: SourceDiscovered},
		{Path: "pricing/build/test-results/test/TEST-com.acme.PriceTest.xml", Kind: KindJUnit, Format: FormatJUnit, Source: SourceDiscovered},
	}
	if !reflect.DeepEqual(reports, want) {
		t.Fatalf("reports:\n got %+v\nwant %+v", reports, want)
	}
}
