package rust

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "rust/stack-extension"

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture writes a Cargo workspace: api depends on pricing, both members of
// the root workspace, and gen is a crate of its own outside it.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "Cargo.toml", "[workspace]\nmembers = [\"crates/*\"]\n\n[workspace.dependencies]\npricing = { path = \"crates/pricing\" }\n")
	writeFile(t, root, "Cargo.lock", "version = 3\n")
	writeFile(t, root, "rust-toolchain.toml", "[toolchain]\nchannel = \"1.82.0\"\n")
	writeFile(t, root, "crates/pricing/Cargo.toml", "[package]\nname = \"pricing\"\nversion = \"0.1.0\"\n")
	writeFile(t, root, "crates/pricing/src/lib.rs", "pub fn price() -> u32 { 1 }\n")
	writeFile(t, root, "crates/api/Cargo.toml", "[package]\nname = \"api\"\nversion = \"0.1.0\"\n\n[dependencies]\npricing = { workspace = true }\n")
	writeFile(t, root, "crates/api/src/lib.rs", "pub fn serve() {}\n")
	writeFile(t, root, "crates/api/tests/http.rs", "#[test]\nfn serves() {}\n")
	writeFile(t, root, "tools/gen/Cargo.toml", "[package]\nname = \"gen\"\nversion = \"0.1.0\"\n")
	writeFile(t, root, "tools/gen/src/main.rs", "fn main() {}\n")
	return root
}

func stack(t *testing.T) stacks.Stack {
	t.Helper()
	s, err := Stack()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func probe(t *testing.T, root string) wsproto.ProbeResult {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{".", "crates/api", "crates/pricing", "docs", "tools/gen"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTheCratesOfACargoWorkspaceBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "crates-become-projects", "the-crates-of-a-cargo-workspace-become-projects-with-their-edges")
	result := probe(t, fixture(t))
	if len(result.Projects) != 3 {
		t.Fatalf("projects = %+v, diagnostics = %v", result.Projects, result.Diagnostics)
	}
	want := map[string]struct {
		name, root string
		deps       []string
	}{
		"crates/api":     {"api", ".", []string{"crates/pricing"}},
		"crates/pricing": {"pricing", ".", nil},
		"tools/gen":      {"gen", "tools/gen", nil},
	}
	for _, p := range result.Projects {
		w, ok := want[p.Path]
		if !ok {
			t.Errorf("unexpected project %s", p.Path)
			continue
		}
		var meta stacks.Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if p.SourceName != w.name || !reflect.DeepEqual(p.Dependencies, w.deps) || meta.Component != w.name || meta.Root != w.root {
			t.Errorf("%s = %+v, metadata %+v", p.Path, p, meta)
		}
		if !reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"rust"}) {
			t.Errorf("%s extensions %v tags %v", p.Path, p.Extensions, p.Tags)
		}
	}
	var api stacks.Metadata
	_ = json.Unmarshal(result.Projects[0].Metadata, &api)
	if api.Toolchain != (stacks.Expected{Version: "1.82.0", Source: "rust-toolchain.toml"}) {
		t.Errorf("api toolchain = %+v", api.Toolchain)
	}
}

