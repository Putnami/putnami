package readers

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "evidence-readers/report-readers"

var update = flag.Bool("update", false, "rewrite the golden files from the readers' output")

// coverageReaders and every reader name the readers by their fixture directory.
var coverageReaders = map[string]func([]byte) (CoverageReport, []diag.Diagnostic){
	"lcov":      ReadLCOV,
	"cobertura": ReadCobertura,
	"clover":    ReadClover,
	"gocover":   ReadGoCover,
	"jacoco":    ReadJaCoCo,
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fixtures lists the real reports of one fixture directory, golden files left out.
func fixtures(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("testdata", dir))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".golden.json") {
			paths = append(paths, filepath.Join("testdata", dir, entry.Name()))
		}
	}
	if len(paths) == 0 {
		t.Fatalf("testdata/%s holds no fixture", dir)
	}
	return paths
}

// golden compares result with the golden file of fixture, or rewrites it
// under -update.
func golden(t *testing.T, fixture string, result any) {
	t.Helper()
	got, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := fixture + ".golden.json"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := read(t, path)
	if !bytes.Equal(got, want) {
		t.Errorf("%s: output differs from %s; run the tests with -update and review the diff\ngot:\n%s", fixture, path, got)
	}
}

func TestRealJUnitReportsReadToTheirExpectedCases(t *testing.T) {
	for _, fixture := range fixtures(t, "junit") {
		report, diagnostics := ReadJUnit(read(t, fixture))
		if len(diagnostics) != 0 {
			t.Fatalf("%s: %v", fixture, diagnostics)
		}
		statuses := map[string]int{}
		for _, testCase := range report.Cases {
			statuses[testCase.Status]++
		}
		if statuses[StatusFailed] != 1 || statuses[StatusSkipped] != 1 || statuses[StatusPassed] < 2 {
			t.Errorf("%s: statuses %v, want one failed, one skipped and at least two passed", fixture, statuses)
		}
		golden(t, fixture, report)
	}
	spectest.Proves(t, feature, "junit-reads-every-case", "real-junit-reports-read-to-their-expected-cases")
}

func TestTheJUnitFileAttributeIsKept(t *testing.T) {
	jest, _ := ReadJUnit(read(t, "testdata/junit/jest.xml"))
	for _, testCase := range jest.Cases {
		if testCase.File != "src/calc.test.js" {
			t.Errorf("jest case %q file %q, want src/calc.test.js", testCase.Name, testCase.File)
		}
	}
	phpunit, _ := ReadJUnit(read(t, "testdata/junit/phpunit.xml"))
	if got := phpunit.Cases[0]; got.File != "/app/tests/CalcTest.php" || got.Line != 19 {
		t.Errorf("phpunit case file %q line %d, want /app/tests/CalcTest.php line 19", got.File, got.Line)
	}

	report, diagnostics := ReadJUnit([]byte(`<testsuite name="root" file="root.test.ts">
  <testsuite name="inner" file="C:\repo\inner.test.ts">
    <testcase name="from the nearest suite"/>
    <testcase name="its own" file="own.test.ts"/>
  </testsuite>
  <testsuite name="no file">
    <testcase name="from the root"/>
  </testsuite>
</testsuite>`))
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	files := make([]string, 0, len(report.Cases))
	for _, testCase := range report.Cases {
		files = append(files, testCase.File)
	}
	if want := []string{"C:/repo/inner.test.ts", "own.test.ts", "root.test.ts"}; !slices.Equal(files, want) {
		t.Errorf("files %v, want %v", files, want)
	}
	if want := []Suite{{Name: "root", File: "root.test.ts"}, {Name: "inner", File: "C:/repo/inner.test.ts"}}; !reflect.DeepEqual(report.Cases[0].Suites, want) {
		t.Errorf("suites %+v, want %+v", report.Cases[0].Suites, want)
	}
	spectest.Proves(t, feature, "junit-reads-every-case", "the-file-attribute-is-kept")
}

