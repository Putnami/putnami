package swift

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	observeswift "go.putnami.dev/tooling/observe/detect/swift"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "swift/stack-extension"

// asRuntime makes the test binary stand in for the extension runtime: a
// step that runs {runtime} runs it again with this variable set.
const asRuntime = "PUTNAMI_SWIFT_TEST_RUNTIME"

func TestMain(m *testing.M) {
	if os.Getenv(asRuntime) == "1" {
		s, err := Stack()
		if err == nil {
			err = s.Entrypoint(os.Args[1:], os.Stdin, os.Stdout, "test", nil)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var txtarFile = regexp.MustCompile(`(?m)^-- (.+?) --\n`)

// fixture writes the observe Swift demo repository: App/Shop.xcodeproj uses
// Packages/Pricing, which uses Packages/Networking.
func fixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "shop.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	// The stub tools print their resolved working directory, so the root is
	// resolved too: on macOS the temporary directory is a symlink.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	marks := txtarFile.FindAllStringSubmatchIndex(src, -1)
	for i, m := range marks {
		end := len(src)
		if i+1 < len(marks) {
			end = marks[i+1][0]
		}
		stackstest.WriteFile(t, root, src[m[2]:m[3]], src[m[1]:end])
	}
	return root
}

func stack(t *testing.T) stacks.Stack {
	t.Helper()
	s, err := Stack()
	if err != nil {
		t.Fatal(err)
	}
	s.Runtime = os.Args[0]
	return s
}

var candidates = []string{".", "App", "Packages/Networking", "Packages/Pricing", "Shop.xcworkspace"}

func probe(t *testing.T, root string) wsproto.ProbeResult {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, Paths: candidates})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestThePackagesAndXcodeProjectsBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "components-become-projects", "the-packages-and-xcode-projects-become-projects-with-their-edges")
	result := probe(t, fixture(t))
	if len(result.Projects) != 3 || len(result.Diagnostics) != 0 {
		t.Fatalf("projects = %+v, diagnostics = %v", result.Projects, result.Diagnostics)
	}
	want := map[string]struct {
		name, variant, source string
		deps                  []string
	}{
		"App":                 {"Shop", VariantXcode, "App/Shop.xcodeproj/project.pbxproj", []string{"Packages/Pricing"}},
		"Packages/Networking": {"Networking", VariantSwiftPM, "Packages/Networking/Package.swift", nil},
		"Packages/Pricing":    {"Pricing", VariantSwiftPM, "Packages/Pricing/Package.swift", []string{"Packages/Networking"}},
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
		if p.SourceName != w.name || p.SourceFile != w.source || !reflect.DeepEqual(p.Dependencies, w.deps) ||
			meta.Component != w.name || meta.Root != p.Path || meta.Variant != w.variant {
			t.Errorf("%s = %+v, metadata %+v", p.Path, p, meta)
		}
		if !reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"swift"}) {
			t.Errorf("%s extensions %v tags %v", p.Path, p.Extensions, p.Tags)
		}
	}

	// A directory with a package and an Xcode project keeps the package.
	root := fixture(t)
	stackstest.WriteFile(t, root, "App/Package.swift", "let package = Package(name: \"AppKit\")\n")
	result = probe(t, root)
	if p := result.Projects[0]; p.Path != "App" || p.SourceName != "AppKit" || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != stacks.CodeComponentConflict {
		t.Errorf("conflict: %+v %v", p, result.Diagnostics)
	}
}

// stubs puts first on PATH a swift, llvm-cov, xcodebuild and xcrun that log
// their arguments and write what the real tools write: the stack's own
// build is the external boundary. dir holds what they print.
type stubs struct {
	log, dir string
}

