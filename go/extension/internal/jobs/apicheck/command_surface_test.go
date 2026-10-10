package apicheck

import (
	"encoding/json"
	"errors"
	"fmt"
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

// declareSurface makes the fixture project declare its command surface at
// rel, relative to the project: in its putnami.json, which the check reads at
// the tag, and in the task parameters, as the CLI delivers the option.
func declareSurface(f *fixture, rel string) {
	f.t.Helper()
	f.write("lib/greet/putnami.json", `{"name":"`+projectName+`","tags":["go"],`+
		`"options":{"@putnami/go:validate":{"command-surface":"`+rel+`"}}}`+"\n")
	f.options = Options{CommandSurface: rel}
}

// newSurfaceFixture is a stable project that declares its command surface,
// released at lib/v0.5.0 with greet --loud and --style.
func newSurfaceFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, catalog("package", "stable"))
	declareSurface(f, surfaceFile)
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

// metric returns the value of the metric event called name; ok is false when
// there is none.
func metric(events []runtime.Event, name string) (value float64, ok bool) {
	for _, event := range events {
		if event.Type == runtime.EventMetric && event.Name == name && event.Value != nil {
			return *event.Value, true
		}
	}
	return 0, false
}

// summary returns the message of the summary event; empty when there is none.
func summary(events []runtime.Event) string {
	for _, event := range events {
		if event.Type == runtime.EventSummary {
			return event.Message
		}
	}
	return ""
}

// notCompared returns the warning diagnostics that say a comparison did not
// run.
func notCompared(events []runtime.Event) []runtime.Event {
	var found []runtime.Event
	for _, event := range diagnostics(events, runtime.SeverityWarning) {
		if event.Code == NotComparedCode {
			found = append(found, event)
		}
	}
	return found
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
	// The counts name the kind of change: a command-surface change is not an
	// API change.
	if got := summary(events); got != "1 incompatible command-surface change since lib/v0.5.0 and no commit declares a breaking change" {
		t.Fatalf("summary = %q", got)
	}
	api, apiOK := metric(events, "api-incompatible")
	surface, surfaceOK := metric(events, "command-surface-incompatible")
	if !apiOK || api != 0 || !surfaceOK || surface != 1 {
		t.Fatalf("metrics api-incompatible = %v (%v), command-surface-incompatible = %v (%v), want 0 and 1", api, apiOK, surface, surfaceOK)
	}
}

