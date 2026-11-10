package stacks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
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
	if err := json.Unmarshal(api.Metadata, &meta); err != nil || !reflect.DeepEqual(meta, Metadata{Component: "api", Root: ".", Toolchain: Expected{Version: "1.82.0", Source: "rust-toolchain.toml"}, Manifest: "crates/api/Cargo.toml"}) {
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

func TestASelectorNamesAnInputSetAndLeavesOutADirectory(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"Cargo.toml", "src/main.rs", "dist/app.js", "src/target/mod.rs", "target/release/app"} {
		writeFile(t, root, "tools/gen/"+f, "x")
	}
	writeFile(t, root, "tools/gen/.gen/version.json", "{}")
	writeFile(t, root, "tools/gen/tools/widget/.gen/version.json", "{}")
	writeFile(t, root, "tools/gen/.gen/client.rs", "// generated")
	writeFile(t, root, "tools/gen/.git", "gitdir: /elsewhere")
	if err := os.Symlink("missing", filepath.Join(root, "tools/gen/dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src", filepath.Join(root, "tools/gen/srclink")); err != nil {
		t.Fatal(err)
	}
	s := testStack(t)
	s.Profile.Inputs["package"] = []string{"**/*", "!target/**", "!**/.gen/version.json"}
	s.SelectorSets = []string{"package"}
	if !slices.Contains(s.Selectors(), "rust:package") {
		t.Errorf("selectors %v miss rust:package", s.Selectors())
	}
	answer := resolveOne(t, s, root, "tools/gen", "rust:package")
	if answer.Error != "" {
		t.Fatal(answer.Error)
	}
	for _, want := range []string{"Cargo.toml", ".gen/client.rs", "dist/app.js", "src/main.rs", "src/target/mod.rs"} {
		if !slices.Contains(answer.Files, want) {
			t.Errorf("files %v miss %s", answer.Files, want)
		}
	}
	for _, absent := range []string{"target/release/app", ".gen/version.json", "tools/widget/.gen/version.json", ".git", "dangling", "srclink"} {
		if slices.Contains(answer.Files, absent) {
			t.Errorf("files %v hold %s", answer.Files, absent)
		}
	}
	for _, f := range answer.Files {
		if !slices.ContainsFunc(answer.Basis.Content, func(set []string) bool { return wsproto.SelectsPath(f, set) }) {
			t.Errorf("the basis does not select %s", f)
		}
	}
	if wsproto.SelectsPath("target/release/app", answer.Basis.Content[0]) {
		t.Errorf("the basis selects the excluded build directory: %v", answer.Basis.Content[0])
	}
	if got := resolveOne(t, s, root, "tools/gen", "rust:sources"); got.Error == "" {
		t.Errorf("an input set the stack does not expose answers %v", got.Files)
	}
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
	infos   []string
}

func (r *recorder) DiagnosticWithCode(severity, message, file string, line, column int, code string) {
	r.diags = append(r.diags, severity+":"+code+":"+message)
}
func (r *recorder) Summary(message string) { r.summary = message }
func (r *recorder) Info(message string)    { r.infos = append(r.infos, message) }

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

func TestRunWithAppendsTheArgumentsToTheLastStepOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s := testProfileWithLcov(t)
	task := s.Profile.Tasks[TaskTest]
	task.Steps = append([][]string{{"cargo", "fetch"}}, task.Steps...)
	s.Profile.Tasks[TaskTest] = task
	meta, _ := json.Marshal(Metadata{Component: "api", Root: "."})
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{FullPath: filepath.Join(root, "crates/api"), Metadata: map[string]json.RawMessage{s.Extension: meta}}}

	var selected RootStep
	var lastStep []string
	job := s.RunWith(TaskTest, func(_ *pctx.Context, root RootStep, step []string) []string {
		selected, lastStep = root, step
		return []string{"--filter", "two words"}
	})
	if status, _, err := job(ctx, jsonl.New(), []string{"--report-dir", filepath.Join(root, "out")}); status != statusOK || err != nil {
		t.Fatalf("status %s: %v", status, err)
	}
	log, _ := os.ReadFile(filepath.Join(root, "log"))
	if strings.Contains(string(log), "cargo fetch --filter") || !strings.Contains(string(log), "lcov.info --filter two words") {
		t.Errorf("the arguments reach the last step alone: %s", log)
	}
	if selected.Dir != root || selected.Project != filepath.Join(root, "crates/api") || selected.Program == nil {
		t.Errorf("extra receives the selected root: %+v", selected)
	}
	if len(lastStep) < 2 || lastStep[0] != "cargo" || lastStep[1] != "test" || strings.Contains(strings.Join(lastStep, " "), "{output}") {
		t.Errorf("extra receives the expanded last step: %q", lastStep)
	}
}