func stubTools(t *testing.T) stubs {
	t.Helper()
	bin := t.TempDir()
	s := stubs{log: filepath.Join(bin, "tools.log"), dir: t.TempDir()}
	scripts := map[string]string{
		"swift": `if [ "$1" = format ]; then
  # swift-format parses a file it is named as Swift, whatever its extension.
  prev=""
  for a in "$@"; do
    if [ "$prev" != "--configuration" ]; then case "$a" in *.swift) ;; *) if [ -f "$a" ]; then echo "$a: not Swift" >&2; exit 1; fi;; esac; fi
    prev="$a"
  done
  exit 0
fi
case "$1" in
run) echo "PORT=$PORT" >> "$STUB_DIR/run.log"; exit ${STUB_RUN_EXIT:-0};;
--version) echo "swift-driver version: 1.115 Apple Swift version ${SWIFT_STUB_VERSION:-6.0.2} (swiftlang-6.0.2.1.2 clang-1600.0.26.4)"; echo "Target: arm64-apple-macosx15.0";;
build) if [ "$2" = "--show-bin-path" ]; then echo "Building for debugging..."; echo "$PWD/.build/debug"; fi;;
test)
  name=$(basename "$PWD")
  rm -rf .build/debug/codecov
  mkdir -p .build/debug
  : > ".build/debug/${name}PackageTests.xctest"
  prev=""
  for a in "$@"; do if [ "$prev" = "--xunit-output" ] && [ -f "$STUB_DIR/xunit-$name.xml" ]; then cat "$STUB_DIR/xunit-$name.xml" > "$a"; fi; prev="$a"; done
  # Like SwiftPM, a failed run writes no coverage profile.
  if [ -n "$STUB_FAIL" ]; then exit 1; fi
  mkdir -p .build/debug/codecov; : > .build/debug/codecov/default.profdata;;
esac`,
		"llvm-cov": `cat "$STUB_DIR/lcov-$(basename "$PWD").info"`,
		"xcodebuild": `case "$1" in
-version) echo "Xcode 16.1"; echo "Build version 16B40";;
-showBuildSettings) cat "$STUB_DIR/settings.json";;
test) prev=""; for a in "$@"; do if [ "$prev" = "-resultBundlePath" ]; then mkdir -p "$a"; fi; prev="$a"; done
  if [ -n "$STUB_FAIL" ]; then exit 65; fi;;
esac`,
		"xcrun": `case "$1 $2" in
"simctl list") cat "$STUB_DIR/simctl.json";;
"xcresulttool get") cat "$STUB_DIR/tests.json";;
"xccov view") if [ "$3" = "--report" ]; then cat "$STUB_DIR/report.json"; else cat "$STUB_DIR/archive.json"; fi;;
esac`,
	}
	for name, body := range scripts {
		stackstest.WriteFile(t, bin, name, "#!/bin/sh\necho \"$PWD: "+name+" $*\" >> \""+s.log+"\"\n"+body+"\n")
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_DIR", s.dir)
	t.Setenv(asRuntime, "1")
	return s
}

func (s stubs) write(t *testing.T, name, content string) {
	t.Helper()
	stackstest.WriteFile(t, s.dir, name, content)
}

func (s stubs) ran(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(s.log)
	return string(data)
}

// demoReports writes what the tools report on the demo: the xunit and the
// LCOV of both packages, and the result bundle of the Shop app.
func (s stubs) demoReports(t *testing.T, root string, pricingFails bool) {
	t.Helper()
	pricingCase := `<testcase classname="PricingTests.PriceCalculatorTests" name="testTotalAddsTax" time="0.001"></testcase>`
	if pricingFails {
		pricingCase = `<testcase classname="PricingTests.PriceCalculatorTests" name="testTotalAddsTax" time="0.001"><failure message="XCTAssertEqual failed"></failure></testcase>`
	}
	s.write(t, "xunit-Pricing.xml", `<?xml version="1.0" encoding="UTF-8"?>
<testsuites><testsuite name="TestResults" errors="0" tests="3" failures="0" time="0.003">
`+pricingCase+`
<testcase classname="PricingTests.PriceCalculatorTests" name="testTotalOfNothingIsZero" time="0.000"></testcase>
<testcase classname="PricingTests.PriceCalculatorTests" name="testQuoteURLNamesTheSKU" time="0.001"></testcase>
</testsuite></testsuites>`)
	s.write(t, "xunit-Networking.xml", `<?xml version="1.0" encoding="UTF-8"?>
<testsuites><testsuite name="TestResults" errors="0" tests="1" failures="0" time="0.002">
<testcase classname="NetworkingTests.ClientTests" name="testURLAppendsThePath" time="0.002"></testcase>
</testsuite></testsuites>`)
	// The tests of Pricing link Networking: llvm-cov reports its file too.
	s.write(t, "lcov-Pricing.info", "SF:"+filepath.Join(root, "Packages/Pricing/Sources/Pricing/PriceCalculator.swift")+"\nDA:6,3\nDA:11,2\nDA:12,2\nDA:16,1\nend_of_record\n"+
		"SF:"+filepath.Join(root, "Packages/Networking/Sources/Networking/Client.swift")+"\nDA:7,1\nDA:11,0\nend_of_record\n")
	s.write(t, "lcov-Networking.info", "SF:"+filepath.Join(root, "Packages/Networking/Sources/Networking/Client.swift")+"\nDA:7,1\nDA:11,1\nend_of_record\n")
	s.write(t, "settings.json", `[{"action":"build","target":"Shop","buildSettings":{"PLATFORM_NAME":"iphonesimulator","SDKROOT":"iphonesimulator"}}]`)
	s.write(t, "simctl.json", `{"devices":{
  "com.apple.CoreSimulator.SimRuntime.iOS-17-5":[{"name":"iPhone 15","udid":"OLD-UDID","isAvailable":true}],
  "com.apple.CoreSimulator.SimRuntime.iOS-18-1":[{"name":"iPhone 16","udid":"NEW-UDID","isAvailable":true}],
  "com.apple.CoreSimulator.SimRuntime.watchOS-11-1":[{"name":"Apple Watch","udid":"WATCH","isAvailable":true}]}}`)
	s.write(t, "tests.json", `{"testNodes":[{"name":"Shop","nodeType":"Test Plan","result":"Passed","children":[
  {"name":"ShopTests","nodeType":"Unit test bundle","result":"Passed","children":[
    {"name":"CheckoutTests","nodeType":"Test Suite","result":"Passed","children":[
      {"name":"testTotalAddsTax()","nodeType":"Test Case","result":"Passed","durationInSeconds":0.002},
      {"name":"testEmptyCartIsFree()","nodeType":"Test Case","result":"Passed","durationInSeconds":0.001}]}]}]}]}`)
	checkout := filepath.Join(root, "App/Shop/Checkout.swift")
	s.write(t, "report.json", `{"targets":[
  {"name":"Shop.app","files":[{"path":"`+checkout+`"},{"path":"/Users/runner/Library/Developer/Xcode/DerivedData/Shop/SourcePackages/checkouts/swift-log/Sources/Logging/Logging.swift"}]},
  {"name":"Pricing","files":[{"path":"`+filepath.Join(root, "Packages/Pricing/Sources/Pricing/PriceCalculator.swift")+`"}]},
  {"name":"ShopTests.xctest","files":[{"path":"`+filepath.Join(root, "App/ShopTests/CheckoutTests.swift")+`"}]}]}`)
	s.write(t, "archive.json", `{"`+checkout+`":[
  {"line":1,"isExecutable":false,"executionCount":0},
  {"line":6,"isExecutable":true,"executionCount":2},
  {"line":7,"isExecutable":true,"executionCount":2},
  {"line":8,"isExecutable":true,"executionCount":0}]}`)
}

// runJob runs a task of the component at rel through the extension's job,
// the way the CLI calls it.
func runJob(t *testing.T, root, rel, task string) (string, map[string]any, string, error) {
	t.Helper()
	return runJobWith(t, root, rel, task, stack(t).Jobs()[task], nil)
}

// runJobWith runs a job of the component at rel with params, the way the CLI
// calls it.
func runJobWith(t *testing.T, root, rel, task string, run cli.JobFunc, params job.Params) (string, map[string]any, string, error) {
	t.Helper()
	var meta json.RawMessage
	for _, p := range probeWith(t, root, rel).Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Params: params, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	out := filepath.Join(root, ".putnami/out", strings.ReplaceAll(rel, "/", "_"), task)
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devnull
	status, data, runErr := run(ctx, jsonl.New(), []string{"--report-dir", out})
	os.Stdout = stdout
	return status, data, out, runErr
}

func summary(t *testing.T, data map[string]any, key string) map[string]any {
	t.Helper()
	got, ok := data[key].(map[string]any)
	if !ok {
		t.Fatalf("no %s in %v", key, data)
	}
	return got
}

func TestAPackageRunsSwiftAndReadsItsXunitAndLCOVReports(t *testing.T) {
	spectest.Proves(t, feature, "packages-run-swiftpm", "a-package-runs-swift-and-reads-its-xunit-and-lcov-reports")
	root := fixture(t)
	s := stubTools(t)
	s.demoReports(t, root, false)
	pricing := filepath.Join(root, "Packages/Pricing")
	// Without a .swift-format, lint runs nothing: the default style is no
	// team's style.
	if status, _, _, err := runJob(t, root, "Packages/Pricing", stacks.TaskLint); status != "OK" || err != nil || strings.Contains(s.ran(t), "swift format") {
		t.Fatalf("lint without a configuration: %s %v %s", status, err, s.ran(t))
	}
	stackstest.WriteFile(t, root, "putnami.workspace.json", "{}")
	stackstest.WriteFile(t, root, ".swift-format", `{"indentation": {"spaces": 4}}`)
	for _, task := range []string{stacks.TaskLint, stacks.TaskBuild} {
		if status, _, _, err := runJob(t, root, "Packages/Pricing", task); status != "OK" || err != nil {
			t.Fatalf("%s: %s %v", task, status, err)
		}
	}
	status, data, out, err := runJob(t, root, "Packages/Pricing", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v", status, err)
	}
	if got := summary(t, data, "testSummary"); got["total"] != 3 || got["passed"] != 3 {
		t.Errorf("testSummary = %v", got)
	}
	if got := summary(t, data, "coverageSummary"); got["covered"] != 4 || got["total"] != 4 {
		t.Errorf("only the package's own files are covered: %v", got)
	}
	ran := s.ran(t)
	for _, want := range []string{
		pricing + ": swift --version",
		pricing + ": swift format lint --strict --configuration " + filepath.Join(root, ".swift-format") + " --recursive Package.swift Sources",
		pricing + ": swift build --force-resolved-versions\n",
		pricing + ": swift test --force-resolved-versions --enable-code-coverage --parallel --xunit-output " + filepath.Join(out, "test", "xunit.xml"),
		pricing + ": swift build --show-bin-path",
		pricing + ": llvm-cov export -format=lcov -instr-profile " + filepath.Join(pricing, ".build/debug/codecov/default.profdata") +
			` -ignore-filename-regex (\.build|/Tests)/ ` + filepath.Join(pricing, ".build/debug/PricingPackageTests.xctest"),
	} {
		if !strings.Contains(ran, want) {
			t.Errorf("the tools ran\n%s\nwant %q", ran, want)
		}
	}

	// swift test exits non-zero on a failed case: the task reads it.
	s.demoReports(t, root, true)
	t.Setenv("STUB_FAIL", "1")
	if status, data, _, err := runJob(t, root, "Packages/Pricing", stacks.TaskTest); status != "FAILED" || err == nil || summary(t, data, "testSummary")["failed"] != 1 {
		t.Errorf("a failed case fails the task, and names the case with no coverage profile: %s %v %v", status, err, data)
	}
	// A swift test that fails with no failed case, a compile error, fails.
	s.demoReports(t, root, false)
	if status, _, _, err := runJob(t, root, "Packages/Pricing", stacks.TaskTest); status != "FAILED" || err == nil || err.Error() != stacks.CodeStepFailed {
		t.Errorf("a failed swift test with no failed case fails the task: %s %v", status, err)
	}
	t.Setenv("STUB_FAIL", "")

	// Offline, a mismatch installs nothing and fails the task.
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	stackstest.WriteFile(t, root, "Packages/Pricing/.swift-version", "5.10\n")
	if status, _, _, err := runJob(t, root, "Packages/Pricing", stacks.TaskBuild); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("a toolchain mismatch fails the task: %s %v", status, err)
	}
	stackstest.WriteFile(t, root, "Packages/Pricing/.swift-version", "6.0\n")
	if status, _, _, err := runJob(t, root, "Packages/Pricing", stacks.TaskBuild); status != "OK" || err != nil {
		t.Errorf("6.0 names the 6.0 releases: %s %v", status, err)
	}
}

