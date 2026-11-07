package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	extension "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/gittest"
	"go.putnami.dev/tooling/observe/internal/observe"
)

const feature = "tooling/observe"

func readManifest(t *testing.T) *extension.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	manifest, diags := extension.ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("the manifest does not parse strictly: %v", diags)
	}
	return manifest
}

func TestTheManifestDeclaresAWorkspaceOptionalCommand(t *testing.T) {
	spectest.Proves(t, feature, "runs-without-a-workspace", "the-manifest-declares-a-workspace-optional-command")
	manifest := readManifest(t)
	if manifest.Name != extensionName {
		t.Fatalf("manifest name %q, runtime answers as %q", manifest.Name, extensionName)
	}
	if diags := extension.ValidateManifest(manifest); diag.HasErrors(diags) {
		t.Fatalf("the manifest fails extension validation: %v", diags)
	}
	group, ok := manifest.CommandGroups["observe"]
	if !ok {
		t.Fatal("the manifest declares no observe group")
	}
	sub, ok := group.Subcommands[group.Default]
	if !ok {
		t.Fatalf("the observe group's default %q names no subcommand", group.Default)
	}
	if !sub.RunsWithoutWorkspace() {
		t.Fatal("putnami observe does not run outside a workspace")
	}
	command := manifest.Commands[sub.Command]
	flags := make([]string, 0, len(command.Flags))
	for name := range command.Flags {
		flags = append(flags, name)
	}
	slices.Sort(flags)
	want := slices.Clone(knownFlags)
	slices.Sort(want)
	if !slices.Equal(flags, want) {
		t.Fatalf("the manifest declares flags %v, the runtime reads %v", flags, want)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// The fixture's commits run no automatic maintenance, so every git
	// process has exited when the test removes its directories.
	cmd := gittest.Command(dir, append([]string{"-c", "maintenance.auto=false", "-c", "gc.auto=0"}, args...)...)
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// jobContext writes the job context the CLI gives a user-scope launch: no
// workspace, the caller's directory, and the output global.
func jobContext(t *testing.T, callerDir, invocationDir, output string) string {
	t.Helper()
	params := map[string]any{}
	if output != "" {
		params["output"] = output
	}
	data, err := json.Marshal(map[string]any{
		"protocolVersion": 2,
		"workspaceRoot":   t.TempDir(),
		"outputPath":      t.TempDir(),
		"cacheRoot":       t.TempDir(),
		"workspace":       map[string]any{"name": "user"},
		"project":         map[string]any{"name": "user", "path": "."},
		"extension":       map[string]any{"name": extensionName},
		"job":             map[string]any{"name": "observe-run"},
		"userScope":       map[string]any{"callerDir": callerDir},
		"invocationDir":   invocationDir,
		"params":          params,
	})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "context.json")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// workspacelessRepo is a package.json repository with no Putnami workspace:
// a feature branch one commit past main, a report under an ignored directory.
func workspacelessRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "shop")
	git(t, t.TempDir(), "init", "--quiet", "--initial-branch=main", repo)
	write(t, repo, "package.json", `{"name":"shop","workspaces":["packages/*"]}`)
	write(t, repo, "packages/web/index.js", "export {}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "--quiet", "-m", "root")
	git(t, repo, "checkout", "--quiet", "-b", "feature")
	write(t, repo, "packages/web/index.js", "export const a = 1\n")
	git(t, repo, "commit", "--quiet", "-am", "change")
	write(t, repo, "packages/web/reports/junit.xml", `<testsuites><testsuite name="web"/></testsuites>`)
	write(t, repo, ".gitignore", "reports/\n")
	git(t, repo, "add", ".gitignore")
	git(t, repo, "commit", "--quiet", "-m", "ignore reports")
	return repo
}

func TestWorkspacelessRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string { return workspacelessRepo(t) })
}