func TestAJUnitErrorElementFailsTheCase(t *testing.T) {
	report, diagnostics := ReadJUnit([]byte(`<testsuites tests="3" errors="1" failures="1">
  <testsuite name="s">
    <testcase name="errored"><error type="TypeError" message="x is undefined">stack</error></testcase>
    <testcase name="both"><failure message="first"/><error message="second"/></testcase>
    <testcase name="skipped"><skipped message="later"/></testcase>
  </testsuite>
</testsuites>`))
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	want := []TestCase{
		{Name: "errored", Suites: []Suite{{Name: "s"}}, Status: StatusFailed, Failure: &Failure{Error: true, Type: "TypeError", Message: "x is undefined", Body: "stack"}},
		{Name: "both", Suites: []Suite{{Name: "s"}}, Status: StatusFailed, Failure: &Failure{Message: "first"}},
		{Name: "skipped", Suites: []Suite{{Name: "s"}}, Status: StatusSkipped, Skipped: &Skipped{Message: "later"}},
	}
	if !reflect.DeepEqual(report.Cases, want) {
		t.Errorf("cases %+v, want %+v", report.Cases, want)
	}
	if report.Totals != (Totals{Tests: 3, Errors: 1, Failures: 1}) {
		t.Errorf("totals %+v", report.Totals)
	}
	spectest.Proves(t, feature, "junit-reads-every-case", "an-error-element-fails-the-case")
}

func TestJUnitDurationsRoundToMillisecondsAndClamp(t *testing.T) {
	report, _ := ReadJUnit([]byte(`<testsuites>
  <testcase name="a" time="0.0015"/>
  <testcase name="b" time="-1"/>
  <testcase name="c" time="1e300"/>
  <testcase name="d" time="NaN"/>
  <testcase name="e" time="soon"/>
</testsuites>`))
	durations := make([]int64, 0, len(report.Cases))
	for _, testCase := range report.Cases {
		durations = append(durations, testCase.DurationMs)
	}
	if want := []int64{2, 0, 1<<63 - 1, 0, 0}; !slices.Equal(durations, want) {
		t.Errorf("durations %v, want %v", durations, want)
	}
}

func TestRealCoverageReportsReadToTheirExpectedLines(t *testing.T) {
	for dir, reader := range coverageReaders {
		for _, fixture := range fixtures(t, dir) {
			report, diagnostics := reader(read(t, fixture))
			if len(diagnostics) != 0 {
				t.Fatalf("%s: %v", fixture, diagnostics)
			}
			if len(report.Files) == 0 {
				t.Errorf("%s: no file", fixture)
			}
			for _, file := range report.Files {
				if !slices.IsSortedFunc(file.Lines, func(a, b Line) int { return a.Number - b.Number }) {
					t.Errorf("%s: %s lines are not increasing", fixture, file.Path)
				}
			}
			golden(t, fixture, report)
		}
	}
	spectest.Proves(t, feature, "coverage-reads-every-instrumented-line", "real-coverage-reports-read-to-their-expected-lines")
}

func TestARepeatedLineKeepsItsLargestCount(t *testing.T) {
	lcov, _ := ReadLCOV([]byte("SF:a.js\nDA:2,3\nDA:1,0\nend_of_record\nSF:a.js\nDA:2,1\nDA:1,4\nend_of_record\n"))
	cobertura, _ := ReadCobertura([]byte(`<coverage><packages><package><classes>
  <class filename="a.js"><lines><line number="2" hits="3"/><line number="1" hits="0"/></lines></class>
  <class filename="a.js"><lines><line number="2" hits="1"/><line number="1" hits="4"/></lines></class>
</classes></package></packages></coverage>`))
	clover, _ := ReadClover([]byte(`<coverage><project>
  <file path="a.js"><line num="2" type="stmt" count="3"/><line num="1" type="stmt" count="0"/><line num="1" type="cond" count="4"/><line num="2" type="stmt" count="1"/></file>
</project></coverage>`))
	want := []FileCoverage{{Path: "a.js", Lines: []Line{{1, 4}, {2, 3}}, LineCounts: Counts{Total: 2, Covered: 2}}}
	for name, report := range map[string]CoverageReport{"lcov": lcov, "cobertura": cobertura, "clover": clover} {
		if !reflect.DeepEqual(report.Files, want) {
			t.Errorf("%s: files %+v, want %+v", name, report.Files, want)
		}
	}
	spectest.Proves(t, feature, "coverage-reads-every-instrumented-line", "a-repeated-line-keeps-its-largest-count")
}