// stubTools puts first on PATH a rustc that prints version and a cargo that
// logs its arguments and writes the reports cargo-nextest and
// cargo-llvm-cov write. The real tools are the stack's own build: the stub
// stands at that boundary.
func stubTools(t *testing.T, root, version, junit string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "cargo.log")
	writeFile(t, bin, "rustc", "#!/bin/sh\necho 'rustc "+version+" (stub 2024-10-15)'\n")
	writeFile(t, bin, "cargo", `#!/bin/sh
echo "$PWD: cargo $*" >> "`+log+`"
case "$*" in *nextest*)
  mkdir -p "`+root+`/target/nextest/ci"
  cat > "`+root+`/target/nextest/ci/junit.xml" <<'XML'
`+junit+`
XML
  prev=""
  for a in "$@"; do
    if [ "$prev" = "--output-path" ]; then
      if [ -n "$STUB_LCOV" ]; then cp "$STUB_LCOV" "$a"; else printf 'SF:src/lib.rs\nDA:1,3\nDA:2,0\nDA:3,1\nend_of_record\n' > "$a"; fi
    fi
    prev="$a"
  done;;
build*)
  prev=""
  for a in "$@"; do
    if [ "$prev" = "--package" ]; then mkdir -p "`+root+`/target/debug" && printf 'bin' > "`+root+`/target/debug/$a"; fi
    prev="$a"
  done;;
esac
`)
	for _, f := range []string{"rustc", "cargo"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

const junitPass = `<?xml version="1.0"?>
<testsuites><testsuite name="api::http">
  <testcase classname="api::http" name="serves"/>
  <testcase classname="api" name="routes"/>
</testsuite></testsuites>`

const junitFail = `<?xml version="1.0"?>
<testsuites><testsuite name="api::http">
  <testcase classname="api::http" name="serves"><failure message="assertion failed"/></testcase>
</testsuite></testsuites>`

// runJob runs a task of the crate at rel through the extension's job, the
// way the CLI calls it.
func runJob(t *testing.T, root, rel, task string) (string, map[string]any, string, error) {
	t.Helper()
	status, data, out, _, err := runJobOutput(t, root, rel, task)
	return status, data, out, err
}

// runJobOutput is runJob, with what the job emitted.
func runJobOutput(t *testing.T, root, rel, task string) (string, map[string]any, string, string, error) {
	t.Helper()
	s := stack(t)
	result := probe(t, root)
	var meta json.RawMessage
	for _, p := range result.Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	out := filepath.Join(root, ".putnami/out", strings.ReplaceAll(rel, "/", "_"), task)
	job := s.Jobs()[task]
	emitted, err := os.CreateTemp(t.TempDir(), "emitted")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = emitted
	status, data, runErr := job(ctx, jsonl.New(), []string{"--report-dir", out})
	os.Stdout = stdout
	_ = emitted.Close()
	text, err := os.ReadFile(emitted.Name())
	if err != nil {
		t.Fatal(err)
	}
	return status, data, out, string(text), runErr
}

func TestATaskRunsTheProfileStepsAfterTheToolchainCheck(t *testing.T) {
	spectest.Proves(t, feature, "tasks-run-the-profile", "a-task-runs-the-profile-steps-after-the-toolchain-check")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	status, _, _, err := runJob(t, root, "crates/api", stacks.TaskLint)
	if status != "OK" || err != nil {
		t.Fatalf("lint: %s %v", status, err)
	}
	ran, _ := os.ReadFile(log)
	api := filepath.Join(root, "crates/api")
	for _, want := range []string{
		api + ": cargo fmt --package api --check",
		api + ": cargo clippy --package api --all-targets -- -D warnings",
	} {
		if !strings.Contains(string(ran), want) {
			t.Errorf("lint ran\n%s\nwant %q", ran, want)
		}
	}

	stubTools(t, root, "1.80.0", junitPass)
	if status, _, _, err := runJob(t, root, "crates/api", stacks.TaskBuild); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("a toolchain mismatch fails the task: %s %v", status, err)
	}
}

func TestJUnitAndLCOVBecomeTheTestAndCoverageSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-become-the-summary", "junit-and-lcov-become-the-test-and-coverage-summary")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	status, data, out, err := runJob(t, root, "crates/api", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v", status, err)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 2 || got["passed"] != 2 || got["failed"] != 0 {
		t.Errorf("testSummary = %v", got)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 2 || got["total"] != 3 {
		t.Errorf("coverageSummary = %v", got)
	}
	ran, _ := os.ReadFile(log)
	if !strings.Contains(string(ran), "cargo llvm-cov nextest --package api --profile ci --lcov --output-path "+filepath.Join(out, "test", "lcov.info")) {
		t.Errorf("test ran %s", ran)
	}
	if _, err := os.Stat(filepath.Join(out, "test", "reports", "01-junit.xml")); err != nil {
		t.Errorf("the JUnit report is copied into the task output: %v", err)
	}

	stubTools(t, root, "1.82.0", junitFail)
	if status, data, _, err := runJob(t, root, "crates/api", stacks.TaskTest); status != "FAILED" || err == nil || data["testSummary"].(map[string]any)["failed"] != 1 {
		t.Errorf("a failed case fails the task: %s %v %v", status, err, data)
	}
}