func TestAnXcodeProjectRunsXcodebuildAndReadsItsConvertedResultBundle(t *testing.T) {
	spectest.Proves(t, feature, "xcode-projects-run-xcodebuild", "an-xcode-project-runs-xcodebuild-and-reads-its-converted-result-bundle")
	root := fixture(t)
	stackstest.WriteFile(t, root, "App/.xcode-version", "16.1\n")
	s := stubTools(t)
	s.demoReports(t, root, false)
	app := filepath.Join(root, "App")
	stackstest.WriteFile(t, root, "App/.swift-format", "{}")
	stackstest.WriteFile(t, root, "App/Pods/Alamofire/Source/AF.swift", "")
	stackstest.WriteFile(t, root, "App/README.md", "# Shop\n")
	stackstest.WriteFile(t, root, "App/Info.plist", "<plist/>\n")
	stackstest.WriteFile(t, root, "App/AppDelegate.swift", "")
	for _, task := range []string{stacks.TaskLint, stacks.TaskBuild} {
		if status, _, _, err := runJob(t, root, "App", task); status != "OK" || err != nil {
			t.Fatalf("%s: %s %v", task, status, err)
		}
	}
	status, data, out, err := runJob(t, root, "App", stacks.TaskTest)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v", status, err)
	}
	if got := summary(t, data, "testSummary"); got["total"] != 2 || got["passed"] != 2 {
		t.Errorf("testSummary = %v", got)
	}
	if got := summary(t, data, "coverageSummary"); got["covered"] != 2 || got["total"] != 3 {
		t.Errorf("only the app's own files are covered: %v", got)
	}
	bundle := filepath.Join(out, "test", "Tests.xcresult")
	ran := s.ran(t)
	for _, want := range []string{
		app + ": xcodebuild -version",
		app + ": swift format lint --strict --configuration " + filepath.Join(app, ".swift-format") + " --recursive AppDelegate.swift Shop Shop.xcodeproj ShopTests\n",
		app + ": xcodebuild build -scheme Shop -disableAutomaticPackageResolution -destination generic/platform=iOS Simulator CODE_SIGNING_ALLOWED=NO",
		app + ": xcodebuild -showBuildSettings -scheme Shop -disableAutomaticPackageResolution -json",
		app + ": xcodebuild test -scheme Shop -disableAutomaticPackageResolution -destination platform=iOS Simulator,id=NEW-UDID -enableCodeCoverage YES -resultBundlePath " + bundle + " CODE_SIGNING_ALLOWED=NO",
		app + ": xcrun xcresulttool get test-results tests --path " + bundle,
		app + ": xcrun xccov view --archive --file " + filepath.Join(app, "Shop/Checkout.swift") + " --json " + bundle,
	} {
		if !strings.Contains(ran, want) {
			t.Errorf("the tools ran\n%s\nwant %q", ran, want)
		}
	}
	if strings.Contains(ran, "--file /Users/runner") || strings.Contains(ran, "CheckoutTests.swift --json") || strings.Contains(ran, "PriceCalculator.swift --json") {
		t.Errorf("a package checkout, a test bundle and a local package are no coverage of the app: %s", ran)
	}

	// xcodebuild test exits non-zero on a failed case: the task reads it;
	// with no failed case, the task fails.
	t.Setenv("STUB_FAIL", "1")
	s.write(t, "tests.json", `{"testNodes":[{"name":"ShopTests","nodeType":"Unit test bundle","children":[{"name":"CheckoutTests","nodeType":"Test Suite","children":[
	  {"name":"testTotalAddsTax()","nodeType":"Test Case","result":"Failed","children":[{"name":"XCTAssertEqual failed","nodeType":"Failure Message"}]}]}]}]}`)
	if status, data, _, err := runJob(t, root, "App", stacks.TaskTest); status != "FAILED" || err == nil || summary(t, data, "testSummary")["failed"] != 1 {
		t.Errorf("a failed case fails the task: %s %v %v", status, err, data)
	}
	s.demoReports(t, root, false)
	if status, _, _, err := runJob(t, root, "App", stacks.TaskTest); status != "FAILED" || err == nil || err.Error() != stacks.CodeStepFailed {
		t.Errorf("a failed xcodebuild with no failed case fails the task: %s %v", status, err)
	}
	t.Setenv("STUB_FAIL", "")

	stackstest.WriteFile(t, root, "App/.xcode-version", "15.4\n")
	if status, _, _, err := runJob(t, root, "App", stacks.TaskTest); status != "FAILED" || err == nil || err.Error() != stacks.CodeToolchainMismatch {
		t.Errorf("an Xcode mismatch fails the task: %s %v", status, err)
	}
}