func TestRunFilteredReadsZeroTestsWhenTheFilterSelectsNone(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	// A runner whose filter selects no test writes no report, and exits
	// non-zero when told to fail.
	bin := t.TempDir()
	writeFile(t, bin, "rustc", "#!/bin/sh\necho 'rustc 1.82.0 (stub)'\n")
	writeFile(t, bin, "cargo", "#!/bin/sh\necho \"cargo $*\" >> \"$STUB_LOG\"\ncase \"$*\" in *fail*) exit 1;; esac\n")
	for _, f := range []string{"rustc", "cargo"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := testProfileWithLcov(t)
	meta, _ := json.Marshal(Metadata{Component: "api", Root: "."})
	run := func(filters bool, args ...string) (string, map[string]any, *recorder) {
		t.Helper()
		rec := &recorder{}
		status, data, _ := s.run(context.Background(), runInput{
			task: TaskTest, workspaceRoot: root, projectAbs: filepath.Join(root, "crates/api"), metadata: meta,
			outputRoot: filepath.Join(root, ".putnami/out/api"), stdout: &bytes.Buffer{}, filters: filters,
			stepArgs: func(RootStep, []string) []string { return args },
		}, rec)
		return status, data, rec
	}

	status, data, rec := run(true, "--filter", "nothing")
	if status != statusOK || len(rec.diags) != 0 {
		t.Errorf("a filter that selects no test passes: %s %v", status, rec.diags)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 0 || got["passed"] != 0 || got["failed"] != 0 || data["coverageSummary"] != nil {
		t.Errorf("it reads zero tests and no coverage: %v", data)
	}
	if !slices.ContainsFunc(rec.infos, func(m string) bool { return strings.Contains(m, "selects no test of api") }) {
		t.Errorf("the run says the filter selected no test: %v", rec.infos)
	}
	if status, _, rec := run(true); status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeReportMissing) {
		t.Errorf("an unfiltered run that writes no report fails: %s %v", status, rec.diags)
	}
	if status, _, rec := run(false, "--filter", "nothing"); status != statusFailed || !strings.Contains(rec.codes(), "error:"+CodeReportMissing) {
		t.Errorf("arguments that are no filter keep the missing report an error: %s %v", status, rec.diags)
	}
	if status, _, rec := run(true, "--filter", "fail"); status != statusFailed || rec.codes() != "error:"+CodeStepFailed {
		t.Errorf("a filtered step that fails fails the task: %s %v", status, rec.diags)
	}

	// The job hands the filter the task's output directory, emptied before
	// the steps.
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{FullPath: filepath.Join(root, "crates/api"), Metadata: map[string]json.RawMessage{s.Extension: meta}}}
	out := filepath.Join(root, "out")
	writeFile(t, out, "test/stale", "")
	var output string
	job := s.RunFiltered(TaskTest, func(_ *pctx.Context, root RootStep, _ []string) []string {
		output = root.Output
		entries, _ := os.ReadDir(root.Output)
		if len(entries) != 0 {
			t.Errorf("the output directory holds %v before the steps", entries)
		}
		return []string{"--filter", "nothing"}
	})
	if status, data, err := job(ctx, jsonl.New(), []string{"--report-dir", out}); status != statusOK || err != nil || data["testSummary"].(map[string]any)["total"] != 0 {
		t.Errorf("RunFiltered: %s %v %v", status, err, data)
	}
	if output != filepath.Join(out, TaskTest) {
		t.Errorf("the filter received the output %q", output)
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

func TestRunOfADelegatedStackRunsInTheRootAndNamesTheTool(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"tasks": {"lint": {"steps": [["sh", "-c", "pwd > \"$STUB_LOG\""]], "dir": "root"}}}}}`)
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s := testStack(t)
	s.Profile.Delegated = true

	status, data, rec := runTask(t, s, root, TaskLint)
	if status != statusOK {
		t.Fatalf("status %s: %v", status, rec.diags)
	}
	if data["delegatedTo"] != "rust" {
		t.Errorf("the result names the tool it delegated to: %v", data)
	}
	got, _ := os.ReadFile(filepath.Join(root, "log"))
	want, _ := filepath.EvalSymlinks(root)
	if dir, _ := filepath.EvalSymlinks(strings.TrimSpace(string(got))); dir != want {
		t.Errorf("a root task runs in the stack root %s, ran in %s", want, got)
	}

	s.Profile.Delegated = false
	if _, data, _ := runTask(t, s, root, TaskLint); data["delegatedTo"] != nil {
		t.Errorf("a stack that keeps Putnami's cache names no delegation: %v", data)
	}
}

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

func TestWorkspaceRootJobRunsOnceAtEachRootTheWorkspaceIncludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "putnami.workspace.json", `{"disable": {"tags": ["e2e"]}}`)
	s := testStack(t)
	var dirs, manifests []string
	job := s.WorkspaceRootJob(FailOnMissingToolchain, func(_ context.Context, step RootStep, _ *jsonl.Emitter) error {
		dirs = append(dirs, step.Dir)
		manifests = append(manifests, step.Manifests...)
		return nil
	})
	ref := func(path string, extensions []string, config string) pctx.ProjectRef {
		return pctx.ProjectRef{Name: path, Path: path, Extensions: extensions, Config: json.RawMessage(config)}
	}
	own := []string{s.Extension}
	ctx := &pctx.Context{WorkspaceRoot: root, WorkspaceProjects: []pctx.ProjectRef{
		ref("crates/api", own, `{}`),
		ref("crates/pricing", own, `{}`),
		ref("crates/sample", own, `{"tags": ["e2e"]}`),
		ref("crates/other", []string{"@putnami/go"}, `{}`),
	}}
	writeFile(t, root, "crates/api/Cargo.toml", "")
	stubTools(t, "1.82.0", junitPass)
	status, _, err := job(ctx, jsonl.New(), nil)
	if status != statusOK || err != nil {
		t.Fatalf("job: %s %v", status, err)
	}
	if len(dirs) != 1 || dirs[0] != filepath.Join(root, ".") {
		t.Errorf("dirs = %v", dirs)
	}
	// The step lists the manifest of every component the job keeps at the
	// root, not the disabled one's.
	if want := []string{filepath.Join(root, "crates/api/Cargo.toml"), filepath.Join(root, "crates/pricing/Cargo.toml")}; !reflect.DeepEqual(manifests, want) {
		t.Errorf("manifests = %v, want %v", manifests, want)
	}
}

func TestWorkspaceRootJobWarnsWhenTheToolchainIsMissing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "crates/api/Cargo.toml", "")
	s := testStack(t)
	t.Setenv("PATH", t.TempDir())
	ctx := &pctx.Context{WorkspaceRoot: root, WorkspaceProjects: []pctx.ProjectRef{{Name: "crates/api", Path: "crates/api", Extensions: []string{s.Extension}}}}
	ran := false
	step := func(context.Context, RootStep, *jsonl.Emitter) error { ran = true; return nil }
	if status, _, err := s.WorkspaceRootJob(WarnOnMissingToolchain, step)(ctx, jsonl.New(), nil); status != statusOK || err != nil || ran {
		t.Errorf("warn: %s %v ran=%v", status, err, ran)
	}
	if status, _, err := s.WorkspaceRootJob(FailOnMissingToolchain, step)(ctx, jsonl.New(), nil); status != statusFailed || err == nil {
		t.Errorf("fail: %s %v", status, err)
	}
}

// provideStack is the test stack with a Provide that installs a stub toolchain
// in dir, and the count of its calls.
func provideStack(t *testing.T, dir, version string) (Stack, *int) {
	t.Helper()
	s := testProfileWithLcov(t)
	s.Profile.Toolchain.Provisioned = "STUB_PROVIDED"
	calls := 0
	s.Provide = func(_ context.Context, toolchain Toolchain, expected []Expected, info func(string)) (Provided, error) {
		calls++
		if toolchain.Command[0] != "rustc" || len(expected) != 1 {
			t.Errorf("Provide gets the resolved toolchain and each check's expected version: %v %v", toolchain.Command, expected)
		}
		info("installing " + expected[0].Version)
		writeFile(t, dir, "rustc", "#!/bin/sh\necho 'rustc "+version+" (stub 2024-10-15)'\n")
		writeFile(t, dir, "cargo", "#!/bin/sh\necho \"$STUB_FROM_PROVIDE\" > \"$STUB_ROOT/from-provide\"\nprintf '"+junitPass+"' > /dev/null\n")
		for _, name := range []string{"rustc", "cargo"} {
			if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return Provided{Executable: filepath.Join(dir, "rustc"), Env: []string{"STUB_FROM_PROVIDE=yes"}}, nil
	}
	return s, &calls
}

func TestATaskInstallsAMissingToolchainAndRunsIt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("PATH", t.TempDir())
	s, calls := provideStack(t, t.TempDir(), "1.82.0")
	status, _, rec := runTask(t, s, root, TaskLint)
	if status != statusOK {
		t.Fatalf("status %s: %v", status, rec.diags)
	}
	if *calls != 1 {
		t.Errorf("Provide ran %d times", *calls)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "from-provide")); strings.TrimSpace(string(got)) != "yes" {
		t.Errorf("the provided environment reaches the steps: %q", got)
	}
}

func TestATaskLeavesASatisfyingToolchainAlone(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s, calls := provideStack(t, t.TempDir(), "1.82.0")
	if status, _, rec := runTask(t, s, root, TaskLint); status != statusOK {
		t.Fatalf("status %s: %v", status, rec.diags)
	}
	if *calls != 0 {
		t.Errorf("Provide ran %d times", *calls)
	}
}

func TestAnOfflineTaskProvidesNothing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	s, calls := provideStack(t, t.TempDir(), "1.82.0")
	status, _, rec := runTask(t, s, root, TaskLint)
	if status != statusFailed || !strings.Contains(rec.codes(), CodeToolchainMissing) {
		t.Errorf("status %s: %v", status, rec.diags)
	}
	if *calls != 0 {
		t.Errorf("Provide ran %d times", *calls)
	}
}

func TestAProfileWithNoProvisionedToolchainInstallsNothing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	s, calls := provideStack(t, t.TempDir(), "1.82.0")
	// A variant such as an Xcode project's names no provisioned toolchain:
	// it keeps its check only.
	s.Profile.Toolchain.Provisioned = ""
	status, _, rec := runTask(t, s, root, TaskLint)
	if status != statusFailed || !strings.Contains(rec.codes(), CodeToolchainMissing) || strings.Contains(strings.Join(rec.diags, " "), "network") {
		t.Errorf("status %s: %v", status, rec.diags)
	}
	ctx := &pctx.Context{WorkspaceRoot: root, WorkspaceProjects: []pctx.ProjectRef{{Name: "crates/api", Path: "crates/api", Extensions: []string{s.Extension}}}}
	ran := false
	step := func(context.Context, RootStep, *jsonl.Emitter) error { ran = true; return nil }
	if status, _, err := s.WorkspaceRootJob(WarnOnMissingToolchain, step)(ctx, jsonl.New(), nil); status != statusOK || err != nil || ran {
		t.Errorf("a workspace job leaves it out: %s %v ran=%v", status, err, ran)
	}
	if *calls != 0 {
		t.Errorf("Provide ran %d times", *calls)
	}
}

func TestAFailedInstallIsAMissingToolchain(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("PATH", t.TempDir())
	s := testProfileWithLcov(t)
	s.Profile.Toolchain.Provisioned = "STUB_PROVIDED"
	s.Provide = func(context.Context, Toolchain, []Expected, func(string)) (Provided, error) {
		return Provided{}, errors.New("no network")
	}
	status, _, rec := runTask(t, s, root, TaskLint)
	if status != statusFailed || !strings.Contains(strings.Join(rec.diags, " "), "no network") {
		t.Errorf("status %s: %v", status, rec.diags)
	}
}

func TestToolsReachThePathOfTheTask(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	tools := t.TempDir()
	writeFile(t, tools, "probe-tool", "#!/bin/sh\necho reached > \"$STUB_ROOT/tool\"\n")
	if err := os.Chmod(filepath.Join(tools, "probe-tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := testProfileWithLcov(t)
	task := s.Profile.Tasks[TaskLint]
	task.Steps = [][]string{{"sh", "-c", "probe-tool"}}
	s.Profile.Tasks[TaskLint] = task
	var tasks []string
	var components []string
	s.Tools = func(_ context.Context, name string, meta Metadata, _ []string, _ func(string)) ([]string, error) {
		tasks = append(tasks, name)
		components = append(components, meta.Component)
		return []string{tools}, nil
	}
	if status, _, rec := runTask(t, s, root, TaskLint); status != statusOK {
		t.Fatalf("status %s: %v", status, rec.diags)
	}
	if !slices.Equal(tasks, []string{TaskLint}) || !slices.Equal(components, []string{"api"}) {
		t.Errorf("Tools ran for %v of %v", tasks, components)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "tool")); strings.TrimSpace(string(got)) != "reached" {
		t.Errorf("the tool ran: %q", got)
	}
	s.Tools = func(context.Context, string, Metadata, []string, func(string)) ([]string, error) {
		return nil, errors.New("no archive")
	}
	if status, _, rec := runTask(t, s, root, TaskLint); status != statusFailed || !strings.Contains(strings.Join(rec.diags, " "), "no archive") {
		t.Errorf("status %s: %v", status, rec.diags)
	}
}

func TestStepAdaptsEachStepAndTheFilterReadsTheAdaptedOne(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	tools := t.TempDir()
	writeFile(t, tools, "stated-cargo", "#!/bin/sh\necho \"stated $*\" >> \"$STUB_LOG\"\nexec cargo \"$@\"\n")
	if err := os.Chmod(filepath.Join(tools, "stated-cargo"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := testProfileWithLcov(t)
	task := s.Profile.Tasks[TaskTest]
	task.Steps = append([][]string{{"cargo", "fetch"}}, task.Steps...)
	s.Profile.Tasks[TaskTest] = task
	var seen []string
	s.Step = func(_ context.Context, name string, meta Metadata, root RootStep, step []string, _ func(string)) ([]string, error) {
		seen = append(seen, name+" "+meta.Component+" "+root.Component+" "+strings.Join(step, " "))
		if root.Program == nil || len(root.Env) == 0 {
			t.Errorf("Step receives the selected root: %+v", root)
		}
		return append([]string{filepath.Join(tools, "stated-cargo")}, append(step[1:], "--locked")...), nil
	}
	meta, _ := json.Marshal(Metadata{Component: "api", Root: "."})
	var lastStep []string
	rec := &recorder{}
	status, _, _ := s.run(context.Background(), runInput{
		task: TaskTest, workspaceRoot: root, projectAbs: filepath.Join(root, "crates/api"), metadata: meta,
		outputRoot: filepath.Join(root, ".putnami/out/api"), stdout: &bytes.Buffer{},
		stepArgs: func(_ RootStep, step []string) []string { lastStep = step; return []string{"--filter", "x"} },
	}, rec)
	if status != statusOK {
		t.Fatalf("status %s: %v", status, rec.diags)
	}
	if len(seen) != 2 || !strings.HasPrefix(seen[0], "test api api cargo fetch") || !strings.HasPrefix(seen[1], "test api api cargo test --package api") {
		t.Errorf("Step adapts each expanded step: %q", seen)
	}
	if len(lastStep) == 0 || filepath.Base(lastStep[0]) != "stated-cargo" || lastStep[len(lastStep)-1] != "--locked" {
		t.Errorf("the filter reads the adapted last step: %q", lastStep)
	}
	log, _ := os.ReadFile(filepath.Join(root, "log"))
	if !strings.Contains(string(log), "stated fetch --locked\n") || !strings.Contains(string(log), "--locked --filter x\n") {
		t.Errorf("each adapted step runs, the filter after the adapted arguments: %s", log)
	}

	s.Step = func(context.Context, string, Metadata, RootStep, []string, func(string)) ([]string, error) {
		return nil, errors.New("no release to install")
	}
	if status, _, rec := runTask(t, s, root, TaskTest); status != statusFailed || rec.codes() != "error:"+CodeToolchainMissing || !strings.Contains(rec.diags[0], "no release to install") {
		t.Errorf("a step that cannot be adapted fails the task: %s %v", status, rec.diags)
	}
}

func TestProbeNamesABareDirectoryAndADelegatedStackServesTheGraphJob(t *testing.T) {
	s := testStack(t)
	s.Detect = func(string, wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		return []Component{{Path: "3rdparty/python", Name: "3rdparty/python", SourceFile: "3rdparty/python/BUILD", Bare: true}}, nil
	}
	result, err := s.Probe(t.TempDir(), wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"3rdparty/python"}})
	if err != nil {
		t.Fatal(err)
	}
	want := wsproto.ProbeProject{Path: "3rdparty/python", SourceName: "3rdparty/python", SourceFile: "3rdparty/python/BUILD"}
	if len(result.Projects) != 1 || !reflect.DeepEqual(result.Projects[0], want) {
		t.Errorf("a bare directory gets its name alone: %+v", result.Projects)
	}

	if _, ok := s.Jobs()[TaskGraph]; ok {
		t.Error("a stack that keeps Putnami's cache serves no graph job")
	}
	s.Profile.Delegated = true
	graph, ok := s.Jobs()[TaskGraph]
	if !ok {
		t.Fatal("a delegated stack serves the graph job")
	}
	if status, data, err := graph(nil, nil, nil); status != statusOK || data != nil || err != nil {
		t.Errorf("the graph job passes and runs nothing: %s %v %v", status, data, err)
	}
}

func TestAFailedStepOfADelegatedTaskNamesItsTool(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/api/Cargo.toml", "")
	t.Setenv("STUB_ROOT", root)
	t.Setenv("STUB_LOG", filepath.Join(root, "log"))
	stubTools(t, "1.82.0", junitPass)
	s := testProfileWithLcov(t)
	s.Profile.Delegated = true
	task := s.Profile.Tasks[TaskLint]
	task.Steps = [][]string{{"false"}}
	s.Profile.Tasks[TaskLint] = task
	status, data, _ := runTask(t, s, root, TaskLint)
	if status != statusFailed || data["delegatedTo"] != s.Profile.ID {
		t.Errorf("status %s, data %v", status, data)
	}
}

func TestProbeAttributesTheEdgesOfAComponentThatNamesTheirSource(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	s := testStack(t)
	source := wsproto.DependencySourceBuildGraph
	s.Detect = func(string, wsproto.ProbeRequest) ([]Component, []diag.Diagnostic) {
		return []Component{
			{Path: "ws", Name: "ws", Root: "ws", EdgeSource: source},
			{Path: "ws/a", Name: "a", Root: "ws", Dependencies: []string{"ws/b"}, EdgeSource: source},
			{Path: "ws/b", Name: "b", Root: "ws", EdgeSource: source},
			{Path: "plain", Name: "plain", Root: ".", Dependencies: []string{"ws/b"}},
		}, nil
	}
	request := wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"plain", "ws", "ws/a", "ws/b"}}

	result, err := s.Probe(root, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range result.Projects {
		if p.DependencySources != nil {
			t.Errorf("%s answers provenance to a request that did not ask: %v", p.Path, p.DependencySources)
		}
	}

	request.DependencySources = true
	result, err = s.Probe(root, request)
	if err != nil {
		t.Fatal(err)
	}
	if diags := wsproto.ValidateProbeResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	got := map[string]map[string]wsproto.DependencySource{}
	for _, p := range result.Projects {
		got[p.Path] = p.DependencySources
	}
	want := map[string]map[string]wsproto.DependencySource{
		// The stack-root edge takes the component's source too.
		"ws/a": {"ws": source, "ws/b": source},
		"ws/b": {"ws": source},
		// A component with no edge is attributed all the same.
		"ws": {},
		// A component that names no source stays unattributed.
		"plain": nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dependencySources = %v, want %v", got, want)
	}
}
