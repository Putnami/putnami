package stacks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
)

// fakeDetector names two crates of a root workspace, api depending on
// pricing, and a standalone tool that is its own root.
func fakeDetector(root string, request wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
	all := map[string]Component{
		"crates/api":     {Path: "crates/api", Name: "api", SourceFile: "crates/api/Cargo.toml", Root: ".", Dependencies: []string{"crates/pricing"}},
		"crates/pricing": {Path: "crates/pricing", Name: "pricing", SourceFile: "crates/pricing/Cargo.toml", Root: "."},
		"tools/gen":      {Path: "tools/gen", Name: "gen", SourceFile: "tools/gen/Cargo.toml", Root: "tools/gen"},
		"odd":            {Path: "odd", Name: "odd", Root: "elsewhere"},
	}
	var out []Component
	for _, p := range request.Paths {
		if c, ok := all[p]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func testStack(t *testing.T) Stack {
	t.Helper()
	return Stack{Extension: "@putnami/rust", Profile: mustProfile(t), Detect: fakeDetector}
}

func TestProbeAnswersEachComponent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	s := testStack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"crates/api", "crates/pricing", "odd", "tools/gen", "docs"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 3 {
		t.Fatalf("projects = %+v", result.Projects)
	}
	api := result.Projects[0]
	if api.Path != "crates/api" || api.SourceName != "api" || !reflect.DeepEqual(api.Dependencies, []string{"crates/pricing"}) ||
		!reflect.DeepEqual(api.Extensions, []string{"@putnami/rust"}) || !reflect.DeepEqual(api.Tags, []string{"rust"}) {
		t.Errorf("api = %+v", api)
	}
	var meta Metadata
	if err := json.Unmarshal(api.Metadata, &meta); err != nil || meta != (Metadata{Component: "api", Root: ".", Toolchain: Expected{Version: "1.82.0", Source: "rust-toolchain.toml"}}) {
		t.Errorf("metadata = %s (%v)", api.Metadata, err)
	}
	for _, want := range []string{"crates/api/Cargo.toml", "crates/api/rust-toolchain.toml", "Cargo.lock", "Cargo.toml", "rust-toolchain.toml"} {
		if !slices.Contains(api.WatchedFiles, want) {
			t.Errorf("api watches no %s: %v", want, api.WatchedFiles)
		}
	}
	if gen := result.Projects[2]; !slices.Contains(gen.WatchedFiles, "tools/gen/Cargo.lock") {
		t.Errorf("a component that is its own root watches its lockfile: %v", gen.WatchedFiles)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != CodeStackRootUnsupported {
		t.Errorf("diagnostics = %v", result.Diagnostics)
	}
}

func TestAnUnnamedComponentIsNamedByItsDirectory(t *testing.T) {
	s := Stack{Extension: "@putnami/php", Profile: mustProfile(t), Detect: func(string, wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		return []Component{{Path: ".", SourceFile: "composer.json", Root: "."}}, nil
	}}
	result, err := s.Probe(t.TempDir(), wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"."}})
	if err != nil || len(result.Projects) != 1 {
		t.Fatalf("projects = %+v, %v", result.Projects, err)
	}
	var meta Metadata
	if p := result.Projects[0]; p.SourceName != "" || json.Unmarshal(p.Metadata, &meta) != nil || meta.Component != "." {
		t.Errorf("project = %+v, metadata %+v", p, meta)
	}
}

func resolveOne(t *testing.T, s Stack, root, project, selector string) wsproto.InputResolveAnswer {
	t.Helper()
	result := s.ResolveInputs(root, wsproto.InputResolveRequest{
		Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
		Inputs: []wsproto.InputResolveQuery{{Project: project, Selector: selector}},
	})
	if diags := wsproto.ValidateInputResolveResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	return result.Inputs[0]
}

func TestResolveInputsAnswersTheProfileFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Cargo.lock", "")
	writeFile(t, root, "crates/api/Cargo.toml", "[package]\nname = \"api\"\n")
	writeFile(t, root, "crates/api/src/lib.rs", "")
	writeFile(t, root, "crates/api/src/a/b.rs", "")
	writeFile(t, root, "crates/api/fixtures/x.json", "")
	writeFile(t, root, "crates/api/README.md", "")
	s := testStack(t)

	got := resolveOne(t, s, root, "crates/api", "rust:lint")
	if want := []string{"Cargo.toml", "src/a/b.rs", "src/lib.rs"}; got.Error != "" || !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("lint = %+v, want files %v", got, want)
	}

	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"inputs": {"sources": ["src/**", "fixtures/**"]}}}}`)
	got = resolveOne(t, s, root, "crates/api", "rust:lint")
	if want := []string{"Cargo.toml", "fixtures/x.json", "putnami.json", "src/a/b.rs", "src/lib.rs"}; !reflect.DeepEqual(got.Files, want) {
		t.Fatalf("an input override is honored: %v", got.Files)
	}

	writeFile(t, root, "tools/gen/Cargo.lock", "")
	gen := resolveOne(t, s, root, "tools/gen", "rust:test")
	if !slices.Contains(gen.Files, "Cargo.lock") {
		t.Errorf("a component that is its own root reads its lockfile: %v", gen.Files)
	}

	if bad := resolveOne(t, s, root, "crates/api", "rust:fmt"); bad.Error == "" {
		t.Errorf("an unknown selector is an error: %+v", bad)
	}
	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"inputs": {"lockfiles": ["{root}/deny.toml"]}}}}`)
	if bad := resolveOne(t, s, root, "crates/api", "rust:test"); !strings.Contains(bad.Error, "deny.toml") {
		t.Errorf("an override cannot add a stack-root input: %+v", bad)
	}
}

type recorder struct {
	diags   []string
	summary string
}

func (r *recorder) DiagnosticWithCode(severity, message, file string, line, column int, code string) {
	r.diags = append(r.diags, severity+":"+code+":"+message)
}
func (r *recorder) Summary(message string) { r.summary = message }
func (r *recorder) Info(string)            {}

func (r *recorder) codes() string {
	var out []string
	for _, d := range r.diags {
		parts := strings.SplitN(d, ":", 3)
		out = append(out, parts[0]+":"+parts[1])
	}
	return strings.Join(out, ",")
}