func TestAPutnamiJSONOverrideChangesTheStepsAndTheInputs(t *testing.T) {
	spectest.Proves(t, feature, "overrides-are-honored", "a-putnami-json-override-changes-the-steps-and-the-inputs")
	root := fixture(t)
	s := stack(t)
	resolve := func() wsproto.InputResolveAnswer {
		result := s.ResolveInputs(root, wsproto.InputResolveRequest{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
			Inputs: []wsproto.InputResolveQuery{{Project: "crates/api", Selector: "rust:lint"}}})
		if diags := wsproto.ValidateInputResolveResult(&result); len(diags) != 0 {
			t.Fatalf("the answer breaks the protocol: %v", diags)
		}
		return result.Inputs[0]
	}
	if got := resolve(); !reflect.DeepEqual(got.Files, []string{"Cargo.toml", "src/lib.rs", "tests/http.rs"}) {
		t.Fatalf("lint inputs = %+v", got)
	}

	writeFile(t, root, "crates/api/fixtures/routes.json", "{}")
	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {
	  "inputs": {"sources": ["src/**", "fixtures/**"]},
	  "tasks": {"lint": {"steps": [["cargo", "clippy", "--package", "{component}"]]}}
	}}}`)
	if got := resolve(); !reflect.DeepEqual(got.Files, []string{"Cargo.toml", "fixtures/routes.json", "putnami.json", "src/lib.rs", "tests/http.rs"}) {
		t.Errorf("an input override is honored: %v", got.Files)
	}
	log := stubTools(t, root, "1.82.0", junitPass)
	if status, _, _, err := runJob(t, root, "crates/api", stacks.TaskLint); status != "OK" || err != nil {
		t.Fatalf("lint: %s %v", status, err)
	}
	ran, _ := os.ReadFile(log)
	if lines := strings.Split(strings.TrimSpace(string(ran)), "\n"); len(lines) != 1 || !strings.HasSuffix(lines[0], "cargo clippy --package api") {
		t.Errorf("a step override is honored: %q", ran)
	}

	writeFile(t, root, "crates/api/putnami.json", `{"options": {"@putnami/rust": {"tasks": {"lint": {"stepz": []}}}}}`)
	if status, _, _, err := runJob(t, root, "crates/api", stacks.TaskLint); status != "FAILED" || err == nil {
		t.Errorf("an invalid override fails the task: %s %v", status, err)
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	stackstest.CheckManifest(t, "putnami.extension.json", stack(t))
	if got := stack(t).Selectors(); !slices.Equal(got, []string{"rust:build", "rust:lint", "rust:test"}) {
		t.Errorf("selectors = %v", got)
	}
}

func TestLintDoesNotReadTheLockfileOrTheNextestConfiguration(t *testing.T) {
	spectest.Proves(t, feature, "inputs-are-per-task", "lint-does-not-read-the-lockfile-or-the-nextest-configuration")
	root := fixture(t)
	writeFile(t, root, "crates/api/Cargo.lock", "version = 3\n")
	writeFile(t, root, "crates/api/rustfmt.toml", "edition = \"2021\"\n")
	writeFile(t, root, "crates/api/benches/load.rs", "fn main() {}\n")
	s := stack(t)
	files := map[string][]string{}
	for _, task := range stacks.TaskNames {
		result := s.ResolveInputs(root, wsproto.InputResolveRequest{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
			Inputs: []wsproto.InputResolveQuery{{Project: "crates/api", Selector: s.Selector(task)}}})
		if diags := wsproto.ValidateInputResolveResult(&result); len(diags) != 0 {
			t.Fatalf("%s: the answer breaks the protocol: %v", task, diags)
		}
		files[task] = result.Inputs[0].Files
	}
	for task, want := range map[string][]string{
		stacks.TaskLint:  {"Cargo.toml", "benches/load.rs", "putnami.json", "rustfmt.toml", "src/lib.rs", "tests/http.rs"},
		stacks.TaskTest:  {"Cargo.lock", "Cargo.toml", "benches/load.rs", "src/lib.rs", "tests/http.rs"},
		stacks.TaskBuild: {"Cargo.lock", "Cargo.toml", "src/lib.rs"},
	} {
		got := slices.DeleteFunc(slices.Clone(files[task]), func(f string) bool { return f == "putnami.json" && task != stacks.TaskLint })
		if task == stacks.TaskLint {
			got = append(slices.DeleteFunc(got, func(f string) bool { return f == "putnami.json" }), "putnami.json")
			slices.Sort(got)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s reads %v, want %v", task, got, want)
		}
	}
	_, lintRoot := s.Profile.TaskPatterns(stacks.TaskLint)
	for _, f := range []string{"Cargo.lock", ".config/nextest.toml"} {
		if slices.Contains(lintRoot, f) {
			t.Errorf("lint reads the stack-root file %s: %v", f, lintRoot)
		}
	}
	_, testRoot := s.Profile.TaskPatterns(stacks.TaskTest)
	if !slices.Contains(testRoot, ".config/nextest.toml") || !slices.Contains(testRoot, "Cargo.lock") || slices.Contains(testRoot, "rustfmt.toml") {
		t.Errorf("test stack-root files = %v", testRoot)
	}

	stubTools(t, root, "1.82.0", junitPass)
	status, _, out, err := runJob(t, root, "crates/api", stacks.TaskBuild)
	if status != "OK" || err != nil {
		t.Fatalf("build: %s %v", status, err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "build", "artifacts", "api")); err != nil || string(data) != "bin" {
		t.Errorf("the crate's binary is copied into the build output: %q %v", data, err)
	}
}

const junitMixed = `<?xml version="1.0"?>
<testsuites>
<testsuite name="api::http">
  <testcase classname="api::http" name="serves"/>
