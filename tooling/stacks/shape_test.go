package stacks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// trxRun is a TRX report with one passed, one failed and one skipped case.
const trxRun = `<?xml version="1.0" encoding="utf-8"?>
<TestRun id="1" name="run" xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010">
  <Results>
    <UnitTestResult testId="a" testName="Adds" duration="00:00:00.0010000" outcome="Passed" />
    <UnitTestResult testId="b" testName="Rounds" duration="00:00:00.0010000" outcome="Failed"><Output><ErrorInfo><Message>off by one</Message></ErrorInfo></Output></UnitTestResult>
    <UnitTestResult testId="c" testName="Later" outcome="NotExecuted" />
  </Results>
  <TestDefinitions>
    <UnitTest id="a"><TestMethod codeBase="/w/Pricing.Tests.dll" className="Pricing.Tests.CalculatorTest" /></UnitTest>
    <UnitTest id="b"><TestMethod codeBase="/w/Pricing.Tests.dll" className="Pricing.Tests.CalculatorTest" /></UnitTest>
    <UnitTest id="c"><TestMethod codeBase="/w/Pricing.Tests.dll" className="Pricing.Tests.CalculatorTest" /></UnitTest>
  </TestDefinitions>
  <ResultSummary outcome="Failed"><Counters total="3" executed="2" passed="1" failed="1" notExecuted="1" /></ResultSummary>
</TestRun>`

func TestAnExclusionKeepsBuildOutputsOutOfTheSelector(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "crates/api/Cargo.toml", "")
	writeFile(t, root, "crates/api/src/lib.rs", "")
	writeFile(t, root, "crates/api/target/debug/gen.rs", "")
	writeFile(t, root, "crates/api/nested/target/gen.rs", "")
	s := testStack(t)
	s.Profile.Inputs["sources"] = []string{"**/*.rs", "!**/target/**"}

	got := resolveOne(t, s, root, "crates/api", "rust:lint")
	if want := []string{"Cargo.toml", "src/lib.rs"}; got.Error != "" || !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("lint = %+v, want files %v", got, want)
	}
	if !slices.Contains(got.Basis.Content[0], "!**/target/**") {
		t.Errorf("the basis carries the exclusion: %v", got.Basis.Content)
	}
	// The basis selects what the answer read: a build output it excludes
	// leaves the digest, so the answer, unchanged.
	project := filepath.Join(root, "crates/api")
	before, err := wsproto.InputBasisDigest(project, got.Basis)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "crates/api/target/debug/gen.rs", "changed")
	writeFile(t, root, "crates/api/target/debug/new.rs", "")
	if after, _ := wsproto.InputBasisDigest(project, got.Basis); after != before {
		t.Errorf("an excluded file moved the basis digest")
	}
	writeFile(t, root, "crates/api/src/lib.rs", "changed")
	if after, _ := wsproto.InputBasisDigest(project, got.Basis); after == before {
		t.Errorf("a selected file left the basis digest unchanged")
	}
}

func TestTRXReportsCountWithTheJUnitOnes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "out/run.trx", trxRun)
	writeFile(t, dir, "out/junit.xml", junitPass)
	s, d := ReadReports(&Reports{JUnit: []string{"out/junit.xml"}, TRX: []string{"out/*.trx"}}, globUnder(dir), dir, Attribution{})
	if s.Tests != (TestTotals{Total: 6, Passed: 3, Failed: 1, Skipped: 2}) {
		t.Errorf("tests = %+v", s.Tests)
	}
	if codes(d) != CodeTestFailed || !strings.Contains(d[0].Message, "Pricing.Tests.CalculatorTest > Rounds") {
		t.Errorf("diagnostics = %v", d)
	}
	if !reflect.DeepEqual(s.Reports, []string{"out/junit.xml", "out/run.trx"}) {
		t.Errorf("reports = %v", s.Reports)
	}

	_, d = ReadReports(&Reports{TRX: []string{"out/none/*.trx"}}, globUnder(dir), dir, Attribution{})
	if codes(d) != CodeReportMissing {
		t.Errorf("a TRX pattern that matches nothing is a missing report: %v", d)
	}
	// One test report, of either format, is enough.
	if _, d = ReadReports(&Reports{JUnit: []string{"out/none.xml"}, TRX: []string{"out/*.trx"}}, globUnder(dir), dir, Attribution{}); diag.HasErrors(d) && codes(d) != CodeTestFailed {
		t.Errorf("a JUnit pattern with no match beside a TRX report is no missing report: %v", d)
	}
}