// stubTools puts a rustc that prints version and a cargo that writes the
// reports first on PATH.
func stubTools(t *testing.T, version string, junit string) {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, bin, "rustc", "#!/bin/sh\necho 'rustc "+version+" (stub)'\n")
	cargo := "#!/bin/sh\nset -e\necho \"cargo $*\" >> \"$STUB_LOG\"\n" +
		"mkdir -p \"$STUB_ROOT/target\"\ncat > \"$STUB_ROOT/target/junit.xml\" <<'XML'\n" + junit + "\nXML\n" +
		"for a in \"$@\"; do case \"$a\" in */lcov.info) printf 'SF:src/lib.rs\\nDA:1,1\\nDA:2,0\\nend_of_record\\n' > \"$a\";; esac; done\n"
	writeFile(t, bin, "cargo", cargo)
	for _, f := range []string{"rustc", "cargo"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func runTask(t *testing.T, s Stack, root, task string) (string, map[string]any, *recorder) {
	t.Helper()
	meta, _ := json.Marshal(Metadata{Component: "api", Root: "."})
	rec := &recorder{}
	var out bytes.Buffer
	status, data, _ := s.run(context.Background(), runInput{
		task: task, workspaceRoot: root, projectAbs: filepath.Join(root, "crates/api"),
		metadata: meta, outputRoot: filepath.Join(root, ".putnami/out/api/"+task), stdout: &out,
	}, rec)
	return status, data, rec
}

func testProfileWithLcov(t *testing.T) Stack {
	s := testStack(t)
	task := s.Profile.Tasks[TaskTest]
	task.Steps = [][]string{{"cargo", "test", "--package", "{component}", "--lcov", "{output}/lcov.info"}}
	s.Profile.Tasks[TaskTest] = task
	return s
}

func TestRunRunsTheStepsAndReadsTheReports(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s := testProfileWithLcov(t)

	status, data, rec := runTask(t, s, root, TaskTest)
	if status != statusOK {
		t.Fatalf("status %s: %v", status, rec.diags)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 3 || got["passed"] != 2 || got["skipped"] != 1 {
		t.Errorf("testSummary = %v", got)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 1 || got["total"] != 2 {
		t.Errorf("coverageSummary = %v", got)
	}
	log, _ := os.ReadFile(filepath.Join(root, "log"))
	if !strings.Contains(string(log), "cargo test --package api --lcov "+filepath.Join(root, ".putnami/out/api/test/test/lcov.info")) {
		t.Errorf("the step ran with its placeholders expanded: %s", log)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami/out/api/test/test/reports/01-junit.xml")); err != nil {
		t.Errorf("the stack-root report is copied into the task output: %v", err)
	}
}

func TestRunFailsOnAFailedTestAMismatchAndAMissingReport(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))

	stubTools(t, "1.82.0", junitFail)
	if status, _, rec := runTask(t, testProfileWithLcov(t), root, TaskTest); status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeTestFailed) {
		t.Errorf("a failed case fails the task: %s %v", status, rec.diags)
	}

	stubTools(t, "1.80.0", junitPass)
	if status, _, rec := runTask(t, testProfileWithLcov(t), root, TaskTest); status != statusFailed || rec.codes() != "error:"+CodeToolchainMismatch {
		t.Errorf("a mismatch fails the task: %s %v", status, rec.diags)
	}
	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"toolchain": {"check": "warn"}}}}`)
	if status, _, rec := runTask(t, testProfileWithLcov(t), root, TaskTest); status != statusOK || rec.codes() != "warning:"+CodeToolchainMismatch {
		t.Errorf("warn lets the task run: %s %v", status, rec.diags)
	}
	// A file the task's inputs select is a source: a report pattern that
	// matches it does not remove it.
	writeFile(t, root, "crates/api/src/junit.xml", junitPass)
	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"toolchain": {"check": "warn"}, "tasks": {"lint": {"steps": [["true"]], "reports": {"junit": ["src/junit.xml"]}}}}}}`)
	if status, _, rec := runTask(t, s0(t), root, TaskLint); status != statusOK {
		t.Errorf("lint reads its source report: %s %v", status, rec.diags)
	}
	if _, err := os.Stat(filepath.Join(root, "crates/api/src/junit.xml")); err != nil {
		t.Errorf("a source the inputs select is kept: %v", err)
	}
	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"toolchain": {"check": "warn"}}}}`)
	// A report another run left at the stack root is removed before the
	// steps: a step that writes none fails, it does not read the old one.
	if _, err := os.Stat(filepath.Join(root, "target/junit.xml")); err != nil {
		t.Fatalf("the previous run left its report: %v", err)
	}

	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"toolchain": {"check": "warn"}, "tasks": {"test": {"steps": [["true"]]}}}}}`)
	if status, _, rec := runTask(t, s0(t), root, TaskTest); status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeReportMissing) {
		t.Errorf("a test that writes no report fails: %s %v", status, rec.diags)
	}

	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"toolchain": {"check": "warn"}, "tasks": {"lint": {"steps": [["false"]]}}}}}`)
	if status, _, rec := runTask(t, s0(t), root, TaskLint); status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeStepFailed) {
		t.Errorf("a failing step fails the task: %s %v", status, rec.diags)
	}
	if status, _, rec := runTask(t, s0(t), root, TaskBuild); status != statusOK {
		t.Errorf("a task the profile does not declare passes: %s %v", status, rec.diags)
	}
}

// A test runner that exits non-zero on a failed case still has its report
// read: the failed case names the failure, and a run that wrote no report is
// a failed step, not a missing report.
func TestRunReadsTheReportsOfAFailedTestRun(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	t.Setenv("JUNIT_FAIL", junitFail)
	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"tasks": {"test": {
	  "steps": [["sh", "-c", "printf '%s' \"$JUNIT_FAIL\" > \"$0\"; exit 1", "{output}/junit.xml"], ["true"]],
	  "reports": {"junit": ["{output}/junit.xml"], "coverage": []}
	}}}}}`)
	status, data, rec := runTask(t, s0(t), root, TaskTest)
	if status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeTestFailed) || data["testSummary"].(map[string]any)["failed"] != 1 {
		t.Errorf("the failed case is read: %s %v %v", status, rec.diags, data)
	}

	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"tasks": {"test": {
	  "steps": [["false"]], "reports": {"junit": ["{output}/none.xml"], "coverage": []}
	}}}}}`)
	if status, _, rec := runTask(t, s0(t), root, TaskTest); status != statusFailed || rec.codes() != "error:"+CodeStepFailed {
		t.Errorf("a run that wrote no report is a failed step: %s %v", status, rec.diags)
	}
}

func s0(t *testing.T) Stack { return testStack(t) }

func TestRunRefusesAProjectWithoutComponentMetadata(t *testing.T) {
	rec := &recorder{}
	status, _, _ := testStack(t).run(context.Background(), runInput{task: TaskTest, workspaceRoot: t.TempDir(), projectAbs: t.TempDir(), outputRoot: t.TempDir()}, rec)
	if status != statusFailed || rec.codes() != "error:"+CodeNotAComponent {
		t.Errorf("%s %v", status, rec.diags)
	}
}

func TestEntrypointServesTheControlCalls(t *testing.T) {
	s := testStack(t)
	var out bytes.Buffer
	if err := s.Entrypoint([]string{"__putnami", "runtime-info"}, strings.NewReader(""), &out, "1.2.3", nil); err != nil || !strings.Contains(out.String(), "@putnami/rust") {
		t.Fatalf("runtime-info: %v %s", err, out.String())
	}
	if got := s.Selectors(); !reflect.DeepEqual(got, []string{"rust:lint", "rust:test"}) {
		t.Errorf("Selectors = %v", got)
	}
	if jobs := s.Jobs(); len(jobs) != 2 || jobs["lint"] == nil || jobs["test"] == nil {
		t.Errorf("Jobs = %v", jobs)
	}
	called := false
	if err := s.Entrypoint([]string{"lint"}, strings.NewReader(""), &out, "1.2.3", func(map[string]cli.JobFunc) { called = true }); err != nil || !called {
		t.Errorf("a job argv reaches the dispatcher: %v", err)
	}
}

func TestRunWithoutCoverageSupportReadsNoCoverageReport(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s := testProfileWithLcov(t)
	task := s.Profile.Tasks[TaskTest]
	task.WithoutCoverage = &WithoutCoverage{Probe: []string{"false"}, Steps: [][]string{{"cargo", "test", "--package", "{component}"}}}
	s.Profile.Tasks[TaskTest] = task

	status, data, rec := runTask(t, s, root, TaskTest)
	if status != statusOK || rec.codes() != "warning:"+CodeCoverageUnavailable {
		t.Fatalf("status %s: %v", status, rec.diags)
	}
	if _, ok := data["coverageSummary"]; ok {
		t.Errorf("no coverage summary is written: %v", data)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 3 {
		t.Errorf("testSummary = %v", got)
	}

	task.WithoutCoverage.Probe = []string{"true"}
	s.Profile.Tasks[TaskTest] = task
	status, data, rec = runTask(t, s, root, TaskTest)
	if _, ok := data["coverageSummary"]; status != statusOK || !ok || rec.codes() != "" {
		t.Errorf("a toolchain with coverage support keeps the coverage report: %s %v %v", status, data, rec.diags)
	}
}