var (
	classAttr = regexp.MustCompile(`classname="([^"]+)"`)
	lcovFile  = regexp.MustCompile(`(?m)^SF:(.+)$`)
	coberFile = regexp.MustCompile(`filename="([^"]+)"`)
)

func TestEveryCaseAndCoverageLineOfTheDemoPlacesOnAComponent(t *testing.T) {
	spectest.Proves(t, feature, "reports-are-attributed", "every-case-and-coverage-line-of-the-demo-places-on-a-component")
	root := fixture(t)
	s := stubTools(t)
	s.demoReports(t, root, false)
	projects := map[string]bool{}
	for _, p := range probe(t, root).Projects {
		projects[p.Path] = true
	}
	owner := func(rel string) string {
		for dir := rel; dir != "." && dir != "/"; dir = filepath.Dir(dir) {
			if projects[filepath.ToSlash(dir)] {
				return filepath.ToSlash(dir)
			}
		}
		return ""
	}
	detection := observeswift.Detect(root)
	cases, files := 0, 0
	for path := range projects {
		status, _, out, err := runJob(t, root, path, stacks.TaskTest)
		if status != "OK" || err != nil {
			t.Fatalf("%s test: %s %v", path, status, err)
		}
		reports, _ := filepath.Glob(filepath.Join(out, "test", "*"))
		for _, report := range reports {
			data, err := os.ReadFile(report)
			if err != nil {
				continue
			}
			for _, m := range classAttr.FindAllStringSubmatch(string(data), -1) {
				cases++
				file, component, ok := detection.Locate(m[1])
				if !ok || owner(component) != path || owner(file) != path {
					t.Errorf("%s: the case class %s places on %q (%q), not on %s", report, m[1], component, file, path)
				}
			}
			for _, re := range []*regexp.Regexp{lcovFile, coberFile} {
				for _, m := range re.FindAllStringSubmatch(string(data), -1) {
					files++
					rel, err := filepath.Rel(root, m[1])
					if err != nil || owner(filepath.ToSlash(rel)) != path {
						t.Errorf("%s: the covered file %s is under no component, or not under %s", report, m[1], path)
					}
				}
			}
		}
	}
	// 3 Pricing cases, 1 Networking case and 2 Shop cases; one covered file
	// per component.
	if cases != 6 || files != 3 {
		t.Errorf("the reports name %d cases and %d files", cases, files)
	}
}

