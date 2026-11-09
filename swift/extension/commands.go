package swift

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.putnami.dev/tooling/stacks"
)

// Runtime command names: a profile step runs them as {runtime} <name>.
const (
	CommandSwiftPMTest     = "swiftpm-test"
	CommandSwiftFormatLint = "swift-format-lint"
	CommandXcodebuild      = "xcodebuild"
	CommandXcresult        = "xcresult"
)

// Commands are the runtime commands of @putnami/swift.
func Commands() map[string]stacks.Command {
	return map[string]stacks.Command{
		CommandSwiftPMTest:     swiftPMTest,
		CommandSwiftFormatLint: swiftFormatLint,
		CommandXcodebuild:      xcodebuild,
		CommandXcresult:        xcresult,
	}
}

// run runs a program in the current directory and returns its standard
// output; its standard error goes to stderr.
func run(stderr io.Writer, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...) //nolint:gosec // the stack's own tools
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out.Bytes(), nil
}

// xcrunTool returns the argument vector that runs a developer tool: the
// tool on PATH, else through xcrun, which finds it in the selected Xcode.
func xcrunTool(tool string) ([]string, error) {
	if _, err := exec.LookPath(tool); err == nil {
		return []string{tool}, nil
	}
	if _, err := exec.LookPath("xcrun"); err == nil {
		return []string{"xcrun", tool}, nil
	}
	return nil, fmt.Errorf("%s is not on PATH, and there is no xcrun to find it", tool)
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// swiftPMTest runs the tests of the package in the current directory with
// code coverage and an xunit report in --output, with the versions
// Package.resolved pins, then exports the coverage of the package's own
// sources to --output/lcov.info. Arguments after "--" go to swift test, and
// its configuration flags to the bin-path lookup too. A swift test that fails
// with a failed case in its reports is the reports' verdict, so the command
// passes and the task reads them; one that fails without is an error.
//
//	{runtime} swiftpm-test --output <dir> [-- <swift test arguments>]
func swiftPMTest(args []string, stdout, stderr io.Writer) error {
	fs := newFlags(CommandSwiftPMTest, stderr)
	output := fs.String("output", "", "the directory of the reports")
	filter := fs.String("filter", "", "the tests to run, as swift test --filter reads it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return errors.New("--output names no directory")
	}
	extra := fs.Args()
	if *filter != "" {
		extra = append(extra, "--filter", *filter)
	} else if i := slices.Index(extra, "--filter"); i >= 0 && i+1 < len(extra) {
		// A filter after "--" is a filter too.
		*filter = extra[i+1]
	}
	if err := os.MkdirAll(*output, 0o755); err != nil {
		return err
	}
	// A report an earlier run left is no report of this one.
	stale, _ := filepath.Glob(filepath.Join(*output, "xunit*.xml"))
	for _, f := range append(stale, filepath.Join(*output, "lcov.info")) {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	argv := slices.Concat([]string{"test", "--force-resolved-versions", "--enable-code-coverage", "--parallel",
		"--xunit-output", filepath.Join(*output, "xunit.xml")}, extra)
	cmd := exec.Command("swift", argv...) //nolint:gosec // the stack's own tool
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if !reportsFailure(filepath.Join(*output, "xunit*.xml")) {
			return fmt.Errorf("swift %s: %w", strings.Join(argv, " "), err)
		}
		// SwiftPM writes no coverage profile after a failed run: the failed
		// cases are the verdict, and the task reads them.
		fmt.Fprintln(stderr, "swift test failed a case; the task reads the reports")
		if err := exportLCOV(stderr, configFlags(extra), filepath.Join(*output, "lcov.info")); err != nil {
			fmt.Fprintf(stderr, "no coverage after the failed run: %v\n", err)
		}
		return nil
	}
	err := exportLCOV(stderr, configFlags(extra), filepath.Join(*output, "lcov.info"))
	if *filter == "" || ranTests(*output) {
		return err
	}
	// A filter also reaches the components the selected ones depend on,
	// where it may select no test: such a run passes with empty reports.
	if err != nil {
		fmt.Fprintf(stderr, "no coverage for the filter %q, which selected no test: %v\n", *filter, err)
	}
	return emptyReports(*output)
}

// ranTests reports whether an xunit report in output holds a test case.
func ranTests(output string) bool {
	matches, _ := filepath.Glob(filepath.Join(output, "xunit*.xml"))
	for _, m := range matches {
		data, err := os.ReadFile(m) //nolint:gosec // the task's own report
		if err == nil && bytes.Contains(data, []byte("<testcase")) {
			return true
		}
	}
	return false
}

// emptyReports writes the xunit and LCOV reports a filtered run that ran no
// test leaves missing, so the task reads zero tests instead of no report.
func emptyReports(output string) error {
	if matches, _ := filepath.Glob(filepath.Join(output, "xunit*.xml")); len(matches) == 0 {
		if err := os.WriteFile(filepath.Join(output, "xunit.xml"), []byte(xml.Header+"<testsuites></testsuites>\n"), 0o644); err != nil { //nolint:gosec // the task's own report
			return err
		}
	}
	lcov := filepath.Join(output, "lcov.info")
	if _, err := os.Stat(lcov); errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(lcov, nil, 0o644) //nolint:gosec // the task's own report
	}
	return nil
}