</testsuite>
<testsuite name="api">
  <testcase classname="api" name="routes"/>
  <testcase classname="api::bin/api" name="starts"/>
</testsuite>
<testsuite name="pricing::integration">
  <testcase classname="pricing::integration" name="totals"/>
</testsuite>
<testsuite name="loose suite">
  <testcase classname="loose case" name="stray"/>
</testsuite>
</testsuites>`

func TestEachNextestCaseGoesToTheCrateItsBinaryNames(t *testing.T) {
	spectest.Proves(t, feature, "cases-are-attributed", "each-nextest-case-goes-to-the-crate-its-binary-names")
	root := fixture(t)
	stubTools(t, root, "1.82.0", junitMixed)
	status, data, _, emitted, err := runJobOutput(t, root, "crates/api", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v\n%s", status, err, emitted)
	}
	// Three cases of api, one of no crate; the pricing case is pricing's.
	if got := data["testSummary"].(map[string]any); got["total"] != 4 || got["passed"] != 4 {
		t.Errorf("testSummary = %v", got)
	}
	if !strings.Contains(emitted, stacks.CodeTestUnattributed) || !strings.Contains(emitted, `loose case \u003e stray`) {
		t.Errorf("a case of no crate is reported:\n%s", emitted)
	}
	if strings.Contains(emitted, "pricing::integration \\u003e totals") {
		t.Errorf("the pricing case is not this crate's:\n%s", emitted)
	}
	s := stack(t)
	if pkg, ok := s.CaseComponent("my-lib::it", ""); !ok || s.ComponentKey(pkg) != s.ComponentKey("my_lib") {
		t.Errorf("- and _ read as one: %q %v", pkg, ok)
	}
	if pkg, ok := s.CaseComponent("", "worker::bin/worker"); !ok || pkg != "worker" {
		t.Errorf("the suite name names the crate when the class name does not: %q %v", pkg, ok)
	}
}

func TestEachCoverageFileGoesToTheCrateThatHoldsIt(t *testing.T) {
	spectest.Proves(t, feature, "coverage-is-attributed", "each-coverage-file-goes-to-the-crate-that-holds-it")
	root := fixture(t)
	lcov := filepath.Join(t.TempDir(), "lcov.info")
	writeFile(t, filepath.Dir(lcov), "lcov.info", "SF:"+filepath.Join(root, "crates/api/src/lib.rs")+"\nDA:1,1\nDA:2,0\nend_of_record\n"+
		"SF:tests/http.rs\nDA:1,1\nend_of_record\n"+
		"SF:"+filepath.Join(root, "crates/pricing/src/lib.rs")+"\nDA:1,1\nDA:2,1\nDA:3,1\nend_of_record\n"+
		"SF:/opt/cargo/registry/src/itoa/lib.rs\nDA:1,1\nend_of_record\n")
	t.Setenv("STUB_LCOV", lcov)
	stubTools(t, root, "1.82.0", junitPass)
	status, data, _, emitted, err := runJobOutput(t, root, "crates/api", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v\n%s", status, err, emitted)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 2 || got["total"] != 3 {
		t.Errorf("coverageSummary = %v, want api's two files alone", got)
	}
	if !strings.Contains(emitted, stacks.CodeCoverageUnmapped) || !strings.Contains(emitted, "/opt/cargo/registry/src/itoa/lib.rs") {
		t.Errorf("a file in no crate is reported:\n%s", emitted)
	}
	if strings.Contains(emitted, "crates/pricing/src/lib.rs") {
		t.Errorf("pricing's file is pricing's, not unmapped:\n%s", emitted)
	}
}
