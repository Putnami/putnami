package apicheck

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/runtime"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

const (
	feature     = "go/go-project-toolchain"
	requirement = "api-compatibility-marker"

	projectName = "go.example.com/lib"
	firstTag    = "lib/v0.4.0"

	greetSource = "package lib\n\n" +
		"// Greet greets.\nfunc Greet(name string) string { return \"hello \" + name }\n\n" +
		"// Wave waves.\nfunc Wave() {}\n"
	waveOnlySource = "package lib\n\n// Wave waves.\nfunc Wave() {}\n"
)

// catalog is a support catalog that gives the fixture project status, listed
// under kind.
func catalog(kind, status string) string {
	return `{"protocolVersion":1,"entries":[{"id":"` + projectName + `","kind":"` + kind + `","status":"` + status + `"}]}`
}

// fixture is a workspace in a git repository: the scope lib/ declares the
// version line "lib/v{version}", and its project lib/greet is the Go module
// go.example.com/lib, tagged lib/v0.4.0 with Greet and Wave exported.
type fixture struct {
	t       *testing.T
	root    string
	project string
	// options are the project's options: check passes them to Check, and run
	// delivers them as task parameters.
	options Options
	// tags are the project's resolved tags as the job context carries them:
	// check passes them to Check, and run delivers them as project.tags. They
	// start as the tags the project's putnami.json declares, sorted.
	tags []string
}

func newFixture(t *testing.T, supportCatalog string, projectTags ...string) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, project: filepath.Join(root, "lib", "greet")}
	f.write("putnami.workspace.json", `{"name":"fixture"}`+"\n")
	if supportCatalog != "" {
		f.write("putnami.support.json", supportCatalog+"\n")
	}
	f.write("lib/putnami.json", `{"line":{},"includes":["greet"]}`+"\n")
	f.tags = append([]string{"go"}, projectTags...)
	slices.Sort(f.tags)
	tags, err := json.Marshal(f.tags)
	if err != nil {
		t.Fatal(err)
	}
	f.write("lib/greet/putnami.json", `{"name":"`+projectName+`","tags":`+string(tags)+`}`+"\n")
	f.write("lib/greet/go.mod", "module "+projectName+"\n\ngo 1.25\n")
	f.write("lib/greet/greet.go", greetSource)
	f.git("init", "-q")
	f.commit("feat: greet")
	f.git("tag", firstTag)
	return f
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()
	path := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// git runs git in the fixture root with no user or system configuration.