// reportsFailure reports whether a JUnit report the glob matches holds a
// failed or errored case.
func reportsFailure(glob string) bool {
	matches, _ := filepath.Glob(glob)
	for _, m := range matches {
		data, err := os.ReadFile(m) //nolint:gosec // the task's own report
		if err == nil && (bytes.Contains(data, []byte("<failure")) || bytes.Contains(data, []byte("<error"))) {
			return true
		}
	}
	return false
}

// configFlags keeps the swift test arguments that change where the build
// lands: the configuration, the scratch path and the triple.
func configFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-c" || a == "--configuration" || a == "--scratch-path" || a == "--build-path" || a == "--triple":
			if i+1 < len(args) {
				out = append(out, a, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--configuration=") || strings.HasPrefix(a, "--scratch-path=") || strings.HasPrefix(a, "--build-path=") || strings.HasPrefix(a, "--triple="):
			out = append(out, a)
		}
	}
	return out
}

// exportLCOV writes the LCOV of the last swift test --enable-code-coverage:
// llvm-cov export of the package's test binary with the merged profile, of
// the files under the package outside .build and Tests.
func exportLCOV(stderr io.Writer, flags []string, output string) error {
	binOut, err := run(stderr, "swift", slices.Concat([]string{"build", "--show-bin-path"}, flags)...)
	if err != nil {
		return err
	}
	binPath := strings.TrimSpace(lastLine(string(binOut)))
	profdata := filepath.Join(binPath, "codecov", "default.profdata")
	if _, err := os.Stat(profdata); err != nil {
		return fmt.Errorf("no coverage profile at %s: run swift test --enable-code-coverage first", profdata)
	}
	binaries, err := testBinaries(binPath)
	if err != nil {
		return err
	}
	llvmCov, err := xcrunTool("llvm-cov")
	if err != nil {
		return err
	}
	argv := slices.Concat(llvmCov[1:], []string{"export", "-format=lcov", "-instr-profile", profdata,
		"-ignore-filename-regex", `(\.build|/Tests)/`, binaries[0]})
	for _, other := range binaries[1:] {
		argv = append(argv, "-object", other)
	}
	lcov, err := run(stderr, llvmCov[0], argv...)
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	return os.WriteFile(output, ownRecords(lcov, dir), 0o644) //nolint:gosec // the task's own report
}