func TestAPutnamiJSONOverrideChangesTheStepsAndTheInputs(t *testing.T) {
	spectest.Proves(t, feature, "overrides-are-honored", "a-putnami-json-override-changes-the-steps-and-the-inputs")
	root := fixture(t)
	s := stack(t)
	resolve := func(project, selector string) wsproto.InputResolveAnswer {
		result := s.ResolveInputs(root, wsproto.InputResolveRequest{Version: wsproto.InputResolveProtocolVersion, Extension: s.Extension,
			Inputs: []wsproto.InputResolveQuery{{Project: project, Selector: selector}}})
		if diags := wsproto.ValidateInputResolveResult(&result); len(diags) != 0 {
			t.Fatalf("the answer breaks the protocol: %v", diags)
		}
		return result.Inputs[0]
	}
	stackstest.WriteFile(t, root, "Packages/Pricing/.build/debug/Generated.swift", "")
	if got := resolve("Packages/Pricing", "swift:lint"); !reflect.DeepEqual(got.Files, []string{"Package.swift", "Sources/Pricing/PriceCalculator.swift"}) {
		t.Errorf("lint reads no test: %+v", got)
	}
	if got := resolve("Packages/Pricing", "swift:test"); !slices.Contains(got.Files, "Tests/PricingTests/PriceCalculatorTests.swift") {
		t.Errorf("test reads the tests: %+v", got)
	}
	stackstest.WriteFile(t, root, "App/Shop/Model.xcdatamodeld/Model.xcdatamodel/contents", "")
	stackstest.WriteFile(t, root, "App/Shop.xcodeproj/xcuserdata/me.xcuserdatad/UserInterfaceState.xcuserstate", "")
	stackstest.WriteFile(t, root, "App/DerivedData/Build/x.o", "")
	if got := resolve("App", "swift:test"); !reflect.DeepEqual(got.Files, []string{
		"Shop.xcodeproj/project.pbxproj", "Shop.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved",
		"Shop/Checkout.swift", "Shop/Model.xcdatamodeld/Model.xcdatamodel/contents", "Shop/ShopApp.swift", "ShopTests/CheckoutTests.swift",
	}) {
		t.Errorf("the Xcode project reads every file of its directory but the build outputs: %+v", got)
	}
	if got := resolve("App", "swift:lint"); !reflect.DeepEqual(got.Files, []string{"Shop/Checkout.swift", "Shop/ShopApp.swift", "ShopTests/CheckoutTests.swift"}) {
		t.Errorf("the Xcode lint reads its Swift files: %+v", got)
	}

	stackstest.WriteFile(t, root, "App/Fixtures/cart.json", "{}")
	stackstest.WriteFile(t, root, "App/putnami.json", `{"options": {"@putnami/swift": {
	  "inputs": {"sources": ["Shop/**", "Fixtures/**"]},
	  "tasks": {"build": {"steps": [["{runtime}", "xcodebuild", "build", "--scheme", "ShopDev", "--destination", "platform=macOS"]]}}
	}}}`)
	if got := resolve("App", "swift:build"); !reflect.DeepEqual(got.Files, []string{"Fixtures/cart.json", "Shop.xcodeproj/project.pbxproj",
		"Shop.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved", "Shop/Checkout.swift", "Shop/Model.xcdatamodeld/Model.xcdatamodel/contents", "Shop/ShopApp.swift", "putnami.json"}) {
		t.Errorf("a sources override keeps the project file and its pins: %v", got.Files)
	}
	tools := stubTools(t)
	tools.demoReports(t, root, false)
	if status, _, _, err := runJob(t, root, "App", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("build: %s %v", status, err)
	}
	if ran := tools.ran(t); !strings.Contains(ran, "xcodebuild build -scheme ShopDev -disableAutomaticPackageResolution -destination platform=macOS CODE_SIGNING_ALLOWED=NO") || strings.Contains(ran, "-showBuildSettings") {
		t.Errorf("a step override is honored: %s", ran)
	}

	stackstest.WriteFile(t, root, "App/putnami.json", `{"options": {"@putnami/swift": {"tasks": {"lint": {"stepz": []}}}}}`)
	if status, _, _, err := runJob(t, root, "App", stacks.TaskLint); status != "FAILED" || err == nil {
		t.Errorf("an invalid override fails the task: %s %v", status, err)
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	stackstest.CheckManifest(t, "putnami.extension.json", stack(t))
	stackstest.CheckSurface(t, "putnami.extension.json")
	if got := stack(t).Selectors(); !slices.Equal(got, []string{"swift:build", "swift:lint", "swift:test"}) {
		t.Errorf("selectors = %v", got)
	}
}