func (f *fixture) git(args ...string) {
	f.t.Helper()
	runGit(f.t, f.root, args...)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		"-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

// commit commits every change; message may carry a body after a blank line.
func (f *fixture) commit(message string) {
	f.t.Helper()
	f.git("add", "-A")
	f.git("commit", "-q", "--allow-empty", "-m", message)
}

func (f *fixture) check() *Report {
	f.t.Helper()
	report, err := Check(f.root, f.project, projectName, f.tags, f.options)
	if err != nil {
		f.t.Fatalf("Check: %v", err)
	}
	return report
}

// run runs the task body the way the extension dispatches it and returns its
// status, its result data and the events it emitted. It redirects os.Stdout,
// so a test that calls it does not run in parallel.
func (f *fixture) run() (string, map[string]any, []runtime.Event) {
	f.t.Helper()
	ctx := &pctx.Context{
		WorkspaceRoot: f.root,
		Project:       pctx.Project{Name: projectName, Path: "lib/greet", FullPath: f.project, Tags: f.tags},
	}
	if f.options.CommandSurface != "" {
		value, err := json.Marshal(f.options.CommandSurface)
		if err != nil {
			f.t.Fatal(err)
		}
		ctx.Params = pctx.Params{commandSurfaceOption: value}
	}
	var status string
	var data map[string]any
	var runErr error
	output := captureStdout(f.t, func() {
		status, data, runErr = Run(ctx, jsonl.NewForVersion(1), nil)
	})
	if runErr != nil {
		f.t.Fatalf("Run: %v", runErr)
	}
	return status, data, decodeEvents(f.t, output)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = file
	defer func() {
		os.Stdout = original
		_ = file.Close()
	}()
	fn()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decodeEvents(t *testing.T, output string) []runtime.Event {
	t.Helper()
	var events []runtime.Event
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		var event runtime.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	return events
}

// diagnostics returns the messages of the diagnostics of one severity.
func diagnostics(events []runtime.Event, severity runtime.DiagnosticSeverity) []runtime.Event {
	var found []runtime.Event
	for _, event := range events {
		if event.Type == runtime.EventDiagnostic && event.Severity != nil && *event.Severity == severity {
			found = append(found, event)
		}
	}
	return found
}

func TestARemovedFunctionUnderAFeatCommitFailsValidate(t *testing.T) {
	spectest.Proves(t, feature, requirement, "an-incompatible-change-without-a-marker-fails-naming-package-and-symbol")
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat: drop the greeting")

	status, data, events := f.run()
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	errorsFound := diagnostics(events, runtime.SeverityError)
	if len(errorsFound) != 1 {
		t.Fatalf("error diagnostics = %+v, want one", errorsFound)
	}
	want := `go.example.com/lib: func Greet removed since lib/v0.4.0; declare the breaking change with "!" or a BREAKING CHANGE: footer`
	if errorsFound[0].Message != want || errorsFound[0].Code != Code {
		t.Fatalf("diagnostic = %q (code %q)\nwant        %q (code %q)", errorsFound[0].Message, errorsFound[0].Code, want, Code)
	}
	if data["incompatible"] != 1 || data["tag"] != firstTag {
		t.Fatalf("data = %v", data)
	}
}

func TestABreakingMarkerAllowsTheChange(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-breaking-marker-since-the-tag-allows-the-change")
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat!: drop the greeting")
	f.write("lib/greet/README.md", "# lib\n")
	f.commit("docs: describe the module")

	status, data, events := f.run()
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	infos := diagnostics(events, runtime.SeverityInfo)
	want := "go.example.com/lib: func Greet removed since lib/v0.4.0; a commit since lib/v0.4.0 declares the breaking change"
	if len(infos) != 1 || infos[0].Message != want {
		t.Fatalf("info diagnostics = %+v\nwant one: %q", infos, want)
	}
	if data["breakingDeclared"] != true {
		t.Fatalf("data = %v, want breakingDeclared", data)
	}
}

// A branch and its squash merge hold one tree and declare one break with
// different commits. The check names no commit, so both runs give the same
// status, data and events, and one cached verdict can serve both.
func TestABranchAndItsSquashMergeGiveTheSameOutput(t *testing.T) {
	spectest.Proves(t, feature, "api-verdict-names-no-commit", "a-branch-and-its-squash-merge-give-one-verdict")
	f := newFixture(t, catalog("package", "stable"))
	f.git("checkout", "-q", "-b", "feature")
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat!: drop the greeting")
	f.write("lib/greet/README.md", "# lib\n")
	f.commit("docs: describe the module")
	branchStatus, branchData, branchEvents := f.run()

	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--squash", "feature")
	f.commit("feat!: drop the greeting (#3)")
	squashStatus, squashData, squashEvents := f.run()

	if branchStatus != "OK" || branchData["breakingDeclared"] != true {
		t.Fatalf("branch status = %q, data = %v, want the declared break", branchStatus, branchData)
	}
	if squashStatus != branchStatus {
		t.Fatalf("status = %q on the squash, %q on the branch", squashStatus, branchStatus)
	}
	branchJSON, squashJSON := canonicalOutput(t, branchData, branchEvents), canonicalOutput(t, squashData, squashEvents)
	if branchJSON != squashJSON {
		t.Fatalf("the squash merge gave another output:\nbranch %s\nsquash %s", branchJSON, squashJSON)
	}
}

// canonicalOutput is a run's data and events without their emission times.
func canonicalOutput(t *testing.T, data map[string]any, events []runtime.Event) string {
	t.Helper()
	timeless := make([]runtime.Event, len(events))
	for i, event := range events {
		event.Time = ""
		timeless[i] = event
	}
	encoded, err := json.Marshal(map[string]any{"data": data, "events": timeless})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestABreakingChangeFooterAllowsTheChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("refactor: drop the greeting\n\nBREAKING CHANGE: Greet is gone")
	report := f.check()
	if len(report.Changes) != 1 || !report.Breaking {
		t.Fatalf("report = %+v", report)
	}
}

func TestAMarkerOnACommitOutsideTheProjectDoesNotCount(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("other/notes.md", "elsewhere\n")
	f.commit("feat!: break something else")
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat: drop the greeting")
	report := f.check()
	if len(report.Changes) != 1 || report.Breaking {
		t.Fatalf("report = %+v, want one change and no marker", report)
	}
}

func TestAnUncommittedRemovalIsChecked(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/greet.go", waveOnlySource)
	report := f.check()
	if len(report.Changes) != 1 || report.Changes[0].Symbol != "func Greet" || report.Breaking {
		t.Fatalf("report = %+v", report)
	}
}

func TestAPreviewOrExperimentalProjectIsSkipped(t *testing.T) {
	spectest.Proves(t, "support/support-catalog", "stable-only-consumers", "go-extension-skips-a-preview-or-experimental-project")
	spectest.Proves(t, feature, requirement, "a-preview-or-experimental-project-is-not-checked")
	f := newFixture(t, catalog("package", "experimental"))
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat: drop the greeting")

	status, _, events := f.run()
	if status != "SKIP" {
		t.Fatalf("status = %q, want SKIP", status)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	preview := newFixture(t, catalog("package", "preview"))
	if report := preview.check(); !strings.Contains(report.Skip, "is preview in putnami.support.json") {
		t.Fatalf("report = %+v", report)
	}
}

func TestAProjectTheCatalogDoesNotListIsSkipped(t *testing.T) {
	spectest.Proves(t, "support/support-catalog", "stable-only-consumers", "go-extension-skips-a-project-the-catalog-does-not-list")
	spectest.Proves(t, feature, requirement, "an-unlisted-project-is-not-checked-when-a-catalog-exists")
	f := newFixture(t, `{"protocolVersion":1,"entries":[{"id":"go.example.com/other","kind":"package","status":"stable"}]}`)
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat: drop the greeting")

	status, _, events := f.run()
	if status != "SKIP" {
		t.Fatalf("status = %q, want SKIP", status)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	want := "putnami.support.json does not list the package go.example.com/lib, so it promises no compatibility: its API is not checked"
	if report := f.check(); report.Skip != want {
		t.Fatalf("skip = %q\nwant   %q", report.Skip, want)
	}
}

func TestTheProtocolTagSelectsTheProtocolSubject(t *testing.T) {
	t.Parallel()
	listedAsPackage := newFixture(t, catalog("package", "stable"), "protocol")
	listedAsPackage.write("lib/greet/greet.go", waveOnlySource)
	if report := listedAsPackage.check(); !strings.Contains(report.Skip, "does not list the protocol "+projectName) {
		t.Fatalf("a protocol project the catalog lists only as a package is unlisted: %+v", report)
	}
	listedAsProtocol := newFixture(t, catalog("protocol", "stable"), "protocol")
	listedAsProtocol.write("lib/greet/greet.go", waveOnlySource)
	if report := listedAsProtocol.check(); report.Skip != "" || len(report.Changes) != 1 {
		t.Fatalf("report = %+v, want the protocol entry to check it", report)
	}
}

// The check reads the project's tags from the job context and from no file:
// the tags the project's putnami.json and its scope's declare do not select
// the catalog subject.
func TestTheSupportSubjectFollowsTheContextTags(t *testing.T) {
	const (
		unlistedPackage  = "does not list the package " + projectName
		unlistedProtocol = "does not list the protocol " + projectName
	)
	for _, testCase := range []struct {
		name string
		// project and scope are the tags the two putnami.json files declare;
		// nil declares none.
		project, scope []string
		// context are the tags the job context carries.
		context []string
		// listed is the kind the catalog lists the project under, as stable.
		listed string
		// skip is part of the reason the project is not checked; empty when
		// the context makes it the listed subject, so it is checked.
		skip string
	}{
		{
			name:    "the context says protocol and the project declares another tag",
			project: []string{"go"}, context: []string{"go", "protocol"},
			listed: "protocol",
		},
		{
			name:    "the context says protocol and no file declares a tag",
			context: []string{"protocol"},
			listed:  "protocol",
		},
		{
			name:    "the context says protocol against a package entry",
			project: []string{"go"}, context: []string{"go", "protocol"},
			listed: "package", skip: unlistedProtocol,
		},
		{
			name:    "the project declares protocol and the context does not carry it",
			project: []string{"go", "protocol"}, context: []string{"go"},
			listed: "protocol", skip: unlistedPackage,
		},
		{
			name:  "the scope declares protocol and the context does not carry it",
			scope: []string{"protocol"}, context: []string{"go"},
			listed: "protocol", skip: unlistedPackage,
		},
		{
			name:    "the files declare protocol against a package entry",
			project: []string{"protocol"}, scope: []string{"protocol"}, context: []string{"go"},
			listed: "package",
		},
		{
			name:    "the context carries an empty list",
			project: []string{"protocol"}, scope: []string{"protocol"}, context: []string{},
			listed: "package",
		},
		{
			name:    "an empty list against a protocol entry",
			project: []string{"protocol"}, context: []string{},
			listed: "protocol", skip: unlistedPackage,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t, catalog(testCase.listed, "stable"))
			declared := func(tags []string) string {
				if tags == nil {
					return ""
				}
				encoded, err := json.Marshal(tags)
				if err != nil {
					t.Fatal(err)
				}
				return `,"tags":` + string(encoded)
			}
			f.write("lib/putnami.json", `{"line":{},"includes":["greet"]`+declared(testCase.scope)+`}`+"\n")
			f.write("lib/greet/putnami.json", `{"name":"`+projectName+`"`+declared(testCase.project)+`}`+"\n")
			f.write("lib/greet/greet.go", waveOnlySource)
			f.commit("feat: drop the greeting")
			f.tags = testCase.context

			report := f.check()
			status, _, events := f.run()
			if testCase.skip != "" {
				if !strings.Contains(report.Skip, testCase.skip) {
					t.Errorf("skip = %q, want it to hold %q", report.Skip, testCase.skip)
				}
				if status != "SKIP" {
					t.Errorf("status = %q, want SKIP", status)
				}
				return
			}
			if report.Skip != "" || len(report.Changes) != 1 {
				t.Errorf("report = %+v, want the project checked and one change", report)
			}
			if found := diagnostics(events, runtime.SeverityError); status != "FAILED" || len(found) != 1 {
				t.Errorf("status = %q, error diagnostics = %+v, want FAILED with one", status, found)
			}
		})
	}
}

// The tags of a project that declares none come from a workspace provider or
// from its scopes, and the job context carries the resolved list. An edit to
// the scope's putnami.json alone does not change the verdict: the context
// carries the same tags before and after.
func TestAScopeTagEditAloneDoesNotChangeTheVerdict(t *testing.T) {
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/putnami.json", `{"name":"`+projectName+`"}`+"\n")
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat: drop the greeting")
	f.tags = []string{"provided"}

	before := f.check()
	beforeStatus, beforeData, _ := f.run()
	if before.Skip != "" || len(before.Changes) != 1 || beforeStatus != "FAILED" {
		t.Fatalf("report = %+v, status = %q, want the package checked and failed", before, beforeStatus)
	}

	f.write("lib/putnami.json", `{"line":{},"includes":["greet"],"tags":["protocol"]}`+"\n")
	after := f.check()
	afterStatus, afterData, _ := f.run()
	if !reflect.DeepEqual(after, before) {
		t.Errorf("the scope edit changed the report:\nbefore %+v\nafter  %+v", before, after)
	}
	if afterStatus != beforeStatus || !reflect.DeepEqual(afterData, beforeData) {
		t.Errorf("the scope edit changed the result: %q %v, then %q %v", beforeStatus, beforeData, afterStatus, afterData)
	}
}

// A job context without project.tags gives no subject to read the catalog
// for. The check fails as an invalid configuration and names the member. It
// does not read the tags the files declare, and it does not read the absent
// member as "no tags".
// When the catalog gives the project the same answer as a package and as a
// protocol, the tags cannot change the verdict, so a job context without
// project.tags gets that answer.
func TestAContextWithoutTagsTakesTheVerdictBothKindsShare(t *testing.T) {
	both := `{"protocolVersion":1,"entries":[` +
		`{"id":"` + projectName + `","kind":"package","status":"stable"},` +
		`{"id":"` + projectName + `","kind":"protocol","status":"stable"}]}`
	f := newFixture(t, both)
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat: drop the greeting")
	report, err := Check(f.root, f.project, projectName, nil, f.options)
	if err != nil || report.Skip != "" || len(report.Changes) != 1 {
		t.Fatalf("listed stable both ways: report = %+v, err = %v, want the project checked", report, err)
	}

	f = newFixture(t, `{"protocolVersion":1,"entries":[{"id":"other","kind":"package","status":"stable"}]}`)
	report, err = Check(f.root, f.project, projectName, nil, f.options)
	if err != nil || !strings.Contains(report.Skip, "neither a stable package nor a stable protocol") {
		t.Fatalf("not listed: report = %+v, err = %v, want the project skipped", report, err)
	}
}

func TestAContextWithoutTagsFailsTheCheckWhenACatalogExists(t *testing.T) {
	for _, listed := range []string{"package", "protocol"} {
		f := newFixture(t, catalog(listed, "stable"), "protocol")
		f.write("lib/greet/greet.go", waveOnlySource)
		f.commit("feat: drop the greeting")

		report, err := Check(f.root, f.project, projectName, nil, f.options)
		if err == nil {
			t.Fatalf("listed as %s: report = %+v, want an error", listed, report)
		}
		if !errors.Is(err, protocolcli.ErrInvalidConfig) {
			t.Errorf("listed as %s: err = %v, want it classified as invalid configuration", listed, err)
		}
		if !strings.Contains(err.Error(), "`project.tags`") || !strings.Contains(err.Error(), projectName) {
			t.Errorf("listed as %s: err = %v, want the member and the project named", listed, err)
		}

		ctx := &pctx.Context{
			WorkspaceRoot: f.root,
			Project:       pctx.Project{Name: projectName, Path: "lib/greet", FullPath: f.project},
		}
		var status string
		var runErr error
		captureStdout(t, func() {
			status, _, runErr = Run(ctx, jsonl.NewForVersion(1), nil)
		})
		if status != "FAILED" || !errors.Is(runErr, protocolcli.ErrInvalidConfig) {
			t.Errorf("listed as %s: status = %q, err = %v, want FAILED as invalid configuration", listed, status, runErr)
		}
	}
}

// A workspace with no catalog reads no tag, so a job context without
// project.tags is checked like any other.
func TestAContextWithoutTagsIsCheckedWhenThereIsNoCatalog(t *testing.T) {
	f := newFixture(t, "")
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("feat: drop the greeting")
	f.tags = nil

	if report := f.check(); report.Skip != "" || len(report.Changes) != 1 {
		t.Fatalf("report = %+v, want the project checked and one change", report)
	}
	if status, _, _ := f.run(); status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
}

func TestAWorkspaceWithoutACatalogChecksEveryProject(t *testing.T) {
	spectest.Proves(t, "support/support-catalog", "stable-only-consumers", "go-extension-checks-every-project-without-a-catalog")
	t.Parallel()
	f := newFixture(t, "")
	f.write("lib/greet/greet.go", waveOnlySource)
	if report := f.check(); report.Skip != "" || len(report.Changes) != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestAnInvalidCatalogFailsTheTask(t *testing.T) {
	spectest.Proves(t, "support/support-catalog", "stable-only-consumers", "go-extension-fails-on-an-invalid-catalog")
	t.Parallel()
	f := newFixture(t, `{"protocolVersion":1,"entries":[{"id":"x","kind":"package","status":"beta"}]}`)
	_, err := Check(f.root, f.project, projectName, f.tags, Options{})
	if err == nil || !strings.Contains(err.Error(), "putnami.support.json is invalid") {
		t.Fatalf("err = %v, want the invalid catalog named", err)
	}
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("err = %v, want it classified as invalid configuration", err)
	}
}

func TestALineWithoutATagHasNothingToCompare(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-line-without-a-reachable-tag-passes")
	f := newFixture(t, catalog("package", "stable"))
	f.git("tag", "-d", firstTag)
	f.write("lib/greet/greet.go", waveOnlySource)

	status, data, events := f.run()
	if status != "OK" || data["compared"] != false {
		t.Fatalf("status = %q, data = %v, want OK without a comparison", status, data)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	if report := f.check(); !strings.Contains(report.Note, "no release tag of line lib/v{version} is reachable from HEAD") || report.Warning != "" {
		t.Fatalf("report = %+v", report)
	}

	// Tags of other lines suggest the checkout lacks this line's: a warning.
	f.git("tag", "other/v1.0.0")
	status, data, events = f.run()
	if status != "OK" || data["compared"] != false {
		t.Fatalf("status = %q, data = %v, want OK without a comparison", status, data)
	}
	warnings := diagnostics(events, runtime.SeverityWarning)
	if len(warnings) != 1 || warnings[0].Code != NotComparedCode ||
		!strings.Contains(warnings[0].Message, "no release tag of line lib/v{version} is reachable from HEAD, though the repository has other tags") {
		t.Fatalf("warnings = %+v", warnings)
	}
}

// A tag that is not a release of the line is never the baseline, however near
// HEAD it sits.
func TestATagThatIsNotAReleaseIsNoBaseline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.commit("chore: a later commit")
	f.git("tag", "lib/vnext-experiment")
	f.git("tag", "lib/v0.5.0-rc.1")
	f.commit("chore: one more commit")
	if report := f.check(); report.Tag != firstTag || report.Warning != "" {
		t.Fatalf("report = %+v, want the baseline %s", report, firstTag)
	}
}

// An older release a merged branch carries does not replace a later release:
// the baseline is the highest release tag HEAD reaches. The branch holds two
// commits before its tag, so `git describe` finds that tag nearest.
func TestAnOlderReleaseOfAMergedBranchIsNoBaseline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.commit("feat: more")
	f.git("tag", "lib/v0.5.0")
	f.commit("fix: repair")
	f.commit("docs: explain")
	f.git("checkout", "-q", "-b", "hotfix", firstTag)
	f.commit("fix: hotfix")
	f.commit("test: cover the hotfix")
	f.git("tag", "lib/v0.4.1")
	f.git("checkout", "-q", "main")
	f.git("merge", "-q", "--no-ff", "-m", "merge the hotfix", "hotfix")
	if report := f.check(); report.Tag != "lib/v0.5.0" {
		t.Fatalf("report = %+v, want the baseline lib/v0.5.0", report)
	}
}

// A branch that merged main reaches the release main tagged after the branch
// started, through the merge's second parent: that release is the baseline.
func TestAReleaseReachedThroughAMergeOfMainIsTheBaseline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.git("checkout", "-q", "-b", "feature")
	f.commit("feat: start")
	f.git("checkout", "-q", "main")
	f.commit("feat: ship")
	f.git("tag", "lib/v0.5.0")
	f.git("checkout", "-q", "feature")
	f.git("merge", "-q", "--no-ff", "-m", "merge main", "main")
	if report := f.check(); report.Tag != "lib/v0.5.0" {
		t.Fatalf("report = %+v, want the baseline lib/v0.5.0", report)
	}
}