// ownRecords keeps the LCOV records of the files under dir: a local package
// the tests link is another component, with its own coverage.
func ownRecords(lcov []byte, dir string) []byte {
	var out, record bytes.Buffer
	keep := false
	for _, line := range strings.SplitAfter(string(lcov), "\n") {
		if sf, ok := strings.CutPrefix(strings.TrimSpace(line), "SF:"); ok {
			record.Reset()
			keep = under(sf, dir)
		}
		record.WriteString(line)
		if strings.TrimSpace(line) == "end_of_record" {
			if keep {
				out.Write(record.Bytes())
			}
			record.Reset()
			keep = false
		}
	}
	return out.Bytes()
}

// under reports whether the absolute file is under the directory dir, the
// two compared with their symbolic links resolved: a tool names a file by
// its real path.
func under(file, dir string) bool {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	if real, err := filepath.EvalSymlinks(file); err == nil {
		file = real
	}
	rel, err := filepath.Rel(dir, file)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// swiftFormatLint runs swift format lint --strict on the paths that exist,
// with the .swift-format of the current directory, else of the workspace
// root: the two the task's key reads. Without one, nothing is linted:
// swift-format's default style is no team's style.
//
//	{runtime} swift-format-lint <path>...
func swiftFormatLint(args []string, stdout, stderr io.Writer) error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	config := ""
	for _, d := range []string{dir, workspaceRoot(dir)} {
		if d == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(d, ".swift-format")); err == nil && !info.IsDir() {
			config = filepath.Join(d, ".swift-format")
			break
		}
	}
	if config == "" {
		fmt.Fprintln(stderr, "no .swift-format in the component or at the workspace root: nothing to lint")
		return nil
	}
	var paths []string
	for _, p := range args {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if p != "." {
			paths = append(paths, p)
			continue
		}
		// "." is the directory's entries, without the hidden ones and the
		// build outputs and checkouts the task's key leaves out.
		entries, err := os.ReadDir(".")
		if err != nil {
			return err
		}
		// swift-format lints a file it is named whatever its extension: keep
		// the directories, which it walks for Swift files, and the Swift files.
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || slices.Contains(lintSkipped, name) {
				continue
			}
			if e.IsDir() || strings.HasSuffix(name, ".swift") {
				paths = append(paths, name)
			}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	argv := slices.Concat([]string{"format", "lint", "--strict", "--configuration", config, "--recursive"}, paths)
	cmd := exec.Command("swift", argv...) //nolint:gosec // the stack's own tool
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("swift %s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// lintSkipped are the directories a lint of "." leaves out: the profile's
// excludes.
var lintSkipped = []string{"DerivedData", "SourcePackages", "Pods", "Carthage", "xcuserdata", "node_modules"}

// workspaceRoot is the nearest directory at or above dir that holds
// putnami.workspace.json, or "".
func workspaceRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "putnami.workspace.json")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// testBinaries finds the test products of a package in its bin path. The
// native build system writes one, <Package>PackageTests.xctest, a file on
// Linux and a bundle whose executable is Contents/MacOS/<name> on macOS. The
// Swift Build backend of Swift 6.4 writes one per test target: the bundle
// <Target>.xctest on macOS, and on Linux the library <Target>.so that
// <Target>-test-runner loads.
func testBinaries(binPath string) ([]string, error) {
	matches, _ := filepath.Glob(filepath.Join(binPath, "*PackageTests.xctest"))
	if len(matches) == 0 {
		matches, _ = filepath.Glob(filepath.Join(binPath, "*.xctest"))
	}
	if len(matches) == 0 {
		runners, _ := filepath.Glob(filepath.Join(binPath, "*-test-runner"))
		for _, runner := range runners {
			library := strings.TrimSuffix(runner, "-test-runner") + ".so"
			if _, err := os.Stat(library); err == nil {
				matches = append(matches, library)
			}
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no *PackageTests.xctest, <Target>.xctest or <Target>.so beside <Target>-test-runner in %s: the package has no test target, or swift test did not build it", binPath)
	}
	sort.Strings(matches)
	out := make([]string, 0, len(matches))
	for _, product := range matches {
		info, err := os.Stat(product)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, product)
			continue
		}
		inner := filepath.Join(product, "Contents", "MacOS", strings.TrimSuffix(filepath.Base(product), ".xctest"))
		if _, err := os.Stat(inner); err != nil {
			return nil, fmt.Errorf("the test bundle %s holds no executable: %w", product, err)
		}
		out = append(out, inner)
	}
	return out, nil
}

