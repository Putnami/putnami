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
	feature     = "typescript/typescript-project-toolchain"
	requirement = "api-compatibility-marker"

	projectName = "@example/lib"
	firstTag    = "ts/v0.4.0"

	manifest = `{"name":"@example/lib","exports":{".":"./src/index.ts","./hooks":{"default":"./src/hooks.ts"}}}` + "\n"

	greetSource = "export function greet(name: string): string {\n  return `hello ${name}`;\n}\n\n" +
		"export function wave(): void {}\n"
	waveOnlySource = "export function wave(): void {}\n"
	hooksSource    = "export interface Hook {\n  name: string;\n}\n"
)

// catalog is a support catalog that gives the fixture project status, listed
// under kind.
func catalog(kind, status string) string {
	return `{"protocolVersion":1,"entries":[{"id":"` + projectName + `","kind":"` + kind + `","status":"` + status + `"}]}`
}

// fixture is a workspace in a git repository: the scope ts/ declares the
// version line "ts/v{version}", and its project ts/lib is the package
// @example/lib, tagged ts/v0.4.0 with greet and wave exported from "." and
// Hook from "./hooks". The typescript package resolves from the workspace
// root, as it does after `putnami install`.
type fixture struct {
	t       *testing.T
	root    string
	project string
	// tags are the project's resolved tags as the job context carries them:
	// check passes them to Check, and run delivers them as project.tags. They
	// start as the tags the project's putnami.json declares, sorted.
	tags []string
}

func newFixture(t *testing.T, supportCatalog string, projectTags ...string) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{t: t, root: root, project: filepath.Join(root, "ts", "lib")}
	f.write("putnami.workspace.json", `{"name":"fixture"}`+"\n")
	f.write(".gitignore", "node_modules\n")
	if supportCatalog != "" {
		f.write("putnami.support.json", supportCatalog+"\n")
	}
	f.write("ts/putnami.json", `{"line":{},"includes":["lib"]}`+"\n")
	f.tags = append([]string{"ts"}, projectTags...)
	slices.Sort(f.tags)
	tags, err := json.Marshal(f.tags)
	if err != nil {
		t.Fatal(err)
	}
	f.write("ts/lib/putnami.json", `{"name":"`+projectName+`","tags":`+string(tags)+`}`+"\n")
	f.write("ts/lib/package.json", manifest)
	f.write("ts/lib/src/index.ts", greetSource)
	f.write("ts/lib/src/hooks.ts", hooksSource)
	installTypeScript(t, root)
	f.git("init", "-q")
	f.commit("feat: greet")
	f.git("tag", firstTag)
	return f
}

// installTypeScript makes the typescript package resolve from root by linking
// the one this repository installs.
func installTypeScript(t *testing.T, root string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		installed := filepath.Join(dir, "node_modules", "typescript")
		if _, err := os.Stat(filepath.Join(installed, "package.json")); err == nil {
			modules := filepath.Join(root, "node_modules")
			if err := os.MkdirAll(modules, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(installed, filepath.Join(modules, "typescript")); err != nil {
				t.Fatal(err)
			}
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("the typescript package is not installed in this repository: run ./putnamiw install")
		}
		dir = parent
	}
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

func (f *fixture) remove(rel string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.root, filepath.FromSlash(rel))); err != nil {
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
	report, err := Check(f.root, f.project, projectName, f.tags)
	if err != nil {
		f.t.Fatalf("Check: %v", err)
	}
	return report
}

// messages returns the messages of a report's changes.
func messages(report *Report) []string {
	found := make([]string, len(report.Changes))
	for i, change := range report.Changes {
		found[i] = change.Package + ": " + change.Message
	}
	return found
}

