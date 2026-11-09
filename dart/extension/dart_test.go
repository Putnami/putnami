package dart

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "dart/stack-extension"

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

// tempDir is a temporary directory by its real path: a step's $PWD names that
// path, and on macOS the temporary directory sits behind the /var symlink.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// fixture writes the shop fixture, the base tree of the putnami observe Dart
// demo, and a pure Dart package of its own beside it, tools/gen, with a
// library, an executable and a test.
func fixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "shop.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	root := tempDir(t)
	parts := strings.Split(string(data), "\n-- ")
	for _, part := range parts[1:] {
		header, body, _ := strings.Cut(part, "\n")
		writeFile(t, root, strings.TrimSuffix(header, " --"), body+"\n")
	}
	writeFile(t, root, "tools/gen/pubspec.yaml", "name: gen\nenvironment:\n  sdk: ^3.7.0\ndependencies:\n  args: ^2.6.0\n")
	writeFile(t, root, "tools/gen/pubspec.lock", "packages: {}\n")
	writeFile(t, root, "tools/gen/lib/gen.dart", "String greet() => 'hi';\n")
	writeFile(t, root, "tools/gen/bin/gen.dart", "void main() {}\n")
	writeFile(t, root, "tools/gen/test/gen_test.dart", "void main() {}\n")
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
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension,
		Paths: []string{".", "apps/mobile", "mono", "mono/app", "mono/core", "packages/model", "packages/pricing", "packages/ui", "tools/gen", "docs"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestThePackagesOfAPubWorkspaceBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "packages-become-projects", "the-packages-of-a-pub-workspace-become-projects-with-their-edges")
	result := probe(t, fixture(t))
	if len(result.Projects) != 4 {
		t.Fatalf("projects = %+v, diagnostics = %v", result.Projects, result.Diagnostics)
	}
	want := map[string]struct {
		deps []string
		meta stacks.Metadata
	}{
		"apps/mobile": {[]string{"packages/pricing", "packages/ui"}, stacks.Metadata{Component: "mobile", Root: ".", Variant: VariantFlutter,
			Toolchain:  stacks.Expected{Version: ">=3.29.0", Source: "apps/mobile/pubspec.yaml"},
			Toolchains: []stacks.Expected{{Version: "^3.7.0", Source: "apps/mobile/pubspec.yaml"}}}},
		"packages/pricing": {nil, stacks.Metadata{Component: "pricing", Root: ".", Variant: VariantFlutter,
			Toolchains: []stacks.Expected{{Version: "^3.7.0", Source: "packages/pricing/pubspec.yaml"}}}},
		"packages/ui": {nil, stacks.Metadata{Component: "ui", Root: ".", Variant: VariantFlutter,
			Toolchain:  stacks.Expected{Version: ">=3.29.0", Source: "packages/ui/pubspec.yaml"},
			Toolchains: []stacks.Expected{{Version: "^3.7.0", Source: "packages/ui/pubspec.yaml"}}}},
		"tools/gen": {nil, stacks.Metadata{Component: "gen", Root: "tools/gen", Variant: VariantDart,
			Toolchain: stacks.Expected{Version: "^3.7.0", Source: "tools/gen/pubspec.yaml"}}},
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
		w.meta.Manifest = p.Path + "/pubspec.yaml"
		if p.SourceName != w.meta.Component || !reflect.DeepEqual(p.Dependencies, w.deps) || !reflect.DeepEqual(meta, w.meta) {
			t.Errorf("%s = %+v, metadata %+v, want %+v", p.Path, p, meta, w.meta)
		}
		if !reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"dart"}) || p.SourceFile != p.Path+"/pubspec.yaml" {
			t.Errorf("%s extensions %v tags %v source %s", p.Path, p.Extensions, p.Tags, p.SourceFile)
		}
	}
}