func TestStatedTotalsStandWhenTheReportListsNone(t *testing.T) {
	stated, _ := ReadLCOV([]byte("TN:\nSF:a.ts\nFNF:5\nFNH:3\nLF:20\nLH:15\nend_of_record\n"))
	if want := []FileCoverage{{Path: "a.ts", LineCounts: Counts{Total: 20, Covered: 15}, Functions: Counts{Total: 5, Covered: 3}}}; !reflect.DeepEqual(stated.Files, want) {
		t.Errorf("stated: files %+v, want %+v", stated.Files, want)
	}
	counted, _ := ReadLCOV([]byte("SF:a.ts\nFN:1,f\nFN:4,9,g,h\nFNDA:2,f\nFNDA:0,g,h\nFNF:9\nFNH:9\nDA:1,2\nDA:2,0\nLF:9\nLH:9\nend_of_record\n"))
	if want := (FileCoverage{Path: "a.ts", Lines: []Line{{1, 2}, {2, 0}}, LineCounts: Counts{Total: 2, Covered: 1}, Functions: Counts{Total: 2, Covered: 1}}); !reflect.DeepEqual(counted.Files[0], want) {
		t.Errorf("counted: file %+v, want %+v", counted.Files[0], want)
	}
	jest, _ := ReadClover(read(t, "testdata/clover/jest.xml"))
	if got := jest.Files[0].Functions; got != (Counts{Total: 3, Covered: 2}) {
		t.Errorf("jest clover functions %+v, want the metrics' 3 and 2", got)
	}
	phpunit, _ := ReadClover(read(t, "testdata/clover/phpunit.xml"))
	if got := phpunit.Files[0].Functions; got != (Counts{Total: 3, Covered: 2}) {
		t.Errorf("phpunit clover functions %+v, want the method lines' 3 and 2", got)
	}
	spectest.Proves(t, feature, "coverage-reads-every-instrumented-line", "stated-totals-stand-when-the-report-lists-none")
}

func TestALineTakesTheLargestCountOfItsBlocks(t *testing.T) {
	report, diagnostics := ReadGoCover([]byte("mode: count\n" +
		"example.com/a/a.go:3.10,5.2 2 4\n" +
		"example.com/a/a.go:5.2,7.3 1 0\n" +
		"example.com/a/a.go:9.1,9.20 0 7\n"))
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	want := []FileCoverage{{Path: "example.com/a/a.go", Lines: []Line{{3, 4}, {4, 4}, {5, 4}, {6, 0}, {7, 0}}, LineCounts: Counts{Total: 5, Covered: 3}}}
	if !reflect.DeepEqual(report.Files, want) {
		t.Errorf("files %+v, want %+v", report.Files, want)
	}
	spectest.Proves(t, feature, "go-cover-blocks-become-lines", "a-line-takes-the-largest-count-of-its-blocks")
}

