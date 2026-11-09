package swift

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The jobs run the commands in a child process; these run them in this one.

func TestSwiftPMTestExportsTheLCOVOfTheTestBinary(t *testing.T) {
	root := fixture(t)
	s := stubTools(t)
	s.demoReports(t, root, false)
	pkg := filepath.Join(root, "Packages/Networking")
	t.Chdir(pkg)
	cmd := Commands()[CommandSwiftPMTest]
	if err := cmd(nil, io.Discard, io.Discard); err == nil {
		t.Error("--output is required")
	}
	if err := exportLCOV(io.Discard, nil, "lcov.info"); err == nil || !strings.Contains(err.Error(), "no coverage profile") {
		t.Errorf("no profile before swift test: %v", err)
	}
	writeFile(t, pkg, ".build/debug/codecov/default.profdata", "")
	if err := exportLCOV(io.Discard, nil, "lcov.info"); err == nil || !strings.Contains(err.Error(), "PackageTests.xctest") {
		t.Errorf("no test binary: %v", err)
	}
	// A macOS test product is a bundle; swift test leaves it, and the stub
	// writes the Linux file beside it.
	writeFile(t, pkg, ".build/debug/NetworkingPackageTests.xctest/Contents/MacOS/NetworkingPackageTests", "")
	if err := exportLCOV(io.Discard, nil, filepath.Join(pkg, "lcov.info")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.ran(t), "NetworkingPackageTests.xctest/Contents/MacOS/NetworkingPackageTests") {
		t.Errorf("llvm-cov reads the bundle's executable: %s", s.ran(t))
	}
	_ = os.RemoveAll(filepath.Join(pkg, ".build"))
	if err := cmd([]string{"--output", "out", "--", "-c", "release", "--filter", "ClientTests"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(pkg, "out/lcov.info")); !strings.Contains(string(data), "Client.swift") {
		t.Errorf("lcov = %s", data)
	}
	ran := s.ran(t)
	if !strings.Contains(ran, "--xunit-output "+filepath.Join("out", "xunit.xml")+" -c release --filter ClientTests") || !strings.Contains(ran, "swift build --show-bin-path -c release\n") {
		t.Errorf("the extra arguments reach swift test, and the configuration the bin path: %s", ran)
	}
}

func TestOwnRecordsKeepsThePackageFiles(t *testing.T) {
	lcov := "TN:\nSF:/r/Pkg/Sources/A.swift\nDA:1,1\nend_of_record\nSF:/r/Other/Sources/B.swift\nDA:1,0\nend_of_record\n"
	if got := string(ownRecords([]byte(lcov), "/r/Pkg")); got != "SF:/r/Pkg/Sources/A.swift\nDA:1,1\nend_of_record\n" {
		t.Errorf("ownRecords = %q", got)
	}
	if under("/r/Pkg2/a.swift", "/r/Pkg") || !under("/r/Pkg/a.swift", "/r/Pkg") {
		t.Error("under compares whole path segments")
	}
	if got := configFlags([]string{"-c", "release", "--filter", "X", "--scratch-path=/tmp/b", "--triple"}); strings.Join(got, " ") != "-c release --scratch-path=/tmp/b" {
		t.Errorf("configFlags = %v", got)
	}
}

func TestSwiftFormatLintNeedsAConfiguration(t *testing.T) {
	root := fixture(t)
	s := stubTools(t)
	t.Chdir(filepath.Join(root, "Packages/Networking"))
	cmd := Commands()[CommandSwiftFormatLint]
	writeFile(t, root, "Packages/Networking/.swift-format", "{}")
	if err := cmd([]string{"Missing"}, io.Discard, io.Discard); err != nil || strings.Contains(s.ran(t), "swift format") {
		t.Errorf("no path to lint lints nothing: %v %s", err, s.ran(t))
	}
	if err := cmd([]string{"Sources"}, io.Discard, io.Discard); err != nil || !strings.Contains(s.ran(t), "--configuration "+filepath.Join(root, "Packages/Networking/.swift-format")+" --recursive Sources") {
		t.Errorf("the component's configuration wins: %v %s", err, s.ran(t))
	}
	if got := workspaceRoot(t.TempDir()); got != "" {
		t.Errorf("a directory under no workspace has no root: %q", got)
	}
}

func TestXcodebuildPicksADestinationFromTheSchemePlatform(t *testing.T) {
	root := fixture(t)
	s := stubTools(t)
	s.demoReports(t, root, false)
	t.Chdir(filepath.Join(root, "App"))
	cmd := Commands()[CommandXcodebuild]
	for _, bad := range [][]string{nil, {"archive"}, {"build"}, {"test", "--scheme", "Shop"}, {"build", "--bogus"}} {
		if err := cmd(bad, io.Discard, io.Discard); err == nil {
			t.Errorf("%v is refused", bad)
		}
	}
	s.write(t, "settings.json", `note: Using new build system
[{"buildSettings":{"PLATFORM_NAME":"macosx"}}]`)
	if err := cmd([]string{"test", "--scheme", "Shop", "--project", "Shop.xcodeproj", "--result-bundle", "out/T.xcresult"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.ran(t), "xcodebuild test -project Shop.xcodeproj -scheme Shop -disableAutomaticPackageResolution -destination platform=macOS -enableCodeCoverage YES") {
		t.Errorf("a Mac scheme tests on macOS: %s", s.ran(t))
	}
	s.write(t, "settings.json", `[{"buildSettings":{"PLATFORM_NAME":"watchsimulator"}}]`)
	if err := cmd([]string{"test", "--scheme", "Shop", "--result-bundle", "out/T.xcresult"}, io.Discard, io.Discard); err != nil || !strings.Contains(s.ran(t), "platform=watchOS Simulator,id=WATCH") {
		t.Errorf("a watch scheme tests on a watch simulator: %v %s", err, s.ran(t))
	}
	s.write(t, "settings.json", `[{"buildSettings":{"PLATFORM_NAME":"driverkit"}}]`)
	if err := cmd([]string{"build", "--scheme", "Shop"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--destination") {
		t.Errorf("a platform with no default asks for a destination: %v", err)
	}
	s.write(t, "settings.json", `[{"buildSettings":{}}]`)
	if err := cmd([]string{"build", "--scheme", "Shop"}, io.Discard, io.Discard); err == nil {
		t.Error("settings with no platform are refused")
	}
	s.write(t, "settings.json", `[{"buildSettings":{"PLATFORM_NAME":"xrsimulator"}}]`)
	if err := cmd([]string{"test", "--scheme", "Shop", "--result-bundle", "out/T.xcresult"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "no available visionOS simulator") {
		t.Errorf("no simulator of the platform: %v", err)
	}
}

func TestXcresultWritesFailuresAndSkips(t *testing.T) {
	root := fixture(t)
	s := stubTools(t)
	s.demoReports(t, root, false)
	s.write(t, "tests.json", `{"testNodes":[{"nodeType":"Test Plan","children":[
  {"name":"Shop UITests","nodeType":"UI test bundle","children":[
    {"name":"LaunchTests","nodeType":"Test Suite","children":[
      {"name":"testLaunch()","nodeType":"Test Case","result":"Failed","children":[{"name":"Run","nodeType":"Device","children":[{"name":"button not found","nodeType":"Failure Message"}]}]},
      {"name":"testDark()","nodeType":"Test Case","result":"Skipped"},
      {"name":"testBare()","nodeType":"Test Case","result":"Failed"}]}]}]}]}`)
	s.write(t, "archive.json", `[{"line":3,"isExecutable":true,"executionCount":1}]`)
	t.Chdir(filepath.Join(root, "App"))
	cmd := Commands()[CommandXcresult]
	if err := cmd([]string{"--bundle", "missing.xcresult", "--junit", "j.xml"}, io.Discard, io.Discard); err == nil {
		t.Error("a missing bundle is refused")
	}
	if err := cmd([]string{"--bundle", "x"}, io.Discard, io.Discard); err == nil {
		t.Error("no report to write is refused")
	}
	writeFile(t, root, "App/T.xcresult/Info.plist", "")
	if err := cmd([]string{"--bundle", "T.xcresult", "--junit", "out/j.xml", "--cobertura", "out/c.xml"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	junit, _ := os.ReadFile("out/j.xml")
	for _, want := range []string{`classname="Shop_UITests.LaunchTests" name="testLaunch"`, `<failure message="button not found">`, `<failure message="failed">`, `<skipped>`, `failures="2"`, `skipped="1"`} {
		if !strings.Contains(string(junit), want) {
			t.Errorf("junit lacks %s:\n%s", want, junit)
		}
	}
	cobertura, _ := os.ReadFile("out/c.xml")
	if !strings.Contains(string(cobertura), `<line number="3" hits="1">`) || !strings.Contains(string(cobertura), `lines-valid="1"`) {
		t.Errorf("cobertura = %s", cobertura)
	}
	s.write(t, "tests.json", `not json`)
	if err := cmd([]string{"--bundle", "T.xcresult", "--junit", "out/j.xml"}, io.Discard, io.Discard); err == nil {
		t.Error("unreadable test results are an error")
	}
}

func TestCoberturaOfNoLineIsComplete(t *testing.T) {
	data, err := coberturaReport(map[string][]coveredLine{"/a/b.swift": {{Line: 1}}})
	if err != nil || !strings.Contains(string(data), `line-rate="1"`) {
		t.Errorf("%s %v", data, err)
	}
	if _, err := archiveLines([]byte(`{"other":[{"line":1}]}`), "/x.swift"); err != nil {
		t.Error(err)
	}
	if _, err := archiveLines([]byte(`nope`), "/x.swift"); err == nil {
		t.Error("an unreadable archive is an error")
	}
	if _, err := newestDevice([]byte(`nope`), "iOS"); err == nil {
		t.Error("an unreadable device list is an error")
	}
}
