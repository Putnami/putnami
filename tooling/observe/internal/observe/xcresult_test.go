package observe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/observe/internal/analyze"
	"go.putnami.dev/tooling/observe/internal/reports"
)

const swiftFeature = "tooling/observe-swift"

// fakeXcrun puts an xcrun first on the PATH that prints the outputs under
// testdata/xcrun/swift for the commands observe runs, records its arguments
// in the returned file, and fails the commands fail names.
func fakeXcrun(t *testing.T, fail string) string {
	t.Helper()
	fixtures, err := filepath.Abs(filepath.Join("testdata", "xcrun", "swift"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
echo "$*" >> '` + calls + `'
case "$1 $2 $3" in
` + fail + `
"xcresulttool get test-results") cat '` + fixtures + `/tests.json' ;;
"xccov view --report") cat '` + fixtures + `/report.json' ;;
"xccov view --archive") cat '` + fixtures + `/archive.json' ;;
*) echo "xcrun: error: unknown command $*" >&2; exit 72 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "xcrun"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// observeSwiftBundle observes the Swift demo with an .xcresult bundle in
// place of its converted JUnit and Cobertura reports.
func observeSwiftBundle(t *testing.T) Observation {
	t.Helper()
	root, base := demoRepositoryWithout(t, "swift", func(name string) bool { return strings.HasPrefix(name, "ci/build/reports/") })
	writeDemoFile(t, root, "build/Logs/Test/Run-Shop.xcresult/Info.plist", "<plist version=\"1.0\"><dict/></plist>\n")
	writeDemoFile(t, root, "build/Logs/Test/Run-Shop.xcresult/Data/data.0~abc", "binary\n")
	env := demos["swift"].env
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	return Run(context.Background(), opts)
}

func diagnosticsOf(o Observation, code string) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, d := range o.Diagnostics {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

func TestTheSwiftDemoReadsItsXCResultBundleThroughXcrun(t *testing.T) {
	spectest.Proves(t, swiftFeature, "xcresult-discovery", "an-xcresult-bundle-is-a-test-and-coverage-report")
	spectest.Proves(t, swiftFeature, "xcresult-read", "a-bundle-reads-through-xcrun")
	calls := fakeXcrun(t, "")
	o := observeSwiftBundle(t)
	noUnplacedReports(t, o)
	noWarnings(t, o)
	bundle := "build/Logs/Test/Run-Shop.xcresult"
	if want := []reports.Report{
		{Path: bundle, Kind: reports.KindJUnit, Format: reports.FormatXCResult, Source: reports.SourceDiscovered},
		{Path: bundle, Kind: reports.KindCoverage, Format: reports.FormatXCResult, Source: reports.SourceDiscovered},
	}; !reflect.DeepEqual(o.Reports, want) {
		t.Fatalf("reports %+v, want the bundle as both kinds and none of its files", o.Reports)
	}
	recorded, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, line := range strings.Split(strings.TrimSpace(string(recorded)), "\n") {
		command, path, _ := strings.Cut(line, " /")
		if !strings.HasSuffix(path, bundle) {
			t.Errorf("xcrun %s names %q, want the bundle's absolute path last", command, path)
		}
		commands = append(commands, command)
	}
	if want := []string{"xcresulttool get test-results tests --path", "xccov view --report --json", "xccov view --archive --json"}; !reflect.DeepEqual(commands, want) {
		t.Errorf("xcrun ran %q, want %q", commands, want)
	}

	// The bundle tells what the converted reports tell.
	converted := observeDemo(t, "swift")
	counts := func(list []evidence.ComponentTests) map[string][3]int {
		out := map[string][3]int{}
		for _, c := range list {
			out[c.Component] = [3]int{c.Passed, c.Failed, c.Skipped}
		}
		return out
	}
	if got, want := counts(o.Analysis.Tests), counts(converted.Analysis.Tests); !reflect.DeepEqual(got, want) || len(got) != 3 {
		t.Errorf("tests %v, want %v", got, want)
	}
	if !reflect.DeepEqual(o.Analysis.Coverage, converted.Analysis.Coverage) {
		t.Errorf("coverage %+v, want %+v", o.Analysis.Coverage, converted.Analysis.Coverage)
	}
	if !reflect.DeepEqual(o.Analysis.ChangedLineCoverage, converted.Analysis.ChangedLineCoverage) || o.Analysis.ChangedLineCoverage == nil {
		t.Errorf("changed-line coverage %+v, want %+v", o.Analysis.ChangedLineCoverage, converted.Analysis.ChangedLineCoverage)
	}
	validEvidence(t, o)
}

func TestAFailedXcrunCommandIsADiagnostic(t *testing.T) {
	spectest.Proves(t, swiftFeature, "xcresult-read", "a-failed-xcrun-command-is-a-diagnostic")
	fakeXcrun(t, `"xccov view --archive") echo "Error: Failed to load coverage archive" >&2; exit 1 ;;
"xcresulttool get test-results") echo '{"devices":[]}' ;;`)
	o := observeSwiftBundle(t)
	unread := diagnosticsOf(o, analyze.CodeXCResultUnread)
	if len(unread) != 2 {
		t.Fatalf("diagnostics %+v, want one %s per failed command", o.Diagnostics, analyze.CodeXCResultUnread)
	}
	for i, want := range []string{"xcrun xcresulttool get test-results tests --path", "xcrun xccov view --archive --json"} {
		d := unread[i]
		if d.Severity != diag.Warning || !strings.HasPrefix(d.Message, want+" build/Logs/Test/Run-Shop.xcresult") || !strings.Contains(d.Message, "xcresultparser") {
			t.Errorf("diagnostic %+v, want a warning that names %q and the conversion", d, want)
		}
	}
	if !strings.Contains(unread[0].Message, "no testNodes") || !strings.Contains(unread[1].Message, "Failed to load coverage archive") {
		t.Errorf("diagnostics %+v, want the command's error and the reader's", unread)
	}
	// The report still gives each component its totals; without the
	// archive's lines no changed line is measured.
	if len(o.Analysis.Coverage) != 3 || len(o.Analysis.Tests) != 0 {
		t.Errorf("coverage %+v tests %+v, want the report's totals and no test", o.Analysis.Coverage, o.Analysis.Tests)
	}
}

func TestAMissingXcrunNamesTheConversion(t *testing.T) {
	spectest.Proves(t, swiftFeature, "xcresult-tools-missing", "a-missing-xcrun-names-the-conversion")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	o := observeSwiftBundle(t)
	missing := diagnosticsOf(o, analyze.CodeXCResultToolsMissing)
	if len(missing) != 1 || missing[0].Severity != diag.Warning || missing[0].Field != "build/Logs/Test/Run-Shop.xcresult" ||
		!strings.Contains(missing[0].Message, "xcrun is not on this runner's PATH") || !strings.Contains(missing[0].Message, "--output-format junit") {
		t.Fatalf("diagnostics %+v, want one warning for the bundle that names the conversion", o.Diagnostics)
	}
	if len(o.Analysis.Tests) != 0 || len(o.Analysis.Coverage) != 0 || len(o.Analysis.Components) != 7 {
		t.Errorf("tests %+v coverage %+v, want the components and nothing from the bundle", o.Analysis.Tests, o.Analysis.Coverage)
	}
	validEvidence(t, o)
}

func TestXcrunIsBoundedInTimeAndOutput(t *testing.T) {
	spectest.Proves(t, swiftFeature, "xcresult-read", "each-xcrun-command-is-bounded")
	ctx := context.Background()
	if out, err := runBounded(ctx, time.Minute, 64, "sh", "-c", "printf 'xccov output'"); err != nil || string(out) != "xccov output" {
		t.Errorf("a bounded run: %q %v", out, err)
	}
	if _, err := runBounded(ctx, time.Minute, 64, "sh", "-c", "head -c 100000 /dev/zero"); !errors.Is(err, errOutputTooLarge) {
		t.Errorf("an output past its bound: %v, want %v", err, errOutputTooLarge)
	}
	if _, err := runBounded(ctx, 100*time.Millisecond, 64, "sh", "-c", "sleep 5"); err == nil || !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("a command past its time: %v", err)
	}
	// A child that keeps the output open does not hold the run past the
	// command's bound and the wait delay.
	start := time.Now()
	if _, err := runBounded(ctx, 100*time.Millisecond, 64, "sh", "-c", "sleep 4 & sleep 4"); err == nil {
		t.Error("a forking command past its time succeeded")
	}
	if elapsed := time.Since(start); elapsed > xcrunWaitDelay+5*time.Second {
		t.Errorf("a forking command past its time held the run %s", elapsed)
	}
	long := strings.Repeat("e", 2*xcrunStderrBytes)
	if _, err := runBounded(ctx, time.Minute, 64, "sh", "-c", "echo "+long+" >&2; exit 3"); err == nil || !strings.Contains(err.Error(), "exit status 3") || len(err.Error()) > xcrunStderrBytes+64 {
		t.Errorf("a failed command: %v, want its status and at most %d bytes of its error", err, xcrunStderrBytes)
	}
	t.Setenv("PATH", t.TempDir())
	if systemXCRun(ctx) != nil {
		t.Error("a PATH without xcrun gives an xcrun")
	}
}