// stubTools puts first on PATH a dart and a flutter that print their
// version, log their arguments, and write the reports dart test, flutter
// test, format_coverage and tojunit write. The real SDKs are the stack's own
// build: the stubs stand at that boundary.
func stubTools(t *testing.T, flutterVersion, dartVersion, junit string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "tools.log")
	writeFile(t, bin, "junit.xml", junit)
	common := `echo "$PWD: $tool $*" >> "` + log + `"
if [ "$1 $2" = "pub get" ]; then
  case " $* " in
    *" --enforce-lockfile "*) [ -f pubspec.lock ] || { echo "Cannot do --enforce-lockfile without an existing pubspec.lock" >&2; exit 66; };;
    *) [ -f pubspec.lock ] || echo "packages: {}" > pubspec.lock;;
  esac
  mkdir -p .dart_tool && echo "{\"by\": \"$tool\"}" > .dart_tool/package_config.json
fi
for a in "$@"; do
  case "$a" in
    --file-reporter=json:*) printf '{"type":"done","success":true}\n' > "${a#--file-reporter=json:}";;
    --coverage=*) mkdir -p "${a#--coverage=}";;
    --out=*) printf 'SF:lib/gen.dart\nDA:1,1\nDA:2,0\nend_of_record\n' > "${a#--out=}";;
  esac
done
prev=""
for a in "$@"; do
  if [ "$prev" = "--output" ]; then cp "` + filepath.Join(bin, "junit.xml") + `" "$a"; fi
  prev="$a"
done
`
	writeFile(t, bin, "flutter", `#!/bin/sh
tool=flutter
if [ "$1" = --version ]; then printf 'Flutter `+flutterVersion+` • channel stable • https://github.com/flutter/flutter.git\nTools • Dart `+dartVersion+`\n'; exit 0; fi
`+common+`
if [ "$1" = test ]; then mkdir -p coverage; printf 'SF:lib/src/price.dart\nDA:3,1\nDA:6,1\nDA:9,0\nend_of_record\n' > coverage/lcov.info; fi
echo "PORT=$PORT" >> "`+log+`.env"
exit ${STUB_EXIT:-0}
`)
	writeFile(t, bin, "dart", `#!/bin/sh
tool=dart
if [ "$1" = --version ]; then echo 'Dart SDK version: `+dartVersion+` (stable) (Wed Feb 5 2025) on "linux_x64"'; exit 0; fi
`+common+`
echo "PORT=$PORT" >> "`+log+`.env"
exit ${STUB_EXIT:-0}
`)
	for _, tool := range []string{"tojunit", "format_coverage"} {
		writeFile(t, bin, tool, "#!/bin/sh\ntool="+tool+"\n"+common)
	}
	for _, f := range []string{"dart", "flutter", "tojunit", "format_coverage"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The step tests run the SDKs on PATH: an offline run installs none.
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	return log
}

const junitPass = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites><testsuite name="test.price" tests="2">
  <testcase classname="test.price" name="formats euros"/>
  <testcase classname="test.price" name="adds the VAT"/>
</testsuite></testsuites>`

const junitFail = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites><testsuite name="test.price" tests="1">
  <testcase classname="test.price" name="formats euros"><failure message="Expected: '€19.99'"/></testcase>
</testsuite></testsuites>`

// runJob runs a task of the package at rel through the extension's job, the
// way the CLI calls it.
func runJob(t *testing.T, root, rel, task string) (string, map[string]any, string, error) {
	t.Helper()
	s := stack(t)
	var meta json.RawMessage
	for _, p := range probe(t, root).Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	out := filepath.Join(root, ".putnami/out", strings.ReplaceAll(rel, "/", "_"), task)
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	status, data, runErr := s.Jobs()[task](ctx, jsonl.New(), []string{"--report-dir", out})
	os.Stdout = stdout
	return status, data, out, runErr
}