// run runs the task body the way the extension dispatches it and returns its
// status, its result data and the events it emitted. It redirects os.Stdout,
// so a test that calls it does not run in parallel.
func (f *fixture) run() (string, map[string]any, []runtime.Event) {
	f.t.Helper()
	ctx := &pctx.Context{
		WorkspaceRoot: f.root,
		Project:       pctx.Project{Name: projectName, Path: "ts/lib", FullPath: f.project, Tags: f.tags},
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

// diagnostics returns the diagnostics of one severity.
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
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting")

	status, data, events := f.run()
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	errorsFound := diagnostics(events, runtime.SeverityError)
	if len(errorsFound) != 1 {
		t.Fatalf("error diagnostics = %+v, want one", errorsFound)
	}
	want := `@example/lib: function greet removed since ts/v0.4.0; declare the breaking change with "!" or a BREAKING CHANGE: footer`
	if errorsFound[0].Message != want || errorsFound[0].Code != Code {
		t.Fatalf("diagnostic = %q (code %q)\nwant        %q (code %q)", errorsFound[0].Message, errorsFound[0].Code, want, Code)
	}
	if data["incompatible"] != 1 || data["tag"] != firstTag || data["entryPoints"] != 2 {
		t.Fatalf("data = %v", data)
	}
}

func TestABreakingMarkerAllowsTheChange(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-breaking-marker-since-the-tag-allows-the-change")
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat!: drop the greeting")
	f.write("ts/lib/README.md", "# lib\n")
	f.commit("docs: describe the package")

	status, data, events := f.run()
	if status != "OK" {
		t.Fatalf("status = %q, want OK", status)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	infos := diagnostics(events, runtime.SeverityInfo)
	want := "@example/lib: function greet removed since ts/v0.4.0; a commit since ts/v0.4.0 declares the breaking change"
	if len(infos) != 1 || infos[0].Message != want {
		t.Fatalf("info diagnostics = %+v\nwant one: %q", infos, want)
	}
	if data["breakingDeclared"] != true {
		t.Fatalf("data = %v, want breakingDeclared", data)
	}
	if _, named := data["breakingCommit"]; named {
		t.Fatalf("data = %v, want no commit named", data)
	}
}

func TestABreakingChangeFooterAllowsTheChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("refactor: drop the greeting\n\nBREAKING CHANGE: greet is gone")
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
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting")
	report := f.check()
	if len(report.Changes) != 1 || report.Breaking {
		t.Fatalf("report = %+v, want one change and no marker", report)
	}
}

func TestAnAddedExportPasses(t *testing.T) {
	spectest.Proves(t, feature, requirement, "an-addition-is-compatible")
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/src/index.ts", greetSource+"\nexport function bow(depth?: number): void {}\n\nexport * from \"./extra\";\n")
	f.write("ts/lib/src/extra.ts", "export const extra = 1;\n")
	f.write("ts/lib/package.json", strings.Replace(manifest, `".":`, `"./extra":"./src/extra.ts",".":`, 1))
	f.commit("feat: bow")

	status, data, events := f.run()
	if status != "OK" || data["incompatible"] != 0 || data["compared"] != true {
		t.Fatalf("status = %q, data = %v", status, data)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
}

func TestAChangedSignatureWarnsAndPassesAndARenamedParameterIsSilent(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-changed-signature-warns-and-passes-and-a-renamed-parameter-is-silent")
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/src/index.ts", strings.Replace(greetSource, "greet(name: string)", "greet(who: string)", 1))
	f.commit("refactor: rename the parameter")
	if report := f.check(); len(report.Changes) != 0 || len(report.TypeChanges) != 0 {
		t.Fatalf("report = %+v, want no change", report)
	}

	f.write("ts/lib/src/index.ts", strings.Replace(greetSource, "greet(name: string)", "greet(name: string, loud: boolean)", 1))
	f.commit("feat: greet loudly")
	status, data, events := f.run()
	if status != "OK" || data["incompatible"] != 0 || data["typeChanges"] != 1 || data["compared"] != true {
		t.Fatalf("status = %q, data = %v, want a pass with one type change", status, data)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	warnings := diagnostics(events, runtime.SeverityWarning)
	want := "@example/lib: function greet changed from (string) => string to (string, boolean) => string since ts/v0.4.0; " +
		"a change of written type does not fail the check"
	if len(warnings) != 1 || warnings[0].Code != TypeChangeCode || !strings.HasPrefix(warnings[0].Message, want) {
		t.Fatalf("warnings = %+v\nwant one that starts with %q", warnings, want)
	}
	if location := warnings[0].Location; location == nil || location.File != "ts/lib/src/index.ts" || location.Line != 1 {
		t.Fatalf("location = %+v, want ts/lib/src/index.ts:1", location)
	}

	// Once a commit declares the release breaking, the warning is information.
	f.write("ts/lib/src/hooks.ts", hooksSource+"\n")
	f.commit("feat!: greet loudly")
	_, data, events = f.run()
	if found := diagnostics(events, runtime.SeverityWarning); len(found) != 0 || data["breakingDeclared"] != true {
		t.Fatalf("warnings = %+v, data = %v, want none under a breaking marker", found, data)
	}
}

func TestATypeChangeNextToARemovalFailsOnTheRemovalAlone(t *testing.T) {
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/src/index.ts", "export function greet(name: number): string { return ''; }\n")
	f.commit("feat: greet by number")

	status, data, events := f.run()
	if status != "FAILED" || data["incompatible"] != 1 || data["typeChanges"] != 1 {
		t.Fatalf("status = %q, data = %v", status, data)
	}
	found := diagnostics(events, runtime.SeverityError)
	if len(found) != 1 || found[0].Code != Code || !strings.HasPrefix(found[0].Message, "@example/lib: function wave removed since ts/v0.4.0") {
		t.Fatalf("error diagnostics = %+v, want the removal of wave alone", found)
	}
}

func TestAnUncommittedRemovalIsChecked(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/src/index.ts", waveOnlySource)
	report := f.check()
	if len(report.Changes) != 1 || report.Changes[0].Message != "function greet removed" || report.Breaking {
		t.Fatalf("report = %+v", report)
	}
}

func TestAPreviewOrExperimentalProjectIsSkipped(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-preview-or-experimental-project-is-not-checked")
	f := newFixture(t, catalog("package", "experimental"))
	f.write("ts/lib/src/index.ts", waveOnlySource)
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
	spectest.Proves(t, feature, requirement, "an-unlisted-project-is-not-checked-when-a-catalog-exists")
	f := newFixture(t, `{"protocolVersion":1,"entries":[{"id":"@example/other","kind":"package","status":"stable"}]}`)
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting")

	status, _, events := f.run()
	if status != "SKIP" {
		t.Fatalf("status = %q, want SKIP", status)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	want := "putnami.support.json does not list the package @example/lib, so it promises no compatibility: its API is not checked"
	if report := f.check(); report.Skip != want {
		t.Fatalf("skip = %q\nwant   %q", report.Skip, want)
	}
}

func TestAWorkspaceWithoutACatalogChecksEveryProject(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-workspace-without-a-catalog-checks-every-project")
	f := newFixture(t, "")
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting")
	status, data, _ := f.run()
	if status != "FAILED" || data["incompatible"] != 1 {
		t.Fatalf("status = %q, data = %v, want the removal to fail", status, data)
	}
}

func TestAnInvalidCatalogFailsTheCheck(t *testing.T) {
	t.Parallel()
	f := newFixture(t, `{"protocolVersion":1,"entries":[{"id":"x"}]}`)
	if _, err := Check(f.root, f.project, projectName, f.tags); err == nil || !strings.Contains(err.Error(), "putnami.support.json is invalid") {
		t.Fatalf("err = %v, want an invalid catalog", err)
	}
}

func TestTheProtocolTagSelectsTheProtocolSubject(t *testing.T) {
	t.Parallel()
	listedAsPackage := newFixture(t, catalog("package", "stable"), "protocol")
	if report := listedAsPackage.check(); !strings.Contains(report.Skip, "does not list the protocol "+projectName) {
		t.Fatalf("a protocol project the catalog lists only as a package is unlisted: %+v", report)
	}
	listedAsProtocol := newFixture(t, catalog("protocol", "stable"), "protocol")
	listedAsProtocol.write("ts/lib/src/index.ts", waveOnlySource)
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
			project: []string{"ts"}, context: []string{"protocol", "ts"},
			listed: "protocol",
		},
		{
			name:    "the context says protocol and no file declares a tag",
			context: []string{"protocol"},
			listed:  "protocol",
		},
		{
			name:    "the context says protocol against a package entry",
			project: []string{"ts"}, context: []string{"protocol", "ts"},
			listed: "package", skip: unlistedProtocol,
		},
		{
			name:    "the project declares protocol and the context does not carry it",
			project: []string{"protocol", "ts"}, context: []string{"ts"},
			listed: "protocol", skip: unlistedPackage,
		},
		{
			name:  "the scope declares protocol and the context does not carry it",
			scope: []string{"protocol"}, context: []string{"ts"},
			listed: "protocol", skip: unlistedPackage,
		},
		{
			name:    "the files declare protocol against a package entry",
			project: []string{"protocol"}, scope: []string{"protocol"}, context: []string{"ts"},
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
			f.write("ts/putnami.json", `{"line":{},"includes":["lib"]`+declared(testCase.scope)+`}`+"\n")
			f.write("ts/lib/putnami.json", `{"name":"`+projectName+`"`+declared(testCase.project)+`}`+"\n")
			f.write("ts/lib/src/index.ts", waveOnlySource)
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
	f.write("ts/lib/putnami.json", `{"name":"`+projectName+`"}`+"\n")
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting")
	f.tags = []string{"provided"}

	before := f.check()
	beforeStatus, beforeData, _ := f.run()
	if before.Skip != "" || len(before.Changes) != 1 || beforeStatus != "FAILED" {
		t.Fatalf("report = %+v, status = %q, want the package checked and failed", before, beforeStatus)
	}

	f.write("ts/putnami.json", `{"line":{},"includes":["lib"],"tags":["protocol"]}`+"\n")
	after := f.check()
	afterStatus, afterData, _ := f.run()
	if !reflect.DeepEqual(after, before) {
		t.Errorf("the scope edit changed the report:\nbefore %+v\nafter  %+v", before, after)
	}
	if afterStatus != beforeStatus || !reflect.DeepEqual(afterData, beforeData) {
		t.Errorf("the scope edit changed the result: %q %v, then %q %v", beforeStatus, beforeData, afterStatus, afterData)
	}
}

// When the catalog gives the project the same answer as a package and as a
// protocol, the tags cannot change the verdict, so a job context without
// project.tags gets that answer.
func TestAContextWithoutTagsTakesTheVerdictBothKindsShare(t *testing.T) {
	both := `{"protocolVersion":1,"entries":[` +
		`{"id":"` + projectName + `","kind":"package","status":"stable"},` +
		`{"id":"` + projectName + `","kind":"protocol","status":"stable"}]}`
	f := newFixture(t, both)
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting")
	report, err := Check(f.root, f.project, projectName, nil)
	if err != nil || report.Skip != "" || len(report.Changes) != 1 {
		t.Fatalf("listed stable both ways: report = %+v, err = %v, want the project checked", report, err)
	}

	f = newFixture(t, `{"protocolVersion":1,"entries":[{"id":"other","kind":"package","status":"stable"}]}`)
	report, err = Check(f.root, f.project, projectName, nil)
	if err != nil || !strings.Contains(report.Skip, "neither a stable package nor a stable protocol") {
		t.Fatalf("not listed: report = %+v, err = %v, want the project skipped", report, err)
	}
}

// A job context without project.tags gives no subject to read the catalog
// for. When the two readings disagree, the check fails as an invalid
// configuration and names the member. It does not read the tags the files
// declare, and it does not read the absent member as "no tags".
func TestAContextWithoutTagsFailsTheCheckWhenACatalogExists(t *testing.T) {
	for _, listed := range []string{"package", "protocol"} {
		f := newFixture(t, catalog(listed, "stable"), "protocol")
		f.write("ts/lib/src/index.ts", waveOnlySource)
		f.commit("feat: drop the greeting")

		report, err := Check(f.root, f.project, projectName, nil)
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
			Project:       pctx.Project{Name: projectName, Path: "ts/lib", FullPath: f.project},
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
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting")
	f.tags = nil

	if report := f.check(); report.Skip != "" || len(report.Changes) != 1 {
		t.Fatalf("report = %+v, want the project checked and one change", report)
	}
	if status, _, _ := f.run(); status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
}

func TestALineWithoutAReachableTagPasses(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-line-without-a-reachable-tag-passes")
	f := newFixture(t, catalog("package", "stable"))
	f.git("tag", "-d", firstTag)
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting")

	status, data, events := f.run()
	if status != "OK" || data["compared"] != false {
		t.Fatalf("status = %q, data = %v, want OK without a comparison", status, data)
	}
	if found := diagnostics(events, runtime.SeverityWarning); len(found) != 0 {
		t.Fatalf("warnings = %+v, want none in a repository without tags", found)
	}

	// With tags of another line only, the line's own may not have been fetched.
	f.git("tag", "other/v1.0.0")
	_, _, events = f.run()
	warnings := diagnostics(events, runtime.SeverityWarning)
	if len(warnings) != 1 || warnings[0].Code != NotComparedCode ||
		!strings.Contains(warnings[0].Message, "no release tag of line ts/v{version} is reachable from HEAD, though the repository has other tags") {
		t.Fatalf("warnings = %+v", warnings)
	}
}

// A tag that is not a release of the line is never the baseline, however near
// HEAD it sits: the removal is compared with the release before it.
func TestATagThatIsNotAReleaseIsNoBaseline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.git("tag", "-d", firstTag)
	f.git("tag", "ts/v0.3.0")
	f.commit("chore: a later commit")
	f.git("tag", "ts/vnext-experiment")
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting")
	if report := f.check(); report.Tag != "ts/v0.3.0" || report.Warning != "" || len(report.Changes) != 1 {
		t.Fatalf("report = %+v, want the removal compared with ts/v0.3.0", report)
	}
}

func TestAPackageAbsentAtTheTagPasses(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-package-absent-at-the-tag-passes")
	f := newFixture(t, `{"protocolVersion":1,"entries":[{"id":"@example/new","kind":"package","status":"stable"}]}`)
	f.write("ts/new/putnami.json", `{"name":"@example/new","tags":["ts"]}`+"\n")
	f.write("ts/new/package.json", `{"name":"@example/new","main":"index.ts"}`+"\n")
	f.write("ts/new/index.ts", "export const fresh = 1;\n")
	f.commit("feat: a new package")

	report, err := Check(f.root, filepath.Join(f.root, "ts", "new"), "@example/new", []string{"ts"})
	if err != nil {
		t.Fatal(err)
	}
	// A catalog calls the package stable, and no directory of the tag holds
	// it: it may have released from a place the check did not find.
	if report.Tag != "" || report.Note != "" ||
		!strings.Contains(report.Warning, "@example/new is stable and the tag ts/v0.4.0 holds no package.json that declares it") {
		t.Fatalf("report = %+v, want a warning and no comparison", report)
	}

	// Without a catalog nothing calls the package stable: it is new.
	f.remove("putnami.support.json")
	report, err = Check(f.root, filepath.Join(f.root, "ts", "new"), "@example/new", []string{"ts"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Tag != "" || report.Warning != "" || !strings.Contains(report.Note, "has no package.json at ts/v0.4.0") {
		t.Fatalf("report = %+v, want a note and no comparison", report)
	}
}

func TestAPackageWhoseDirectoryMovedIsComparedWithItsOldDirectory(t *testing.T) {
	f := newFixture(t, catalog("package", "stable"))
	if err := os.Rename(f.project, filepath.Join(f.root, "ts", "moved")); err != nil {
		t.Fatal(err)
	}
	f.project = filepath.Join(f.root, "ts", "moved")
	f.commit("refactor: move the package")

	report := f.check()
	if report.Tag != firstTag || report.MovedFrom != "ts/lib" || report.Compared != 2 || len(report.Changes) != 0 {
		t.Fatalf("report = %+v, want a comparison with ts/lib and no change", report)
	}

	f.write("ts/moved/src/index.ts", waveOnlySource)
	if got := messages(f.check()); len(got) != 1 || got[0] != "@example/lib: function greet removed" {
		t.Fatalf("changes = %q, want the removal of greet", got)
	}

	// A breaking marker on a commit that touched only the old directory counts.
	f.commit("feat: drop the greeting")
	if report := f.check(); report.Breaking {
		t.Fatal("breaking = true, want no marker yet")
	}
	f.write("ts/lib/NOTES.md", "the package moved to ts/moved\n")
	f.commit("feat!: declared in the old directory")
	if changed, err := gitOutput(f.root, "show", "--name-only", "--format=", "HEAD"); err != nil || strings.TrimSpace(changed) != "ts/lib/NOTES.md" {
		t.Fatalf("the marker commit touches %q (%v), want only the old directory", changed, err)
	}
	if report := f.check(); !report.Breaking {
		t.Fatal("breaking = false, want the marker of the commit in the old directory")
	}
	f.remove("ts/lib/NOTES.md")

	// Two directories of the tag that declare the name give no baseline.
	f.write("ts/lib/package.json", manifest)
	f.write("ts/copy/package.json", manifest)
	f.write("ts/moved/src/index.ts", greetSource)
	f.commit("chore: two copies")
	f.git("tag", "ts/v0.5.0")
	f.remove("ts/lib/package.json")
	f.remove("ts/copy/package.json")
	f.remove("ts/moved/package.json")
	f.write("ts/again/package.json", manifest)
	f.project = filepath.Join(f.root, "ts", "again")
	if report := f.check(); !strings.Contains(report.Warning,
		"the tag ts/v0.5.0 holds 3 package.json files that declare @example/lib (ts/copy, ts/lib, ts/moved), so the check cannot tell which one released") {
		t.Fatalf("report = %+v, want a warning for an ambiguous baseline", report)
	}
}

func TestAPackageWithoutAnEntryPointIsNotCalledCompatible(t *testing.T) {
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/package.json", `{"name":"@example/lib"}`+"\n")
	f.commit("chore: declare no entry point")
	f.git("tag", "ts/v0.5.0")

	status, data, events := f.run()
	if status != "OK" || data["compared"] != false {
		t.Fatalf("status = %q, data = %v, want OK without a comparison", status, data)
	}
	warnings := diagnostics(events, runtime.SeverityWarning)
	if len(warnings) != 1 || warnings[0].Code != NotComparedCode || !strings.Contains(warnings[0].Message, "declares no entry point") {
		t.Fatalf("warnings = %+v", warnings)
	}
	for _, event := range events {
		if strings.Contains(event.Message, "is compatible") {
			t.Fatalf("the check called an API it did not compare compatible: %q", event.Message)
		}
	}
}

func TestAnEntryPointThatOnlyNamesAssetsIsNotCalledCompatible(t *testing.T) {
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/package.json", `{"name":"@example/lib","exports":{"./style.css":"./style.css"}}`+"\n")
	f.write("ts/lib/style.css", "a {}\n")
	f.commit("chore: ship a stylesheet only")
	f.git("tag", "ts/v0.5.0")

	status, data, events := f.run()
	if status != "OK" || data["compared"] != false || data["comparedEntryPoints"] != 0 {
		t.Fatalf("status = %q, data = %v, want OK without a comparison", status, data)
	}
	for _, event := range events {
		if strings.Contains(event.Message, "is compatible") {
			t.Fatalf("the check called an API it did not compare compatible: %q", event.Message)
		}
	}
}

func TestACommonJSEntryPointWarnsAndIsNotCompared(t *testing.T) {
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/src/index.ts", "module.exports = { greet() {} };\n")
	f.commit("chore: a CommonJS entry point")
	f.git("tag", "ts/v0.5.0")
	f.write("ts/lib/src/index.ts", "module.exports = {};\n")

	status, data, events := f.run()
	if status != "OK" || data["notCompared"] != 1 || data["comparedEntryPoints"] != 1 {
		t.Fatalf("status = %q, data = %v", status, data)
	}
	warnings := diagnostics(events, runtime.SeverityWarning)
	if len(warnings) != 1 || warnings[0].Code != NotComparedCode ||
		!strings.Contains(warnings[0].Message, "@example/lib: src/index.ts at ts/v0.5.0 has no import and no export, or assigns its exports with `export =`, so it is a script or a CommonJS module") {
		t.Fatalf("warnings = %+v", warnings)
	}
}

func TestAnEntryPointWithoutAnExtensionIsResolvedAndCompared(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/package.json", `{"name":"@example/lib","main":"./src/index","types":"src"}`+"\n")
	f.commit("chore: name the entry point without its extension")
	f.git("tag", "ts/v0.5.0")
	f.write("ts/lib/src/index.ts", waveOnlySource)

	report := f.check()
	// Both conditions name one subpath: its removal is reported once.
	want := []string{"@example/lib: function greet removed"}
	if got := messages(report); !slices.Equal(got, want) {
		t.Fatalf("changes = %q, want %q", got, want)
	}

	// A path that resolves to no source file is named in a warning.
	f.write("ts/lib/package.json", `{"name":"@example/lib","main":"./lib/index","types":"src"}`+"\n")
	f.write("ts/lib/src/index.ts", greetSource)
	report = f.check()
	if len(report.NotCompared) != 1 || !strings.Contains(report.NotCompared[0], "the entry point names lib/index, which the project does not hold") {
		t.Fatalf("not compared = %q", report.NotCompared)
	}
}

// Removing the declaration of the entry points does not end the comparison:
// every entry point the tag declared is gone.
func TestARemovedOrNarrowedManifestIsIncompatible(t *testing.T) {
	spectest.Proves(t, feature, requirement, "a-removed-manifest-or-entry-point-is-incompatible")
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/package.json", `{"name":"@example/lib","exports":{".":"./src/index.ts"}}`+"\n")
	if got := messages(f.check()); len(got) != 1 || got[0] != "@example/lib/hooks: entry point removed" {
		t.Fatalf("changes = %q, want the removed entry point", got)
	}

	f.remove("ts/lib/package.json")
	got := messages(f.check())
	if len(got) != 2 || got[0] != "@example/lib: entry point removed" || got[1] != "@example/lib/hooks: entry point removed" {
		t.Fatalf("changes = %q, want both entry points removed", got)
	}
	status, _, _ := f.run()
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
}

func TestAMovedSourceFileIsCompatible(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.remove("ts/lib/src/index.ts")
	f.write("ts/lib/lib/main.ts", "export { greet } from \"./greet.js\";\nexport function wave(): void {}\n")
	f.write("ts/lib/lib/greet.ts", "export function greet(name: string): string {\n  return name;\n}\n")
	f.write("ts/lib/package.json", strings.Replace(manifest, "./src/index.ts", "./lib/main.ts", 1))
	if report := f.check(); len(report.Changes) != 0 || len(report.NotCompared) != 0 {
		t.Fatalf("report = %+v, want a compatible move", report)
	}
}

// What no marker could fix is a warning: the check compares nothing rather
// than fail every run until the next tag.
func TestAReleasedPackageTheCheckCannotReadWarns(t *testing.T) {
	spectest.Proves(t, feature, requirement, "an-unreadable-release-warns-instead-of-failing")
	for name, breakIt := range map[string]func(f *fixture){
		"a package.json that does not parse": func(f *fixture) { f.write("ts/lib/package.json", "{not json\n") },
		"a source file that does not parse":  func(f *fixture) { f.write("ts/lib/src/index.ts", "export function (((\n") },
	} {
		f := newFixture(t, catalog("package", "stable"))
		breakIt(f)
		f.commit("chore: release something unreadable")
		f.git("tag", "ts/v0.5.0")
		f.write("ts/lib/package.json", manifest)
		f.write("ts/lib/src/index.ts", waveOnlySource)
		f.commit("feat: drop the greeting")

		status, data, events := f.run()
		if status != "OK" || data["compared"] != false {
			t.Fatalf("%s: status = %q, data = %v, want OK without a comparison", name, status, data)
		}
		warnings := diagnostics(events, runtime.SeverityWarning)
		if len(warnings) != 1 || warnings[0].Code != NotComparedCode || !strings.Contains(warnings[0].Message, "at ts/v0.5.0") ||
			!strings.Contains(warnings[0].Message, "so the API is not compared") {
			t.Fatalf("%s: warnings = %+v", name, warnings)
		}
	}
}

func TestAWorkingTreeTheCheckCannotReadFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/src/index.ts", "export function (((\n")
	if _, err := Check(f.root, f.project, projectName, f.tags); err == nil || !strings.Contains(err.Error(), "src/index.ts:1 does not parse") {
		t.Fatalf("err = %v, want the syntax error", err)
	}
	f.write("ts/lib/src/index.ts", greetSource)
	f.write("ts/lib/package.json", "{not json\n")
	if _, err := Check(f.root, f.project, projectName, f.tags); err == nil || !strings.Contains(err.Error(), "package.json does not parse") {
		t.Fatalf("err = %v, want the manifest error", err)
	}
}

func TestAnEntryPointOnUncommittedBuildOutputWarns(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/package.json", strings.Replace(manifest, "./src/hooks.ts", "./dist/hooks.js", 1))
	f.write("ts/lib/src/index.ts", waveOnlySource)
	f.commit("feat: drop the greeting and ship built hooks")

	status, data, events := f.run()
	if status != "FAILED" || data["notCompared"] != 1 || data["incompatible"] != 1 {
		t.Fatalf("status = %q, data = %v", status, data)
	}
	warnings := diagnostics(events, runtime.SeverityWarning)
	if len(warnings) != 1 || warnings[0].Code != NotComparedCode ||
		!strings.Contains(warnings[0].Message, "@example/lib/hooks: the entry point names dist/hooks.js, which the project does not hold") {
		t.Fatalf("warnings = %+v", warnings)
	}
}

func TestOnlySourceFilesGitWouldCommitAreRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("ts/lib/.gitignore", "generated\n")
	f.write("ts/lib/generated/x.ts", "export const x = 1;\n")
	f.write("ts/lib/node_modules/dep/index.ts", "export const dep = 1;\n")
	f.write("ts/lib/README.md", "# lib\n")
	f.write("ts/lib/src/new.ts", "export const fresh = 1;\n")
	files, err := filesInTree(f.project)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(paths(files), " "); got != "src/hooks.ts src/index.ts src/new.ts" {
		t.Fatalf("read %q", got)
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
	report, err := Check(root, project, projectName, []string{"ts"})
	if err != nil || !strings.Contains(report.Warning, "not in a git repository") || report.Note != "" {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
}

func TestARepositoryWithoutACommitHasNothingToCompare(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	report, err := Check(root, root, projectName, []string{"ts"})
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
	report, err := Check(clone, filepath.Join(clone, "ts", "lib"), projectName, f.tags)
	if err != nil || !strings.Contains(report.Warning, "shallow") {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
}

func TestWithoutTheTypeScriptPackageTheCheckFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	if err := os.RemoveAll(filepath.Join(f.root, "node_modules")); err != nil {
		t.Fatal(err)
	}
	_, err := Check(f.root, f.project, projectName, f.tags)
	if err == nil || !strings.Contains(err.Error(), "the typescript package is not installed") || !strings.Contains(err.Error(), "putnami install") {
		t.Fatalf("err = %v, want the missing parser", err)
	}
}

func TestLinePatternFollowsTheNearestLineScope(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	if got := linePattern(f.root, f.project); got != "ts/v{version}" {
		t.Fatalf("linePattern = %q", got)
	}
	f.write("ts/putnami.json", `{"line":{"tag":"lib-v{version}"},"includes":["lib"]}`)
	if got := linePattern(f.root, f.project); got != "lib-v{version}" {
		t.Fatalf("linePattern = %q, want the declared tag", got)
	}
	f.write("ts/putnami.json", `{"includes":["lib"]}`)
	if got := linePattern(f.root, f.project); got != "v{version}" {
		t.Fatalf("linePattern = %q, want the root line", got)
	}
}
