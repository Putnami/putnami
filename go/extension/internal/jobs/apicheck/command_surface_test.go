package apicheck

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/runtime"
	pctx "go.putnami.dev/sdk/extension/context"
)

const (
	// cliRequirement is the requirement the command-surface checks prove.
	cliRequirement = "cli-compatibility-marker"
	// surfaceFile is where the fixture CLI commits its command surface.
	surfaceFile = "command-surface.json"
	// surfaceTag is the line tag that holds the fixture's released surface.
	surfaceTag = "lib/v0.5.0"
	// surfaceLocation is the document's workspace-relative path.
	surfaceLocation = "lib/greet/" + surfaceFile
)

func loudFlag() protocolcli.CommandSurfaceFlag {
	return protocolcli.CommandSurfaceFlag{Long: "--loud", Short: "-l", Type: protocolcli.CommandFlagBool}
}

func styleFlag() protocolcli.CommandSurfaceFlag {
	return protocolcli.CommandSurfaceFlag{Long: "--style", Type: protocolcli.CommandFlagValue, Values: []string{"formal", "plain"}}
}

// surfaceDocument is the canonical document of a CLI whose one command,
// greet, takes a required name and the given flags.
func surfaceDocument(t *testing.T, flags ...protocolcli.CommandSurfaceFlag) string {
	t.Helper()
	surface, err := protocolcli.NewCommandSurface(
		[]protocolcli.CommandSurfaceFlag{{Long: "--verbose", Short: "-v", Type: protocolcli.CommandFlagBool}},
		[]protocolcli.CommandSurfaceCommand{{
			Path:        "greet",
			Flags:       flags,
			Positionals: []protocolcli.CommandSurfacePositional{{Name: "name", Required: true}},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	data, err := protocolcli.MarshalCommandSurface(surface)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// newSurfaceFixture is a stable project that declares its command surface,
// released at lib/v0.5.0 with greet --loud and --style.
func newSurfaceFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, catalog("package", "stable"))
	f.options = Options{CommandSurface: surfaceFile}
	f.write(surfaceLocation, surfaceDocument(t, loudFlag(), styleFlag()))
	f.commit("feat: the greet command")
	f.git("tag", surfaceTag)
	return f
}

// logged reports whether a log event carries message.
func logged(events []runtime.Event, message string) bool {
	for _, event := range events {
		if event.Type == runtime.EventLog && event.Message == message {
			return true
		}
	}
	return false
}

func TestARemovedFlagUnderAFeatCommitFailsValidate(t *testing.T) {
	spectest.Proves(t, feature, cliRequirement, "an-incompatible-cli-change-without-a-marker-fails-naming-command-and-flag")
	f := newSurfaceFixture(t)
	f.write(surfaceLocation, surfaceDocument(t, styleFlag()))
	f.commit("feat: a quieter greet")

	status, data, events := f.run()
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	errorsFound := diagnostics(events, runtime.SeverityError)
	if len(errorsFound) != 1 {
		t.Fatalf("error diagnostics = %+v, want one", errorsFound)
	}
	want := `command "greet": flag --loud removed since lib/v0.5.0; declare the breaking change with "!" or a BREAKING CHANGE: footer`
	if errorsFound[0].Message != want || errorsFound[0].Code != Code {
		t.Fatalf("diagnostic = %q (code %q)\nwant        %q (code %q)", errorsFound[0].Message, errorsFound[0].Code, want, Code)
	}
	if location := errorsFound[0].Location; location == nil || location.File != surfaceLocation || location.Line != 0 {
		t.Fatalf("location = %+v, want the document %s", location, surfaceLocation)
	}
	if data["incompatible"] != 1 || data["commandSurfaceIncompatible"] != 1 || data["commandSurface"] != surfaceLocation ||
		data["commandSurfaceCompared"] != true || data["tag"] != surfaceTag {
		t.Fatalf("data = %v", data)
	}
}

func TestAnAddedFlagUnderAFeatCommitPasses(t *testing.T) {
	spectest.Proves(t, feature, cliRequirement, "an-added-cli-flag-is-compatible")
	f := newSurfaceFixture(t)
	polite := protocolcli.CommandSurfaceFlag{Long: "--polite", Type: protocolcli.CommandFlagBool}
	f.write(surfaceLocation, surfaceDocument(t, loudFlag(), styleFlag(), polite))
	f.commit("feat: a polite greet")

	status, data, events := f.run()
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	if data["incompatible"] != 0 || data["commandSurfaceIncompatible"] != 0 || data["commandSurfaceCompared"] != true {
		t.Fatalf("data = %v", data)
	}
	if !logged(events, "the command surface command-surface.json is compatible with lib/v0.5.0") {
		t.Fatalf("events = %+v, want the surface reported compatible", events)
	}
}

func TestABreakingMarkerAllowsACommandSurfaceChange(t *testing.T) {
	spectest.Proves(t, feature, cliRequirement, "a-breaking-marker-allows-a-cli-change")
	f := newSurfaceFixture(t)
	f.write(surfaceLocation, surfaceDocument(t, styleFlag()))
	f.commit("feat!: a quieter greet")

	status, data, events := f.run()
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	infos := diagnostics(events, runtime.SeverityInfo)
	if len(infos) != 1 || infos[0].Code != Code ||
		!strings.HasPrefix(infos[0].Message, `command "greet": flag --loud removed since lib/v0.5.0, declared breaking by `) ||
		!strings.HasSuffix(infos[0].Message, `"feat!: a quieter greet"`) {
		t.Fatalf("info diagnostics = %+v", infos)
	}
	if data["breakingCommit"] == "" || data["breakingCommit"] == nil {
		t.Fatalf("data = %v, want the breaking commit", data)
	}
}

func TestAFlagThatStartsTakingAValueFailsValidate(t *testing.T) {
	spectest.Proves(t, feature, cliRequirement, "a-cli-flag-type-change-is-incompatible")
	f := newSurfaceFixture(t)
	loud := loudFlag()
	loud.Type = protocolcli.CommandFlagValue
	f.write(surfaceLocation, surfaceDocument(t, loud, styleFlag()))
	f.commit("feat: a loudness level")

	status, _, events := f.run()
	errorsFound := diagnostics(events, runtime.SeverityError)
	if status != "FAILED" || len(errorsFound) != 1 ||
		errorsFound[0].Message != `command "greet": flag --loud now takes a value since lib/v0.5.0; `+markerHint {
		t.Fatalf("status = %q, error diagnostics = %+v", status, errorsFound)
	}
}

// The released document is the baseline of the surface alone: a tag that
// does not hold it still has an API to compare.
func TestATagWithoutTheCommandSurfaceGivesANote(t *testing.T) {
	spectest.Proves(t, feature, cliRequirement, "a-tag-without-the-cli-document-gives-a-note")
	f := newFixture(t, catalog("package", "stable"))
	f.options = Options{CommandSurface: surfaceFile}
	f.write(surfaceLocation, surfaceDocument(t, loudFlag()))
	f.commit("feat: the greet command")

	status, data, events := f.run()
	if status != "OK" || data["compared"] != true || data["commandSurfaceCompared"] != false || data["tag"] != firstTag {
		t.Fatalf("status = %q, data = %v", status, data)
	}
	note := "the command surface command-surface.json does not exist at lib/v0.4.0: there is no released command surface to compare with"
	if !logged(events, note) {
		t.Fatalf("events = %+v, want the note %q", events, note)
	}

	f.write("lib/greet/greet.go", waveOnlySource)
	report := f.check()
	if report.SurfaceNote != note || len(report.CommandChanges) != 0 || len(report.Changes) != 1 {
		t.Fatalf("report = %+v, want the note and the removed function", report)
	}
}

func TestAMalformedCommandSurfaceFailsTheCheck(t *testing.T) {
	t.Parallel()
	inTree := newSurfaceFixture(t)
	inTree.write(surfaceLocation, `{"protocolVersion":1}`+"\n")
	_, err := Check(inTree.root, inTree.project, projectName, inTree.options)
	if err == nil || !strings.Contains(err.Error(), "the command surface command-surface.json is invalid") ||
		!errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("err = %v, want the invalid document named as invalid configuration", err)
	}

	atTag := newFixture(t, catalog("package", "stable"))
	atTag.options = Options{CommandSurface: surfaceFile}
	atTag.write(surfaceLocation, `{"protocolVersion":2}`+"\n")
	atTag.commit("feat: a document")
	atTag.git("tag", surfaceTag)
	atTag.write(surfaceLocation, surfaceDocument(t, loudFlag()))
	_, err = Check(atTag.root, atTag.project, projectName, atTag.options)
	if err == nil || !strings.Contains(err.Error(), "the command surface command-surface.json at lib/v0.5.0 is invalid") {
		t.Fatalf("err = %v, want the released document named invalid", err)
	}
}

func TestACommandSurfaceAtTheTagMustBeARegularFileWithinTheBound(t *testing.T) {
	t.Parallel()
	link := newFixture(t, catalog("package", "stable"))
	link.options = Options{CommandSurface: surfaceFile}
	link.write("lib/greet/real.json", surfaceDocument(t, loudFlag()))
	if err := os.Symlink("real.json", filepath.Join(link.project, surfaceFile)); err != nil {
		t.Fatal(err)
	}
	link.commit("feat: a linked document")
	link.git("tag", surfaceTag)
	if err := os.Remove(filepath.Join(link.project, surfaceFile)); err != nil {
		t.Fatal(err)
	}
	link.write(surfaceLocation, surfaceDocument(t, loudFlag()))
	if _, err := Check(link.root, link.project, projectName, link.options); err == nil ||
		!strings.Contains(err.Error(), "command-surface.json at lib/v0.5.0 is not a regular file") {
		t.Fatalf("err = %v, want the linked document refused", err)
	}

	large := newFixture(t, catalog("package", "stable"))
	large.options = Options{CommandSurface: surfaceFile}
	large.write(surfaceLocation, strings.Repeat(" ", protocolcli.CommandSurfaceMaxBytes+1))
	large.commit("feat: a large document")
	large.git("tag", surfaceTag)
	large.write(surfaceLocation, surfaceDocument(t, loudFlag()))
	if _, err := Check(large.root, large.project, projectName, large.options); err == nil ||
		!strings.Contains(err.Error(), "over the limit") {
		t.Fatalf("err = %v, want the oversized document refused unread", err)
	}
}

// A declared document is a file of the project: a typo, a directory or a
// symbolic link fails the check rather than turning the comparison off.
func TestADeclaredCommandSurfaceMustBeAFileOfTheProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	if err := os.MkdirAll(filepath.Join(f.project, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("lib/greet/real/surface.json", surfaceDocument(t, loudFlag()))
	if err := os.Symlink("real/surface.json", filepath.Join(f.project, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(f.project, "through")); err != nil {
		t.Fatal(err)
	}
	for option, want := range map[string]string{
		"missing.json":         "the command surface missing.json that option command-surface names does not exist in the project",
		"folder":               "the command surface folder is not a regular file",
		"greet.go/x.json":      "the command surface greet.go/x.json is not a regular file",
		"linked.json":          "the command surface linked.json goes through the symbolic link linked.json",
		"through/surface.json": "the command surface through/surface.json goes through the symbolic link through",
		"/etc/surface.json":    `option command-surface is "/etc/surface.json", want the path of a file of the project`,
		"../surface.json":      `option command-surface is "../surface.json", want the path of a file of the project`,
		".":                    `option command-surface is ".", want the path of a file of the project`,
	} {
		_, err := Check(f.root, f.project, projectName, Options{CommandSurface: option})
		if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, protocolcli.ErrInvalidConfig) {
			t.Errorf("option %q: err = %v, want %q as invalid configuration", option, err, want)
		}
	}
	if report, err := Check(f.root, f.project, projectName, Options{CommandSurface: "./real/../real/surface.json"}); err != nil || report.CommandSurface != "real/surface.json" {
		t.Errorf("a path with dot segments: report = %+v, err = %v, want it cleaned", report, err)
	}
}

// The skip rules apply first: a project the check skips is not read, while
// a malformed option is refused for every project.
func TestASkippedProjectDoesNotReadItsCommandSurface(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "experimental"))
	report, err := Check(f.root, f.project, projectName, Options{CommandSurface: "missing.json"})
	if err != nil || report.Skip == "" {
		t.Fatalf("report = %+v, err = %v, want the project skipped", report, err)
	}
	if _, err := Check(f.root, f.project, projectName, Options{CommandSurface: "../x.json"}); !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("err = %v, want the option refused", err)
	}
}