func ran(t *testing.T, log string) string {
	t.Helper()
	data, _ := os.ReadFile(log)
	if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestATaskRunsTheVariantStepsAfterTheToolchainCheck(t *testing.T) {
	spectest.Proves(t, feature, "tasks-run-the-profile", "a-task-runs-the-variant-steps-after-the-toolchain-check")
	root := fixture(t)
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	mobile, gen := filepath.Join(root, "apps/mobile"), filepath.Join(root, "tools/gen")

	if status, _, _, err := runJob(t, root, "apps/mobile", stacks.TaskLint); status != "OK" || err != nil {
		t.Fatalf("lint mobile: %s %v", status, err)
	}
	if got, want := ran(t, log), mobile+": dart analyze lib\n"; got != want {
		t.Errorf("a Flutter package resolves with flutter and analyzes lib:\n%s\nwant\n%s", got, want)
	}
	if status, _, _, err := runJob(t, root, "tools/gen", stacks.TaskLint); status != "OK" || err != nil {
		t.Fatalf("lint gen: %s %v", status, err)
	}
	if got, want := ran(t, log), gen+": dart analyze lib\n"+gen+": dart analyze bin\n"; got != want {
		t.Errorf("a Dart package resolves with dart and analyzes lib and bin:\n%s\nwant\n%s", got, want)
	}

	status, _, out, err := runJob(t, root, "tools/gen", stacks.TaskBuild)
	if status != "OK" || err != nil {
		t.Fatalf("build gen: %s %v", status, err)
	}
	if got, want := ran(t, log), gen+": dart compile exe bin/gen.dart -o "+filepath.Join(out, "build", "gen")+"\n"; got != want {
		t.Errorf("a Dart package compiles each executable:\n%s\nwant\n%s", got, want)
	}
	if status, _, _, err := runJob(t, root, "apps/mobile", stacks.TaskBuild); status != "OK" || err != nil || ran(t, log) != "" {
		t.Errorf("an app with no platform directory builds nothing: %s %v", status, err)
	}
	writeFile(t, root, "apps/mobile/web/index.html", "<html></html>\n")
	if status, _, _, err := runJob(t, root, "apps/mobile", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("build mobile: %s %v", status, err)
	}
	if got, want := ran(t, log), mobile+": flutter build web\n"; got != want {
		t.Errorf("an app builds each platform directory it holds:\n%s\nwant\n%s", got, want)
	}
	writeFile(t, root, "packages/ui/web/index.html", "<html></html>\n")
	if status, _, _, err := runJob(t, root, "packages/ui", stacks.TaskBuild); status != "OK" || err != nil || strings.Contains(ran(t, log), "flutter build") {
		t.Errorf("a package with no lib/main.dart is no app: %s %v", status, err)
	}

	stubTools(t, "3.27.4", "3.7.2", junitPass)
	if status, _, _, err := runJob(t, root, "apps/mobile", stacks.TaskLint); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("a Flutter version outside environment.flutter fails the task: %s %v", status, err)
	}
	stubTools(t, "3.29.2", "3.6.2", junitPass)
	if status, _, _, err := runJob(t, root, "packages/pricing", stacks.TaskLint); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("the Dart SDK a Flutter SDK bundles is checked against environment.sdk: %s %v", status, err)
	}
	if status, _, _, err := runJob(t, root, "tools/gen", stacks.TaskLint); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("a Dart version outside environment.sdk fails the task: %s %v", status, err)
	}
	writeFile(t, root, "apps/mobile/putnami.json", `{"options": {"@putnami/dart": {"toolchain": {"version": "3.29.2"}}}}`)
	stubTools(t, "3.35.0", "3.9.0", junitPass)
	if status, _, _, err := runJob(t, root, "apps/mobile", stacks.TaskLint); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("an exact toolchain.version pins Flutter, whatever pub's reading of the pubspec: %s %v", status, err)
	}
	writeFile(t, root, "tools/gen/pubspec.yaml", "name: gen\nenvironment:\n  sdk: '>=2.17.0 <3.0.0'\n")
	stubTools(t, "3.29.2", "3.7.2", junitPass)
	if status, _, _, err := runJob(t, root, "tools/gen", stacks.TaskLint); status != "OK" || err != nil {
		t.Errorf("a null-safe <3.0.0 reads as <4.0.0, as pub reads it: %s %v", status, err)
	}
}

func TestTojunitAndLCOVBecomeTheTestAndCoverageSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-become-the-summary", "tojunit-and-lcov-become-the-test-and-coverage-summary")
	root := fixture(t)
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	pricing := filepath.Join(root, "packages/pricing")
	status, data, out, err := runJob(t, root, "packages/pricing", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test pricing: %s %v", status, err)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 2 || got["passed"] != 2 || got["failed"] != 0 {
		t.Errorf("testSummary = %v", got)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 2 || got["total"] != 3 {
		t.Errorf("coverageSummary = %v", got)
	}
	tests := filepath.Join(out, "test", "tests.json")
	if got, want := ran(t, log), pricing+": flutter test --no-pub --coverage --file-reporter=json:"+tests+"\n"+
		pricing+": tojunit --input "+tests+" --output "+filepath.Join(out, "test", "junit.xml")+" --base "+pricing+"/\n"; got != want {
		t.Errorf("test ran\n%s\nwant\n%s", got, want)
	}
	if _, err := os.Stat(filepath.Join(out, "test", "reports", "01-lcov.info")); err != nil {
		t.Errorf("coverage/lcov.info is copied into the task output: %v", err)
	}

	status, data, out, err = runJob(t, root, "tools/gen", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test gen: %s %v", status, err)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 1 || got["total"] != 2 {
		t.Errorf("a Dart package's format_coverage report is read: %v", got)
	}
	if got := ran(t, log); !strings.Contains(got, ": dart test --coverage="+filepath.Join(out, "test", "coverage")) ||
		!strings.Contains(got, ": format_coverage --lcov --in="+filepath.Join(out, "test", "coverage")+" --out="+filepath.Join(out, "test", "lcov.info")) {
		t.Errorf("a Dart package runs dart test and format_coverage:\n%s", got)
	}

	stubTools(t, "3.29.2", "3.7.2", junitFail)
	if status, data, _, err := runJob(t, root, "packages/pricing", stacks.TaskTest); status != "FAILED" || err == nil || data["testSummary"].(map[string]any)["failed"] != 1 {
		t.Errorf("a failed case fails the task: %s %v %v", status, err, data)
	}
	// A runner exits non-zero on a failed case: tojunit still converts its
	// report, so the failed case names the failure.
	t.Setenv("STUB_EXIT", "1")
	if status, data, _, err := runJob(t, root, "packages/pricing", stacks.TaskTest); status != "FAILED" || err == nil || data["testSummary"].(map[string]any)["failed"] != 1 {
		t.Errorf("a failing runner's case is still named: %s %v %v", status, err, data)
	}
}