// runMeta runs task on crates/api with the given metadata.
func runMeta(t *testing.T, s Stack, root, task string, meta Metadata) (string, map[string]any, *recorder) {
	t.Helper()
	data, _ := json.Marshal(meta)
	rec := &recorder{}
	var out strings.Builder
	status, result, _ := s.run(context.Background(), runInput{
		task: task, workspaceRoot: root, projectAbs: filepath.Join(root, "crates/api"),
		metadata: data, outputRoot: filepath.Join(root, ".putnami/out/api/"+task), stdout: &out,
	}, rec)
	return status, result, rec
}

func TestAComponentRunsOnlyTheTasksItsDetectorNames(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	// A machine version that would fail the check: a skipped task never
	// reaches it.
	stubTools(t, "1.0.0", junitPass)

	s := testStack(t)
	s.Detect = func(string, wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		return []Component{{Path: "crates/api", Name: "api", SourceFile: "crates/api/Cargo.toml", Root: ".", Tasks: []string{TaskBuild, TaskLint}}}, nil
	}
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"crates/api"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe = %+v %v", result, err)
	}
	var meta Metadata
	if err := json.Unmarshal(result.Projects[0].Metadata, &meta); err != nil || !reflect.DeepEqual(meta.Tasks, []string{TaskLint, TaskBuild}) {
		t.Fatalf("metadata tasks = %v (%v), want them in run order", meta.Tasks, err)
	}

	status, data, rec := runMeta(t, s, root, TaskTest, meta)
	if status != statusOK || len(rec.diags) != 0 || data != nil {
		t.Errorf("a task the component does not run passes with nothing: %s %v %v", status, rec.diags, data)
	}
	if _, err := os.Stat(filepath.Join(root, "log")); err == nil {
		t.Errorf("a skipped task ran a step")
	}
	if status, _, rec := runMeta(t, s, root, TaskLint, meta); status != statusFailed || rec.codes() != "error:"+CodeToolchainMismatch {
		t.Errorf("a task the component runs is checked: %s %v", status, rec.diags)
	}
}

func TestAStepNamesTheComponentManifest(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s := testStack(t)
	lint := s.Profile.Tasks[TaskLint]
	lint.Steps = [][]string{{"cargo", "fmt", "--manifest-path", "{manifest}"}}
	s.Profile.Tasks[TaskLint] = lint

	meta := Metadata{Component: "api", Root: ".", Manifest: "crates/api/Cargo.toml"}
	if status, _, rec := runMeta(t, s, root, TaskLint, meta); status != statusOK {
		t.Fatalf("lint: %s %v", status, rec.diags)
	}
	log, _ := os.ReadFile(filepath.Join(root, "log"))
	if want := "cargo fmt --manifest-path " + filepath.Join(root, "crates/api/Cargo.toml"); !strings.Contains(string(log), want) {
		t.Errorf("log = %s, want %s", log, want)
	}

	meta.Manifest = ""
	if status, _, rec := runMeta(t, s, root, TaskLint, meta); status != statusFailed || rec.codes() != "error:"+CodeProjectConfigInvalid {
		t.Errorf("a step that names {manifest} without one fails: %s %v", status, rec.diags)
	}
}

func TestAStackVersionRuleReplacesTheCoreOne(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.83.1", junitPass)
	s := testStack(t)
	meta := Metadata{Component: "api", Root: "."}
	if status, _, rec := runMeta(t, s, root, TaskLint, meta); status != statusFailed || rec.codes() != "error:"+CodeToolchainMismatch {
		t.Fatalf("the core rule refuses 1.83.1 for 1.82.0: %s %v", status, rec.diags)
	}

	var asked []string
	s.MatchVersion = func(r string, expected Expected, machine string) bool {
		asked = append(asked, r, expected.Version, expected.Source, machine)
		return true
	}
	if status, _, rec := runMeta(t, s, root, TaskLint, meta); status != statusOK || len(rec.diags) != 0 {
		t.Errorf("the stack's rule accepts it: %s %v", status, rec.diags)
	}
	if want := []string{root, "1.82.0", "rust-toolchain.toml", "1.83.1"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("the rule was asked %v, want %v", asked, want)
	}
	s.MatchVersion = func(string, Expected, string) bool { return false }
	if status, _, _ := runMeta(t, s, root, TaskLint, meta); status != statusFailed {
		t.Errorf("the stack's rule refuses it: %s", status)
	}
}

