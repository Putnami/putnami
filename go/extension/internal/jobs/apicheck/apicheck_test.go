package apicheck

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	tags, err := json.Marshal(append([]string{"go"}, projectTags...))
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
	report, err := Check(f.root, f.project, projectName)
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
		Project:       pctx.Project{Name: projectName, Path: "lib/greet", FullPath: f.project},
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
	if len(infos) != 1 || !strings.Contains(infos[0].Message, "func Greet removed since lib/v0.4.0, declared breaking by") ||
		!strings.Contains(infos[0].Message, `"feat!: drop the greeting"`) {
		t.Fatalf("info diagnostics = %+v", infos)
	}
	if data["breakingCommit"] == "" || data["breakingCommit"] == nil {
		t.Fatalf("data = %v, want the breaking commit", data)
	}
}

func TestABreakingChangeFooterAllowsTheChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/greet.go", waveOnlySource)
	f.commit("refactor: drop the greeting\n\nBREAKING CHANGE: Greet is gone")
	report := f.check()
	if len(report.Changes) != 1 || report.Breaking == nil || report.Breaking.Subject != "refactor: drop the greeting" {
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
	if len(report.Changes) != 1 || report.Breaking != nil {
		t.Fatalf("report = %+v, want one change and no marker", report)
	}
}

func TestAnUncommittedRemovalIsChecked(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.write("lib/greet/greet.go", waveOnlySource)
	report := f.check()
	if len(report.Changes) != 1 || report.Changes[0].Symbol != "func Greet" || report.Breaking != nil {
		t.Fatalf("report = %+v", report)
	}
}

func TestAPreviewOrExperimentalProjectIsSkipped(t *testing.T) {
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

func TestTheProtocolTagSelectsTheProtocolSubject(t *testing.T) {
	t.Parallel()
	listedAsPackage := newFixture(t, catalog("package", "experimental"), "protocol")
	listedAsPackage.write("lib/greet/greet.go", waveOnlySource)
	if report := listedAsPackage.check(); report.Skip != "" || len(report.Changes) != 1 {
		t.Fatalf("a protocol project the catalog lists only as a package is unlisted, so stable: %+v", report)
	}
	listedAsProtocol := newFixture(t, catalog("protocol", "experimental"), "protocol")
	if report := listedAsProtocol.check(); report.Skip == "" {
		t.Fatalf("report = %+v, want the protocol entry to skip it", report)
	}
}

func TestScopeTagsApplyWhenTheProjectDeclaresNone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	project := filepath.Join(root, "protocols", "lib")
	for path, content := range map[string]string{
		"protocols/putnami.json":     `{"tags":["protocol"],"includes":["lib"]}`,
		"protocols/lib/putnami.json": `{"name":"` + projectName + `"}`,
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if tags := projectTags(root, project); len(tags) != 1 || tags[0] != "protocol" {
		t.Fatalf("tags = %v, want the scope's", tags)
	}
}

func TestAWorkspaceWithoutACatalogChecksEveryProject(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	f.write("lib/greet/greet.go", waveOnlySource)
	if report := f.check(); report.Skip != "" || len(report.Changes) != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestAnInvalidCatalogFailsTheTask(t *testing.T) {
	t.Parallel()
	f := newFixture(t, `{"protocolVersion":1,"entries":[{"id":"x","kind":"package","status":"beta"}]}`)
	_, err := Check(f.root, f.project, projectName)
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
	f.git("tag", "other/v1.0.0")
	f.write("lib/greet/greet.go", waveOnlySource)

	status, data, events := f.run()
	if status != "OK" || data["compared"] != false {
		t.Fatalf("status = %q, data = %v, want OK without a comparison", status, data)
	}
	if found := diagnostics(events, runtime.SeverityError); len(found) != 0 {
		t.Fatalf("error diagnostics = %+v, want none", found)
	}
	if report := f.check(); !strings.Contains(report.Note, "no tag of line lib/v{version} is reachable") {
		t.Fatalf("report = %+v", report)
	}
}

// The nearest tag the line's glob matches is the baseline candidate, as it is
// for the version bump: when the line's pattern could not render it, the line
// has no baseline, even with an older renderable tag behind it.
func TestATagTheLineCouldNotRenderIsNoBaseline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, catalog("package", "stable"))
	f.commit("chore: a later commit")
	f.git("tag", "lib/v")
	f.write("lib/greet/greet.go", waveOnlySource)
	if report := f.check(); report.Note == "" || report.Tag != "" {
		t.Fatalf("report = %+v, want no baseline", report)
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
	report, err := Check(root, project, projectName)
	if err != nil || !strings.Contains(report.Note, "not in a git repository") {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
}

func TestARepositoryWithoutACommitHasNothingToCompare(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	report, err := Check(root, root, projectName)
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
	report, err := Check(clone, filepath.Join(clone, "lib", "greet"), projectName)
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

func TestAnUnusableTagPatternIsAnError(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{"v*", "lib/v{version}/{version}", "-v{version}", "no-placeholder"} {
		if _, _, err := lastReachableTag(t.TempDir(), pattern); err == nil {
			t.Errorf("lastReachableTag(%q) succeeded, want an error", pattern)
		}
	}
}