// xcodebuild runs xcodebuild test or build on a scheme of the project in the
// current directory, with a destination it picks from the scheme's platform
// unless --destination names one. A test writes its result bundle, with code
// coverage, to --result-bundle.
//
//	{runtime} xcodebuild test|build --scheme <scheme> [--result-bundle <path>] [--destination <spec>] [--project <name.xcodeproj>]
func xcodebuild(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || (args[0] != "test" && args[0] != "build") {
		return errors.New("the first argument is test or build")
	}
	action := args[0]
	fs := newFlags(CommandXcodebuild, stderr)
	scheme := fs.String("scheme", "", "the scheme")
	bundle := fs.String("result-bundle", "", "the result bundle a test writes")
	filter := fs.String("filter", "", "the tests to run, as xcodebuild -only-testing reads it")
	junit := fs.String("junit", "", "the JUnit report a test converts its result bundle into")
	cobertura := fs.String("cobertura", "", "the Cobertura report a test converts its result bundle into")
	destination := fs.String("destination", "", "the xcodebuild destination")
	project := fs.String("project", "", "the .xcodeproj, when the directory holds several")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *scheme == "" {
		return errors.New("--scheme names no scheme")
	}
	base := []string{"-scheme", *scheme, "-disableAutomaticPackageResolution"}
	if *project != "" {
		base = append([]string{"-project", *project}, base...)
	}
	if *destination == "" {
		platform, err := schemePlatform(stderr, base)
		if err != nil {
			return err
		}
		*destination, err = pickDestination(stderr, platform, action == "test")
		if err != nil {
			return err
		}
	}
	if action == "test" {
		// A marker of an earlier run is no verdict of this one.
		if err := os.Remove(failedMarker(*bundle)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	argv := append([]string{action}, base...)
	argv = append(argv, "-destination", *destination)
	if action == "test" {
		if *bundle == "" {
			return errors.New("a test names its --result-bundle")
		}
		// xcodebuild refuses to overwrite a result bundle.
		if err := os.RemoveAll(*bundle); err != nil {
			return err
		}
		argv = append(argv, "-enableCodeCoverage", "YES", "-resultBundlePath", *bundle)
		if *filter != "" {
			argv = append(argv, "-only-testing:"+*filter)
		}
	}
	argv = append(argv, "CODE_SIGNING_ALLOWED=NO")
	cmd := exec.Command("xcodebuild", argv...) //nolint:gosec // the stack's own tool
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		err = fmt.Errorf("xcodebuild %s: %w", strings.Join(argv, " "), err)
		if action != "test" {
			return err
		}
		if _, statErr := os.Stat(*bundle); statErr != nil {
			return err
		}
		// A failed test still writes its bundle: the xcresult conversion
		// reads the failed cases, and fails when there is none.
		if err := os.WriteFile(failedMarker(*bundle), []byte(err.Error()+"\n"), 0o644); err != nil { //nolint:gosec // beside the task's own bundle
			return err
		}
	}
	if action != "test" || (*junit == "" && *cobertura == "") {
		return nil
	}
	convert := []string{"--bundle", *bundle}
	if *junit != "" {
		convert = append(convert, "--junit", *junit)
	}
	if *cobertura != "" {
		convert = append(convert, "--cobertura", *cobertura)
	}
	return xcresult(convert, stdout, stderr)
}

// failedMarker is the file the xcodebuild command writes beside a result
// bundle when xcodebuild test exits non-zero.
func failedMarker(bundle string) string {
	return strings.TrimRight(bundle, string(filepath.Separator)) + ".failed"
}

// schemePlatform reads the platform the scheme builds for: PLATFORM_NAME of
// its first target's build settings.
func schemePlatform(stderr io.Writer, base []string) (string, error) {
	out, err := run(stderr, "xcodebuild", append(append([]string{"-showBuildSettings"}, base...), "-json")...)
	if err != nil {
		return "", err
	}
	var targets []struct {
		BuildSettings map[string]string `json:"buildSettings"`
	}
	if err := json.Unmarshal(jsonStart(out), &targets); err != nil {
		return "", fmt.Errorf("read xcodebuild -showBuildSettings -json: %w", err)
	}
	for _, t := range targets {
		if p := t.BuildSettings["PLATFORM_NAME"]; p != "" {
			return p, nil
		}
	}
	return "", errors.New("the scheme's build settings name no PLATFORM_NAME: pass --destination")
}

// jsonStart drops what a tool prints before its JSON document.
func jsonStart(out []byte) []byte {
	if i := bytes.IndexAny(out, "[{"); i > 0 {
		return out[i:]
	}
	return out
}

// simulatorOS maps an SDK platform name to the simulator OS it runs on.
var simulatorOS = map[string]string{
	"iphoneos": "iOS", "iphonesimulator": "iOS",
	"appletvos": "tvOS", "appletvsimulator": "tvOS",
	"watchos": "watchOS", "watchsimulator": "watchOS",
	"xros": "visionOS", "xrsimulator": "visionOS",
}

// pickDestination is the destination of a platform: macOS for a Mac scheme;
// for a simulator platform, the generic simulator to build, and the first
// available device of the newest runtime to test.
func pickDestination(stderr io.Writer, platform string, test bool) (string, error) {
	if platform == "macosx" {
		return "platform=macOS", nil
	}
	osName, ok := simulatorOS[platform]
	if !ok {
		return "", fmt.Errorf("the scheme builds for %s, which has no default destination: pass --destination", platform)
	}
	if !test {
		return "generic/platform=" + osName + " Simulator", nil
	}
	out, err := run(stderr, "xcrun", "simctl", "list", "devices", "available", "-j")
	if err != nil {
		return "", err
	}
	udid, err := newestDevice(out, osName)
	if err != nil {
		return "", err
	}
	return "platform=" + osName + " Simulator,id=" + udid, nil
}

// newestDevice picks, from `xcrun simctl list devices available -j`, the
// first available device of the newest runtime of an OS.
func newestDevice(list []byte, osName string) (string, error) {
	var doc struct {
		Devices map[string][]struct {
			Name        string `json:"name"`
			UDID        string `json:"udid"`
			IsAvailable *bool  `json:"isAvailable"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(jsonStart(list), &doc); err != nil {
		return "", fmt.Errorf("read xcrun simctl list devices -j: %w", err)
	}
	prefix := "com.apple.CoreSimulator.SimRuntime." + strings.ReplaceAll(osName, "visionOS", "xrOS") + "-"
	var best []int
	udid := ""
	for runtime, devices := range doc.Devices {
		v, ok := strings.CutPrefix(runtime, prefix)
		if !ok {
			continue
		}
		version := versionParts(v)
		for _, d := range devices {
			if d.IsAvailable != nil && !*d.IsAvailable {
				continue
			}
			if udid == "" || newer(version, best) {
				best, udid = version, d.UDID
			}
			break
		}
	}
	if udid == "" {
		return "", fmt.Errorf("no available %s simulator: create one, or pass --destination", osName)
	}
	return udid, nil
}

func versionParts(v string) []int {
	var out []int
	for _, p := range strings.Split(v, "-") {
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func newer(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return len(a) > len(b)
}