func TestTheOutputDirectoryStartsEmpty(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s := testStack(t)
	test := s.Profile.Tasks[TaskTest]
	test.Steps = [][]string{{"cargo", "test"}}
	test.Reports = &Reports{TRX: []string{"{output}/*.trx"}}
	s.Profile.Tasks[TaskTest] = test

	stale := filepath.Join(root, ".putnami/out/api/test/test/old.trx")
	writeFile(t, root, ".putnami/out/api/test/test/old.trx", trxRun)
	status, _, rec := runMeta(t, s, root, TaskTest, Metadata{Component: "api", Root: "."})
	if status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeReportMissing) || strings.Contains(rec.codes(), CodeTestFailed) {
		t.Errorf("a report an earlier run left is not read: %s %v", status, rec.diags)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("the stale report is still there")
	}
}

func TestTheProbeWatchesTheManifestAndWhatTheDetectorRead(t *testing.T) {
	root := t.TempDir()
	s := testStack(t)
	s.Profile.Markers = []string{"*.csproj"}
	s.Detect = func(string, wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		return []Component{{Path: "src/Api", Name: "Api", SourceFile: "src/Api/Api.csproj", Root: ".", Watched: []string{"src/Directory.Build.props"}}}, nil
	}
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"src/Api"}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("probe = %+v %v", result, err)
	}
	watched := result.Projects[0].WatchedFiles
	for _, want := range []string{"src/Api/Api.csproj", "src/Directory.Build.props"} {
		if !slices.Contains(watched, want) {
			t.Errorf("watches no %s: %v", want, watched)
		}
	}
	if slices.Contains(watched, "src/Api/*.csproj") {
		t.Errorf("a wildcard marker is watched as a file: %v", watched)
	}
}

// stubTest puts first on PATH a rustc that prints 1.82.0 and a cargo that
// writes report into the directory it is given and exits with code.
func stubTest(t *testing.T, report string, code int) {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, bin, "rustc", "#!/bin/sh\necho 'rustc 1.82.0 (stub)'\n")
	writeFile(t, bin, "report.trx", report)
	writeFile(t, bin, "cargo", "#!/bin/sh\nif [ -n \""+report+"\" ]; then cp \""+filepath.Join(bin, "report.trx")+"\" \"$2/run.trx\"; fi\nexit "+strconv.Itoa(code)+"\n")
	for _, f := range []string{"rustc", "cargo"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAFailedTestStepStillReadsItsReports(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	s := testStack(t)
	test := s.Profile.Tasks[TaskTest]
	test.Steps = [][]string{{"cargo", "test", "{output}"}}
	test.Reports = &Reports{TRX: []string{"{output}/*.trx"}}
	s.Profile.Tasks[TaskTest] = test
	meta := Metadata{Component: "api", Root: "."}

	stubTest(t, trxRun, 1)
	status, data, rec := runMeta(t, s, root, TaskTest, meta)
	if status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeTestFailed) || !strings.Contains(strings.Join(rec.diags, "\n"), "Rounds") {
		t.Errorf("a failed case names the failure: %s %v", status, rec.diags)
	}
	if got, _ := data["testSummary"].(map[string]any); got["failed"] != 1 || got["total"] != 3 {
		t.Errorf("the summary is returned: %v", data)
	}

	stubTest(t, "", 3)
	if status, _, rec := runMeta(t, s, root, TaskTest, meta); status != statusFailed || rec.codes() != "error:"+CodeStepFailed {
		t.Errorf("a step that fails with no failed case is a failed step, not a missing report: %s %v", status, rec.diags)
	}
}

func TestAToolchainListsItsVersionsAndAStackDecidesAMatch(t *testing.T) {
	tc := mustProfile(t).Toolchain
	tc.Command = []string{"sh", "-c", "printf '8.0.133 [/sdk]\\n9.0.100 [/sdk]\\n'"}
	tc.Pattern = `(?m)^(?<version>[0-9]+\.[0-9]+\.[0-9]+)\s`
	versions, err := tc.MachineVersions(context.Background(), t.TempDir(), os.Environ())
	if err != nil || !reflect.DeepEqual(versions, []string{"8.0.133", "9.0.100"}) {
		t.Fatalf("versions = %v %v", versions, err)
	}
	if first, _ := tc.MachineVersion(context.Background(), t.TempDir(), os.Environ()); first != "8.0.133" {
		t.Errorf("MachineVersion = %s", first)
	}
	loose := func(e, m string) bool { return strings.HasPrefix(m, e[:2]) }
	if v := tc.CompareWith(Expected{Version: "8.0.100", Source: "global.json"}, "8.0.133", loose); v.Code != "" {
		t.Errorf("the stack's rule decides an exact version: %+v", v)
	}
	if v := tc.CompareWith(Expected{Version: "7.0.100", Source: "global.json"}, "8.0.133", loose); v.Code != CodeToolchainMismatch {
		t.Errorf("a version the rule refuses is a mismatch: %+v", v)
	}
}