func resolve(t *testing.T, root, project, selector string) wsproto.InputResolveAnswer {
	t.Helper()
	s := stack(t)
	result := s.ResolveInputs(root, wsproto.InputResolveRequest{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
		Inputs: []wsproto.InputResolveQuery{{Project: project, Selector: selector}}})
	if diags := wsproto.ValidateInputResolveResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	return result.Inputs[0]
}

func TestTheLintSelectorReadsNoTestFile(t *testing.T) {
	spectest.Proves(t, feature, "lint-skips-test-changes", "the-lint-selector-reads-no-test-file")
	root := fixture(t)
	writeFile(t, root, "packages/pricing/assets/rates.json", "{}")
	writeFile(t, root, "apps/mobile/web/index.html", "<html></html>\n")
	writeFile(t, root, "apps/mobile/linux/CMakeLists.txt", "project(mobile)\n")
	writeFile(t, root, "apps/mobile/linux/flutter/ephemeral/generated_config.cmake", "# written by flutter build\n")
	writeFile(t, root, "apps/mobile/android/app/src/main/AndroidManifest.xml", "<manifest/>\n")
	writeFile(t, root, "apps/mobile/android/app/src/main/java/io/flutter/plugins/GeneratedPluginRegistrant.java", "// written by flutter pub get\n")
	if got := resolve(t, root, "packages/pricing", "dart:lint").Files; !reflect.DeepEqual(got, []string{"lib/pricing.dart", "lib/src/price.dart", "pubspec.yaml"}) {
		t.Errorf("lint inputs = %v", got)
	}
	if got := resolve(t, root, "packages/pricing", "dart:test").Files; !reflect.DeepEqual(got, []string{"assets/rates.json", "lib/pricing.dart", "lib/src/price.dart", "pubspec.yaml", "test/price_test.dart"}) {
		t.Errorf("test inputs = %v", got)
	}
	if got := resolve(t, root, "apps/mobile", "dart:build").Files; !reflect.DeepEqual(got, []string{"android/app/src/main/AndroidManifest.xml", "lib/main.dart", "linux/CMakeLists.txt", "pubspec.yaml", "web/index.html"}) {
		t.Errorf("build inputs = %v", got)
	}
	if got := resolve(t, root, "tools/gen", "dart:lint").Files; !reflect.DeepEqual(got, []string{"bin/gen.dart", "lib/gen.dart", "pubspec.lock", "pubspec.yaml"}) {
		t.Errorf("a package that is its own stack root reads its lockfile: %v", got)
	}
}