func TestObserveRunsInARepositoryWithoutAWorkspace(t *testing.T) {
	spectest.Proves(t, feature, "runs-without-a-workspace", "observe-runs-in-a-repository-without-a-workspace")
	spectest.Proves(t, feature, "caller-directory-writes", "observe-writes-only-git-history-in-the-caller-directory")
	repo := workspacelessRepo(t)
	before := git(t, repo, "status", "--porcelain=v2", "--ignored", "--branch", "--untracked-files=all")
	if _, err := os.Stat(filepath.Join(repo, "putnami.workspace.json")); !os.IsNotExist(err) {
		t.Fatal("the fixture must not be a Putnami workspace")
	}

	invocation := filepath.Join(repo, "packages", "web")
	t.Chdir(invocation)
	env := map[string]string{
		"GITLAB_CI":                           "true",
		"CI_SERVER_HOST":                      "gitlab.com",
		"CI_PROJECT_ID":                       "4242",
		"CI_PROJECT_PATH":                     "acme/shop",
		"CI_COMMIT_SHA":                       strings.Repeat("c", 40),
		"CI_MERGE_REQUEST_IID":                "7",
		"CI_MERGE_REQUEST_DIFF_BASE_SHA":      git(t, repo, "rev-parse", "main"),
		"CI_MERGE_REQUEST_TARGET_BRANCH_NAME": "main",
		"CI_MERGE_REQUEST_SOURCE_BRANCH_NAME": "feature",
		"CI_PIPELINE_ID":                      "1",
		"CI_JOB_ID":                           "2",
	}
	var stdout, stderr bytes.Buffer
	evidencePath := filepath.Join(t.TempDir(), "evidence.json")
	code := run([]string{"run", "--junit", "reports/*.xml", "--evidence", evidencePath, "--putnamiContext", jobContext(t, invocation, invocation, "json")},
		func(k string) string { return env[k] }, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var o observe.Observation
	if err := json.Unmarshal(stdout.Bytes(), &o); err != nil {
		t.Fatalf("output is not one JSON document: %v\n%s", err, stdout.String())
	}
	if o.Subject.ProjectID != "4242" || o.Subject.Host != "gitlab.com" || o.Subject.ChangeRequest == nil || o.Subject.ChangeRequest.Number != 7 {
		t.Fatalf("subject %+v", o.Subject)
	}
	if o.Subject.BaseSHA != git(t, repo, "rev-parse", "main") || o.Subject.SHA != git(t, repo, "rev-parse", "HEAD") {
		t.Fatalf("base %s sha %s", o.Subject.BaseSHA, o.Subject.SHA)
	}
	changed := make([]string, 0, len(o.ChangedFiles))
	for _, f := range o.ChangedFiles {
		changed = append(changed, f.Status+" "+f.Path)
	}
	if want := []string{"added .gitignore", "modified packages/web/index.js"}; !slices.Equal(changed, want) {
		t.Fatalf("changed %v, want %v", changed, want)
	}
	if len(o.Reports) != 1 || o.Reports[0].Path != "packages/web/reports/junit.xml" {
		t.Fatalf("reports %+v", o.Reports)
	}
	codes := make([]string, 0, len(o.Diagnostics))
	for _, d := range o.Diagnostics {
		codes = append(codes, d.Code)
	}
	if !slices.Equal(codes, []string{"observe.report_missing"}) {
		t.Fatalf("diagnostics %v: the coverage report is missing and nothing else", o.Diagnostics)
	}
	data, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatalf("no evidence document: %v", err)
	}
	if doc, problems := evidence.ParseAndValidate(data); diag.HasErrors(problems) || doc.Subject.BaseSHA != o.Subject.BaseSHA || len(doc.ChangedFiles) != 2 {
		t.Fatalf("evidence %s: %v", data, problems)
	}
	if after := git(t, repo, "status", "--porcelain=v2", "--ignored", "--branch", "--untracked-files=all"); after != before {
		t.Fatalf("observe changed the caller's directory:\nbefore %s\nafter  %s", before, after)
	}

	stdout.Reset()
	if code := run([]string{"run", "--no-fetch", "--putnamiContext", jobContext(t, invocation, invocation, "")}, func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("text run exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"putnami observe: GitLab CI, gitlab.com/acme/shop (project 4242)", "merge request !7", "merge base with main from the merge request", "modified  packages/web/index.js", "junit     packages/web/reports/junit.xml"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("the summary lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestTheMoonGraphFlagIsResolvedAgainstTheInvocationDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	opts, _, err := parseRun([]string{"--moon-graph", "out/moon-graph.json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "out", "moon-graph.json"); opts.MoonGraph != want {
		t.Fatalf("moon graph %q, want %q", opts.MoonGraph, want)
	}
	if opts, _, _ := parseRun(nil, nil); opts.MoonGraph != "" {
		t.Fatalf("moon graph %q without the flag", opts.MoonGraph)
	}
}

func TestAUsageErrorExitsTwo(t *testing.T) {
	spectest.Proves(t, feature, "report-discovery", "a-missing-or-malformed-report-is-a-diagnostic")
	t.Chdir(t.TempDir())
	for name, args := range map[string][]string{
		"no subcommand":       {},
		"another command":     {"verify"},
		"an unknown flag":     {"run", "--jnuit", "a.xml"},
		"a positional":        {"run", "extra"},
		"an empty base":       {"run", "--base"},
		"a fetch value":       {"run", "--fetch=maybe"},
		"an empty moon graph": {"run", "--moon-graph"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(args, func(string) string { return "" }, &stdout, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2: %s", code, stderr.String())
			}
		})
	}

	// A job context that cannot be read is the runtime's failure, not a usage error.
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--putnamiContext", filepath.Join(t.TempDir(), "missing.json")}, func(string) string { return "" }, &out, &errOut); code != 1 {
		t.Fatalf("an unreadable job context exits %d, want 1: %s", code, errOut.String())
	}

	// Outside a repository, with nothing to read, observe still exits 0.
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--base", "main"}, func(string) string { return "" }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "observe.not_a_repository") {
		t.Fatalf("output %s", stdout.String())
	}
}