func TestATaskAsksTheDetectorAgainWhenItRuns(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	stubTest(t, trxRun, 0)
	s := testStack(t)
	test := s.Profile.Tasks[TaskTest]
	test.Steps = [][]string{{"cargo", "test", "{output}"}}
	test.Reports = &Reports{TRX: []string{"{output}/*.trx"}}
	s.Profile.Tasks[TaskTest] = test
	tasks := []string{TaskLint, TaskTest}
	s.Detect = func(string, wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		return []Component{{Path: "crates/api", Name: "api", Root: ".", Tasks: tasks}}, nil
	}
	// The probe said lint only; the detector now names test too.
	meta := Metadata{Component: "api", Root: ".", Tasks: []string{TaskLint}}
	if status, data, rec := runMeta(t, s, root, TaskTest, meta); status != statusFailed || data["testSummary"] == nil {
		t.Errorf("the fresh answer runs the test: %s %v %v", status, rec.diags, data)
	}
	tasks = []string{TaskLint}
	meta.Tasks = []string{TaskLint, TaskTest}
	if status, data, _ := runMeta(t, s, root, TaskTest, meta); status != statusOK || data != nil {
		t.Errorf("the fresh answer skips the test: %s %v", status, data)
	}

	tasks = []string{"publish"}
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"crates/api"}})
	if err != nil || len(result.Projects) != 0 || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != CodeNotAComponent {
		t.Errorf("a component that runs no profile task is no project: %+v %v", result, err)
	}
}

func TestNoExclusionRemovesTheProjectConfig(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "crates/api/Cargo.toml", "")
	writeFile(t, root, "crates/api/src/lib.rs", "")
	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"inputs": {"sources": ["src/**", "!**/*.json"]}}}}`)
	got := resolveOne(t, testStack(t), root, "crates/api", "rust:lint")
	if !slices.Contains(got.Files, "putnami.json") || len(got.Basis.Content) != 2 || !slices.Contains(got.Basis.Content[1], "putnami.json") || slices.ContainsFunc(got.Basis.Content[1], func(p string) bool { return strings.HasPrefix(p, "!") }) {
		t.Errorf("putnami.json stays an input and a basis set of its own: %+v", got)
	}
}

func TestARootProjectAnswerLeavesOutVersionControlAndPutnamiState(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Cargo.toml", "")
	writeFile(t, root, "src/lib.rs", "")
	writeFile(t, root, ".git/HEAD", "ref")
	writeFile(t, root, ".putnami/out/a", "")
	writeFile(t, root, "tests/fixtures/broken/Cargo.toml", "not toml [")
	s := testStack(t)
	s.Profile.Inputs["sources"] = []string{"**"}
	got := resolveOne(t, s, root, ".", "rust:lint")
	// A marker directory the detector names no project stays the crate's.
	if want := []string{"Cargo.toml", "src/lib.rs", "tests/fixtures/broken/Cargo.toml"}; !reflect.DeepEqual(got.Files, want) {
		t.Errorf("files = %v, want %v", got.Files, want)
	}
	before, err := wsproto.InputBasisDigest(root, got.Basis)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".git/index", ".putnami/out/b"} {
		writeFile(t, root, f, "written")
	}
	if after, _ := wsproto.InputBasisDigest(root, got.Basis); after != before {
		t.Errorf("a write to version-control or Putnami state moved the basis")
	}
}

func TestAFallbackNamesTheInstalledVersionsWhenTheCommandFails(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	bin := t.TempDir()
	writeFile(t, bin, "rustc", "#!/bin/sh\nif [ \"$1\" = \"--list\" ]; then echo 'rustc 1.80.0'; echo 'rustc 1.81.0'; exit 0; fi\necho 'no toolchain 1.82.0 installed' >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "rustc"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := testStack(t)
	meta := Metadata{Component: "api", Root: "."}
	if status, _, rec := runMeta(t, s, root, TaskLint, meta); status != statusFailed || rec.codes() != "error:"+CodeToolchainMissing {
		t.Errorf("without a fallback, a failing command is a missing toolchain: %s %v", status, rec.diags)
	}
	s.Profile.Toolchain.Fallback = []string{"rustc", "--list"}
	s.Profile.Toolchain.Pattern = `(?m)^rustc (?<version>\S+)`
	status, _, rec := runMeta(t, s, root, TaskLint, meta)
	if status != statusFailed || rec.codes() != "error:"+CodeToolchainMismatch || !strings.Contains(rec.diags[0], "1.80.0, 1.81.0") {
		t.Errorf("the fallback names the installed versions in a mismatch: %s %v", status, rec.diags)
	}
	// A listed version that satisfies the expected one leaves the failure
	// unexplained: the toolchain is missing, not mismatched.
	writeFile(t, bin, "rustc", "#!/bin/sh\nif [ \"$1\" = \"--list\" ]; then echo 'rustc 1.82.0'; exit 0; fi\nexit 1\n")
	if status, _, rec := runMeta(t, s, root, TaskLint, meta); status != statusFailed || rec.codes() != "error:"+CodeToolchainMissing {
		t.Errorf("a satisfying listed version is no mismatch: %s %v", status, rec.diags)
	}
	writeFile(t, bin, "rustc", "#!/bin/sh\nif [ \"$1\" = \"--list\" ]; then echo 'rustc 1.80.0'; echo 'rustc 1.81.0'; exit 0; fi\nexit 1\n")
	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"toolchain": {"check": "warn"}, "tasks": {"lint": {"steps": [["true"]]}}}}}`)
	if status, _, rec := runMeta(t, s, root, TaskLint, meta); status != statusOK || rec.codes() != "warning:"+CodeToolchainMismatch {
		t.Errorf("warn relaxes it: %s %v", status, rec.diags)
	}
}