func TestAPutnamiJSONOverrideChangesTheStepsAndTheInputs(t *testing.T) {
	spectest.Proves(t, feature, "overrides-are-honored", "a-putnami-json-override-changes-the-steps-and-the-inputs")
	root := fixture(t)
	writeFile(t, root, "packages/pricing/fixtures/rates.json", "{}")
	writeFile(t, root, "packages/pricing/putnami.json", `{"options": {"@putnami/dart": {
	  "inputs": {"code": ["lib/**", "bin/**", "fixtures/**"]},
	  "tasks": {"lint": {"steps": [["flutter", "analyze", "--no-pub"]]}}
	}}}`)
	if got := resolve(t, root, "packages/pricing", "dart:lint").Files; !reflect.DeepEqual(got, []string{"fixtures/rates.json", "lib/pricing.dart", "lib/src/price.dart", "pubspec.yaml", "putnami.json"}) {
		t.Errorf("an input override is honored: %v", got)
	}
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	if status, _, _, err := runJob(t, root, "packages/pricing", stacks.TaskLint); status != "OK" || err != nil {
		t.Fatalf("lint: %s %v", status, err)
	}
	if got := ran(t, log); got != filepath.Join(root, "packages/pricing")+": flutter analyze --no-pub\n" {
		t.Errorf("a step override is honored on the flutter variant: %q", got)
	}

	for _, bad := range []string{
		`{"options": {"@putnami/dart": {"tasks": {"lint": {"stepz": []}}}}}`,
		`{"options": {"@putnami/dart": {"variants": {}}}}`,
		`{"options": {"@putnami/dart": {"inputs": {"code": ["lib/**"]}}}}`,
		`{"options": {"@putnami/dart": {"tasks": {"lint": {"targets": [{"name": "lib", "when": ["lib"], "hosts": ["darwin"]}]}}}}}`,
		`{"options": {"@putnami/dart": {"inputs": {"manifests": ["pubspec.yaml", "{root}/melos.yaml"]}}}}`,
	} {
		writeFile(t, root, "packages/pricing/putnami.json", bad)
		if status, _, _, err := runJob(t, root, "packages/pricing", stacks.TaskLint); status != "FAILED" || err == nil || err.Error() != stacks.CodeProjectConfigInvalid {
			t.Errorf("%s fails the task: %s %v", bad, status, err)
		}
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	stackstest.CheckManifest(t, "putnami.extension.json", stack(t))
	if got := stack(t).Selectors(); !slices.Equal(got, []string{"dart:build", "dart:lint", "dart:test"}) {
		t.Errorf("selectors = %v", got)
	}
}

func TestAPureDartPackageInAFlutterWorkspaceResolvesWithFlutter(t *testing.T) {
	spectest.Proves(t, feature, "variant-follows-the-resolver", "a-pure-dart-package-in-a-flutter-workspace-resolves-with-flutter")
	root := fixture(t)
	pubspec, err := os.ReadFile(filepath.Join(root, "pubspec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "pubspec.yaml", strings.Replace(string(pubspec), "  - packages/ui\n", "  - packages/ui\n  - packages/model\n", 1))
	writeFile(t, root, "packages/model/pubspec.yaml", "name: model\nresolution: workspace\nenvironment:\n  sdk: ^3.7.0\ndev_dependencies:\n  test: ^1.25.0\n")
	writeFile(t, root, "packages/model/lib/model.dart", "class Item {}\n")
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"packages/model", "packages/pricing", "tools/gen"}})
	if err != nil {
		t.Fatal(err)
	}
	variants := map[string]string{}
	for _, p := range result.Projects {
		var meta stacks.Metadata
		_ = json.Unmarshal(p.Metadata, &meta)
		variants[p.Path] = meta.Variant
	}
	if want := map[string]string{"packages/model": VariantDartInFlutter, "packages/pricing": VariantFlutter, "tools/gen": VariantDart}; !reflect.DeepEqual(variants, want) {
		t.Errorf("variants = %v, want %v", variants, want)
	}
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	if status, _, _, err := runJob(t, root, "packages/model", stacks.TaskLint); status != "OK" || err != nil {
		t.Fatalf("lint model: %s %v", status, err)
	}
	model := filepath.Join(root, "packages/model")
	if got, want := ran(t, log), model+": dart analyze lib\n"; got != want {
		t.Errorf("a pure Dart package of a Flutter workspace analyzes with dart:\n%s\nwant\n%s", got, want)
	}
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, _, err := runProjectJob(t, root, "packages/model", InstallJob(s), nil); status != "OK" || err != nil {
		t.Fatalf("install model: %s %v", status, err)
	}
	if got, want := ran(t, log), root+": flutter pub get --enforce-lockfile\n"; got != want {
		t.Errorf("its stack root resolves with flutter:\n%s\nwant\n%s", got, want)
	}
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if status, _, err := runProjectJob(t, root, "packages/model", InstallJob(s), nil); status != "OK" || err != nil || ran(t, log) != "" {
		t.Errorf("an offline install resolves nothing: %s %v", status, err)
	}

	// A dart that comes first on PATH is not the Flutter SDK's: run uses the
	// one beside the selected flutter.
	other := t.TempDir()
	writeFile(t, other, "dart", "#!/bin/sh\necho \"OTHER $*\" >> "+log+"\n")
	if err := os.Chmod(filepath.Join(other, "dart"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", other+string(os.PathListSeparator)+os.Getenv("PATH"))
	if status, _, err := runProjectJob(t, root, "packages/model", RunJob(s), nil); status != "OK" || err != nil {
		t.Fatalf("run model: %s %v", status, err)
	}
	if got := ran(t, log); got != model+": dart run\n" {
		t.Errorf("run uses the Flutter SDK's dart: %q", got)
	}
}

func TestANestedPubWorkspaceIsAStackRoot(t *testing.T) {
	spectest.Proves(t, feature, "packages-become-projects", "the-packages-of-a-pub-workspace-become-projects-with-their-edges")
	root := tempDir(t)
	writeFile(t, root, "mono/pubspec.yaml", "name: mono\npublish_to: none\nenvironment:\n  sdk: ^3.7.0\nworkspace:\n  - app\n  - core\n")
	writeFile(t, root, "mono/pubspec.lock", "packages: {}\n")
	writeFile(t, root, "mono/app/pubspec.yaml", "name: app\nresolution: workspace\nenvironment:\n  sdk: ^3.7.0\ndependencies:\n  core:\n")
	writeFile(t, root, "mono/app/bin/app.dart", "void main() {}\n")
	writeFile(t, root, "mono/core/pubspec.yaml", "name: core\nresolution: workspace\nenvironment:\n  sdk: ^3.7.0\n")
	writeFile(t, root, "mono/core/lib/core.dart", "int one() => 1;\n")
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: []string{"mono", "mono/app", "mono/core"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		got[p.Path] = p
	}
	var mono, app stacks.Metadata
	_ = json.Unmarshal(got["mono"].Metadata, &mono)
	_ = json.Unmarshal(got["mono/app"].Metadata, &app)
	if mono.Component != "" || mono.Root != "mono" || app.Root != "mono" {
		t.Errorf("mono = %+v, app = %+v", mono, app)
	}
	if want := []string{"mono", "mono/core"}; !reflect.DeepEqual(got["mono/app"].Dependencies, want) {
		t.Errorf("app depends on its workspace root, which carries the lockfile, and on core: %v", got["mono/app"].Dependencies)
	}
	if files := resolve(t, root, "mono", "dart:test").Files; !slices.Contains(files, "pubspec.lock") || !slices.Contains(files, "pubspec.yaml") {
		t.Errorf("the workspace root's selector answers its lockfile: %v", files)
	}

	// The jobs that run at a stack root run at the input-only root too.
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	monoDir := filepath.Join(root, "mono")
	// The input-only root resolves nothing itself, whatever SDK its own
	// pubspec admits: its members' installs resolve it.
	if status, _, err := runProjectJob(t, root, "mono", InstallJob(s), nil); status != "OK" || err != nil || ran(t, log) != "" {
		t.Fatalf("install mono: %s %v", status, err)
	}
	for _, name := range []string{"mono/app", "mono/core"} {
		if status, _, err := runProjectJob(t, root, name, InstallJob(s), nil); status != "OK" || err != nil {
			t.Fatalf("install %s: %s %v", name, status, err)
		}
	}
	if got := ran(t, log); got != monoDir+": dart pub get --enforce-lockfile\n" {
		t.Errorf("the root resolves once, then is current: %q", got)
	}
	for _, jobFunc := range []cli.JobFunc{WorkspaceFetchJob(s), WorkspaceInstallJob(s), DepsUpgradeJob(s)} {
		if status, err := runWorkspaceJob(t, root, jobFunc, job2Force(), "mono", "mono/app", "mono/core"); status != "OK" || err != nil {
			t.Errorf("a workspace job over a nested workspace: %s %v", status, err)
		}
	}
	if got := ran(t, log); strings.Count(got, monoDir+": dart pub") != 3 {
		t.Errorf("each workspace job runs once at the nested root: %q", got)
	}
	if status, _, _, err := runJob(t, root, "mono", stacks.TaskLint); status != "OK" || err != nil {
		t.Errorf("the input-only root's lint runs no step: %s %v", status, err)
	}
}

// job2Force is the force flag of workspace-install.
func job2Force() job.Params { return job.Params{"force": json.RawMessage(`true`)} }