func TestADiscoveredBundleBesideJUnitReportsGivesNoTests(t *testing.T) {
	spectest.Proves(t, swiftFeature, "xcresult-discovery", "a-discovered-bundle-beside-junit-reports-gives-no-tests")
	calls := fakeXcrun(t, "")
	root, base := demoRepository(t, "swift")
	writeDemoFile(t, root, "build/Test.xcresult/Info.plist", "<plist/>\n")
	env := demos["swift"].env
	opts := Options{
		Dir:      root,
		Env:      func(key string) string { return env[key] },
		ReadFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		Resolve:  func(p string) string { return filepath.Join(root, p) },
		Producer: Producer{Name: "@putnami/observe", Version: "1.0.0"},
	}
	opts.Overrides.Base = base
	o := Run(context.Background(), opts)
	converted := observeDemo(t, "swift")
	if !reflect.DeepEqual(o.Analysis.Tests, converted.Analysis.Tests) || !reflect.DeepEqual(o.Analysis.Coverage, converted.Analysis.Coverage) {
		t.Errorf("tests %+v coverage %+v, want those of the converted reports", o.Analysis.Tests, o.Analysis.Coverage)
	}
	skipped := diagnosticsOf(o, reports.CodeXCResultTestsSkipped)
	if len(skipped) != 1 || skipped[0].Severity != diag.Warning || !strings.Contains(skipped[0].Message, "--junit") {
		t.Errorf("diagnostics %+v, want one warning that names --junit", o.Diagnostics)
	}
	if recorded, _ := os.ReadFile(calls); strings.Contains(string(recorded), "xcresulttool") {
		t.Errorf("xcrun ran %s, want no test command", recorded)
	}
	if unmapped := diagnosticsOf(o, analyze.CodeCoverageUnmapped); len(unmapped) > 1 {
		t.Errorf("diagnostics %+v, want at most one per bundle", unmapped)
	}
}