func TestAnAddedSymbolPasses(t *testing.T) {
	spectest.Proves(t, feature, requirement, "an-addition-is-compatible")
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/greet.go", greetSource+"\n// Bow bows.\nfunc Bow() {}\n")
	f.write("lib/greet/extra/extra.go", "package extra\n\n// New makes one.\nfunc New() int { return 1 }\n")
	f.commit("feat: bow")

	status, data, _ := f.run()
	if status != "OK" || data["incompatible"] != 0 || data["packages"] != 1 {
		t.Fatalf("status = %q, data = %v", status, data)
	}
}

func TestRenamedParametersPass(t *testing.T) {
	spectest.Proves(t, feature, requirement, "renamed-parameters-are-compatible")
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/greet.go", strings.Replace(greetSource, "Greet(name string) string { return \"hello \" + name }",
		"Greet(who string) string { return \"hello \" + who }", 1))
	f.commit("refactor: rename the parameter")
	if report := f.check(); len(report.Changes) != 0 {
		t.Fatalf("changes = %+v, want none", report.Changes)
	}

	f.write("lib/greet/greet.go", strings.Replace(greetSource, "Greet(name string) string", "Greet(name string, loud bool) string", 1))
	report := f.check()
	if len(report.Changes) != 1 || report.Changes[0].Message != "func Greet changed from func(string) string to func(string, bool) string" {
		t.Fatalf("changes = %+v", report.Changes)
	}
	if report.Changes[0].Pos.File != "greet.go" || report.Changes[0].Pos.Line != 4 {
		t.Fatalf("position = %+v, want greet.go:4", report.Changes[0].Pos)
	}
}