func TestCoverletPathsAndCrossComponentCoverage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/Pricing/Pricing.csproj", "")
	writeFile(t, root, "src/Pricing/Calculator.cs", "")
	writeFile(t, root, "tests/Pricing.Tests/Pricing.Tests.csproj", "")
	// Coverlet on Linux: the source "/", each file the absolute path without
	// its leading slash.
	rel := strings.TrimPrefix(filepath.ToSlash(filepath.Join(root, "src/Pricing/Calculator.cs")), "/")
	writeFile(t, root, "out/coverage.cobertura.xml", `<coverage><sources><source>/</source></sources><packages><package name="p"><classes>
		<class name="c" filename="`+rel+`"><lines><line number="1" hits="1"/><line number="2" hits="0"/></lines></class>
	</classes></package></packages></coverage>`)
	s := testStack(t)
	s.Profile.Markers = []string{"*.csproj"}
	s.Detect = func(_ string, r wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		var out []Component
		for _, p := range r.Paths {
			if p == "src/Pricing" || p == "tests/Pricing.Tests" {
				out = append(out, Component{Path: p, Name: filepath.Base(p), Root: "."})
			}
		}
		return out, nil
	}
	reports := &Reports{Coverage: []Coverage{{Format: "cobertura", Paths: []string{"out/*.xml"}}}}
	project := filepath.Join(root, "tests/Pricing.Tests")
	sum, d := ReadReports(reports, globUnder(root), project, s.attribution(root, project, project, "Pricing.Tests"))
	if sum.Coverage == nil || sum.Coverage.Total != 0 || sum.OtherFiles != 1 || len(d) != 0 {
		t.Errorf("by default another component's file is left to it: %+v %v", sum, d)
	}
	s.CrossComponentCoverage = true
	sum, d = ReadReports(reports, globUnder(root), project, s.attribution(root, project, project, "Pricing.Tests"))
	if sum.Coverage == nil || sum.Coverage.Total != 2 || sum.Coverage.Covered != 1 || len(d) != 0 {
		t.Errorf("a test project counts the code it covers: %+v %v", sum, d)
	}
}

func TestAStackRootExclusionJoinsTheWorkspaceInput(t *testing.T) {
	p := mustProfile(t)
	p.Inputs["manifests"] = append(p.Inputs["manifests"], "{root}/**/global.json", "!{root}/**/bin/**")
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	_, root := p.TaskPatterns(TaskLint)
	if !slices.Contains(root, "**/global.json") || !slices.Contains(root, "!**/bin/**") {
		t.Errorf("root patterns = %v", root)
	}
	if !slices.Contains(p.RootPatterns(), "!**/bin/**") {
		t.Errorf("RootPatterns = %v", p.RootPatterns())
	}
}