func TestARepeatedGoBlockSumsItsCounts(t *testing.T) {
	for _, tc := range []struct {
		mode, first, second string
		want                int64
	}{
		{"set", "1", "0", 1},
		{"count", "2", "3", 5},
		{"atomic", "2", "3", 5},
	} {
		block := "example.com/a/a.go:3.10,3.20 1 "
		report, diagnostics := ReadGoCover([]byte("mode: " + tc.mode + "\n" + block + tc.first + "\n" + block + tc.second + "\n"))
		if len(diagnostics) != 0 {
			t.Fatal(diagnostics)
		}
		if got := report.Files[0].Lines[0].Hits; got != tc.want {
			t.Errorf("mode %s: hits %d, want %d", tc.mode, got, tc.want)
		}
	}
	atomic, _ := ReadGoCover(read(t, "testdata/gocover/atomic.coverprofile"))
	if got := atomic.Files[1].Lines[0]; got != (Line{Number: 3, Hits: 2}) {
		t.Errorf("atomic fixture fmtx.go line 3 %+v, want the two binaries' 1 and 1 summed", got)
	}
	spectest.Proves(t, feature, "go-cover-blocks-become-lines", "a-repeated-block-sums-its-counts")
}

func TestConcatenatedGoProfilesReadAsOne(t *testing.T) {
	one, _ := ReadGoCover([]byte("mode: set\nexample.com/a/a.go:1.1,1.9 1 1\nexample.com/b/b.go:2.1,2.9 1 0\n"))
	two, diagnostics := ReadGoCover([]byte("mode: set\nexample.com/a/a.go:1.1,1.9 1 1\n\nmode: set\nexample.com/b/b.go:2.1,2.9 1 0\n"))
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	if !reflect.DeepEqual(one, two) {
		t.Errorf("concatenated %+v, want %+v", two, one)
	}
	spectest.Proves(t, feature, "go-cover-blocks-become-lines", "concatenated-profiles-read-as-one")
}

func TestRealJaCoCoReportsReadToTheirExpectedLines(t *testing.T) {
	for fixture, want := range map[string]FileCoverage{
		"testdata/jacoco/maven.xml": {
			Path:       "com/example/calc/Calc.java",
			Lines:      []Line{{8, 1}, {12, 1}, {13, 1}, {15, 0}, {20, 0}, {21, 0}},
			LineCounts: Counts{Total: 6, Covered: 3},
			Functions:  Counts{Total: 3, Covered: 2},
		},
		"testdata/jacoco/gradle-kotlin.xml": {
			Path:       "com/example/calc/Calc.kt",
			Lines:      []Line{{5, 1}, {9, 1}, {10, 1}, {12, 0}, {17, 0}, {18, 0}},
			LineCounts: Counts{Total: 6, Covered: 3},
			Functions:  Counts{Total: 3, Covered: 2},
		},
	} {
		report, diagnostics := ReadJaCoCo(read(t, fixture))
		if len(diagnostics) != 0 {
			t.Fatalf("%s: %v", fixture, diagnostics)
		}
		if len(report.Files) != 1 || !reflect.DeepEqual(report.Files[0], want) {
			t.Errorf("%s: files %+v, want %+v", fixture, report.Files, want)
		}
	}
	spectest.Proves(t, feature, "jacoco-reads-source-files", "real-jacoco-reports-read-to-their-expected-lines")
}