func TestAWorkspaceOutsideGitHasNothingToCompare(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "lib")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	// The temporary directory may sit inside a repository; git must not find it.
	t.Setenv("GIT_CEILING_DIRECTORIES", root)
	report, err := Check(root, project, projectName, []string{"go"}, Options{})
	if err != nil || !strings.Contains(report.Warning, "not in a git repository") || report.Note != "" {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
}

func TestOnlyTheFilesTheSurfaceReadsAreLoadedFromTheTag(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	for _, path := range []string{"internal/x/x.go", "testdata/x.go", "vendor/a/a.go", "_old/x.go", "sub/x.go", "greet_test.go"} {
		f.write("lib/greet/"+path, "package x\n")
	}
	f.write("lib/greet/sub/go.mod", "module go.example.com/sub\n")
	f.commit("chore: more files")
	f.git("tag", "lib/v0.5.0")

	files, err := filesAtTag(f.project, "lib/v0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	if want := []string{"go.mod", "greet.go", "sub/go.mod"}; !slices.Equal(paths, want) {
		t.Fatalf("loaded %q, want %q", paths, want)
	}
	if string(files[1].Data) != greetSource {
		t.Fatalf("greet.go = %q", files[1].Data)
	}
}

// The task's cache key reads no file below a directory the CLI's `**` walk
// skips (out, dist, node_modules, vendor) or whose name starts with a dot. The
// check reads none of them either, in the working tree and at the tag, so a
// change there can neither leave a stored verdict stale nor show as a package
// added or removed on one side.
func TestAPackageUnderADirectoryTheKeyDoesNotReadIsNotCompared(t *testing.T) {
	t.Parallel()
	unkeyed := []string{"dist/format", "out/format", "node_modules/format", "vendor/format", ".cache/format", "nested/dist/format"}
	f := newFixture(t, catalog("package", "stable"))
	for _, dir := range unkeyed {
		f.write("lib/greet/"+dir+"/format.go", "package format\n\n// Format formats.\nfunc Format() {}\n")
	}
	f.commit("feat: format packages")
	f.git("tag", "lib/v0.5.0")

	files, err := filesAtTag(f.project, "lib/v0.5.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if inUnkeyedDirectory(file.Path) {
			t.Errorf("loaded %s from the tag, below a directory the key does not read", file.Path)
		}
	}
	current, err := filesInTree(f.project)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range current {
		if inUnkeyedDirectory(file.Path) {
			t.Errorf("read %s from the working tree, below a directory the key does not read", file.Path)
		}
	}

	for _, dir := range unkeyed {
		f.write("lib/greet/"+dir+"/format.go", "package format\n")
	}
	f.commit("feat: drop the format functions")
	report := f.check()
	if report.Tag != "lib/v0.5.0" || len(report.Changes) != 0 || report.Packages != 1 {
		t.Fatalf("report = %+v, want lib/v0.5.0 compared over its one keyed package with no change", report)
	}

	// The control: the same removal in a keyed directory is a change.
	f.write("lib/greet/format/format.go", "package format\n\n// Format formats.\nfunc Format() {}\n")
	f.commit("feat: a keyed format package")
	f.git("tag", "lib/v0.6.0")
	f.write("lib/greet/format/format.go", "package format\n")
	f.commit("feat: drop it")
	if report := f.check(); len(report.Changes) != 1 || report.Changes[0].Symbol != "func Format" {
		t.Fatalf("report = %+v, want the keyed removal reported", report)
	}
}