func TestTheCommandSurfaceOptionComesFromTheTaskParameters(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		params  pctx.Params
		want    string
		invalid bool
	}{
		{params: nil},
		{params: pctx.Params{"command-surface": json.RawMessage(`"cli.json"`)}, want: "cli.json"},
		{params: pctx.Params{"commandSurface": json.RawMessage(`"cli.json"`)}, want: "cli.json"},
		{params: pctx.Params{"command-surface": json.RawMessage(`"kebab.json"`), "commandSurface": json.RawMessage(`"camel.json"`)}, want: "kebab.json"},
		{params: pctx.Params{"command-surface": json.RawMessage(`null`)}},
		{params: pctx.Params{"command-surface": json.RawMessage(`true`)}, invalid: true},
		{params: pctx.Params{"command-surface": json.RawMessage(`["cli.json"]`)}, invalid: true},
	} {
		options, err := optionsFromParams(testCase.params)
		if testCase.invalid {
			if !errors.Is(err, protocolcli.ErrInvalidConfig) {
				t.Errorf("%v: err = %v, want invalid configuration", testCase.params, err)
			}
			continue
		}
		if err != nil || options.CommandSurface != testCase.want {
			t.Errorf("%v: options = %+v, err = %v, want %q", testCase.params, options, err, testCase.want)
		}
	}
}