func TestJaCoCoGroupsAndTheDefaultPackageAreRead(t *testing.T) {
	report, diagnostics := ReadJaCoCo([]byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><!DOCTYPE report PUBLIC "-//JACOCO//DTD Report 1.1//EN" "report.dtd">
<report name="shop">
  <group name="api">
    <group name="nested">
      <package name="com/acme/api"><sourcefile name="Api.kt"><line nr="3" mi="0" ci="2"/><line nr="4" mi="0" ci="0"/></sourcefile></package>
    </group>
  </group>
  <package name=""><sourcefile name="Main.java"><line nr="1" mi="1" ci="0"/></sourcefile></package>
  <package name="com/acme/gen"><sourcefile name="Gen.java"><counter type="LINE" missed="2" covered="1"/><counter type="METHOD" missed="0" covered="1"/></sourcefile></package>
</report>`))
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	want := []FileCoverage{
		{Path: "com/acme/api/Api.kt", Lines: []Line{{3, 1}}, LineCounts: Counts{Total: 1, Covered: 1}},
		{Path: "Main.java", Lines: []Line{{1, 0}}, LineCounts: Counts{Total: 1}},
		{Path: "com/acme/gen/Gen.java", LineCounts: Counts{Total: 3, Covered: 1}, Functions: Counts{Total: 1, Covered: 1}},
	}
	if !reflect.DeepEqual(report.Files, want) {
		t.Errorf("files %+v, want %+v", report.Files, want)
	}
	spectest.Proves(t, feature, "jacoco-reads-source-files", "jacoco-groups-and-the-default-package-are-read")
}

func TestPathsAreKeptAsStated(t *testing.T) {
	clover, _ := ReadClover(read(t, "testdata/clover/jest.xml"))
	if got := clover.Files[0].Path; got != "/app/src/calc.js" {
		t.Errorf("clover path %q, want the path attribute /app/src/calc.js", got)
	}
	named, _ := ReadClover([]byte(`<coverage><project><package><file name="src\a.php"/></package><file name="b.php"/></project></coverage>`))
	if got := []string{named.Files[0].Path, named.Files[1].Path}; !slices.Equal(got, []string{"src/a.php", "b.php"}) {
		t.Errorf("clover names %v, want src/a.php then b.php", got)
	}
	cobertura, _ := ReadCobertura(read(t, "testdata/cobertura/phpunit.xml"))
	if !slices.Equal(cobertura.Sources, []string{"/app/src"}) || cobertura.Files[0].Path != "Calc.php" {
		t.Errorf("cobertura sources %v file %q, want /app/src and Calc.php", cobertura.Sources, cobertura.Files[0].Path)
	}
	goCover, _ := ReadGoCover(read(t, "testdata/gocover/set.coverprofile"))
	if got := goCover.Files[0].Path; got != "example.com/calc/calc.go" {
		t.Errorf("go cover path %q, want the import path", got)
	}
	lcov, _ := ReadLCOV([]byte("SF:src\\nested\\a.ts\nend_of_record\n"))
	if got := lcov.Files[0].Path; got != "src/nested/a.ts" {
		t.Errorf("lcov path %q, want slash form", got)
	}
	spectest.Proves(t, feature, "paths-as-the-report-states", "paths-are-kept-as-stated")
}

func TestEveryMalformedFixtureReturnsItsCode(t *testing.T) {
	var expectations map[string]struct{ Reader, Code string }
	if err := json.Unmarshal(read(t, "testdata/malformed/expectations.json"), &expectations); err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, fixture := range fixtures(t, "malformed") {
		name := filepath.Base(fixture)
		if name == "expectations.json" {
			continue
		}
		expectation, ok := expectations[name]
		if !ok {
			t.Errorf("%s has no expectation", name)
			continue
		}
		listed++
		var result any
		var diagnostics []diag.Diagnostic
		if expectation.Reader == "junit" {
			result, diagnostics = ReadFile(fixture, ReadJUnit)
		} else {
			result, diagnostics = ReadFile(fixture, coverageReaders[expectation.Reader])
		}
		if len(diagnostics) != 1 || diagnostics[0].Code != expectation.Code || diagnostics[0].Severity != diag.Error || diagnostics[0].Field != fixture {
			t.Errorf("%s: diagnostics %v, want one %s error on %s", name, diagnostics, expectation.Code, fixture)
		}
		if !reflect.ValueOf(result).IsZero() {
			t.Errorf("%s: result %+v, want none", name, result)
		}
	}
	if listed != len(expectations) {
		t.Errorf("%d fixtures for %d expectations", listed, len(expectations))
	}

	if _, diagnostics := ReadFile("testdata/no-such-report.xml", ReadJUnit); len(diagnostics) != 1 || diagnostics[0].Code != CodeMissing {
		t.Errorf("missing file: %v, want %s", diagnostics, CodeMissing)
	}
	if _, diagnostics := ReadFile("", ReadJUnit); len(diagnostics) != 1 || diagnostics[0].Code != CodeMissing {
		t.Errorf("empty path: %v, want %s", diagnostics, CodeMissing)
	}
	if _, diagnostics := ReadFile("testdata", ReadLCOV); len(diagnostics) != 1 || diagnostics[0].Code != CodeUnreadable {
		t.Errorf("directory: %v, want %s", diagnostics, CodeUnreadable)
	}
	huge := make([]byte, MaxReportBytes+1)
	for name, reader := range allReaders() {
		if diagnostics := reader(huge); len(diagnostics) != 1 || diagnostics[0].Code != CodeTooLarge {
			t.Errorf("%s over the bound: %v, want %s", name, diagnostics, CodeTooLarge)
		}
	}
	spectest.Proves(t, feature, "malformed-reports-are-diagnostics", "every-malformed-fixture-returns-its-code")
}

// allReaders adapts every reader to one shape.
func allReaders() map[string]func([]byte) []diag.Diagnostic {
	readers := map[string]func([]byte) []diag.Diagnostic{
		"junit": func(data []byte) []diag.Diagnostic { _, d := ReadJUnit(data); return d },
		"sbom":  func(data []byte) []diag.Diagnostic { _, d := ReadSBOM(data); return d },
	}
	for name, reader := range coverageReaders {
		readers[name] = func(data []byte) []diag.Diagnostic { _, d := reader(data); return d }
	}
	return readers
}

func TestAReportOverTheLineBoundIsTooLarge(t *testing.T) {
	var profile strings.Builder
	profile.WriteString("mode: set\n")
	for block := range MaxLineMarks/1000 + 1 {
		fmt.Fprintf(&profile, "example.com/a/a.go:1.%d,1000.1 1 1\n", 1+block)
	}
	if _, diagnostics := ReadGoCover([]byte(profile.String())); len(diagnostics) != 1 || diagnostics[0].Code != CodeTooLarge {
		t.Errorf("overlapping blocks over the bound: %v, want %s", diagnostics, CodeTooLarge)
	}
	var lcov strings.Builder
	lcov.WriteString("SF:a.js\n")
	for line := range MaxLineMarks + 1 {
		fmt.Fprintf(&lcov, "DA:%d,1\n", 1+line%10)
	}
	lcov.WriteString("end_of_record\n")
	if _, diagnostics := ReadLCOV([]byte(lcov.String())); len(diagnostics) != 1 || diagnostics[0].Code != CodeTooLarge {
		t.Errorf("repeated DA lines over the bound: %v, want %s", diagnostics, CodeTooLarge)
	}
	spectest.Proves(t, feature, "malformed-reports-are-diagnostics", "every-malformed-fixture-returns-its-code")
}

func TestANestedTestsuitesElementKeepsItsCases(t *testing.T) {
	report, diagnostics := ReadJUnit([]byte(`<testsuites><testsuites><testsuite name="s"><testcase name="x"/></testsuite></testsuites><testcase name="y"/></testsuites>`))
	if len(diagnostics) != 0 {
		t.Fatal(diagnostics)
	}
	want := []TestCase{{Name: "x", Suites: []Suite{{Name: "s"}}, Status: StatusPassed}, {Name: "y", Status: StatusPassed}}
	if !reflect.DeepEqual(report.Cases, want) {
		t.Errorf("cases %+v, want %+v", report.Cases, want)
	}
}

func TestJUnitSuitesNestUpToMaxSuiteDepth(t *testing.T) {
	nested := func(depth int) []byte {
		return []byte(strings.Repeat(`<testsuite name="s">`, depth) + `<testcase name="a"/><testcase name="b"/>` + strings.Repeat(`</testsuite>`, depth))
	}
	report, diagnostics := ReadJUnit(nested(MaxSuiteDepth))
	if diagnostics != nil || len(report.Cases) != 2 || len(report.Cases[1].Suites) != MaxSuiteDepth {
		t.Fatalf("%d suites: %v, %d cases, want 2 cases under %d suites", MaxSuiteDepth, diagnostics, len(report.Cases), MaxSuiteDepth)
	}
	if _, diagnostics := ReadJUnit(nested(MaxSuiteDepth + 1)); len(diagnostics) != 1 || diagnostics[0].Code != CodeMalformed {
		t.Errorf("%d suites: %v, want %s", MaxSuiteDepth+1, diagnostics, CodeMalformed)
	}
}

func TestCoberturaMethodsOfTwoClassesInOneFileStayDistinct(t *testing.T) {
	report, _ := ReadCobertura([]byte(`<coverage><packages><package><classes>
  <class name="A" filename="a.php"><methods><method name="run" signature="" hits="1"/></methods></class>
  <class name="B" filename="a.php"><methods><method name="run" signature="" hits="0"/></methods></class>
</classes></package></packages></coverage>`))
	if got := report.Files[0].Functions; got != (Counts{Total: 2, Covered: 1}) {
		t.Errorf("functions %+v, want two methods, one covered", got)
	}
}

// TestArbitraryBytesNeverPanic feeds every reader every fixture cut at every
// length and with each byte flipped in turn: each returns a result or one
// malformed or too_large diagnostic, and none panics. The fixtures that are
// too large stay out: TestEveryMalformedFixtureReturnsItsCode reads them.
// FuzzReaders explores further.
func TestArbitraryBytesNeverPanic(t *testing.T) {
	var expectations map[string]struct{ Code string }
	if err := json.Unmarshal(read(t, "testdata/malformed/expectations.json"), &expectations); err != nil {
		t.Fatal(err)
	}
	var inputs [][]byte
	for _, dir := range []string{"junit", "lcov", "cobertura", "clover", "gocover", "sbom", "malformed"} {
		for _, fixture := range fixtures(t, dir) {
			if expectations[filepath.Base(fixture)].Code == CodeTooLarge {
				continue
			}
			data := read(t, fixture)
			for cut := 0; cut <= len(data); cut += max(1, len(data)/64) {
				inputs = append(inputs, data[:cut])
			}
			for at := 0; at < len(data); at += max(1, len(data)/64) {
				flipped := slices.Clone(data)
				flipped[at] ^= 0xff
				inputs = append(inputs, flipped)
			}
		}
	}
	for name, reader := range allReaders() {
		for _, input := range inputs {
			diagnostics := reader(input)
			if len(diagnostics) > 1 || (len(diagnostics) == 1 && diagnostics[0].Code != CodeMalformed && diagnostics[0].Code != CodeTooLarge) {
				t.Fatalf("%s: %v for %q", name, diagnostics, input)
			}
		}
	}
	spectest.Proves(t, feature, "malformed-reports-are-diagnostics", "arbitrary-bytes-never-panic")
}

func FuzzReaders(f *testing.F) {
	for _, dir := range []string{"junit", "lcov", "cobertura", "clover", "gocover", "sbom"} {
		entries, _ := os.ReadDir(filepath.Join("testdata", dir))
		for _, entry := range entries {
			if data, err := os.ReadFile(filepath.Join("testdata", dir, entry.Name())); err == nil {
				f.Add(data)
			}
		}
	}
	readers := allReaders()
	f.Fuzz(func(t *testing.T, data []byte) {
		for name, reader := range readers {
			if diagnostics := reader(data); len(diagnostics) > 1 {
				t.Fatalf("%s: %v", name, diagnostics)
			}
		}
	})
}