func TestARepositoryWithoutACommitHasNothingToCompare(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	report, err := Check(root, root, projectName, []string{"go"}, Options{})
	if err != nil || !strings.Contains(report.Note, "no commit") {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
}

func TestAShallowCloneWarnsInsteadOfComparing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.commit("chore: a second commit")
	clone := filepath.Join(t.TempDir(), "clone")
	runGit(t, f.root, "clone", "-q", "--depth", "1", "file://"+f.root, clone)
	report, err := Check(clone, filepath.Join(clone, "lib", "greet"), projectName, f.tags, Options{})
	if err != nil || !strings.Contains(report.Warning, "shallow") {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
}

func TestLinePatternFollowsTheNearestLineScope(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	if got := linePattern(f.root, f.project); got != "lib/v{version}" {
		t.Fatalf("linePattern = %q", got)
	}
	f.write("lib/putnami.json", `{"line":{"tag":"greet-v{version}"},"includes":["greet"]}`)
	if got := linePattern(f.root, f.project); got != "greet-v{version}" {
		t.Fatalf("linePattern = %q, want the declared tag", got)
	}
	f.write("lib/putnami.json", `{"includes":["greet"]}`)
	if got := linePattern(f.root, f.project); got != "v{version}" {
		t.Fatalf("linePattern = %q, want the root line", got)
	}
}