// An API change and a command-surface change in one release are counted
// apart, in the summary and in the metrics.
// It runs serially, since f.run redirects os.Stdout.
func TestTheCountsTellAPIChangesFromCommandSurfaceChanges(t *testing.T) {
	f := newSurfaceFixture(t)
	f.write(surfaceLocation, surfaceDocument(t))
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat: less to greet with")

	status, data, events := f.run()
	if status != "FAILED" || data["incompatible"] != 3 || data["commandSurfaceIncompatible"] != 2 {
		t.Fatalf("status = %q, data = %v", status, data)
	}
	want := "1 incompatible API change and 2 incompatible command-surface changes since lib/v0.5.0 and no commit declares a breaking change"
	if got := summary(events); got != want {
		t.Fatalf("summary = %q\nwant      %q", got, want)
	}
	api, _ := metric(events, "api-incompatible")
	surface, _ := metric(events, "command-surface-incompatible")
	if api != 1 || surface != 2 {
		t.Fatalf("metrics api-incompatible = %v, command-surface-incompatible = %v, want 1 and 2", api, surface)
	}

	// A marker counts on a commit that touches the project.
	f.write("lib/greet/CHANGES.md", "Greet, --loud and --style are gone.\n")
	f.commit("feat!: less to greet with")
	status, _, events = f.run()
	declared := "1 incompatible API change and 2 incompatible command-surface changes since lib/v0.5.0; " +
		"a commit since lib/v0.5.0 declares the breaking change"
	found := false
	for _, event := range events {
		found = found || (event.Type == runtime.EventLog && event.Message == declared)
	}
	if status != "OK" || !found {
		t.Fatalf("status = %q, events = %+v, want the declared counts", status, events)
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
	want := `command "greet": flag --loud removed since lib/v0.5.0; a commit since lib/v0.5.0 declares the breaking change`
	if len(infos) != 1 || infos[0].Code != Code || infos[0].Message != want {
		t.Fatalf("info diagnostics = %+v\nwant one: %q", infos, want)
	}
	if data["breakingDeclared"] != true {
		t.Fatalf("data = %v, want breakingDeclared", data)
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

// The released document is the baseline of the surface alone: a tag at which
// the project declared none still has an API to compare.
func TestATagWithoutTheCommandSurfaceGivesANote(t *testing.T) {
	spectest.Proves(t, feature, cliRequirement, "a-tag-without-the-cli-document-gives-a-note")
	f := newFixture(t, catalog("package", "stable"))
	declareSurface(f, surfaceFile)
	f.write(surfaceLocation, surfaceDocument(t, loudFlag()))
	f.commit("feat: the greet command")

	status, data, events := f.run()
	if status != "OK" || data["compared"] != true || data["commandSurfaceCompared"] != false || data["tag"] != firstTag {
		t.Fatalf("status = %q, data = %v", status, data)
	}
	note := "the project declares no command surface at lib/v0.4.0: there is no released command surface to compare with"
	if !logged(events, note) || len(notCompared(events)) != 0 || data["commandSurfaceNotCompared"] != note {
		t.Fatalf("events = %+v, data = %v, want the note %q and no warning", events, data, note)
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

	// A released document of a version this check reads must be well formed.
	atTag := newFixture(t, catalog("package", "stable"))
	declareSurface(atTag, surfaceFile)
	atTag.write(surfaceLocation, `{"protocolVersion":1}`+"\n")
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
	declareSurface(link, surfaceFile)
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
	declareSurface(large, surfaceFile)
	large.write(surfaceLocation, strings.Repeat(" ", protocolcli.CommandSurfaceMaxBytes+1))
	large.commit("feat: a large document")
	large.git("tag", surfaceTag)
	large.write(surfaceLocation, surfaceDocument(t, loudFlag()))
	if _, err := Check(large.root, large.project, projectName, large.options); err == nil ||
		!strings.Contains(err.Error(), "over the limit") {
		t.Fatalf("err = %v, want the oversized document refused unread", err)
	}
}

// A declared document is a file of the project: a typo, a name in another
// case, a directory or a symbolic link fails the check rather than turning
// the comparison off. The case is checked on every file system, because git
// matches the name exactly at the tag.
func TestADeclaredCommandSurfaceMustBeAFileOfTheProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	if err := os.MkdirAll(filepath.Join(f.project, "folder.json"), 0o755); err != nil {
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
		"folder.json":          "the command surface folder.json is not a regular file",
		"greet.go/x.json":      "the command surface greet.go/x.json is not a regular file",
		"linked.json":          "the command surface linked.json goes through the symbolic link linked.json",
		"through/surface.json": "the command surface through/surface.json goes through the symbolic link through",
		"real/Surface.json":    "the command surface real/Surface.json that option command-surface names differs in case from real/surface.json on disk",
		"Real/surface.json":    "the command surface Real/surface.json that option command-surface names differs in case from real on disk",
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

// The task's cache key reads the project's .json files outside the directories
// a run writes and the key's walk skips, so the working-tree document must be
// one of them: a document the key does not read could change under a stored
// verdict. The released document has no such rule, because the key reads the
// whole tree the tag holds.
func TestADeclaredCommandSurfaceIsAFileTheCacheKeyReads(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	for option, want := range map[string]string{
		"surface.yaml":           `option command-surface is "surface.yaml", want a .json file`,
		"surface":                `option command-surface is "surface", want a .json file`,
		".gen/surface.json":      `option command-surface is ".gen/surface.json", which is inside .gen`,
		"docs/.cli/surface.json": `option command-surface is "docs/.cli/surface.json", which is inside .cli`,
		"dist/surface.json":      `option command-surface is "dist/surface.json", which is inside dist`,
		"out/surface.json":       `option command-surface is "out/surface.json", which is inside out`,
		"vendor/surface.json":    `option command-surface is "vendor/surface.json", which is inside vendor`,
		"node_modules/s.json":    `option command-surface is "node_modules/s.json", which is inside node_modules`,
	} {
		_, err := Check(f.root, f.project, projectName, Options{CommandSurface: option})
		if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, protocolcli.ErrInvalidConfig) {
			t.Errorf("option %q: err = %v, want %q as invalid configuration", option, err, want)
		}
	}
	f.write("lib/greet/.surface.json", surfaceDocument(t, loudFlag()))
	f.write("lib/greet/_docs/surface.json", surfaceDocument(t, loudFlag()))
	for _, option := range []string{".surface.json", "_docs/surface.json"} {
		if _, err := Check(f.root, f.project, projectName, Options{CommandSurface: option}); err != nil {
			t.Errorf("option %q: err = %v, want the keyed document read", option, err)
		}
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
		{params: pctx.Params{"command-surface": json.RawMessage(`"kebab.json"`), "commandSurface": json.RawMessage(`"camel.json"`)}, want: "camel.json"},
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

// A released document of a later protocol version than this check reads is
// not compared, and says so; the API is still compared.
// It runs serially, since f.run redirects os.Stdout.
func TestACommandSurfaceOfALaterVersionAtTheTagIsNotCompared(t *testing.T) {
	f := newFixture(t, catalog("package", "stable"))
	declareSurface(f, surfaceFile)
	later := fmt.Sprintf(`{"protocolVersion":%d,"commands":[{"path":"greet","aliases":["g"]}]}`, protocolcli.CommandSurfaceVersion+1)
	f.write(surfaceLocation, later+"\n")
	f.commit("feat: a later document")
	f.git("tag", surfaceTag)
	f.write(surfaceLocation, surfaceDocument(t, loudFlag()))
	f.commit("feat: the greet command")

	status, data, events := f.run()
	warnings := notCompared(events)
	if status != "OK" || len(warnings) != 1 || data["compared"] != true || data["commandSurfaceCompared"] != false {
		t.Fatalf("status = %q, warnings = %+v, data = %v", status, warnings, data)
	}
	if !strings.HasPrefix(warnings[0].Message, "the command surface command-surface.json at lib/v0.5.0 is not compared: ") ||
		!strings.HasSuffix(warnings[0].Message, "; update @putnami/go to read it") ||
		warnings[0].Location == nil || warnings[0].Location.File != surfaceLocation {
		t.Fatalf("warning = %+v", warnings[0])
	}

	f.write("lib/greet/greet.go", waveOnlySource)
	if report := f.check(); report.SurfaceWarning == "" || len(report.Changes) != 1 || len(report.CommandChanges) != 0 {
		t.Fatalf("report = %+v, want the warning and the removed function", report)
	}
}

// The released document is the one at the path the project declared at the
// tag, so moving it in the same change keeps the comparison.
// It runs serially, since f.run redirects os.Stdout.
func TestAMovedCommandSurfaceIsComparedWithTheReleasedOne(t *testing.T) {
	f := newSurfaceFixture(t)
	if err := os.Remove(filepath.Join(f.project, surfaceFile)); err != nil {
		t.Fatal(err)
	}
	declareSurface(f, "cli/surface.json")
	f.write("lib/greet/cli/surface.json", surfaceDocument(t, styleFlag()))
	f.commit("feat: a quieter greet, and a new home for its surface")

	status, data, events := f.run()
	errorsFound := diagnostics(events, runtime.SeverityError)
	want := `command "greet": flag --loud removed since lib/v0.5.0; ` + markerHint
	if status != "FAILED" || len(errorsFound) != 1 || errorsFound[0].Message != want ||
		errorsFound[0].Location == nil || errorsFound[0].Location.File != "lib/greet/cli/surface.json" {
		t.Fatalf("status = %q, error diagnostics = %+v", status, errorsFound)
	}
	if data["commandSurface"] != "lib/greet/cli/surface.json" || data["commandSurfaceCompared"] != true {
		t.Fatalf("data = %v", data)
	}

	f.write("lib/greet/cli/surface.json", surfaceDocument(t, loudFlag(), styleFlag()))
	f.commit("fix: keep --loud")
	status, _, events = f.run()
	if status != "OK" || !logged(events, "the command surface cli/surface.json is compatible with command-surface.json at lib/v0.5.0") {
		t.Fatalf("status = %q, events = %+v, want the moved surface compatible", status, events)
	}
}

// A document the tag declares but does not hold cannot be compared: a
// warning says so, rather than a note that reads as no release.
// It runs serially, since f.run redirects os.Stdout.
func TestACommandSurfaceTheTagDeclaresButDoesNotHoldWarns(t *testing.T) {
	f := newFixture(t, catalog("package", "stable"))
	declareSurface(f, surfaceFile)
	f.commit("feat: declare a command surface")
	f.git("tag", surfaceTag)
	f.write(surfaceLocation, surfaceDocument(t, loudFlag()))
	f.commit("feat: the greet command")

	status, data, events := f.run()
	warnings := notCompared(events)
	want := "the project declares the command surface command-surface.json at lib/v0.5.0, but the tag does not hold it, so the command surface is not compared"
	if status != "OK" || len(warnings) != 1 || warnings[0].Message != want || data["commandSurfaceCompared"] != false {
		t.Fatalf("status = %q, warnings = %+v, data = %v", status, warnings, data)
	}
}

// Removing the option in the change that breaks the command surface does not
// end the comparison without a trace: the check reads the declaration at the
// tag whatever the working tree declares, and warns that the released surface
// is not compared. The API is still compared, and a project that never
// declared a document gets no warning and no command-surface data.
// It runs serially, since f.run redirects os.Stdout.
func TestRemovingTheCommandSurfaceOptionWarns(t *testing.T) {
	f := newSurfaceFixture(t)
	f.write("lib/greet/putnami.json", `{"name":"`+projectName+`","tags":["go"]}`+"\n")
	f.options = Options{}
	f.write(surfaceLocation, surfaceDocument(t, styleFlag()))
	f.commit("feat: a quieter greet, with no command surface declared")

	status, data, events := f.run()
	warnings := notCompared(events)
	want := "the project declared the command surface command-surface.json at lib/v0.5.0 and declares none now, " +
		"so the command surface is not compared; declare it with option command-surface to compare it"
	if status != "OK" || len(warnings) != 1 || warnings[0].Message != want ||
		warnings[0].Location == nil || warnings[0].Location.File != "lib/greet/putnami.json" {
		t.Fatalf("status = %q, warnings = %+v, want one warning %q on the project's putnami.json", status, warnings, want)
	}
	if data["compared"] != true || data["tag"] != surfaceTag || data["commandSurface"] != surfaceLocation ||
		data["commandSurfaceCompared"] != false || data["commandSurfaceNotCompared"] != want || data["commandSurfaceIncompatible"] != 0 {
		t.Fatalf("data = %v, want the document declared at the tag, not compared, and why", data)
	}

	f.write("lib/greet/greet.go", waveOnlySource)
	report := f.check()
	if report.SurfaceWarning != want || report.ReleasedSurface != surfaceFile || len(report.Changes) != 1 || len(report.CommandChanges) != 0 {
		t.Fatalf("report = %+v, want the warning and the removed function", report)
	}

	never := newFixture(t, catalog("package", "stable"))
	never.write("lib/greet/greet.go", greetSource+"\n// Bow bows.\nfunc Bow() {}\n")
	never.commit("feat: bow")
	status, data, events = never.run()
	if status != "OK" || len(notCompared(events)) != 0 {
		t.Fatalf("status = %q, events = %+v, want no warning for a project that never declared a document", status, events)
	}
	for member := range data {
		if strings.HasPrefix(member, "commandSurface") {
			t.Fatalf("data = %v, want no command-surface member", data)
		}
	}
}

// A putnami.json at the tag that the check cannot read, or that sets the
// option to something other than a path of the project, leaves the surface
// uncompared with a warning; it never fails the task, since no marker could
// make history readable. The warning stands whether or not the working tree
// declares a document: the check cannot tell what the tag declared.
func TestAnUnreadableDeclarationAtTheTagWarns(t *testing.T) {
	t.Parallel()
	const config = "lib/greet/putnami.json"
	for _, testCase := range []struct {
		name    string
		prepare func(f *fixture)
		want    string
		// dropped is the warning when the working tree declares no
		// document; want when empty.
		dropped string
	}{
		{
			name:    "not JSON",
			prepare: func(f *fixture) { f.write(config, `{"name":`) },
			want:    "the project's putnami.json at lib/v0.5.0 does not parse (",
		},
		{
			name: "not a path",
			prepare: func(f *fixture) {
				f.write(config, `{"options":{"@putnami/go:validate":{"command-surface":true}}}`)
			},
			want: "the project's putnami.json at lib/v0.5.0 sets option command-surface to true, not a path, so the command surface is not compared",
		},
		{
			name:    "out of the project",
			prepare: func(f *fixture) { f.write(config, `{"options":{"validate":{"command-surface":"../out.json"}}}`) },
			want:    `option command-surface is "../out.json" at lib/v0.5.0, not the path of a file of the project, so the command surface is not compared`,
			dropped: `the project declared the command surface "../out.json" at lib/v0.5.0 and declares none now, so the command surface is not compared`,
		},
		{
			name: "a symbolic link",
			prepare: func(f *fixture) {
				f.write("lib/greet/real.json", `{}`)
				if err := os.Remove(filepath.Join(f.root, config)); err != nil {
					f.t.Fatal(err)
				}
				if err := os.Symlink("real.json", filepath.Join(f.root, config)); err != nil {
					f.t.Fatal(err)
				}
			},
			want: "the project's putnami.json at lib/v0.5.0 is not a regular file, so the command surface is not compared",
		},
		{
			name:    "over the bound",
			prepare: func(f *fixture) { f.write(config, strings.Repeat(" ", projectConfigMaxBytes)+"{}") },
			want:    "the project's putnami.json at lib/v0.5.0 is 1048578 bytes, over the limit of 1048576",
		},
	} {
		f := newFixture(t, catalog("package", "stable"))
		testCase.prepare(f)
		f.commit("feat: a declaration")
		f.git("tag", surfaceTag)
		if err := os.Remove(filepath.Join(f.root, config)); err != nil {
			t.Fatal(err)
		}
		declareSurface(f, surfaceFile)
		f.write(surfaceLocation, surfaceDocument(t, loudFlag()))
		f.commit("feat: the greet command")
		report := f.check()
		if !strings.HasPrefix(report.SurfaceWarning, testCase.want) || report.surfaceCompared() {
			t.Errorf("%s: warning %q, want %q", testCase.name, report.SurfaceWarning, testCase.want)
		}

		// The task parameters carry no option: the working tree declares no
		// document.
		f.options = Options{}
		dropped := testCase.dropped
		if dropped == "" {
			dropped = testCase.want
		}
		report = f.check()
		if !strings.HasPrefix(report.SurfaceWarning, dropped) || report.ReleasedSurface != "" || !report.surfaceReported() {
			t.Errorf("%s, declared nowhere now: warning %q, released %q, want %q", testCase.name, report.SurfaceWarning, report.ReleasedSurface, dropped)
		}
	}
}

// The option at the tag is read from every layer of the project's options
// the CLI merges for this task, the most specific one winning, under either
// spelling.
func TestTheDeclarationAtTheTagFollowsTheOptionLayers(t *testing.T) {
	t.Parallel()
	const (
		command   = `"validate":{"command-surface":"a.json"}`
		extension = `"@putnami/go":{"command-surface":"b.json"}`
		both      = `"@putnami/go:validate":{"command-surface":"c.json"}`
	)
	for _, testCase := range []struct{ config, want string }{
		{config: `{}`},
		{config: `{"options":{}}`},
		{config: `{"options":{"build":{"command-surface":"a.json"}}}`},
		{config: `{"options":{` + command + `}}`, want: "a.json"},
		{config: `{"options":{"@putnami/go":{"commandSurface":"b.json"}}}`, want: "b.json"},
		{config: `{"options":{` + command + `,` + extension + `}}`, want: "b.json"},
		{config: `{"options":{` + command + `,` + extension + `,` + both + `}}`, want: "c.json"},
		{config: `{"options":{` + command + `,"@putnami/go:validate":{"command-surface":null}}}`},
		{config: `{"options":{"@putnami/go:validate":{"command-surface":"a.json","commandSurface":"b.json"}}}`, want: "b.json"},
	} {
		got, err := declaredSurfaceOption([]byte(testCase.config))
		if err != nil || got != testCase.want {
			t.Errorf("%s: option = %q, err = %v, want %q", testCase.config, got, err, testCase.want)
		}
	}
}

// deliveredParams returns the task parameters the CLI delivers for a project's
// option layers. It doubles the CLI's merge (tooling/cli/internal/jobs
// mergeParamLayers and projectParamAliases): the command, extension and
// extension-command layers apply in that order, each replacing what the
// earlier ones set, and a kebab-case key also sets its camelCase alias unless
// its layer sets that alias itself. The CLI's half is pinned by
// TestTheCLIDeliversTheWinningCommandSurfaceUnderTheCamelCaseKey in
// tooling/cli/internal/cli.
func deliveredParams(t *testing.T, config string) pctx.Params {
	t.Helper()
	var project struct {
		Options map[string]map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal([]byte(config), &project); err != nil {
		t.Fatal(err)
	}
	params := pctx.Params{}
	for _, name := range []string{"validate", "@putnami/go", "@putnami/go:validate"} {
		layer := project.Options[name]
		for key, value := range layer {
			params[key] = value
		}
		if value, ok := layer["command-surface"]; ok {
			if _, exact := layer["commandSurface"]; !exact {
				params["commandSurface"] = value
			}
		}
	}
	return params
}

// The working tree's side reads the option from the parameters the CLI
// merged, and the tag's side from the putnami.json at the tag. Whatever
// spelling each layer uses, both pick the path of the layer the CLI's merge
// makes win.
func TestBothSidesPickTheSamePathWhateverTheSpelling(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct{ name, config, want string }{
		{
			name:   "a lower kebab-case layer under a higher camelCase one",
			config: `{"options":{"validate":{"command-surface":"a.json"},"@putnami/go:validate":{"commandSurface":"b.json"}}}`,
			want:   "b.json",
		},
		{
			name:   "a lower camelCase layer under a higher kebab-case one",
			config: `{"options":{"validate":{"commandSurface":"a.json"},"@putnami/go:validate":{"command-surface":"b.json"}}}`,
			want:   "b.json",
		},
		{
			name:   "a lower kebab-case layer under a higher camelCase one, the extension's",
			config: `{"options":{"validate":{"command-surface":"a.json"},"@putnami/go":{"commandSurface":"b.json"}}}`,
			want:   "b.json",
		},
		{
			name:   "both spellings in the winning layer",
			config: `{"options":{"validate":{"commandSurface":"c.json"},"@putnami/go:validate":{"command-surface":"a.json","commandSurface":"b.json"}}}`,
			want:   "b.json",
		},
	} {
		tagSide, err := declaredSurfaceOption([]byte(testCase.config))
		if err != nil {
			t.Fatalf("%s: tag side: %v", testCase.name, err)
		}
		options, err := optionsFromParams(deliveredParams(t, testCase.config))
		if err != nil {
			t.Fatalf("%s: working tree side: %v", testCase.name, err)
		}
		if tagSide != testCase.want || options.CommandSurface != testCase.want {
			t.Errorf("%s: the tag side picks %q and the working tree side %q, want %q for both", testCase.name, tagSide, options.CommandSurface, testCase.want)
		}
	}
}
