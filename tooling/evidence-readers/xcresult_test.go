package readers

import (
	"reflect"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestXCResultTestsReadToTheirExpectedCases(t *testing.T) {
	spectest.Proves(t, feature, "xcresult-tests-read-every-case", "an-xcresulttool-output-reads-to-its-expected-cases")
	want := map[string][]struct{ name, classname, status string }{
		"testdata/xcresult/xctest.json": {
			{"testAdd()", "CalcTests.CalcTests", StatusPassed},
			{"testSignOfNonNegativeInput()", "CalcTests.CalcTests", StatusPassed},
			{"testAddWrongOnPurpose()", "CalcTests.CalcTests", StatusFailed},
			{"testSkipped()", "CalcTests.CalcTests", StatusSkipped},
		},
		// A Swift Testing function outside a suite takes the bundle alone, and
		// a nested suite the outermost one.
		"testdata/xcresult/swift-testing.json": {
			{"addition()", "CalcTests", StatusPassed},
			{"positive()", "CalcTests.SignTests", StatusPassed},
			{"addWrongOnPurpose()", "CalcTests", StatusFailed},
			{"skipped()", "CalcTests", StatusSkipped},
		},
	}
	fixtures := fixtures(t, "xcresult")
	if len(fixtures) != len(want) {
		t.Fatalf("fixtures %v, want %d", fixtures, len(want))
	}
	for _, fixture := range fixtures {
		report, diagnostics := ReadFile(fixture, ReadXCResultTests)
		if len(diagnostics) != 0 {
			t.Fatalf("%s: %v", fixture, diagnostics)
		}
		cases := want[fixture]
		if len(report.Cases) != len(cases) {
			t.Fatalf("%s: %d cases, want %d", fixture, len(report.Cases), len(cases))
		}
		for i, c := range cases {
			got := report.Cases[i]
			if got.Name != c.name || got.Classname != c.classname || got.Status != c.status || got.File != "" {
				t.Errorf("%s: case %d is %q %q %s file %q, want %q %q %s and no file", fixture, i, got.Name, got.Classname, got.Status, got.File, c.name, c.classname, c.status)
			}
			if got.Suites[0].Name != "Calc" || got.Suites[1].Name != "CalcTests" {
				t.Errorf("%s: case %q suites %+v, want the plan and the bundle first", fixture, got.Name, got.Suites)
			}
		}
		if report.Totals != (Totals{}) {
			t.Errorf("%s: totals %+v, want none", fixture, report.Totals)
		}
		golden(t, fixture, report)
	}

	xctest, _ := ReadFile("testdata/xcresult/xctest.json", ReadXCResultTests)
	if got := xctest.Cases[0].DurationMs; got != 1 {
		t.Errorf("a duration string with a decimal comma: %d ms, want 1", got)
	}
	failed := xctest.Cases[2].Failure
	if failed == nil || failed.Message != `CalcTests.swift:14: XCTAssertEqual failed: ("4") is not equal to ("5") - two plus two should be five` || failed.Body != "" {
		t.Errorf("failure %+v, want its one message", failed)
	}
	if skipped := xctest.Cases[3].Skipped; skipped == nil || skipped.Message != "Test skipped - not ready" {
		t.Errorf("skip %+v, want its message", skipped)
	}
	swiftTesting, _ := ReadFile("testdata/xcresult/swift-testing.json", ReadXCResultTests)
	if got := swiftTesting.Cases[1].Suites; len(got) != 4 || got[3].Name != "NonNegative" {
		t.Errorf("nested suites %+v, want the plan, the bundle and both suites", got)
	}
	retried := swiftTesting.Cases[2]
	if retried.DurationMs != 2 || retried.Failure == nil || retried.Failure.Body == "" {
		t.Errorf("retried case %+v, want durationInSeconds and the messages of both repetitions", retried)
	}
}

func TestAnXCResultResultMapsToAStatus(t *testing.T) {
	spectest.Proves(t, feature, "xcresult-tests-read-every-case", "an-xcresult-result-maps-to-a-status")
	for result, status := range map[string]string{
		"Passed": StatusPassed, "Expected Failure": StatusPassed, "Failed": StatusFailed,
		"Skipped": StatusSkipped, "unknown": StatusSkipped, "": StatusSkipped,
	} {
		report, diagnostics := ReadXCResultTests([]byte(`{"testNodes":[{"name":"PricingTests","nodeType":"UI test bundle","children":[
  {"name":"Device","nodeType":"Device","children":[{"name":"testPrice()","nodeType":"Test Case","result":"` + result + `","duration":"1.5s",
    "children":[{"name":"Arguments","nodeType":"Arguments","children":[{"name":"why","nodeType":"Failure Message"}]}]}]}]}]}`))
		if len(diagnostics) != 0 {
			t.Fatalf("%q: %v", result, diagnostics)
		}
		want := TestCase{Name: "testPrice()", Classname: "PricingTests", Suites: []Suite{{Name: "PricingTests"}}, Status: status, DurationMs: 1500}
		switch status {
		case StatusFailed:
			want.Failure = &Failure{Message: "why"}
		case StatusSkipped:
			want.Skipped = &Skipped{Message: "why"}
		}
		if !reflect.DeepEqual(report.Cases, []TestCase{want}) {
			t.Errorf("%q: cases %+v, want %+v", result, report.Cases, want)
		}
	}
	for _, duration := range []string{`"duration":"Infs"`, `"duration":"1m 5s"`, `"duration":"NaNs"`} {
		report, diagnostics := ReadXCResultTests([]byte(`{"testNodes":[{"name":"t","nodeType":"Test Case","result":"Passed",` + duration + `}]}`))
		if diagnostics != nil || report.Cases[0].DurationMs != 0 {
			t.Errorf("%s: %+v %v, want 0 ms", duration, report.Cases, diagnostics)
		}
	}
	if report, diagnostics := ReadXCResultTests([]byte(`{"testNodes":[]}`)); diagnostics != nil || report.Cases == nil || len(report.Cases) != 0 {
		t.Errorf("no node: %+v %v, want no case", report, diagnostics)
	}
}

func TestXCResultNodesNestUpToMaxSuiteDepth(t *testing.T) {
	nested := func(depth int) []byte {
		data := `{"name":"t","nodeType":"Test Case","result":"Passed"}`
		for range depth - 1 {
			data = `{"name":"s","nodeType":"Test Suite","children":[` + data + `]}`
		}
		return []byte(`{"testNodes":[` + data + `]}`)
	}
	if report, diagnostics := ReadXCResultTests(nested(MaxSuiteDepth)); diagnostics != nil || len(report.Cases) != 1 {
		t.Fatalf("%d nodes: %v, %d cases", MaxSuiteDepth, diagnostics, len(report.Cases))
	}
	if _, diagnostics := ReadXCResultTests(nested(MaxSuiteDepth + 1)); len(diagnostics) != 1 || diagnostics[0].Code != CodeMalformed {
		t.Errorf("%d nodes: %v, want %s", MaxSuiteDepth+1, diagnostics, CodeMalformed)
	}
}

func TestAnXccovReportReadsToItsFileTotals(t *testing.T) {
	spectest.Proves(t, feature, "xccov-reads-file-totals-and-lines", "an-xccov-report-reads-to-its-file-totals")
	report, diagnostics := ReadFile("testdata/xccov-report/xcodebuild.json", ReadXccovReport)
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	want := []FileCoverage{
		{Path: "/Users/runner/work/calc/calc/Sources/Calc/Calc.swift", LineCounts: Counts{Total: 14, Covered: 8}, Functions: Counts{Total: 3, Covered: 2}},
		{Path: "/Users/runner/work/calc/calc/Tests/CalcTests/CalcTests.swift", LineCounts: Counts{Total: 12, Covered: 12}, Functions: Counts{Total: 4, Covered: 4}},
	}
	if !reflect.DeepEqual(report.Files, want) {
		t.Errorf("files %+v, want %+v", report.Files, want)
	}

	twice, diagnostics := ReadXccovReport([]byte(`{"targets":[
  {"files":[{"path":"/w/a.swift","coveredLines":2,"executableLines":5}]},
  {"files":[{"path":"/w/a.swift","coveredLines":3,"executableLines":4,"functions":[{"name":"f","lineNumber":1,"coveredLines":0,"executionCount":0}]}]}]}`))
	if diagnostics != nil || !reflect.DeepEqual(twice.Files, []FileCoverage{{Path: "/w/a.swift", LineCounts: Counts{Total: 5, Covered: 3}, Functions: Counts{Total: 1}}}) {
		t.Errorf("a file two targets list: %+v %v, want the largest totals", twice.Files, diagnostics)
	}
}

func TestAnXccovArchiveReadsToItsExecutableLines(t *testing.T) {
	spectest.Proves(t, feature, "xccov-reads-file-totals-and-lines", "an-xccov-archive-reads-to-its-executable-lines")
	report, diagnostics := ReadFile("testdata/xccov-archive/xcodebuild.json", ReadXccovArchive)
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	paths := make([]string, 0, len(report.Files))
	for _, file := range report.Files {
		paths = append(paths, file.Path)
	}
	if want := []string{"/Users/runner/work/calc/calc/Sources/Calc/Calc.swift", "/Users/runner/work/calc/calc/Tests/CalcTests/CalcTests.swift"}; !slices.Equal(paths, want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
	calc := report.Files[0]
	var uncovered []int
	for _, line := range calc.Lines {
		if line.Hits == 0 {
			uncovered = append(uncovered, line.Number)
		}
	}
	if calc.LineCounts != (Counts{Total: 14, Covered: 8}) || !slices.Equal(uncovered, []int{8, 9, 13, 14, 15, 16}) {
		t.Errorf("Calc.swift totals %+v, uncovered %v, want 8 of 14 lines and the else branch and neverCalled uncovered", calc.LineCounts, uncovered)
	}
	if empty, diagnostics := ReadXccovArchive([]byte(" {} ")); diagnostics != nil || len(empty.Files) != 0 {
		t.Errorf("an empty archive: %+v %v, want no file", empty, diagnostics)
	}
}