func TestTheRuntimeAnswersTheHandshake(t *testing.T) {
	spectest.Proves(t, feature, "runs-without-a-workspace", "the-manifest-declares-a-workspace-optional-command")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"__putnami", "runtime-info"}, os.Getenv, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var info struct {
		Extension string `json:"extension"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil || info.Extension != extensionName {
		t.Fatalf("handshake %s (%v)", stdout.String(), err)
	}
}

// uploadFixture is a GitLab merge request job in a repository without a
// workspace, and a fake CLI that records what it gets and exits with code.
func uploadFixture(t *testing.T, code string) (repo string, env map[string]string, record string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "shop")
	git(t, t.TempDir(), "init", "--quiet", "--initial-branch=main", repo)
	write(t, repo, "README.md", "shop\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "--quiet", "-m", "root")
	git(t, repo, "checkout", "--quiet", "-b", "feature")
	write(t, repo, "README.md", "shop!\n")
	git(t, repo, "commit", "--quiet", "-am", "change")
	record = t.TempDir()
	cli := filepath.Join(t.TempDir(), "putnami")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + record + "/args'\n" +
		"cp \"$4\" '" + record + "/document'\n" +
		"echo uploaded\n" +
		"exit " + code + "\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env = map[string]string{
		"GITLAB_CI":                           "true",
		"CI_SERVER_HOST":                      "gitlab.com",
		"CI_PROJECT_ID":                       "4242",
		"CI_PROJECT_PATH":                     "acme/shop",
		"CI_COMMIT_SHA":                       git(t, repo, "rev-parse", "HEAD"),
		"CI_MERGE_REQUEST_IID":                "7",
		"CI_MERGE_REQUEST_DIFF_BASE_SHA":      git(t, repo, "rev-parse", "main"),
		"CI_MERGE_REQUEST_TARGET_BRANCH_NAME": "main",
		"CI_MERGE_REQUEST_SOURCE_BRANCH_NAME": "feature",
		"CI_PIPELINE_ID":                      "1",
		"CI_JOB_ID":                           "2",
		"PUTNAMI_CLI_EXECUTABLE":              cli,
	}
	return repo, env, record
}

func TestUploadFixtureIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		repo, _, _ := uploadFixture(t, "0")
		return repo
	})
}

func TestUploadHandsTheEvidenceDocumentToPutnamiCloud(t *testing.T) {
	spectest.Proves(t, "tooling/observe-upload", "upload-hands-the-document-to-the-reporter", "upload-runs-the-selected-reporter-with-the-document")
	spectest.Proves(t, "tooling/observe-upload", "upload-hands-the-document-to-the-reporter", "an-unset-selector-picks-putnami-cloud")
	repo, env, record := uploadFixture(t, "0")
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--no-fetch", "--upload", "--putnamiContext", jobContext(t, repo, repo, "json")},
		func(k string) string { return env[k] }, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var o observe.Observation
	if err := json.Unmarshal(stdout.Bytes(), &o); err != nil {
		t.Fatalf("the reporter's output reached stdout: %v\n%s", err, stdout.String())
	}
	for _, d := range o.Diagnostics {
		if strings.HasPrefix(d.Code, "observe.upload") || strings.HasPrefix(d.Code, "observe.evidence") {
			t.Fatalf("diagnostic %+v", d)
		}
	}
	data, err := os.ReadFile(filepath.Join(record, "args"))
	if err != nil {
		t.Fatalf("the reporter did not run: %v", err)
	}
	args := strings.Fields(string(data))
	if len(args) != 4 || !slices.Equal(args[:3], []string{"cloud", "evidence-report", "--file"}) {
		t.Fatalf("reporter args %v", args)
	}
	if _, err := os.Stat(args[3]); !os.IsNotExist(err) {
		t.Fatalf("the temporary document %s was not removed: %v", args[3], err)
	}
	document, err := os.ReadFile(filepath.Join(record, "document"))
	if err != nil {
		t.Fatal(err)
	}
	if doc, problems := evidence.ParseAndValidate(document); diag.HasErrors(problems) || doc.Subject.ProjectID != "4242" {
		t.Fatalf("evidence %s: %v", document, problems)
	}
	if !strings.Contains(stderr.String(), "uploaded") {
		t.Fatalf("the reporter's output did not reach stderr: %q", stderr.String())
	}

	kept := filepath.Join(t.TempDir(), "evidence.json")
	stdout.Reset()
	if code := run([]string{"run", "--no-fetch", "--upload", "--evidence", kept, "--putnamiContext", jobContext(t, repo, repo, "json")},
		func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	data, _ = os.ReadFile(filepath.Join(record, "args"))
	if args := strings.Fields(string(data)); len(args) != 4 || args[3] != kept {
		t.Fatalf("with --evidence the reporter gets that path, got %v", args)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("the --evidence document was removed: %v", err)
	}
}

func TestAFailedUploadIsADiagnosticAndExitsZero(t *testing.T) {
	spectest.Proves(t, "tooling/observe-upload", "upload-fails-open", "a-failed-upload-is-a-diagnostic")
	repo, env, _ := uploadFixture(t, "1")
	t.Chdir(repo)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"run", "--no-fetch", "--upload", "--putnamiContext", jobContext(t, repo, repo, "json")},
		func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("a refused upload exits %d", code)
	}
	var o observe.Observation
	if err := json.Unmarshal(stdout.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(o.Diagnostics, func(d diag.Diagnostic) bool { return d.Code == "observe.upload_failed" }) {
		t.Fatalf("diagnostics %v lack observe.upload_failed", o.Diagnostics)
	}

	// A document that does not validate is not handed to the reporter.
	delete(env, "CI_PROJECT_ID")
	stdout.Reset()
	if code := run([]string{"run", "--no-fetch", "--upload", "--putnamiContext", jobContext(t, repo, repo, "json")},
		func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
		t.Fatalf("an unwritten document exits %d", code)
	}
	o = observe.Observation{}
	if err := json.Unmarshal(stdout.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(o.Diagnostics))
	for _, d := range o.Diagnostics {
		got = append(got, d.Code)
	}
	if !slices.Contains(got, "observe.evidence_invalid") || !slices.Contains(got, "observe.upload_failed") {
		t.Fatalf("diagnostics %v", got)
	}

	if code := run([]string{"run", "--upload=yes"}, func(k string) string { return env[k] }, &stdout, &stderr); code != 2 {
		t.Fatalf("--upload=yes exits %d, want 2", code)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestPublishPostsTheSummaryOnTheMergeRequest(t *testing.T) {
	spectest.Proves(t, "tooling/observe-host-report", "publish-fails-open", "a-missing-token-publishes-nothing")
	spectest.Proves(t, "tooling/observe-host-report", "publish-note", "a-gitlab-merge-request-gets-a-status-and-a-note")
	repo, env, _ := uploadFixture(t, "0")
	t.Chdir(repo)
	var requests []string
	var note string
	publishClient = &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.Path)
		answer := `{"id":1}`
		switch {
		case req.Method == http.MethodGet:
			answer = `[]`
		case strings.HasSuffix(req.URL.Path, "/notes"):
			var body map[string]string
			_ = json.NewDecoder(req.Body).Decode(&body)
			note = body["body"]
		}
		return &http.Response{StatusCode: 201, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(answer)), Request: req}, nil
	})}
	t.Cleanup(func() { publishClient = nil })

	observeJSON := func() observe.Observation {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"run", "--no-fetch", "--publish", "--putnamiContext", jobContext(t, repo, repo, "json")},
			func(k string) string { return env[k] }, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		var o observe.Observation
		if err := json.Unmarshal(stdout.Bytes(), &o); err != nil {
			t.Fatalf("the publication's output reached stdout: %v\n%s", err, stdout.String())
		}
		return o
	}

	o := observeJSON()
	if len(requests) != 0 || !slices.ContainsFunc(o.Diagnostics, func(d diag.Diagnostic) bool { return d.Code == "observe.publish_token_missing" }) {
		t.Fatalf("without a token: requests %v, diagnostics %v", requests, o.Diagnostics)
	}

	env["PUTNAMI_GITLAB_TOKEN"] = "glpat-test"
	env["CI_API_V4_URL"] = "https://gitlab.com/api/v4"
	o = observeJSON()
	for _, d := range o.Diagnostics {
		if strings.HasPrefix(d.Code, "observe.publish") {
			t.Fatalf("diagnostic %+v", d)
		}
	}
	head := env["CI_COMMIT_SHA"]
	want := []string{
		"POST /api/v4/projects/4242/statuses/" + head,
		"GET /api/v4/projects/4242/merge_requests/7/notes",
		"POST /api/v4/projects/4242/merge_requests/7/notes",
	}
	if !slices.Equal(requests, want) || !strings.HasPrefix(note, "<!-- putnami-observe:summary -->\n") {
		t.Fatalf("requests %v, note %q", requests, note)
	}
}

func TestThePantsGraphFlagNamesTheExport(t *testing.T) {
	spectest.Proves(t, "tooling/observe-pants", "pants-graph-flag", "the-pants-graph-flag-names-the-export")
	dir := t.TempDir()
	t.Chdir(dir)
	opts, _, err := parseRun([]string{"--pants-graph", "dist/pants-graph.json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "dist", "pants-graph.json"); opts.PantsGraph != want {
		t.Errorf("PantsGraph %q, want %q", opts.PantsGraph, want)
	}
	if opts, _, err := parseRun(nil, nil); err != nil || opts.PantsGraph != "" {
		t.Errorf("without the flag: %q, %v", opts.PantsGraph, err)
	}
	if _, _, err := parseRun([]string{"--pants-graph"}, nil); err == nil || !strings.Contains(err.Error(), "--pants-graph needs a value") {
		t.Errorf("an empty --pants-graph: %v", err)
	}
}

func TestTheBazelGraphFlagNamesTheExport(t *testing.T) {
	spectest.Proves(t, "tooling/observe-bazel", "bazel-packages-are-components", "a-query-export-gives-the-packages-and-their-edges")
	dir := t.TempDir()
	t.Chdir(dir)
	opts, _, err := parseRun([]string{"--bazel-graph", "ci/graph.json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "ci", "graph.json"); opts.BazelGraph != want {
		t.Errorf("BazelGraph %q, want %q", opts.BazelGraph, want)
	}
	if opts, _, err := parseRun(nil, nil); err != nil || opts.BazelGraph != "" {
		t.Errorf("without the flag: %q, %v", opts.BazelGraph, err)
	}
	if _, _, err := parseRun([]string{"--bazel-graph"}, nil); err == nil {
		t.Error("an empty --bazel-graph is no usage error")
	}
}

func TestTheDecisionsFlagNamesTheDirectories(t *testing.T) {
	spectest.Proves(t, "tooling/observe-decisions", "madr-records", "records-are-the-markdown-files-of-the-decision-directories")
	t.Chdir(t.TempDir())
	opts, _, err := parseRun([]string{"--decisions", "architecture/decisions,docs/rfcs"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"architecture/decisions", "docs/rfcs"}; !slices.Equal(opts.Decisions, want) {
		t.Errorf("Decisions %v, want %v", opts.Decisions, want)
	}
	if opts, _, err := parseRun(nil, nil); err != nil || opts.Decisions != nil {
		t.Errorf("without the flag: %v, %v", opts.Decisions, err)
	}
	if _, _, err := parseRun([]string{"--decisions"}, nil); err == nil || !strings.Contains(err.Error(), "--decisions needs a value") {
		t.Errorf("an empty --decisions: %v", err)
	}
}
